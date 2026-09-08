package contracttest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const contractFencePrefix = "```json contract "

// MarkdownExample is a JSON fence explicitly marked as contractual. Ordinary
// JSON fences remain illustrative and are intentionally ignored.
type MarkdownExample struct {
	Name        string
	OperationID string
	Direction   string
	Status      string
	Value       any
}

// LoadMarkdownExamples finds marked fences by their metadata, independent of
// heading, line number or textual position. The supported marker is:
// ```json contract operation=<operationId> direction=<request|response> status=<code|-> name=<stable-name>
func LoadMarkdownExamples(path string) ([]MarkdownExample, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Markdown contract: %w", err)
	}
	defer func() { _ = file.Close() }()

	var examples []MarkdownExample
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, contractFencePrefix) {
			if strings.HasPrefix(line, "```json contract") {
				return nil, fmt.Errorf("invalid contract fence marker %q", line)
			}
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, contractFencePrefix))
		metadata := make(map[string]string, len(fields))
		for _, field := range fields {
			key, value, ok := strings.Cut(field, "=")
			if !ok || value == "" {
				return nil, fmt.Errorf("invalid contract fence metadata %q", line)
			}
			if _, duplicate := metadata[key]; duplicate {
				return nil, fmt.Errorf("duplicate contract fence metadata %q in %q", key, line)
			}
			metadata[key] = value
		}
		if metadata["operation"] == "" || metadata["direction"] == "" ||
			metadata["status"] == "" || metadata["name"] == "" {
			return nil, fmt.Errorf("contract fence requires operation, direction, status and name: %q", line)
		}
		if metadata["direction"] != "request" && metadata["direction"] != "response" {
			return nil, fmt.Errorf("contract fence %q has invalid direction %q", metadata["name"], metadata["direction"])
		}
		if metadata["direction"] == "request" && metadata["status"] != "-" {
			return nil, fmt.Errorf("request contract fence %q must use status=-", metadata["name"])
		}
		if metadata["direction"] == "response" && metadata["status"] == "-" {
			return nil, fmt.Errorf("response contract fence %q requires a status", metadata["name"])
		}

		var source strings.Builder
		closed := false
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "```" {
				closed = true
				break
			}
			source.WriteString(scanner.Text())
			source.WriteByte('\n')
		}
		if !closed {
			return nil, fmt.Errorf("unclosed contract fence %q", metadata["name"])
		}
		var value any
		if err := json.Unmarshal([]byte(source.String()), &value); err != nil {
			return nil, fmt.Errorf("decode contract fence %q: %w", metadata["name"], err)
		}
		examples = append(examples, MarkdownExample{
			Name: metadata["name"], OperationID: metadata["operation"],
			Direction: metadata["direction"], Status: metadata["status"], Value: value,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan Markdown contract: %w", err)
	}
	return examples, nil
}
