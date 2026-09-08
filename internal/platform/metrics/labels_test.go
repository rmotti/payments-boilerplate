package metrics

import (
	"strings"
	"testing"
)

func TestNormalizeCollapsesEverythingOutsideTheAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "known value passes through", key: LabelProvider, value: "stripe", want: "stripe"},
		{name: "unknown provider", key: LabelProvider, value: "acme", want: Other},
		{name: "identifier", key: LabelEventKind, value: "evt_1MxYz3Kq", want: Other},
		{name: "correlation id", key: LabelOutcome, value: "corr-8b21-4f", want: Other},
		{name: "secret", key: LabelReason, value: "whsec_live_9f3a", want: Other},
		{name: "error message", key: LabelReason, value: "dial tcp 10.0.0.2:5672: refused", want: Other},
		{name: "free path", key: LabelOperation, value: "/v1/orders/ord_123/checkout", want: Other},
		{name: "empty value", key: LabelOutcome, value: "", want: Other},
		{name: "unknown key", key: "customer.email", value: "person@example.test", want: Other},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Normalize(test.key, test.value); got != test.want {
				t.Fatalf("Normalize(%q, %q) = %q, want %q", test.key, test.value, got, test.want)
			}
		})
	}
}

// The allowlist must not itself contain something that looks like an
// identifier or a secret. It is written by hand, so this is the guard.
func TestAllowlistContainsNoIdentifierShapedValues(t *testing.T) {
	t.Parallel()

	forbidden := []string{"_", "://", "@"}
	for _, key := range LabelKeys() {
		for _, value := range AllowedValues(key) {
			for _, needle := range forbidden {
				if needle == "_" {
					// Underscores are legitimate word separators; only
					// provider-style prefixes are a problem.
					if strings.HasPrefix(value, "evt_") || strings.HasPrefix(value, "sk_") ||
						strings.HasPrefix(value, "whsec_") || strings.HasPrefix(value, "cs_") {
						t.Errorf("%s allows %q, which is shaped like an identifier", key, value)
					}
					continue
				}
				if strings.Contains(value, needle) {
					t.Errorf("%s allows %q, which contains %q", key, value, needle)
				}
			}
		}
	}
}

// Every label a catalogue entry declares must be one this package knows how to
// normalize, or the entry describes a label nothing bounds.
func TestEveryCataloguedLabelIsAllowlisted(t *testing.T) {
	t.Parallel()

	known := make(map[string]struct{})
	for _, key := range LabelKeys() {
		known[key] = struct{}{}
	}
	for _, instrument := range Catalogue() {
		for _, label := range instrument.Labels {
			if _, ok := known[label]; !ok {
				t.Errorf("instrument %s declares label %q, which is not allowlisted", instrument.Name, label)
			}
		}
	}
}
