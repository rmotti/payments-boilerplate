package webhooks

import (
	"context"
	"errors"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

const (
	defaultInspectionLimit = 50
	maxInspectionLimit     = 100
)

var (
	// ErrEventNotFound hides whether an invalid identifier or an absent row was supplied.
	ErrEventNotFound = errors.New("webhook event not found")
	// ErrEventNotReplayable means neither the inbox nor its publication failed.
	ErrEventNotReplayable = errors.New("webhook event is not failed")
	// ErrInvalidStatus means the inspection filter is outside the inbox vocabulary.
	ErrInvalidStatus = errors.New("invalid webhook event status")
	// ErrInvalidLimit means the requested page would be empty or unbounded.
	ErrInvalidLimit = errors.New("inspection limit must be between 1 and 100")
)

// EventInspection is the non-sensitive operational view of one inbox entry
// and its publication. Payload bytes deliberately never cross this boundary.
type EventInspection struct {
	ID              string
	Provider        domain.Provider
	ProviderEventID string
	EventType       string
	Status          domain.Status
	Attempts        int
	ReceivedAt      time.Time
	ProcessedAt     *time.Time
	LastError       string
	UpdatedAt       time.Time
	ReplayCount     int
	LastReplayedAt  *time.Time
	Outbox          *OutboxInspection
}

// OutboxInspection is the publication metadata associated with an inbox entry.
type OutboxInspection struct {
	ID            string
	Status        domain.MessageStatus
	Attempts      int
	PublishedAt   *time.Time
	LastError     string
	NextAttemptAt time.Time
}

// InspectionRepository persists the inspection and replay invariants.
type InspectionRepository interface {
	List(ctx context.Context, status domain.Status, limit int) ([]EventInspection, error)
	Reprocess(ctx context.Context, eventID string) (EventInspection, error)
}

// OperationsService provides authenticated operational inbox use cases.
type OperationsService struct {
	repository InspectionRepository
}

// NewOperationsService binds operational webhook use cases to their repository.
func NewOperationsService(repository InspectionRepository) *OperationsService {
	return &OperationsService{repository: repository}
}

// List returns a bounded, optionally filtered view of recent inbox entries.
func (s *OperationsService) List(ctx context.Context, status domain.Status, limit int) ([]EventInspection, error) {
	if status != "" && !validInspectionStatus(status) {
		return nil, ErrInvalidStatus
	}
	if limit == 0 {
		limit = defaultInspectionLimit
	}
	if limit < 1 || limit > maxInspectionLimit {
		return nil, ErrInvalidLimit
	}
	return s.repository.List(ctx, status, limit)
}

// Reprocess atomically reenqueues an inbox event or outbox publication that failed.
func (s *OperationsService) Reprocess(ctx context.Context, eventID string) (EventInspection, error) {
	if err := domain.ValidateEventID(eventID); err != nil {
		return EventInspection{}, ErrEventNotFound
	}
	return s.repository.Reprocess(ctx, eventID)
}

func validInspectionStatus(status domain.Status) bool {
	switch status {
	case domain.StatusPending, domain.StatusProcessing, domain.StatusProcessed,
		domain.StatusFailed, domain.StatusSkipped:
		return true
	default:
		return false
	}
}
