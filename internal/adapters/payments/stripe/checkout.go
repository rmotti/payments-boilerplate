// Package stripe adapts the Stripe SDK to the payment provider port.
package stripe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/application/payments"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	stripesdk "github.com/stripe/stripe-go/v86"
)

type sessionCreator interface {
	Create(context.Context, *stripesdk.CheckoutSessionCreateParams) (*stripesdk.CheckoutSession, error)
}

// Checkout creates hosted, one-time card and Pix Checkout Sessions.
type Checkout struct {
	sessions   sessionCreator
	successURL string
	cancelURL  string
}

// NewCheckout creates a Stripe Checkout adapter using the official SDK.
func NewCheckout(secretKey, successURL, cancelURL string) *Checkout {
	client := stripesdk.NewClient(secretKey)
	return &Checkout{sessions: client.V1CheckoutSessions, successURL: successURL, cancelURL: cancelURL}
}

// CreateCheckout maps trusted local values to one hosted Stripe session.
func (c *Checkout) CreateCheckout(ctx context.Context, request payments.ProviderRequest) (domain.Session, error) {
	currency := strings.ToLower(string(request.Currency))
	params := &stripesdk.CheckoutSessionCreateParams{
		Mode:              stripesdk.String(stripesdk.CheckoutSessionModePayment),
		SuccessURL:        stripesdk.String(c.successURL),
		CancelURL:         stripesdk.String(c.cancelURL),
		ClientReferenceID: stripesdk.String(request.OrderID),
		PaymentMethodTypes: []*string{
			stripesdk.String(stripesdk.PaymentMethodTypeCard),
			stripesdk.String(stripesdk.PaymentMethodTypePix),
		},
		LineItems: []*stripesdk.CheckoutSessionCreateLineItemParams{{
			Quantity: stripesdk.Int64(int64(request.Quantity)),
			PriceData: &stripesdk.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:   stripesdk.String(currency),
				UnitAmount: stripesdk.Int64(request.UnitAmount),
				ProductData: &stripesdk.CheckoutSessionCreateLineItemPriceDataProductDataParams{
					Name: stripesdk.String(request.ProductID),
				},
			},
		}},
		Metadata: map[string]string{
			"order_id":           request.OrderID,
			"payment_id":         request.PaymentID,
			"payment_attempt_id": request.AttemptID,
		},
	}
	params.SetIdempotencyKey(request.IdempotencyKey)

	session, err := c.sessions.Create(ctx, params)
	if err != nil {
		return domain.Session{}, fmt.Errorf("create Stripe Checkout Session: %w", err)
	}
	if session == nil {
		return domain.Session{}, errors.New("create Stripe Checkout Session: empty response")
	}
	paymentIntentID := ""
	if session.PaymentIntent != nil {
		paymentIntentID = session.PaymentIntent.ID
	}
	return domain.Session{
		ID: session.ID, PaymentIntentID: paymentIntentID, URL: session.URL,
		ExpiresAt: time.Unix(session.ExpiresAt, 0).UTC(),
	}, nil
}
