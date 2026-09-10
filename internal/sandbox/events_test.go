package sandbox

import (
	"encoding/json"
	"testing"
	"time"

	stripeadapter "github.com/rmotti/payments-boilerplate/internal/adapters/payments/stripe"
)

func TestEveryScenarioRendersAndVerifies(t *testing.T) {
	t.Parallel()

	reference := Reference{
		OrderID: "ord_0123456789abcdef0123456789abcdef", PaymentID: "pay_1", AttemptID: "pat_1",
		SessionID: "cs_test_1", PaymentIntentID: "pi_1", Amount: 10000, Currency: "BRL",
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, name := range ScenarioNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			payload, scenario, eventID, err := RenderEvent(name, "evt_test_"+name, reference, now)
			if err != nil {
				t.Fatalf("RenderEvent() error = %v", err)
			}
			if scenario.Name != name || eventID != "evt_test_"+name {
				t.Fatalf("rendered scenario/id = %q/%q", scenario.Name, eventID)
			}
			var event fixtureEnvelope
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatalf("rendered payload is invalid JSON: %v", err)
			}
			if event.APIVersion != stripeadapter.SupportedAPIVersion ||
				event.Data.Object.ID != reference.SessionID || event.Data.Object.AmountTotal != reference.Amount ||
				event.Data.Object.Metadata["payment_attempt_id"] != reference.AttemptID {
				t.Fatalf("rendered event = %#v", event)
			}
			signature, err := StripeSignature(payload, "whsec_local_test", now)
			if err != nil {
				t.Fatalf("StripeSignature() error = %v", err)
			}
			verified, err := stripeadapter.NewWebhook("whsec_local_test").Verify(payload, signature)
			if err != nil {
				t.Fatalf("fixture signature was rejected: %v", err)
			}
			if verified.ID != eventID || verified.Type != event.Type {
				t.Fatalf("verified event = %#v, want id/type %q/%q", verified, eventID, event.Type)
			}
		})
	}
}

func TestRenderEventRejectsUnknownScenarioAndIncompleteReference(t *testing.T) {
	t.Parallel()
	now := time.Now()
	if _, _, _, err := RenderEvent("unknown", "evt_1", Reference{}, now); err == nil {
		t.Fatal("unknown scenario error = nil")
	}
	if _, _, _, err := RenderEvent("card-paid", "evt_1", Reference{}, now); err == nil {
		t.Fatal("incomplete reference error = nil")
	}
}
