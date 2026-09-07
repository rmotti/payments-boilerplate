package payments

import "testing"

// The matrix must be correct for any arrival order, because the provider does
// not order its events. Every case below is therefore written as "this state,
// this outcome", never as a sequence.
func TestDecide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outcome Outcome
		payment Status
		attempt AttemptStatus
		want    Transition
	}{
		// The normal path of each event type.
		{
			name:    "card payment completes",
			outcome: OutcomeSucceeded, payment: StatusPending, attempt: AttemptStatusPending,
			want: Transition{Decision: DecisionApply, Attempt: AttemptStatusSucceeded, Payment: StatusSucceeded, OrderPaid: true},
		},
		{
			name:    "session completes without settling",
			outcome: OutcomePending, payment: StatusPending, attempt: AttemptStatusPending,
			want: Transition{Decision: DecisionApply, Payment: StatusProcessing},
		},
		{
			name:    "asynchronous payment settles after processing",
			outcome: OutcomeSucceeded, payment: StatusProcessing, attempt: AttemptStatusPending,
			want: Transition{Decision: DecisionApply, Attempt: AttemptStatusSucceeded, Payment: StatusSucceeded, OrderPaid: true},
		},
		{
			name:    "asynchronous payment fails after processing",
			outcome: OutcomeFailed, payment: StatusProcessing, attempt: AttemptStatusPending,
			want: Transition{Decision: DecisionApply, Attempt: AttemptStatusFailed, Payment: StatusFailed},
		},
		{
			name:    "session expires unused",
			outcome: OutcomeExpired, payment: StatusPending, attempt: AttemptStatusPending,
			want: Transition{Decision: DecisionApply, Attempt: AttemptStatusExpired},
		},

		// Redelivery of every event type must be a no-op. This is what makes
		// the consumer idempotent under at-least-once delivery.
		{
			name:    "success redelivered",
			outcome: OutcomeSucceeded, payment: StatusSucceeded, attempt: AttemptStatusSucceeded,
			want: Transition{Decision: DecisionNoOp, Reason: "payment already succeeded"},
		},
		{
			name:    "failure redelivered",
			outcome: OutcomeFailed, payment: StatusFailed, attempt: AttemptStatusFailed,
			want: Transition{Decision: DecisionNoOp, Reason: "payment already failed"},
		},
		{
			name:    "completed session redelivered while processing",
			outcome: OutcomePending, payment: StatusProcessing, attempt: AttemptStatusPending,
			want: Transition{Decision: DecisionNoOp, Reason: "payment already processing"},
		},
		{
			name:    "expiry redelivered",
			outcome: OutcomeExpired, payment: StatusPending, attempt: AttemptStatusExpired,
			want: Transition{Decision: DecisionNoOp, Reason: "attempt already expired"},
		},

		// Negative events arriving after a positive terminal state are
		// ordinary reordering and must never regress it.
		{
			name:    "failure arrives after success",
			outcome: OutcomeFailed, payment: StatusSucceeded, attempt: AttemptStatusSucceeded,
			want: Transition{Decision: DecisionNoOp, Reason: "payment already succeeded"},
		},
		{
			name:    "expiry arrives after success",
			outcome: OutcomeExpired, payment: StatusSucceeded, attempt: AttemptStatusSucceeded,
			want: Transition{Decision: DecisionNoOp, Reason: "attempt already succeeded"},
		},
		{
			name:    "completed session arrives after success",
			outcome: OutcomePending, payment: StatusSucceeded, attempt: AttemptStatusSucceeded,
			want: Transition{Decision: DecisionNoOp, Reason: "payment already succeeded; a completed session adds nothing"},
		},
		{
			name:    "completed session arrives after failure",
			outcome: OutcomePending, payment: StatusFailed, attempt: AttemptStatusFailed,
			want: Transition{Decision: DecisionNoOp, Reason: "payment already failed; a completed session adds nothing"},
		},
		{
			name:    "expiry arrives after the payment failed",
			outcome: OutcomeExpired, payment: StatusFailed, attempt: AttemptStatusFailed,
			want: Transition{Decision: DecisionNoOp, Reason: "attempt already failed"},
		},

		// A positive event over a negative terminal state is a contradiction,
		// not a late delivery. It must be escalated, never ignored.
		{
			name:    "success arrives after the payment failed",
			outcome: OutcomeSucceeded, payment: StatusFailed, attempt: AttemptStatusFailed,
			want: Transition{Decision: DecisionConflict, Reason: "provider reports success over a payment already failed"},
		},
		{
			name:    "success arrives after the payment was cancelled",
			outcome: OutcomeSucceeded, payment: StatusCancelled, attempt: AttemptStatusCancelled,
			want: Transition{Decision: DecisionConflict, Reason: "provider reports success over a payment already cancelled"},
		},

		// A positive event over a negative terminal attempt is also a
		// contradiction. In particular, an old expired attempt must not settle a
		// payment while a newer attempt may still be externally payable.
		{
			name:    "success arrives after the session expired",
			outcome: OutcomeSucceeded, payment: StatusPending, attempt: AttemptStatusExpired,
			want: Transition{Decision: DecisionConflict, Reason: "provider reports success over an attempt already expired"},
		},
		{
			name:    "success arrives after the attempt failed",
			outcome: OutcomeSucceeded, payment: StatusPending, attempt: AttemptStatusFailed,
			want: Transition{Decision: DecisionConflict, Reason: "provider reports success over an attempt already failed"},
		},
		{
			name:    "success arrives after the attempt was cancelled",
			outcome: OutcomeSucceeded, payment: StatusPending, attempt: AttemptStatusCancelled,
			want: Transition{Decision: DecisionConflict, Reason: "provider reports success over an attempt already cancelled"},
		},
		{
			name:    "failure arrives after the session expired",
			outcome: OutcomeFailed, payment: StatusPending, attempt: AttemptStatusExpired,
			want: Transition{Decision: DecisionNoOp, Reason: "attempt already expired"},
		},
		{
			name:    "completed session arrives after its attempt expired",
			outcome: OutcomePending, payment: StatusPending, attempt: AttemptStatusExpired,
			want: Transition{
				Decision: DecisionNoOp,
				Reason:   "attempt already expired; a completed session adds nothing",
			},
		},

		// A partially applied aggregate must be completed, not rewritten. The
		// attempt already holds the target state, so touching it again would
		// update zero rows and be read as a violated invariant.
		{
			name:    "success completes a payment whose attempt already succeeded",
			outcome: OutcomeSucceeded, payment: StatusProcessing, attempt: AttemptStatusSucceeded,
			want: Transition{
				Decision: DecisionApply, Payment: StatusSucceeded, OrderPaid: true,
				Reason: "attempt already succeeded; settling the payment only",
			},
		},
		{
			name:    "failure completes a payment whose attempt already failed",
			outcome: OutcomeFailed, payment: StatusProcessing, attempt: AttemptStatusFailed,
			want: Transition{
				Decision: DecisionApply, Payment: StatusFailed,
				Reason: "attempt already failed; failing the payment only",
			},
		},
		{
			name:    "late success cannot undo a refund",
			outcome: OutcomeSucceeded, payment: StatusRefunded, attempt: AttemptStatusSucceeded,
			want: Transition{Decision: DecisionNoOp, Reason: "payment already refunded"},
		},
		{
			name:    "late pending event cannot undo a partial refund",
			outcome: OutcomePending, payment: StatusPartiallyRefunded, attempt: AttemptStatusSucceeded,
			want: Transition{Decision: DecisionNoOp, Reason: "payment already partially_refunded; a completed session adds nothing"},
		},

		{
			name:    "unknown outcome is never guessed",
			outcome: Outcome("weird"), payment: StatusPending, attempt: AttemptStatusPending,
			want: Transition{Decision: DecisionConflict, Reason: "unknown financial outcome: weird"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := Decide(test.outcome, test.payment, test.attempt)
			if got != test.want {
				t.Errorf("Decide(%q, %q, %q)\n got = %#v\nwant = %#v",
					test.outcome, test.payment, test.attempt, got, test.want)
			}
		})
	}
}

// An order only ever becomes paid, and only when the payment succeeds. No
// event in the matrix cancels or expires an order, and a decision that is not
// "apply" must never carry an effect.
func TestOrderOnlyBecomesPaidOnAppliedSuccess(t *testing.T) {
	t.Parallel()

	payments := allPaymentStatuses()
	attempts := []AttemptStatus{
		AttemptStatusCreated, AttemptStatusPending, AttemptStatusSucceeded,
		AttemptStatusFailed, AttemptStatusExpired, AttemptStatusCancelled,
	}
	outcomes := []Outcome{OutcomeSucceeded, OutcomePending, OutcomeFailed, OutcomeExpired}

	for _, outcome := range outcomes {
		for _, payment := range payments {
			for _, attempt := range attempts {
				got := Decide(outcome, payment, attempt)

				if got.Decision != DecisionApply {
					if got.Attempt != "" || got.Payment != "" || got.OrderPaid {
						t.Errorf("Decide(%q, %q, %q) = %#v, want no effect on a %s decision",
							outcome, payment, attempt, got, got.Decision)
					}
					continue
				}
				if got.OrderPaid && got.Payment != StatusSucceeded {
					t.Errorf("Decide(%q, %q, %q) pays the order without succeeding the payment: %#v",
						outcome, payment, attempt, got)
				}
			}
		}
	}
}

// An applied transition must always move something. A decision to apply that
// writes nothing would update zero rows, which the consumer reads as a
// violated invariant and sends to the dead-letter queue.
func TestAppliedTransitionsAlwaysCarryAnEffect(t *testing.T) {
	t.Parallel()

	payments := allPaymentStatuses()
	attempts := []AttemptStatus{
		AttemptStatusCreated, AttemptStatusPending, AttemptStatusSucceeded,
		AttemptStatusFailed, AttemptStatusExpired, AttemptStatusCancelled,
	}
	outcomes := []Outcome{OutcomeSucceeded, OutcomePending, OutcomeFailed, OutcomeExpired}

	for _, outcome := range outcomes {
		for _, payment := range payments {
			for _, attempt := range attempts {
				got := Decide(outcome, payment, attempt)
				if got.Decision != DecisionApply {
					continue
				}
				if got.Attempt == "" && got.Payment == "" && !got.OrderPaid {
					t.Errorf("Decide(%q, %q, %q) applies nothing: %#v", outcome, payment, attempt, got)
				}
				if got.Attempt != "" && got.Attempt == attempt {
					t.Errorf("Decide(%q, %q, %q) rewrites the attempt with its current state",
						outcome, payment, attempt)
				}
				if got.Payment != "" && got.Payment == payment {
					t.Errorf("Decide(%q, %q, %q) rewrites the payment with its current state",
						outcome, payment, attempt)
				}
			}
		}
	}
}

// allPaymentStatuses mirrors every value admitted by the database constraint,
// including states whose producing use cases belong to later roadmap phases.
// Keeping invariant sweeps over the complete vocabulary prevents a future
// state from becoming regressible before its endpoint is implemented.
func allPaymentStatuses() []Status {
	return []Status{
		StatusPending, StatusProcessing, StatusSucceeded, StatusFailed,
		StatusCancelled, StatusPartiallyRefunded, StatusRefunded,
	}
}
