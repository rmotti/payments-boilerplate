package main

import (
	"strings"
	"testing"
)

func TestInstanceIdentityIsUniquePerProcess(t *testing.T) {
	t.Parallel()

	first, err := instanceIdentity("worker")
	if err != nil {
		t.Fatalf("first instanceIdentity() error = %v", err)
	}
	second, err := instanceIdentity("worker")
	if err != nil {
		t.Fatalf("second instanceIdentity() error = %v", err)
	}
	if first == second {
		t.Fatalf("instance identities are equal: %q", first)
	}
	if !strings.Contains(first, "-") || !strings.Contains(second, "-") {
		t.Fatalf("identities = %q and %q, want readable host and random suffix", first, second)
	}
}
