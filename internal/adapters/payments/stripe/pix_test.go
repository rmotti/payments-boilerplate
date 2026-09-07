package stripe

import (
	"context"
	"testing"

	consumer "github.com/rmotti/payments-boilerplate/internal/application/consumer"
	payments "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	webhooks "github.com/rmotti/payments-boilerplate/internal/domain/webhooks"
	stripesdk "github.com/stripe/stripe-go/v86"
)

// pixFlowRepository is a small transactional port substitute used to prove
// the whole provider interpretation and transition sequence without depending
// on a live Stripe account. Repository atomicity has its own PostgreSQL tests.
type pixFlowRepository struct {
	events      map[string]webhooks.Event
	processed   map[string]bool
	aggregate   consumer.Aggregate
	orderStatus string
}

func (r *pixFlowRepository) Process(
	_ context.Context,
	eventID string,
	resolve consumer.Resolver,
	decide consumer.Decider,
) (consumer.Result, error) {
	if r.processed[eventID] {
		return consumer.Result{Note: "event was already processed"}, nil
	}
	entry := consumer.InboxEntry{Event: r.events[eventID], Status: webhooks.StatusPending}
	if _, err := resolve(entry); err != nil {
		return consumer.Result{}, err
	}
	effect, err := decide(entry, r.aggregate)
	if err != nil {
		return consumer.Result{}, err
	}
	applied := effect.AttemptStatus != "" || effect.PaymentStatus != "" || effect.OrderPaid
	if effect.AttemptStatus != "" {
		r.aggregate.AttemptStatus = effect.AttemptStatus
	}
	if effect.PaymentStatus != "" {
		r.aggregate.PaymentStatus = effect.PaymentStatus
	}
	if effect.OrderPaid {
		r.orderStatus = "paid"
		r.aggregate.OrderStatus = "paid"
	}
	r.processed[eventID] = true
	return consumer.Result{Applied: applied, Note: effect.Note}, nil
}

func (*pixFlowRepository) RecordFailure(context.Context, string, error, bool, int) (int, bool, error) {
	return 1, true, nil
}

func TestPixTransitionsFromCheckoutCompletionToFinalPayment(t *testing.T) {
	t.Parallel()

	completed := sessionEvent(t, webhooks.KindCheckoutCompleted, "checkout.session.completed",
		sessionPayload(stripesdk.APIVersion, "checkout.session.completed", "unpaid", 10000))
	succeeded := sessionEvent(t, webhooks.KindCheckoutPaymentSucceeded, "checkout.session.async_payment_succeeded",
		sessionPayload(stripesdk.APIVersion, "checkout.session.async_payment_succeeded", "paid", 10000))
	completed.ID = "evt_pix_completed"
	succeeded.ID = "evt_pix_succeeded"

	repository := &pixFlowRepository{
		events: map[string]webhooks.Event{
			completed.ID: completed,
			succeeded.ID: succeeded,
		},
		processed: make(map[string]bool),
		aggregate: consumer.Aggregate{
			OrderID: "ord_1", OrderStatus: "pending", PaymentID: "pay_1",
			PaymentStatus: payments.StatusPending, Provider: "stripe", Amount: 10000,
			Currency: "BRL", AttemptID: "pat_1", AttemptStatus: payments.AttemptStatusPending,
			SessionID: "cs_test_1",
		},
		orderStatus: "pending",
	}
	service := consumer.NewService(repository, NewSession(), consumer.Config{})

	first, err := service.Handle(context.Background(), completed.ID)
	if err != nil {
		t.Fatalf("handle completed Pix checkout: %v", err)
	}
	if first.Disposition != consumer.DispositionDone || !first.Applied ||
		repository.aggregate.PaymentStatus != payments.StatusProcessing ||
		repository.aggregate.AttemptStatus != payments.AttemptStatusPending || repository.orderStatus != "pending" {
		t.Fatalf("after checkout completion = %#v / %#v / %s, want processing/open/pending",
			first, repository.aggregate, repository.orderStatus)
	}

	second, err := service.Handle(context.Background(), succeeded.ID)
	if err != nil {
		t.Fatalf("handle asynchronous Pix success: %v", err)
	}
	if second.Disposition != consumer.DispositionDone || !second.Applied ||
		repository.aggregate.PaymentStatus != payments.StatusSucceeded ||
		repository.aggregate.AttemptStatus != payments.AttemptStatusSucceeded || repository.orderStatus != "paid" {
		t.Fatalf("after async success = %#v / %#v / %s, want succeeded/succeeded/paid",
			second, repository.aggregate, repository.orderStatus)
	}
}

var _ consumer.Repository = (*pixFlowRepository)(nil)
