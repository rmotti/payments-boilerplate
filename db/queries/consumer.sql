-- Locks the inbox entry the message refers to.
--
-- This is the first lock of the transaction and the one that serializes two
-- deliveries of the same event: a redelivery, or the relay republishing after
-- a lease was lost. The second arrival blocks here until the first commits and
-- then reads status = 'processed', which is what turns it into a no-op.
--
-- The lock is blocking, not SKIP LOCKED. Skipping would mean acknowledging a
-- message whose effect nobody applied, and if the transaction holding the row
-- then rolled back the effect would be lost entirely.
-- name: LockWebhookEvent :one
SELECT id, provider, provider_event_id, event_type, raw_payload, payload,
       status, attempts, received_at
FROM webhook_events
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- Locks the payment aggregate an event acts upon, in one statement.
--
-- Locking only the inbox row is not enough: two *different* events of the same
-- payment can be processed at once, read the same initial state and apply
-- incompatible transitions. The join takes the attempt, its payment and the
-- payment's order together, so events of one payment are serialized against
-- each other.
--
-- The rows are locked in a fixed order — attempt, payment, order — because two
-- transactions taking the same locks in different orders would deadlock. The
-- ORDER BY is what pins that order for the row-level locks PostgreSQL acquires
-- while executing the join.
--
-- The join itself is the correlation check: a row comes back only when the
-- attempt really belongs to that payment and the payment to that order. A
-- mismatch returns nothing, which the caller reports as an inconsistent
-- reference rather than a missing row.
-- name: LockPaymentAggregate :one
SELECT o.id AS order_id, o.status AS order_status,
       p.id AS payment_id, p.status AS payment_status, p.provider AS provider,
       p.amount AS amount, p.currency AS currency,
       a.id AS attempt_id, a.status AS attempt_status,
       a.provider_session_id AS session_id
FROM payment_attempts a
JOIN payments p ON p.id = a.payment_id AND p.provider = a.provider
JOIN orders o ON o.id = p.order_id
WHERE a.id = sqlc.arg(attempt_id)
  AND p.id = sqlc.arg(payment_id)
  AND o.id = sqlc.arg(order_id)
ORDER BY a.id, p.id, o.id
FOR UPDATE OF a, p, o;

-- Moves an attempt, refusing to leave a settled state.
--
-- The WHERE clause is the second line of defence. The transition matrix has
-- already approved this write against state read under the lock, so this can
-- only fail if the matrix and the schema disagree — and the caller treats zero
-- rows during an approved transition as a violated invariant, not a no-op.
-- name: TransitionPaymentAttempt :execrows
UPDATE payment_attempts
SET status = sqlc.arg(status), updated_at = now()
WHERE id = sqlc.arg(id)
  AND status NOT IN ('succeeded', 'failed', 'expired', 'cancelled');

-- Moves a payment, refusing to leave a terminal financial state.
-- name: TransitionPayment :execrows
UPDATE payments
SET status = sqlc.arg(status), updated_at = now()
WHERE id = sqlc.arg(id)
  AND status NOT IN (
      'succeeded', 'failed', 'cancelled', 'partially_refunded', 'refunded'
  );

-- Marks an order paid. Only a pending order moves, so a late event can never
-- revive one that was cancelled or expired.
-- name: MarkOrderPaid :execrows
UPDATE orders
SET status = 'paid', updated_at = now()
WHERE id = sqlc.arg(id)
  AND status = 'pending';

-- Records the provider references on the attempt once an event confirms them.
--
-- The session id is written only when the attempt does not have one yet. An
-- attempt whose session id already differs is a correlation error the caller
-- detects before reaching here; this clause makes the write itself incapable
-- of overwriting a different session.
-- name: AttachAttemptReferences :execrows
UPDATE payment_attempts
SET provider_session_id = coalesce(provider_session_id, sqlc.arg(session_id)),
    provider_payment_intent_id = coalesce(provider_payment_intent_id,
                                          sqlc.narg(payment_intent_id)),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND (provider_session_id IS NULL OR provider_session_id = sqlc.arg(session_id));

-- Closes an inbox entry that produced its effect, or that was deliberately a
-- no-op. Both are processed: the event has been dealt with and must never be
-- applied again.
-- name: MarkWebhookEventProcessed :execrows
UPDATE webhook_events
SET status = 'processed', processed_at = now(), attempts = attempts + 1,
    last_error = sqlc.narg(note), updated_at = now()
WHERE id = sqlc.arg(id)
  AND status <> 'processed';

-- Records a failed attempt and atomically decides whether the inbox entry stays
-- pending or is closed. The same statement that increments attempts compares
-- the resulting value with the budget; doing that later in the service could
-- publish the message to the DLQ while leaving this row pending.
-- name: RecordWebhookEventFailure :one
UPDATE webhook_events
SET attempts = attempts + 1,
    status = CASE
        WHEN sqlc.arg(terminal)::boolean
          OR attempts + 1 >= sqlc.arg(max_attempts)::integer
        THEN 'failed'
        ELSE status
    END,
    last_error = sqlc.arg(last_error), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING attempts, status = 'failed' AS failed;

-- Operational visibility: how many events are waiting or stuck.
-- name: WebhookInboxBacklog :one
SELECT
    count(*) FILTER (WHERE status IN ('pending', 'processing')) AS pending,
    count(*) FILTER (WHERE status = 'failed') AS failed,
    coalesce(extract(epoch FROM now() - min(received_at) FILTER (
        WHERE status IN ('pending', 'processing'))), 0)::float8 AS oldest_age_seconds
FROM webhook_events;
