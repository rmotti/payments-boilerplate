package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	domain "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
)

type stubVerifier struct {
	event ProviderEvent
	err   error
	calls int
	// body records the bytes handed to verification, so a test can prove the
	// use case forwards them untouched.
	body []byte
}

func (s *stubVerifier) Verify(rawBody []byte, _ string) (ProviderEvent, error) {
	s.calls++
	s.body = rawBody
	if s.err != nil {
		return ProviderEvent{}, s.err
	}
	return s.event, nil
}

func (s *stubVerifier) Provider() domain.Provider { return domain.Stripe }

type stubRepository struct {
	stored    bool
	err       error
	events    []domain.Event
	messages  []domain.Message
	ignored   []domain.Event
	callCount int
}

func (s *stubRepository) StoreIgnored(_ context.Context, event domain.Event) (bool, error) {
	s.callCount++
	if s.err != nil {
		return false, s.err
	}
	s.ignored = append(s.ignored, event)
	return s.stored, nil
}

func (s *stubRepository) StorePending(_ context.Context, event domain.Event, message domain.Message) (bool, error) {
	s.callCount++
	if s.err != nil {
		return false, s.err
	}
	s.events = append(s.events, event)
	s.messages = append(s.messages, message)
	return s.stored, nil
}

func newService(verifier Verifier, repository Repository) *Service {
	return NewService(verifier, repository,
		WithClock(func() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) }),
		WithIDGenerators(
			func() (string, error) { return "evt_local", nil },
			func() (string, error) { return "msg_local", nil },
		),
	)
}

func handledEvent() ProviderEvent {
	return ProviderEvent{
		ID: "evt_provider_1", Type: "checkout.session.completed",
		Kind: domain.KindCheckoutCompleted, OccurredAt: time.Unix(1757246400, 0).UTC(),
		Payload: json.RawMessage(`{"id":"evt_provider_1"}`),
	}
}

func TestReceiveStoresEventAndMessageTogether(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{stored: true}
	service := newService(&stubVerifier{event: handledEvent()}, repository)

	outcome, err := service.Receive(context.Background(), ReceiveInput{
		RawBody: []byte(`{"id":"evt_provider_1"}`), Signature: "t=1,v1=abc", CorrelationID: "corr-1",
	})
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if outcome != OutcomeAccepted {
		t.Fatalf("outcome = %q, want accepted", outcome)
	}
	if len(repository.events) != 1 || len(repository.messages) != 1 {
		t.Fatal("the event and its message must be handed over together")
	}
	message := repository.messages[0]
	if message.EventID != repository.events[0].ID {
		t.Fatal("the message must reference the stored event")
	}
	if message.CorrelationID != "corr-1" {
		t.Fatalf("CorrelationID = %q, want the request correlation", message.CorrelationID)
	}
	if message.RoutingKey != "payment.webhook.checkout.completed" {
		t.Fatalf("RoutingKey = %q, want the neutral kind routing key", message.RoutingKey)
	}
	if message.SchemaVersion != domain.SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", message.SchemaVersion, domain.SchemaVersion)
	}
}

func TestReceiveKeepsProviderPayloadOutOfTheMessage(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{stored: true}
	service := newService(&stubVerifier{event: handledEvent()}, repository)

	if _, err := service.Receive(context.Background(), ReceiveInput{
		RawBody: []byte(`{"id":"evt_provider_1"}`), Signature: "t=1,v1=abc",
	}); err != nil {
		t.Fatalf("Receive() error = %v", err)
	}

	// The claim check: everything the broker learns must be a reference plus
	// routing metadata. Any provider payload here would put customer data in a
	// second system and let the two copies drift.
	encoded, err := json.Marshal(repository.messages[0])
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if strings.Contains(string(encoded), "evt_provider_1") {
		t.Fatalf("message must not carry the provider payload: %s", encoded)
	}
}

func TestReceiveForwardsRawBytesToVerification(t *testing.T) {
	t.Parallel()

	verifier := &stubVerifier{event: handledEvent()}
	raw := []byte("{\"id\":\"evt_provider_1\",  \"spaced\": true}\n")
	service := newService(verifier, &stubRepository{stored: true})

	if _, err := service.Receive(context.Background(), ReceiveInput{RawBody: raw}); err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if string(verifier.body) != string(raw) {
		t.Fatal("verification must see the bytes exactly as they arrived")
	}
}

func TestReceiveReportsDuplicateWithoutQueueing(t *testing.T) {
	t.Parallel()

	// stored=false is what the repository reports when ON CONFLICT DO NOTHING
	// matched an event already in the inbox.
	repository := &stubRepository{stored: false}
	service := newService(&stubVerifier{event: handledEvent()}, repository)

	outcome, err := service.Receive(context.Background(), ReceiveInput{RawBody: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if outcome != OutcomeDuplicate {
		t.Fatalf("outcome = %q, want duplicate", outcome)
	}
}

func TestReceiveRecordsUnhandledKindAsIgnored(t *testing.T) {
	t.Parallel()

	unhandled := handledEvent()
	unhandled.Type = "customer.created"
	unhandled.Kind = domain.KindUnknown

	repository := &stubRepository{stored: true}
	service := newService(&stubVerifier{event: unhandled}, repository)

	outcome, err := service.Receive(context.Background(), ReceiveInput{RawBody: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if outcome != OutcomeIgnored {
		t.Fatalf("outcome = %q, want ignored", outcome)
	}
	if len(repository.messages) != 0 {
		t.Fatal("an ignored event must not produce an outbox message")
	}
	if len(repository.ignored) != 1 || repository.ignored[0].Status != domain.StatusSkipped {
		t.Fatal("an ignored event must still be recorded, as skipped")
	}
}

func TestReceiveRejectsUnverifiedSignature(t *testing.T) {
	t.Parallel()

	repository := &stubRepository{stored: true}
	service := newService(&stubVerifier{err: errors.New("no valid signature")}, repository)

	_, err := service.Receive(context.Background(), ReceiveInput{RawBody: []byte(`{}`)})
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("Receive() error = %v, want ErrInvalidSignature", err)
	}
	if repository.callCount != 0 {
		t.Fatal("nothing may be persisted before the signature verifies")
	}
}

func TestReceiveSurfacesStorageFailure(t *testing.T) {
	t.Parallel()

	// The handler turns this into a 500 on purpose: the provider is the only
	// thing that can deliver the event again.
	service := newService(&stubVerifier{event: handledEvent()}, &stubRepository{err: errors.New("postgres down")})

	_, err := service.Receive(context.Background(), ReceiveInput{RawBody: []byte(`{}`)})
	if err == nil {
		t.Fatal("Receive() error = nil, want the storage failure to surface")
	}
	if errors.Is(err, ErrInvalidSignature) {
		t.Fatal("a storage failure must not be reported as a signature problem")
	}
}

// receiptRecorder is what the metrics observer does: it takes one Receipt per
// call and nothing else.
type receiptRecorder struct{ receipts []Receipt }

func (r *receiptRecorder) Received(receipt Receipt) { r.receipts = append(r.receipts, receipt) }

func TestReceiveReportsEveryOutcomeToTheObserver(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		verifier    *stubVerifier
		repository  *stubRepository
		wantOutcome Outcome
		wantKind    domain.Kind
		wantErr     error
	}{
		{
			name: "accepted", verifier: &stubVerifier{event: handledEvent()},
			repository:  &stubRepository{stored: true},
			wantOutcome: OutcomeAccepted, wantKind: domain.KindCheckoutCompleted,
		},
		{
			name: "duplicate", verifier: &stubVerifier{event: handledEvent()},
			repository:  &stubRepository{stored: false},
			wantOutcome: OutcomeDuplicate, wantKind: domain.KindCheckoutCompleted,
		},
		{
			name:     "invalid signature",
			verifier: &stubVerifier{err: errors.New("signature mismatch")},
			// The signature failed, so nothing about the event is known,
			// including its kind.
			repository: &stubRepository{}, wantKind: domain.KindUnknown,
			wantErr: ErrInvalidSignature,
		},
		{
			name: "storage failure", verifier: &stubVerifier{event: handledEvent()},
			repository: &stubRepository{err: errors.New("postgres down")},
			wantKind:   domain.KindCheckoutCompleted, wantErr: errors.New("store"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			recorder := &receiptRecorder{}
			service := NewService(test.verifier, test.repository,
				WithClock(func() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) }),
				WithIDGenerators(
					func() (string, error) { return "evt_local", nil },
					func() (string, error) { return "msg_local", nil },
				),
				WithObserver(recorder))

			_, err := service.Receive(context.Background(), ReceiveInput{RawBody: []byte(`{}`)})
			if (err != nil) != (test.wantErr != nil) {
				t.Fatalf("Receive() error = %v, want error presence %v", err, test.wantErr != nil)
			}
			if len(recorder.receipts) != 1 {
				t.Fatalf("receipts = %d, want 1", len(recorder.receipts))
			}
			receipt := recorder.receipts[0]
			if receipt.Provider != domain.Stripe {
				t.Errorf("provider = %q, want stripe", receipt.Provider)
			}
			if receipt.Kind != test.wantKind {
				t.Errorf("kind = %q, want %q", receipt.Kind, test.wantKind)
			}
			if receipt.Outcome != test.wantOutcome {
				t.Errorf("outcome = %q, want %q", receipt.Outcome, test.wantOutcome)
			}
			if (receipt.Err != nil) != (test.wantErr != nil) {
				t.Errorf("receipt error = %v, want error presence %v", receipt.Err, test.wantErr != nil)
			}
			if receipt.Duration <= 0 {
				t.Error("receipt carries no duration")
			}
		})
	}
}

func TestReceiveReportsBodyReadFailureWithoutVerifyingOrPersisting(t *testing.T) {
	t.Parallel()

	verifier := &stubVerifier{event: handledEvent()}
	repository := &stubRepository{stored: true}
	recorder := &receiptRecorder{}
	service := NewService(verifier, repository, WithObserver(recorder))
	readErr := fmt.Errorf("%w: request body too large", ErrPayloadTooLarge)

	_, err := service.Receive(context.Background(), ReceiveInput{ReadError: readErr})
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("Receive() error = %v, want ErrPayloadTooLarge", err)
	}
	if verifier.calls != 0 {
		t.Fatalf("verification calls = %d, want 0", verifier.calls)
	}
	if repository.callCount != 0 {
		t.Fatalf("repository calls = %d, want 0", repository.callCount)
	}
	if len(recorder.receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(recorder.receipts))
	}
	receipt := recorder.receipts[0]
	if receipt.Provider != domain.Stripe || receipt.Kind != domain.KindUnknown ||
		receipt.Outcome != "" || !errors.Is(receipt.Err, ErrPayloadTooLarge) {
		t.Fatalf("receipt = %+v, want a classified body read failure", receipt)
	}
}

// The receipt is the whole surface a metrics observer sees, so it must not
// carry the identifiers or the bytes of the event.
func TestReceiptCarriesNoIdentifierOrPayload(t *testing.T) {
	t.Parallel()

	recorder := &receiptRecorder{}
	service := NewService(&stubVerifier{event: handledEvent()}, &stubRepository{stored: true},
		WithObserver(recorder))
	if _, err := service.Receive(context.Background(), ReceiveInput{
		RawBody:   []byte(`{"id":"evt_secret","customer_email":"person@example.test"}`),
		Signature: "t=1,v1=deadbeef", CorrelationID: "corr-8b21",
	}); err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if len(recorder.receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(recorder.receipts))
	}
	rendered := strings.Join([]string{
		string(recorder.receipts[0].Provider), string(recorder.receipts[0].Kind),
		string(recorder.receipts[0].Outcome),
	}, " ")
	for _, forbidden := range []string{"evt_", "corr-", "example.test", "deadbeef"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("receipt %q carries %q", rendered, forbidden)
		}
	}
}
