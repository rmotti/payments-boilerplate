package outbox

import (
	"errors"
	"fmt"
)

// ErrNotConfirmed means the broker did not acknowledge the publication. The
// message stays pending: an unconfirmed publish is indistinguishable from a
// lost one, and losing it is the only unacceptable outcome.
var ErrNotConfirmed = errors.New("broker did not confirm the publication")

// ErrNotRouted means the broker accepted the message but no queue matched its
// routing key. A confirm alone does not prove delivery, so the publisher asks
// for mandatory returns and reports them here rather than marking the message
// published. It is a topology problem, so retrying is right: the binding may
// be restored.
var ErrNotRouted = errors.New("broker could not route the message to any queue")

// ErrLeaseLost means the settlement matched no row: this relay no longer held
// the lease when it tried to record the outcome, because the lease expired and
// another instance took the message over.
//
// It must never be silently swallowed. The publication may well have happened,
// but nothing recorded it, so the message will be published again — and the
// cycle that reported success would have been lying.
var ErrLeaseLost = errors.New("lease was lost before the outcome could be recorded")

// PermanentError marks a failure that no amount of retrying will fix, such as
// a message whose body cannot be encoded.
//
// Everything else is transient by default. That default is deliberate: broker
// downtime, closed connections, timeouts and nacks are all conditions that end
// on their own, and treating them as permanent would discard financial events
// because infrastructure had a bad minute.
type PermanentError struct{ Err error }

// Permanent wraps an error as unrecoverable.
func Permanent(err error) error { return &PermanentError{Err: err} }

func (e *PermanentError) Error() string { return fmt.Sprintf("permanent: %v", e.Err) }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether the relay should stop retrying an error.
func IsPermanent(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}
