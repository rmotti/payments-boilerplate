package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// dashboardPath is the versioned Grafana dashboard. The tests below are what
// keeps it honest: a dashboard is only useful if its queries name instruments
// that exist and labels that are allowed.
func dashboardPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the metrics package source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	return filepath.Join(root, "deployments/observability/payments-pipeline.json")
}

type dashboard struct {
	Title  string `json:"title"`
	UID    string `json:"uid"`
	Panels []struct {
		Type    string `json:"type"`
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

func loadDashboard(t *testing.T) dashboard {
	t.Helper()
	data, err := os.ReadFile(dashboardPath(t))
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	var parsed dashboard
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}
	return parsed
}

// promName is how the OTLP-to-Prometheus translation renames an instrument:
// dots become underscores, the second unit suffix is appended, and a counter
// gains "_total". The dashboard is written against those names, so the test
// has to apply the same rule to compare them with the catalogue.
func promName(instrument Instrument) string {
	name := strings.ReplaceAll(instrument.Name, ".", "_")
	if instrument.Unit == "s" {
		name += "_seconds"
	}
	switch instrument.Kind {
	case KindCounter, KindObservableCounter:
		name += "_total"
	case KindHistogram:
		// A histogram is exported as _bucket, _count and _sum series.
	case KindGauge:
	}
	return name
}

// Every metric the dashboard queries must be one this package publishes, or
// the panel is permanently empty and nobody finds out until an incident.
func TestDashboardOnlyQueriesPublishedMetrics(t *testing.T) {
	t.Parallel()

	published := map[string]struct{}{
		// Provided by the otelhttp instrumentation and the runtime
		// instrumentation rather than by this package's catalogue.
		"http_server_request_duration_seconds_bucket": {},
	}
	for _, instrument := range Catalogue() {
		base := promName(instrument)
		if instrument.Kind == KindHistogram {
			for _, suffix := range []string{"_bucket", "_count", "_sum"} {
				published[base+suffix] = struct{}{}
			}
			continue
		}
		published[base] = struct{}{}
	}

	metricReference := regexp.MustCompile(`[a-z_][a-z0-9_]*(?:_total|_seconds|_bucket|_count|_sum)?\s*\{`)
	for _, panel := range loadDashboard(t).Panels {
		for _, target := range panel.Targets {
			for _, match := range metricReference.FindAllString(target.Expr, -1) {
				name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(match), "{"))
				if isPromQLKeyword(name) {
					continue
				}
				if _, ok := published[name]; !ok {
					t.Errorf("panel %q queries %q, which this package does not publish", panel.Title, name)
				}
			}
		}
	}
}

// PromQL functions and aggregations look like metric names to the regexp.
func isPromQLKeyword(name string) bool {
	switch name {
	case "rate", "sum", "max", "min", "avg", "increase", "histogram_quantile",
		"by", "without", "irate", "label_values":
		return true
	default:
		return false
	}
}

// The dashboard must not select on a label the allowlist forbids, which is the
// mechanical version of the rule in ADR 0015.
func TestDashboardUsesNoForbiddenLabel(t *testing.T) {
	t.Parallel()

	allowedLabels := map[string]struct{}{
		// Resource attributes the exporter adds, not application labels.
		"service_name": {}, "deployment_environment_name": {}, "job": {}, "le": {},
	}
	for _, key := range LabelKeys() {
		// The exporter translates dots in attribute keys to underscores.
		allowedLabels[strings.ReplaceAll(key, ".", "_")] = struct{}{}
	}

	selector := regexp.MustCompile(`([a-z_][a-z0-9_.]*)\s*(?:=~|!~|=|!=)\s*"`)
	grouping := regexp.MustCompile(`(?:by|without)\s*\(([^)]*)\)`)
	for _, panel := range loadDashboard(t).Panels {
		for _, target := range panel.Targets {
			for _, match := range selector.FindAllStringSubmatch(target.Expr, -1) {
				if _, ok := allowedLabels[match[1]]; !ok {
					t.Errorf("panel %q selects on label %q, which is not allowlisted", panel.Title, match[1])
				}
			}
			for _, match := range grouping.FindAllStringSubmatch(target.Expr, -1) {
				for _, label := range strings.Split(match[1], ",") {
					label = strings.TrimSpace(label)
					if label == "" {
						continue
					}
					if _, ok := allowedLabels[label]; !ok {
						t.Errorf("panel %q groups by label %q, which is not allowlisted", panel.Title, label)
					}
				}
			}
		}
	}
}

// The four signals E5b requires a dashboard to cover must each have a panel.
func TestDashboardCoversEveryRequiredArea(t *testing.T) {
	t.Parallel()

	parsed := loadDashboard(t)
	expressions := make([]string, 0, len(parsed.Panels))
	for _, panel := range parsed.Panels {
		for _, target := range panel.Targets {
			expressions = append(expressions, target.Expr)
		}
	}
	joined := strings.Join(expressions, "\n")

	required := map[string]string{
		"HTTP":            "http_server_request_duration_seconds",
		"provedor":        ProviderRequestDuration,
		"inbox":           InboxPending,
		"outbox":          OutboxPending,
		"retry":           ConsumerRetries,
		"DLQ":             QueueDepth,
		"pool PostgreSQL": PoolConnections,
	}
	for area, instrument := range required {
		name := strings.ReplaceAll(instrument, ".", "_")
		if !strings.Contains(joined, name) {
			t.Errorf("the dashboard has no panel covering %s (expected a query on %s)", area, name)
		}
	}
}
