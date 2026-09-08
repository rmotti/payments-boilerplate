package payments

import (
	"context"
	"errors"
	"testing"
	"time"

	orderapp "github.com/rmotti/payments-boilerplate/internal/application/orders"
	orders "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
)

type orderReaderStub struct {
	order orders.Order
	err   error
}

func (s orderReaderStub) Get(context.Context, string) (orders.Order, error) {
	return s.order, s.err
}

type repositoryStub struct {
	payment domain.Payment
	attempt domain.Attempt
	err     error
	saved   *domain.Attempt
}

func (s *repositoryStub) PrepareCheckout(_ context.Context, payment domain.Payment, attempt domain.Attempt) (domain.Payment, domain.Attempt, error) {
	if s.err != nil {
		return domain.Payment{}, domain.Attempt{}, s.err
	}
	if s.attempt.ID != "" {
		return s.payment, s.attempt, nil
	}
	return payment, attempt, nil
}

func (s *repositoryStub) SaveSession(_ context.Context, attempt domain.Attempt) error {
	s.saved = &attempt
	return s.err
}

type providerStub struct {
	session domain.Session
	err     error
	request ProviderRequest
	calls   int
}

func (s *providerStub) CreateCheckout(_ context.Context, request ProviderRequest) (domain.Session, error) {
	s.calls++
	s.request = request
	return s.session, s.err
}

func newTestService(repo *repositoryStub, provider *providerStub, order orders.Order) *Service {
	fixed := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	return NewService(orderReaderStub{order: order}, repo, provider,
		WithClock(func() time.Time { return fixed }),
		WithIDGenerators(
			func() (string, error) { return "pay_fixed", nil },
			func() (string, error) { return "pat_fixed", nil },
		),
	)
}

func pendingOrder() orders.Order {
	return orders.Order{
		ID: "ord_0123456789abcdef0123456789abcdef", Status: orders.StatusPending,
		Amount: 20000, Currency: orders.BRL, ProductID: "product_demo", Quantity: 2,
	}
}

func TestCreateCheckoutUsesTrustedOrderDataAndPersistsSession(t *testing.T) {
	t.Parallel()

	expires := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	repo := &repositoryStub{}
	provider := &providerStub{session: domain.Session{
		ID: "cs_test_1", URL: "https://checkout.stripe.com/c/pay/test", ExpiresAt: expires,
	}}
	checkout, err := newTestService(repo, provider, pendingOrder()).CreateCheckout(context.Background(), CreateInput{
		OrderID: pendingOrder().ID, IdempotencyKey: "checkout-key",
	})
	if err != nil {
		t.Fatalf("CreateCheckout() error = %v", err)
	}
	if checkout.URL != provider.session.URL || !checkout.ExpiresAt.Equal(expires) {
		t.Fatalf("CreateCheckout() = %#v, want provider checkout", checkout)
	}
	want := ProviderRequest{
		OrderID: pendingOrder().ID, PaymentID: "pay_fixed", AttemptID: "pat_fixed",
		ProductID: "product_demo", UnitAmount: 10000, Quantity: 2,
		Currency: orders.BRL, IdempotencyKey: "checkout-key",
	}
	if provider.request != want {
		t.Fatalf("provider request = %#v, want %#v", provider.request, want)
	}
	if repo.saved == nil || !repo.saved.HasSession() || repo.saved.ProviderSessionID != "cs_test_1" {
		t.Fatalf("saved attempt = %#v, want attached session", repo.saved)
	}
}

func TestCreateCheckoutReplayReturnsPersistedSessionWithoutProviderCall(t *testing.T) {
	t.Parallel()

	expires := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	repo := &repositoryStub{
		payment: domain.Payment{ID: "pay_original"},
		attempt: domain.Attempt{
			ID: "pat_original", Status: domain.AttemptStatusPending,
			ProviderSessionID: "cs_test_original", CheckoutURL: "https://checkout.stripe.com/original", ExpiresAt: expires,
		},
	}
	provider := &providerStub{}
	checkout, err := newTestService(repo, provider, pendingOrder()).CreateCheckout(context.Background(), CreateInput{
		OrderID: pendingOrder().ID, IdempotencyKey: "checkout-key",
	})
	if err != nil {
		t.Fatalf("CreateCheckout() replay error = %v", err)
	}
	if checkout.URL != repo.attempt.CheckoutURL || provider.calls != 0 {
		t.Fatalf("replay = %#v with %d provider calls, want stored URL and no call", checkout, provider.calls)
	}
}

func TestCreateCheckoutMapsExpectedFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    CreateInput
		order    orders.Order
		orderErr error
		repoErr  error
		want     error
	}{
		{name: "invalid order id", input: CreateInput{OrderID: "bad", IdempotencyKey: "key"}, want: orderapp.ErrOrderNotFound},
		{name: "missing order", input: CreateInput{OrderID: pendingOrder().ID, IdempotencyKey: "key"}, orderErr: orderapp.ErrOrderNotFound, want: orderapp.ErrOrderNotFound},
		{name: "paid order", input: CreateInput{OrderID: pendingOrder().ID, IdempotencyKey: "key"}, order: func() orders.Order { value := pendingOrder(); value.Status = orders.StatusPaid; return value }(), want: ErrOrderNotPayable},
		{name: "active checkout", input: CreateInput{OrderID: pendingOrder().ID, IdempotencyKey: "key"}, order: pendingOrder(), repoErr: ErrCheckoutInProgress, want: ErrCheckoutInProgress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			order := tt.order
			if order.ID == "" {
				order = pendingOrder()
			}
			service := NewService(orderReaderStub{order: order, err: tt.orderErr}, &repositoryStub{err: tt.repoErr}, &providerStub{})
			_, err := service.CreateCheckout(context.Background(), tt.input)
			if !errors.Is(err, tt.want) {
				t.Fatalf("CreateCheckout() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCreateCheckoutWrapsProviderFailure(t *testing.T) {
	t.Parallel()

	provider := &providerStub{err: errors.New("stripe timeout")}
	_, err := newTestService(&repositoryStub{}, provider, pendingOrder()).CreateCheckout(context.Background(), CreateInput{
		OrderID: pendingOrder().ID, IdempotencyKey: "checkout-key",
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("CreateCheckout() error = %v, want %v", err, ErrProviderUnavailable)
	}
}

func TestCreateCheckoutWrapsInvalidProviderSession(t *testing.T) {
	t.Parallel()

	repo := &repositoryStub{}
	provider := &providerStub{session: domain.Session{
		ID: "cs_test_invalid", URL: "http://checkout.stripe.com/invalid",
		ExpiresAt: time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC),
	}}
	_, err := newTestService(repo, provider, pendingOrder()).CreateCheckout(context.Background(), CreateInput{
		OrderID: pendingOrder().ID, IdempotencyKey: "checkout-key",
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("CreateCheckout() error = %v, want %v", err, ErrProviderUnavailable)
	}
	if !errors.Is(err, domain.ErrInvalidSession) {
		t.Fatalf("CreateCheckout() error = %v, want %v", err, domain.ErrInvalidSession)
	}
	if repo.saved != nil {
		t.Fatalf("SaveSession() called with invalid provider session: %#v", repo.saved)
	}
}

// callRecorder is the surface the metrics observer sees: provider timing and
// idempotency outcomes, with no identifier in either.
type callRecorder struct {
	calls     []ProviderCall
	replays   int
	conflicts int
}

func (r *callRecorder) ProviderCalled(call ProviderCall) { r.calls = append(r.calls, call) }
func (r *callRecorder) Replayed()                        { r.replays++ }
func (r *callRecorder) Conflicted()                      { r.conflicts++ }

func newObservedService(
	repo *repositoryStub, provider *providerStub, order orders.Order, observer Observer,
) *Service {
	fixed := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	return NewService(orderReaderStub{order: order}, repo, provider,
		WithClock(func() time.Time { return fixed }),
		WithIDGenerators(
			func() (string, error) { return "pay_fixed", nil },
			func() (string, error) { return "pat_fixed", nil },
		),
		WithObserver(observer),
	)
}

func TestProviderCallIsReportedOnSuccessAndOnFailure(t *testing.T) {
	t.Parallel()

	success := &callRecorder{}
	provider := &providerStub{session: domain.Session{
		ID: "cs_test_1", URL: "https://checkout.stripe.com/c/pay/test",
		ExpiresAt: time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC),
	}}
	if _, err := newObservedService(&repositoryStub{}, provider, pendingOrder(), success).
		CreateCheckout(context.Background(), CreateInput{
			OrderID: pendingOrder().ID, IdempotencyKey: "checkout-key",
		}); err != nil {
		t.Fatalf("CreateCheckout() error = %v", err)
	}
	if len(success.calls) != 1 || success.calls[0].Err != nil {
		t.Fatalf("successful calls = %#v, want one without error", success.calls)
	}
	if success.calls[0].Operation != ProviderOperationCreateCheckout {
		t.Errorf("operation = %q, want create_checkout", success.calls[0].Operation)
	}
	if success.calls[0].Provider != domain.Stripe {
		t.Errorf("provider = %q, want stripe", success.calls[0].Provider)
	}
	if success.calls[0].Duration <= 0 {
		t.Error("provider call carries no duration")
	}

	failure := &callRecorder{}
	if _, err := newObservedService(&repositoryStub{},
		&providerStub{err: errors.New("provider unavailable")}, pendingOrder(), failure).
		CreateCheckout(context.Background(), CreateInput{
			OrderID: pendingOrder().ID, IdempotencyKey: "checkout-key",
		}); err == nil {
		t.Fatal("CreateCheckout() error = nil, want the provider failure")
	}
	if len(failure.calls) != 1 || failure.calls[0].Err == nil {
		t.Fatalf("failed calls = %#v, want one carrying the error", failure.calls)
	}
}

// A replay must be counted and must not reach the provider, which is the
// whole point of the idempotency key.
func TestIdempotentReplayIsReportedWithoutAProviderCall(t *testing.T) {
	t.Parallel()

	recorder := &callRecorder{}
	repo := &repositoryStub{
		payment: domain.Payment{ID: "pay_original"},
		attempt: domain.Attempt{
			ID: "pat_original", Status: domain.AttemptStatusPending,
			ProviderSessionID: "cs_test_original", CheckoutURL: "https://checkout.stripe.com/original",
			ExpiresAt: time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC),
		},
	}
	provider := &providerStub{}
	if _, err := newObservedService(repo, provider, pendingOrder(), recorder).
		CreateCheckout(context.Background(), CreateInput{
			OrderID: pendingOrder().ID, IdempotencyKey: "checkout-key",
		}); err != nil {
		t.Fatalf("CreateCheckout() replay error = %v", err)
	}
	if recorder.replays != 1 || len(recorder.calls) != 0 {
		t.Fatalf("replays = %d with %d provider calls, want 1 and 0", recorder.replays, len(recorder.calls))
	}
}

func TestIdempotencyConflictIsReported(t *testing.T) {
	t.Parallel()

	recorder := &callRecorder{}
	if _, err := newObservedService(&repositoryStub{err: ErrIdempotencyKeyConflict},
		&providerStub{}, pendingOrder(), recorder).
		CreateCheckout(context.Background(), CreateInput{
			OrderID: pendingOrder().ID, IdempotencyKey: "checkout-key",
		}); err == nil {
		t.Fatal("CreateCheckout() error = nil, want the conflict")
	}
	if recorder.conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1", recorder.conflicts)
	}
}
