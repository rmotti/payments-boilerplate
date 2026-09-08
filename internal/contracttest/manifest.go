package contracttest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// CoverageEntry records the lowest test layer that proves one declared
// operation/status pair. A status belongs in exactly one entry; E2E does not
// need to duplicate behavior already proven by a handler or integration test.
type CoverageEntry struct {
	OperationID string
	Status      int
	Layer       string
	CoveredBy   string
}

// ValidateManifest requires an exact operation/status match with the spec.
// Consequently, adding a status such as 429 breaks the contract suite until a
// test is deliberately assigned to it.
func ValidateManifest(document *openapi3.T, entries []CoverageEntry) error {
	want := make(map[string]struct{})
	for _, pathItem := range document.Paths.Map() {
		for _, operation := range pathItem.Operations() {
			for status := range operation.Responses.Map() {
				if status == "default" {
					continue
				}
				want[operation.OperationID+" "+status] = struct{}{}
			}
		}
	}

	got := make(map[string]struct{}, len(entries))
	var problems []string
	for _, entry := range entries {
		key := entry.OperationID + " " + strconv.Itoa(entry.Status)
		if entry.OperationID == "" || entry.Status < 100 || entry.Status > 599 {
			problems = append(problems, "invalid entry "+key)
		}
		if entry.Layer != "handler" && entry.Layer != "integration" && entry.Layer != "e2e" {
			problems = append(problems, key+": invalid layer "+entry.Layer)
		}
		if strings.TrimSpace(entry.CoveredBy) == "" {
			problems = append(problems, key+": CoveredBy is empty")
		}
		if _, duplicate := got[key]; duplicate {
			problems = append(problems, key+": duplicate entry")
		}
		got[key] = struct{}{}
	}
	for key := range want {
		if _, exists := got[key]; !exists {
			problems = append(problems, key+": missing coverage")
		}
	}
	for key := range got {
		if _, exists := want[key]; !exists {
			problems = append(problems, key+": not declared in OpenAPI")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("invalid contract coverage manifest:\n- %s", strings.Join(problems, "\n- "))
}
