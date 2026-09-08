package worker

import (
	"strings"
	"testing"
)

func TestInstanceIdentityIsUniquePerProcess(t *testing.T) {
	t.Parallel()

	first, err := InstanceIdentity("worker")
	if err != nil {
		t.Fatalf("first InstanceIdentity() error = %v", err)
	}
	second, err := InstanceIdentity("worker")
	if err != nil {
		t.Fatalf("second InstanceIdentity() error = %v", err)
	}
	if first == second {
		t.Fatalf("instance identities are equal: %q", first)
	}
	if !strings.Contains(first, "-") || !strings.Contains(second, "-") {
		t.Fatalf("identities = %q and %q, want readable host and random suffix", first, second)
	}
}
