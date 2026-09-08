package repositories

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	dbgen "github.com/rmotti/payments-boilerplate/internal/adapters/postgres/queries"
)

// TestOperationalQueriesNeverSelectStoredPayloads guards the E8A-4 invariant:
// no query the operational API runs may return raw_payload or payload.
// Both columns can carry full customer data from the provider (see
// docs/decisions/0017-sensitive-data-and-error-handling.md), and the
// operational surface is reachable by any integrator holding a valid API
// key. This is checked two ways so a regression cannot slip past either:
//
//  1. The generated sqlc row types for the operational queries are inspected
//     by reflection. If a future edit to db/queries/operations.sql ever adds
//     raw_payload or payload to a SELECT list, sqlc regenerates a row struct
//     carrying a RawPayload or Payload field, and this test fails without
//     needing to know the query text.
//  2. The source SQL file itself is scanned so the invariant is also visible
//     at the place a reviewer would look to change it.
func TestOperationalQueriesNeverSelectStoredPayloads(t *testing.T) {
	t.Run("generated row types", func(t *testing.T) {
		forbidden := []string{"RawPayload", "Payload"}
		rowTypes := []any{
			dbgen.ListWebhookEventsRow{},
			dbgen.GetWebhookEventRow{},
		}
		for _, row := range rowTypes {
			typ := reflect.TypeOf(row)
			for i := 0; i < typ.NumField(); i++ {
				name := typ.Field(i).Name
				for _, bad := range forbidden {
					if name == bad {
						t.Fatalf("%s has field %q: an operational query row must never carry a stored payload column",
							typ.Name(), name)
					}
				}
			}
		}
	})

	t.Run("source SQL", func(t *testing.T) {
		path := operationsSQLPath(t)
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, block := range splitSQLStatements(string(contents)) {
			if !strings.Contains(strings.ToUpper(block), "SELECT") {
				continue
			}
			if columnReferenced(block, "raw_payload") {
				t.Fatalf("a statement in %s selects raw_payload:\n%s", path, block)
			}
			if columnReferenced(block, "payload") {
				t.Fatalf("a statement in %s selects payload:\n%s", path, block)
			}
		}
	})
}

// splitSQLStatements breaks the file into its ";"-terminated statements. It
// is intentionally naive — the operational queries file has no string
// literals containing ";" — which keeps the check readable as a source
// scan rather than a SQL parser.
func splitSQLStatements(source string) []string {
	parts := strings.Split(source, ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			statements = append(statements, part)
		}
	}
	return statements
}

var columnWordPattern = regexp.MustCompile(`(?i)\b(raw_payload|payload)\b`)

// columnReferenced reports whether column appears as a column reference
// (bare, or table-qualified like "e.payload") in a SQL statement, ignoring
// occurrences inside "--" comments.
func columnReferenced(statement, column string) bool {
	for _, line := range strings.Split(statement, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		for _, match := range columnWordPattern.FindAllString(line, -1) {
			if strings.EqualFold(match, column) {
				return true
			}
		}
	}
	return false
}

func operationsSQLPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve operations_sql_test.go source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "..", ".."))
	return filepath.Join(root, "db", "queries", "operations.sql")
}
