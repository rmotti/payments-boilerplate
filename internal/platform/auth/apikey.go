// Package auth verifies credentials used at the application's trust boundary.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// MinAPIKeyLength is a sanity bound for configured secrets. Operators must
// still generate keys from a cryptographically secure source; length alone
// cannot prove entropy.
const MinAPIKeyLength = 32

// Configuration errors.
var (
	ErrNoAPIKeys       = errors.New("at least one integration API key is required")
	ErrInvalidAPIKey   = fmt.Errorf("integration API keys must have at least %d characters and no surrounding whitespace", MinAPIKeyLength)
	ErrDuplicateAPIKey = errors.New("integration API keys must be unique")
)

// APIKeyVerifier keeps only fixed-size, process-specific authentication tags
// for the configured credentials. It is immutable after construction and safe
// for concurrent use.
type APIKeyVerifier struct {
	secret [sha256.Size]byte
	tags   [][sha256.Size]byte
}

// NewAPIKeyVerifier validates the active integration keys and authenticates
// them with a random in-memory secret. API keys are machine-generated,
// high-entropy tokens rather than human passwords; HMAC avoids retaining a
// reusable plain hash without adding a password KDF to every request.
func NewAPIKeyVerifier(keys []string) (*APIKeyVerifier, error) {
	if len(keys) == 0 {
		return nil, ErrNoAPIKeys
	}

	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if len(key) < MinAPIKeyLength || strings.TrimSpace(key) != key {
			return nil, ErrInvalidAPIKey
		}
		if _, duplicated := seen[key]; duplicated {
			return nil, ErrDuplicateAPIKey
		}
		seen[key] = struct{}{}
	}

	verifier := &APIKeyVerifier{tags: make([][sha256.Size]byte, 0, len(keys))}
	if _, err := rand.Read(verifier.secret[:]); err != nil {
		return nil, fmt.Errorf("generate API key verifier secret: %w", err)
	}
	for _, key := range keys {
		verifier.tags = append(verifier.tags, verifier.tag(key))
	}
	return verifier, nil
}

// Valid reports whether candidate matches any configured key. Every
// comparison operates on fixed-size tags and all active keys are visited.
func (v *APIKeyVerifier) Valid(candidate string) bool {
	if v == nil {
		return false
	}

	candidateTag := v.tag(candidate)
	valid := 0
	for index := range v.tags {
		valid |= subtle.ConstantTimeCompare(candidateTag[:], v.tags[index][:])
	}
	return valid == 1
}

func (v *APIKeyVerifier) tag(value string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, v.secret[:])
	_, _ = mac.Write([]byte(value))
	var tag [sha256.Size]byte
	copy(tag[:], mac.Sum(nil))
	return tag
}

// Fingerprint returns a stable, opaque identifier for a valid credential, and
// false for one that is not configured.
//
// It exists so a rate limiter can count per credential without ever handling
// the key itself. The value is the same HMAC tag the verifier already compares
// against, hex encoded: derived with a secret generated at startup, so it is
// unique to this process, cannot be correlated across restarts or replicas,
// and is not reversible to the key even if it were to escape.
//
// Refusing to fingerprint an unknown credential is the point rather than a
// detail: it is what stops an unauthenticated caller from minting a fresh
// bucket per made-up header value.
func (v *APIKeyVerifier) Fingerprint(candidate string) (string, bool) {
	if !v.Valid(candidate) {
		return "", false
	}
	tag := v.tag(candidate)
	return hex.EncodeToString(tag[:]), true
}
