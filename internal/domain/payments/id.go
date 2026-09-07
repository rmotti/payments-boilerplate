package payments

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const (
	paymentIDPrefix = "pay_"
	attemptIDPrefix = "pat_"
)

// NewPaymentID returns an opaque, non-sequential payment identifier.
func NewPaymentID() (string, error) {
	return newID(paymentIDPrefix)
}

// NewAttemptID returns an opaque, non-sequential attempt identifier.
func NewAttemptID() (string, error) {
	return newID(attemptIDPrefix)
}

func newID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}
	return prefix + hex.EncodeToString(value), nil
}
