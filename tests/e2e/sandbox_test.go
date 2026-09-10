//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/sandbox"
)

func TestSandboxToolDrivesAndCleansAReproducibleFlow(t *testing.T) {
	h := newHarness(t, true)
	cfg := sandbox.Config{APIURL: h.apiBaseURL, APIKey: apiKeyA}
	client := sandbox.NewClient(cfg)

	checkout, err := client.CreateCheckout(context.Background(), 1)
	if err != nil {
		t.Fatalf("create sandbox checkout: %v", err)
	}
	store, err := sandbox.OpenStore(context.Background(), h.databaseURL)
	if err != nil {
		t.Fatalf("open sandbox store: %v", err)
	}
	defer func() { _ = store.Close() }()
	reference, err := store.Reference(context.Background(), checkout.OrderID)
	if err != nil {
		t.Fatalf("read sandbox reference: %v", err)
	}

	now := time.Now().UTC()
	payload, scenario, eventID, err := sandbox.RenderEvent("card-paid", "", reference, now)
	if err != nil {
		t.Fatalf("render sandbox fixture: %v", err)
	}
	signature, err := sandbox.StripeSignature(payload, webhookSecret, now)
	if err != nil {
		t.Fatalf("sign sandbox fixture: %v", err)
	}
	if err := client.PostEvent(context.Background(), payload, signature); err != nil {
		t.Fatalf("post sandbox fixture: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), pollDeadline)
	defer cancel()
	status, err := store.WaitForScenario(waitCtx, checkout.OrderID, eventID, scenario)
	if err != nil {
		t.Fatalf("wait sandbox pipeline: %v; last state: %#v", err, status)
	}

	removed, err := store.Cleanup(context.Background(), checkout.OrderID)
	if err != nil {
		t.Fatalf("clean sandbox flow: %v", err)
	}
	if removed.Orders != 1 || removed.Payments != 1 || removed.Attempts != 1 ||
		removed.WebhookEvents != 1 || removed.OutboxEvents != 1 {
		t.Fatalf("cleanup = %#v, want one row from every sandbox table", removed)
	}
}
