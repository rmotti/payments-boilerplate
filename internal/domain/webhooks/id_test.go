package webhooks

import "testing"

func TestValidateEventID(t *testing.T) {
	t.Parallel()

	if err := ValidateEventID("evt_0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("valid id rejected: %v", err)
	}
	for _, id := range []string{"", "evt_short", "msg_0123456789abcdef0123456789abcdef", "evt_0123456789abcdef0123456789abcdeg"} {
		if err := ValidateEventID(id); err == nil {
			t.Errorf("ValidateEventID(%q) error = nil", id)
		}
	}
}
