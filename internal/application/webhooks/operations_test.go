package webhooks

import (
	"context"
	"errors"
	"testing"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

type inspectionRepositoryStub struct {
	items      []EventInspection
	err        error
	gotStatus  domain.Status
	gotLimit   int
	gotEventID string
}

func (s *inspectionRepositoryStub) List(_ context.Context, status domain.Status, limit int) ([]EventInspection, error) {
	s.gotStatus, s.gotLimit = status, limit
	return s.items, s.err
}

func (s *inspectionRepositoryStub) Reprocess(_ context.Context, eventID string) (EventInspection, error) {
	s.gotEventID = eventID
	return EventInspection{ID: eventID}, s.err
}

func TestOperationsListDefaultsAndValidates(t *testing.T) {
	t.Parallel()

	repository := &inspectionRepositoryStub{}
	service := NewOperationsService(repository)
	if _, err := service.List(context.Background(), domain.StatusFailed, 0); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if repository.gotStatus != domain.StatusFailed || repository.gotLimit != defaultInspectionLimit {
		t.Fatalf("repository input = %q/%d, want failed/%d", repository.gotStatus, repository.gotLimit, defaultInspectionLimit)
	}
	if _, err := service.List(context.Background(), domain.Status("unknown"), 10); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("invalid status error = %v, want %v", err, ErrInvalidStatus)
	}
	if _, err := service.List(context.Background(), "", maxInspectionLimit+1); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("invalid limit error = %v, want %v", err, ErrInvalidLimit)
	}
}

func TestOperationsReprocessValidatesOpaqueID(t *testing.T) {
	t.Parallel()

	repository := &inspectionRepositoryStub{}
	service := NewOperationsService(repository)
	if _, err := service.Reprocess(context.Background(), "evt_invalid"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("invalid id error = %v, want %v", err, ErrEventNotFound)
	}
	if repository.gotEventID != "" {
		t.Fatal("repository called for an invalid id")
	}

	eventID := "evt_0123456789abcdef0123456789abcdef"
	if _, err := service.Reprocess(context.Background(), eventID); err != nil {
		t.Fatalf("Reprocess() error = %v", err)
	}
	if repository.gotEventID != eventID {
		t.Fatalf("repository event id = %q, want %q", repository.gotEventID, eventID)
	}
}
