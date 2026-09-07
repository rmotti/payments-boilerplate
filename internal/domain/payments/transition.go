package payments

// Outcome is the financial result a provider event reports. It is the
// provider-neutral input of the transition matrix: the adapter translates the
// provider's own vocabulary into one of these before the domain sees it.
type Outcome string

// Financial outcomes understood by the transition matrix.
const (
	// OutcomeSucceeded means the provider confirmed the money was received.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomePending means the session completed but payment has not settled.
	// It is the normal path for Pix, boleto and every late-confirmation
	// method, where the session closes before the money arrives.
	OutcomePending Outcome = "pending"
	// OutcomeFailed means the provider reported the payment will not settle.
	OutcomeFailed Outcome = "failed"
	// OutcomeExpired means the checkout session expired unused. It says
	// nothing about the payment, only that this interaction is over.
	OutcomeExpired Outcome = "expired"
)

// Decision is what the transition matrix answers for one event.
type Decision int

// Transition decisions.
const (
	// DecisionApply means the event moves the aggregate forward. The caller
	// must then update exactly one row per entity; anything else is a
	// violated invariant, not a no-op.
	DecisionApply Decision = iota
	// DecisionNoOp means the event carries no new information: the effect is
	// already applied, or the event is a stale one that must not regress a
	// state reached later. The inbox entry is marked processed and the
	// message is acknowledged.
	DecisionNoOp
	// DecisionConflict means the event contradicts a terminal state that was
	// already recorded. It is never silently ignored: it signals a real
	// divergence between the provider and this installation, and the message
	// goes to the dead-letter queue for a human to look at.
	DecisionConflict
)

// String makes decisions readable in logs and test failures.
func (d Decision) String() string {
	switch d {
	case DecisionApply:
		return "apply"
	case DecisionNoOp:
		return "no-op"
	case DecisionConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// Transition is the full effect of one event on the aggregate. A zero target
// means the entity is left untouched.
type Transition struct {
	Decision Decision
	// Attempt, Payment and Order are the states to write. They are only
	// meaningful when Decision is DecisionApply, and an empty value means
	// that entity does not move.
	Attempt AttemptStatus
	Payment Status
	// OrderPaid reports whether the order becomes paid. The order has a
	// single transition in this flow, so a boolean says more than a status
	// would: no event in the matrix cancels or expires an order.
	OrderPaid bool
	// Reason explains a no-op or a conflict, for logs and for the inbox's
	// last_error. It is empty when the decision is to apply.
	Reason string
}

// attemptSettled reports whether an attempt reached a state no event may
// leave. A settled attempt is history: the interaction with the provider is
// over, whatever arrives afterwards.
func attemptSettled(status AttemptStatus) bool {
	switch status {
	case AttemptStatusSucceeded, AttemptStatusFailed, AttemptStatusExpired, AttemptStatusCancelled:
		return true
	default:
		return false
	}
}

// paymentSettled reports whether a payment reached a terminal financial state.
func paymentSettled(status Status) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled,
		StatusPartiallyRefunded, StatusRefunded:
		return true
	default:
		return false
	}
}

// Decide answers what an outcome does to a payment and its attempt.
//
// The provider does not order its events, so this must be correct for any
// arrival order. That is why it is written against the current state rather
// than against a sequence: every answer depends only on where the aggregate
// is now and what the event reports, never on what came before.
//
// The asymmetry between the branches is deliberate. A negative event arriving
// after a positive terminal state is an ordinary reordering and is ignored; a
// positive event arriving over a negative terminal state is a contradiction
// and is escalated. Both mean "the states disagree", but only one of them can
// be explained by the network.
func Decide(outcome Outcome, payment Status, attempt AttemptStatus) Transition {
	switch outcome {
	case OutcomeSucceeded:
		return decideSucceeded(payment, attempt)
	case OutcomePending:
		return decidePending(payment, attempt)
	case OutcomeFailed:
		return decideFailed(payment, attempt)
	case OutcomeExpired:
		return decideExpired(payment, attempt)
	default:
		return Transition{
			Decision: DecisionConflict,
			Reason:   "unknown financial outcome: " + string(outcome),
		}
	}
}

func decideSucceeded(payment Status, attempt AttemptStatus) Transition {
	if payment == StatusSucceeded && attempt == AttemptStatusSucceeded {
		return Transition{Decision: DecisionNoOp, Reason: "payment already succeeded"}
	}
	// A refund presupposes a successful payment. A delayed success notification
	// carries no new information and, above all, must not undo the refund.
	if payment == StatusPartiallyRefunded || payment == StatusRefunded {
		return Transition{Decision: DecisionNoOp, Reason: "payment already " + string(payment)}
	}
	// A success over a payment that was recorded as failed or cancelled is
	// not a late event: it means the provider charged something this
	// installation believes it did not. Ignoring it would hide exactly the
	// divergence the project promises to keep at zero.
	if paymentSettled(payment) {
		return Transition{
			Decision: DecisionConflict,
			Reason:   "provider reports success over a payment already " + string(payment),
		}
	}
	// A positive attempt can complete a partially applied aggregate without
	// rewriting the attempt. Negative terminal attempts are different: success
	// over failed, expired or cancelled contradicts the state already recorded
	// and must be inspected rather than silently settling the payment. This also
	// prevents a late event for an old attempt from succeeding a payment while a
	// newer attempt remains externally payable.
	if attempt == AttemptStatusSucceeded {
		return Transition{
			Decision:  DecisionApply,
			Payment:   StatusSucceeded,
			OrderPaid: true,
			Reason:    "attempt already succeeded; settling the payment only",
		}
	}
	if attemptSettled(attempt) {
		return Transition{
			Decision: DecisionConflict,
			Reason:   "provider reports success over an attempt already " + string(attempt),
		}
	}
	return Transition{
		Decision:  DecisionApply,
		Attempt:   AttemptStatusSucceeded,
		Payment:   StatusSucceeded,
		OrderPaid: true,
	}
}

func decidePending(payment Status, attempt AttemptStatus) Transition {
	if paymentSettled(payment) {
		return Transition{
			Decision: DecisionNoOp,
			Reason:   "payment already " + string(payment) + "; a completed session adds nothing",
		}
	}
	if payment == StatusProcessing {
		return Transition{Decision: DecisionNoOp, Reason: "payment already processing"}
	}
	// A completed-but-unpaid event for an attempt already closed is stale. It
	// must not move the shared payment while a newer attempt may be active.
	if attemptSettled(attempt) {
		return Transition{
			Decision: DecisionNoOp,
			Reason:   "attempt already " + string(attempt) + "; a completed session adds nothing",
		}
	}
	// The attempt deliberately stays open: the asynchronous event that
	// settles this payment will arrive on it.
	return Transition{Decision: DecisionApply, Payment: StatusProcessing}
}

func decideFailed(payment Status, attempt AttemptStatus) Transition {
	if payment == StatusFailed && attempt == AttemptStatusFailed {
		return Transition{Decision: DecisionNoOp, Reason: "payment already failed"}
	}
	// A failure after a success is the classic out-of-order delivery. The
	// money arrived; a later-delivered failure notice does not take it back.
	if payment == StatusSucceeded {
		return Transition{Decision: DecisionNoOp, Reason: "payment already succeeded"}
	}
	if paymentSettled(payment) {
		return Transition{
			Decision: DecisionNoOp,
			Reason:   "payment already " + string(payment),
		}
	}
	// A failed attempt can complete a partially applied aggregate without
	// rewriting itself. Other settled attempts make this a stale negative event:
	// changing the shared payment could invalidate a newer active attempt.
	if attempt == AttemptStatusFailed {
		return Transition{
			Decision: DecisionApply,
			Payment:  StatusFailed,
			Reason:   "attempt already failed; failing the payment only",
		}
	}
	if attemptSettled(attempt) {
		return Transition{Decision: DecisionNoOp, Reason: "attempt already " + string(attempt)}
	}
	return Transition{Decision: DecisionApply, Attempt: AttemptStatusFailed, Payment: StatusFailed}
}

func decideExpired(payment Status, attempt AttemptStatus) Transition {
	// Expiry touches the attempt alone. The payment stays pending so the
	// partial unique index frees a new attempt inside the same payment, and
	// the next checkout reuses it instead of opening a second one.
	if attemptSettled(attempt) {
		return Transition{
			Decision: DecisionNoOp,
			Reason:   "attempt already " + string(attempt),
		}
	}
	if paymentSettled(payment) {
		return Transition{
			Decision: DecisionNoOp,
			Reason:   "payment already " + string(payment) + "; the session expiring changes nothing",
		}
	}
	return Transition{Decision: DecisionApply, Attempt: AttemptStatusExpired}
}
