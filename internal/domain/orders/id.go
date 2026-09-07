package orders

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// IDPrefix marks public order identifiers.
const IDPrefix = "ord_"

// NewID returns an opaque, non-sequential public identifier. It carries no
// timestamp or counter, so it leaks neither volume nor ordering.
func NewID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate order id: %w", err)
	}
	return IDPrefix + hex.EncodeToString(value), nil
}
