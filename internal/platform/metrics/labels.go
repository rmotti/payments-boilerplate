package metrics

import (
	"sort"

	"go.opentelemetry.io/otel/attribute"
)

// Label keys. Every attribute this package records uses one of these, and the
// value is always one of the key's allowed values or Other.
const (
	LabelProvider    = "provider"
	LabelEventKind   = "event.kind"
	LabelOutcome     = "outcome"
	LabelReason      = "reason"
	LabelOperation   = "operation"
	LabelEntity      = "entity"
	LabelFrom        = "from"
	LabelTo          = "to"
	LabelStage       = "stage"
	LabelDisposition = "disposition"
	LabelDestination = "destination"
	LabelQueue       = "queue"
	LabelState       = "state"
	LabelSampler     = "sampler"
)

// Other is the value every unrecognized input collapses to. Cardinality is a
// correctness property here: an identifier, a correlation id, a secret or an
// error message used as a label would multiply series without bound and, in
// the last two cases, publish something that must not leave the process.
const Other = "other"

// allowed enumerates every value each label key may carry. It is not derived
// from the domain on purpose: a new provider event kind or payment status
// must be an explicit decision recorded here and in ADR 0015, never a new
// time series that appears because an enum grew.
var allowed = map[string]map[string]struct{}{
	LabelProvider:  set("stripe"),
	LabelEventKind: set("checkout.completed", "checkout.payment_succeeded", "checkout.payment_failed", "checkout.expired"),
	LabelOutcome: set(
		// Webhook receive outcomes.
		"accepted", "duplicate", "ignored",
		// Provider call and generic outcomes.
		"success", "error",
		// Relay publication and cycle outcomes.
		"published", "retrying", "failed", "lease_lost", "empty",
	),
	LabelReason: set(
		// Webhook receive failures.
		"invalid_signature", "storage", "internal",
		// Relay publication failures.
		"not_confirmed", "not_routed", "permanent", "transient", "lease_lost",
		// Consumer no-ops and failures.
		"already_processed", "stale_event",
		"incompatible_api_version", "unreadable_payload", "missing_reference",
		"aggregate_not_found", "reference_mismatch", "amount_mismatch",
		"state_conflict", "invariant_violated", "unclassified",
	),
	LabelOperation:   set("create_order", "create_checkout"),
	LabelEntity:      set("order", "payment", "attempt"),
	LabelFrom:        set(statuses()...),
	LabelTo:          set(statuses()...),
	LabelStage:       set("lease", "settlement"),
	LabelDisposition: set("done", "retry", "dead", "unrecorded"),
	LabelDestination: set("retry", "dead_letter"),
	LabelState:       set("in_use", "idle"),
	LabelSampler:     set(SamplerBacklog, SamplerBroker),
}

// statuses lists every order, payment and attempt state a transition may name.
// Order and payment vocabularies overlap, so one set covers both ends of a
// transition without letting an unknown status through.
func statuses() []string {
	return []string{
		"pending", "processing", "succeeded", "failed", "cancelled", "expired",
		"partially_refunded", "refunded", "created", "paid",
	}
}

func set(values ...string) map[string]struct{} {
	members := make(map[string]struct{}, len(values))
	for _, value := range values {
		members[value] = struct{}{}
	}
	return members
}

// Normalize maps one value onto the allowlist of its key. An unknown key, an
// unknown value and an empty value all become Other, so a caller can never
// widen the label space by passing something new.
func Normalize(key, value string) string {
	values, known := allowed[key]
	if !known {
		return Other
	}
	if _, ok := values[value]; ok {
		return value
	}
	return Other
}

// Attr builds one normalized attribute.
func Attr(key, value string) attribute.KeyValue {
	return attribute.String(key, Normalize(key, value))
}

// AllowedValues reports the values a key may carry, sorted. Queue is absent:
// its values are the topology's queue names, which are validated against the
// configured topology rather than against a static list.
func AllowedValues(key string) []string {
	values, known := allowed[key]
	if !known {
		return nil
	}
	listed := make([]string, 0, len(values))
	for value := range values {
		listed = append(listed, value)
	}
	sort.Strings(listed)
	return listed
}

// LabelKeys reports every key this package may record, sorted.
func LabelKeys() []string {
	keys := []string{LabelQueue}
	for key := range allowed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
