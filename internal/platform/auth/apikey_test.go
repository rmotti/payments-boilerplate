package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestAPIKeyVerifierAcceptsEveryActiveKey(t *testing.T) {
	t.Parallel()

	current := strings.Repeat("a", MinAPIKeyLength)
	next := strings.Repeat("b", MinAPIKeyLength)
	verifier, err := NewAPIKeyVerifier([]string{current, next})
	if err != nil {
		t.Fatalf("NewAPIKeyVerifier() error = %v", err)
	}

	for _, key := range []string{current, next} {
		if !verifier.Valid(key) {
			t.Errorf("Valid(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"", "wrong", strings.Repeat("c", MinAPIKeyLength)} {
		if verifier.Valid(key) {
			t.Errorf("Valid(%q) = true, want false", key)
		}
	}
}

func TestAPIKeyVerifierUsesInstanceSpecificTags(t *testing.T) {
	t.Parallel()

	key := strings.Repeat("a", MinAPIKeyLength)
	first, err := NewAPIKeyVerifier([]string{key})
	if err != nil {
		t.Fatalf("NewAPIKeyVerifier() first error = %v", err)
	}
	second, err := NewAPIKeyVerifier([]string{key})
	if err != nil {
		t.Fatalf("NewAPIKeyVerifier() second error = %v", err)
	}

	if first.tags[0] == second.tags[0] {
		t.Fatal("independent verifiers produced the same authentication tag")
	}
	if !first.Valid(key) || !second.Valid(key) {
		t.Fatal("instance-specific tags changed credential verification")
	}
}

func TestNewAPIKeyVerifierRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	valid := strings.Repeat("a", MinAPIKeyLength)
	tests := []struct {
		name string
		keys []string
		want error
	}{
		{name: "empty", keys: nil, want: ErrNoAPIKeys},
		{name: "short", keys: []string{"short"}, want: ErrInvalidAPIKey},
		{name: "surrounding whitespace", keys: []string{" " + valid}, want: ErrInvalidAPIKey},
		{name: "duplicate", keys: []string{valid, valid}, want: ErrDuplicateAPIKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewAPIKeyVerifier(tt.keys); !errors.Is(err, tt.want) {
				t.Fatalf("NewAPIKeyVerifier() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNilAPIKeyVerifierRejectsCandidate(t *testing.T) {
	t.Parallel()

	var verifier *APIKeyVerifier
	if verifier.Valid(strings.Repeat("a", MinAPIKeyLength)) {
		t.Fatal("nil verifier accepted a candidate")
	}
}
