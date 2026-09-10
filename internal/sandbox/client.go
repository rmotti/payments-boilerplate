package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxResponseBody = 1 << 20

// Client calls the running local API through its public HTTP contract.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// CheckoutResult contains the reproducible demo order and its hosted session.
type CheckoutResult struct {
	OrderID     string    `json:"orderId"`
	OrderStatus string    `json:"orderStatus"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	CheckoutURL string    `json:"checkoutUrl"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// Order is the API's commercial view of one order.
type Order struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type checkoutResponse struct {
	CheckoutURL string    `json:"checkoutUrl"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewClient creates a bounded HTTP client for a validated sandbox config.
func NewClient(cfg Config) *Client {
	return &Client{
		baseURL: cfg.APIURL,
		apiKey:  cfg.APIKey,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// CreateCheckout creates one server-priced demo order and its real Stripe
// Checkout Session. Both idempotency keys carry a sandbox marker so cleanup
// can refuse to touch orders created by another workflow.
func (c *Client) CreateCheckout(ctx context.Context, quantity int) (CheckoutResult, error) {
	if quantity < 1 || quantity > 1000 {
		return CheckoutResult{}, errors.New("quantity must be between 1 and 1000")
	}
	suffix, err := randomSuffix()
	if err != nil {
		return CheckoutResult{}, err
	}

	var order Order
	if err := c.jsonRequest(ctx, http.MethodPost, "/v1/orders",
		map[string]any{"productId": "product_demo", "quantity": quantity},
		map[string]string{"Idempotency-Key": "sandbox_order_" + suffix}, &order); err != nil {
		return CheckoutResult{}, fmt.Errorf("create sandbox order: %w", err)
	}

	var checkout checkoutResponse
	path := "/v1/orders/" + order.ID + "/checkout"
	if err := c.jsonRequest(ctx, http.MethodPost, path, nil,
		map[string]string{"Idempotency-Key": "sandbox_checkout_" + suffix}, &checkout); err != nil {
		return CheckoutResult{}, fmt.Errorf("create sandbox checkout: %w", err)
	}

	return CheckoutResult{
		OrderID: order.ID, OrderStatus: order.Status, Amount: order.Amount,
		Currency: order.Currency, CheckoutURL: checkout.CheckoutURL,
		ExpiresAt: checkout.ExpiresAt,
	}, nil
}

// GetOrder reads the local commercial state through the authenticated API.
func (c *Client) GetOrder(ctx context.Context, orderID string) (Order, error) {
	var order Order
	if err := c.jsonRequest(ctx, http.MethodGet, "/v1/orders/"+orderID, nil, nil, &order); err != nil {
		return Order{}, fmt.Errorf("get sandbox order: %w", err)
	}
	return order, nil
}

// Health verifies that the local API is ready.
func (c *Client) Health(ctx context.Context) error {
	return c.jsonRequest(ctx, http.MethodGet, "/health", nil, nil, nil)
}

// CheckAuthentication proves that the selected integration key reaches an
// authenticated, read-only operation rather than merely the public health URL.
func (c *Client) CheckAuthentication(ctx context.Context) error {
	return c.jsonRequest(ctx, http.MethodGet, "/v1/webhook-events?limit=1", nil, nil, nil)
}

// PostEvent sends exact signed bytes to the public Stripe webhook endpoint.
func (c *Client) PostEvent(ctx context.Context, payload []byte, signature string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/webhooks/stripe", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Stripe-Signature", signature)
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call local webhook: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("read local webhook response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var reported apiError
		if json.Unmarshal(responseBody, &reported) == nil && reported.Code != "" {
			return fmt.Errorf("local webhook returned %s (%s): %s",
				response.Status, reported.Code, reported.Message)
		}
		return fmt.Errorf("local webhook returned %s", response.Status)
	}
	return nil
}

func (c *Client) jsonRequest(
	ctx context.Context,
	method, path string,
	input any,
	headers map[string]string,
	output any,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		request.Header.Set("X-API-Key", c.apiKey)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call local API: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("read local API response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var reported apiError
		if json.Unmarshal(responseBody, &reported) == nil && reported.Code != "" {
			return fmt.Errorf("local API returned %s (%s): %s", response.Status, reported.Code, reported.Message)
		}
		return fmt.Errorf("local API returned %s", response.Status)
	}
	if output != nil && len(bytes.TrimSpace(responseBody)) > 0 {
		if err := json.Unmarshal(responseBody, output); err != nil {
			return fmt.Errorf("decode local API response: %w", err)
		}
	}
	return nil
}

func randomSuffix() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate sandbox identifier: %w", err)
	}
	return strings.ToLower(hex.EncodeToString(value)), nil
}
