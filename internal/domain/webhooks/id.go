package webhooks

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	eventIDPrefix   = "evt_"
	messageIDPrefix = "msg_"
)

// ErrInvalidEventID is returned for identifiers that cannot name an inbox row.
var ErrInvalidEventID = errors.New("invalid webhook event id")

// ValidateEventID validates the public, opaque inbox identifier without
// revealing whether a particular row exists.
func ValidateEventID(id string) error {
	if !strings.HasPrefix(id, eventIDPrefix) || len(id) != len(eventIDPrefix)+32 {
		return ErrInvalidEventID
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(id, eventIDPrefix)); err != nil {
		return ErrInvalidEventID
	}
	return nil
}

// NewEventID returns an opaque, non-sequential inbox identifier.
func NewEventID() (string, error) { return newID(eventIDPrefix) }

// NewMessageID returns an opaque, non-sequential outbox identifier. It also
// travels as the published messageId.
func NewMessageID() (string, error) { return newID(messageIDPrefix) }

func newID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}
	return prefix + hex.EncodeToString(value), nil
}
