package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestClientCreatesDemoCheckoutWithSandboxKeys(t *testing.T) {
	t.Parallel()

	var orderKey, checkoutKey string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("X-API-Key") != "integration-key" {
			t.Error("request did not carry integration key")
		}
		switch request.URL.Path {
		case "/v1/orders":
			orderKey = request.Header.Get("Idempotency-Key")
			return testResponse(http.StatusCreated,
				`{"id":"ord_1","status":"pending","amount":10000,"currency":"BRL"}`), nil
		case "/v1/orders/ord_1/checkout":
			checkoutKey = request.Header.Get("Idempotency-Key")
			return testResponse(http.StatusCreated,
				`{"checkoutUrl":"https://checkout.stripe.test/cs_1","expiresAt":"2026-09-10T12:00:00Z"}`), nil
		default:
			return testResponse(http.StatusNotFound, `{}`), nil
		}
	})

	client := NewClient(Config{APIURL: "http://localhost:8080", APIKey: "integration-key"})
	client.http.Transport = transport
	result, err := client.CreateCheckout(context.Background(), 1)
	if err != nil {
		t.Fatalf("CreateCheckout() error = %v", err)
	}
	if result.OrderID != "ord_1" || result.CheckoutURL == "" {
		t.Fatalf("CreateCheckout() = %#v", result)
	}
	if !strings.HasPrefix(orderKey, "sandbox_order_") || !strings.HasPrefix(checkoutKey, "sandbox_checkout_") {
		t.Fatalf("idempotency keys = %q/%q, want sandbox markers", orderKey, checkoutKey)
	}
}

func TestPostEventPreservesTheSignedBytes(t *testing.T) {
	t.Parallel()
	payload := []byte("{\n  \"id\": \"evt_exact\"\n}")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		if string(body) != string(payload) {
			t.Errorf("body = %q, want exact signed bytes %q", body, payload)
		}
		if request.Header.Get("Stripe-Signature") != "signed" {
			t.Errorf("Stripe-Signature = %q", request.Header.Get("Stripe-Signature"))
		}
		return testResponse(http.StatusAccepted, `{}`), nil
	})

	client := NewClient(Config{APIURL: "http://localhost:8080"})
	client.http.Transport = transport
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.PostEvent(ctx, payload, "signed"); err != nil {
		t.Fatalf("PostEvent() error = %v", err)
	}
}

func testResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
