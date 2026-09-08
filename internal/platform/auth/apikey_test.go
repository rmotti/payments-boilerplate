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

// The fingerprint is what lets a rate limiter count per credential without
// handling one. It has to be stable for a given key, distinct between keys,
// and absent for anything not configured.
func TestFingerprintIdentifiesValidKeysOnly(t *testing.T) {
	t.Parallel()

	first := strings.Repeat("a", MinAPIKeyLength)
	second := strings.Repeat("b", MinAPIKeyLength)
	verifier, err := NewAPIKeyVerifier([]string{first, second})
	if err != nil {
		t.Fatalf("NewAPIKeyVerifier() error = %v", err)
	}

	firstTag, ok := verifier.Fingerprint(first)
	if !ok {
		t.Fatal("a configured key produced no fingerprint")
	}
	if again, _ := verifier.Fingerprint(first); again != firstTag {
		t.Fatal("the fingerprint of one key is not stable")
	}
	secondTag, ok := verifier.Fingerprint(second)
	if !ok {
		t.Fatal("the second configured key produced no fingerprint")
	}
	if secondTag == firstTag {
		t.Fatal("two distinct keys share a fingerprint")
	}

	// The fingerprint must not be the key, nor contain it: it is handed to a
	// limiter and could end up somewhere the key must never reach.
	for _, tag := range []string{firstTag, secondTag} {
		if strings.Contains(tag, first) || strings.Contains(tag, second) {
			t.Fatal("a fingerprint contains the key it identifies")
		}
	}

	// An unknown credential gets nothing, which is what stops a caller from
	// minting a limiter bucket per invented header value.
	for _, unknown := range []string{"", "short", strings.Repeat("c", MinAPIKeyLength)} {
		if _, ok := verifier.Fingerprint(unknown); ok {
			t.Fatalf("Fingerprint(%q) produced a value for an unconfigured credential", unknown)
		}
	}
}

// Each process derives its own secret, so the same key fingerprints
// differently in two processes. That is deliberate: it keeps the value from
// being a stable cross-deployment identifier for a credential.
func TestFingerprintIsProcessSpecific(t *testing.T) {
	t.Parallel()

	key := strings.Repeat("a", MinAPIKeyLength)
	first, err := NewAPIKeyVerifier([]string{key})
	if err != nil {
		t.Fatalf("NewAPIKeyVerifier() error = %v", err)
	}
	second, err := NewAPIKeyVerifier([]string{key})
	if err != nil {
		t.Fatalf("NewAPIKeyVerifier() error = %v", err)
	}
	firstTag, _ := first.Fingerprint(key)
	secondTag, _ := second.Fingerprint(key)
	if firstTag == secondTag {
		t.Fatal("two verifiers produced the same fingerprint for one key")
	}
}
