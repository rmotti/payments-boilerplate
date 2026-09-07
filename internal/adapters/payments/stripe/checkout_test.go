package stripe

import (
	"context"
	"errors"
	"testing"

	"github.com/rmotti/payments-boilerplate/internal/application/payments"
	orders "github.com/rmotti/payments-boilerplate/internal/domain/orders"
	stripesdk "github.com/stripe/stripe-go/v86"
)

type sessionCreatorStub struct {
	params  *stripesdk.CheckoutSessionCreateParams
	session *stripesdk.CheckoutSession
	err     error
}

func (s *sessionCreatorStub) Create(_ context.Context, params *stripesdk.CheckoutSessionCreateParams) (*stripesdk.CheckoutSession, error) {
	s.params = params
	return s.session, s.err
}

func TestCreateCheckoutMapsHostedCardSession(t *testing.T) {
	t.Parallel()

	creator := &sessionCreatorStub{session: &stripesdk.CheckoutSession{
		ID: "cs_test_1", URL: "https://checkout.stripe.com/c/pay/test", ExpiresAt: 1788793200,
		PaymentIntent: &stripesdk.PaymentIntent{ID: "pi_test_1"},
	}}
	adapter := &Checkout{sessions: creator, successURL: "https://shop.test/success", cancelURL: "https://shop.test/cancel"}
	request := payments.ProviderRequest{
		OrderID: "ord_1", PaymentID: "pay_1", AttemptID: "pat_1", ProductID: "product_demo",
		UnitAmount: 10000, Quantity: 2, Currency: orders.BRL, IdempotencyKey: "checkout-key",
	}
	session, err := adapter.CreateCheckout(context.Background(), request)
	if err != nil {
		t.Fatalf("CreateCheckout() error = %v", err)
	}
	if session.ID != "cs_test_1" || session.PaymentIntentID != "pi_test_1" || session.URL != creator.session.URL {
		t.Fatalf("CreateCheckout() = %#v, want Stripe identifiers", session)
	}
	params := creator.params
	if params == nil || stripesdk.StringValue(params.Mode) != string(stripesdk.CheckoutSessionModePayment) ||
		stripesdk.StringValue(params.SuccessURL) != "https://shop.test/success" ||
		stripesdk.StringValue(params.CancelURL) != "https://shop.test/cancel" {
		t.Fatalf("params = %#v, want hosted payment URLs and payment mode", params)
	}
	if len(params.PaymentMethodTypes) != 1 || stripesdk.StringValue(params.PaymentMethodTypes[0]) != string(stripesdk.PaymentMethodTypeCard) {
		t.Fatalf("payment method types = %#v, want card", params.PaymentMethodTypes)
	}
	line := params.LineItems[0]
	if stripesdk.Int64Value(line.Quantity) != 2 || stripesdk.Int64Value(line.PriceData.UnitAmount) != 10000 ||
		stripesdk.StringValue(line.PriceData.Currency) != "brl" {
		t.Fatalf("line item = %#v, want 2 x 10000 BRL", line)
	}
	if params.Metadata["order_id"] != "ord_1" || params.Metadata["payment_id"] != "pay_1" ||
		params.Metadata["payment_attempt_id"] != "pat_1" || stripesdk.StringValue(params.IdempotencyKey) != "checkout-key" {
		t.Fatalf("metadata/idempotency = %#v/%q, want local references", params.Metadata, stripesdk.StringValue(params.IdempotencyKey))
	}
}

func TestCreateCheckoutWrapsSDKError(t *testing.T) {
	t.Parallel()

	boom := errors.New("network unavailable")
	adapter := &Checkout{sessions: &sessionCreatorStub{err: boom}}
	_, err := adapter.CreateCheckout(context.Background(), payments.ProviderRequest{})
	if !errors.Is(err, boom) {
		t.Fatalf("CreateCheckout() error = %v, want wrapped SDK error", err)
	}
}
