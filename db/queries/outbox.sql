-- Leases a batch of due messages to one relay instance.
--
-- The transaction around this is short and closes before any network I/O: the
-- lease, not a held row lock, is what keeps two relays off the same message.
-- FOR UPDATE SKIP LOCKED still matters, but only to keep two relays from
-- racing on the claim itself, which takes microseconds.
--
-- A row is due when it is pending past its backoff, or when it was leased and
-- the holder never settled it before the deadline, which is how a relay that
-- died releases its work.
--
-- Every instant here comes from now(), never from the caller. PostgreSQL is the
-- single clock: if instances computed deadlines against their own clocks, a
-- machine running a few seconds fast would declare another instance's lease
-- expired while it is still publishing.
-- name: LeaseOutboxBatch :many
UPDATE outbox_events
SET status = 'publishing',
    locked_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::float8),
    locked_by = sqlc.arg(owner),
    updated_at = now()
WHERE id IN (
    SELECT o.id
    FROM outbox_events o
    WHERE o.next_attempt_at <= now()
      AND (o.status = 'pending'
           OR (o.status = 'publishing' AND o.locked_until <= now()))
    ORDER BY o.next_attempt_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
RETURNING id, webhook_event_id, event_type, schema_version, routing_key,
          correlation_id, occurred_at, attempts, locked_until;

-- Only ever called after the broker confirmed the publication.
--
-- The lease must still be ours: locked_by identifies this exact process, and
-- locked_until must not have passed, or another relay may already have taken
-- the message over. Returning the row count is what lets the caller notice a
-- lost lease instead of reporting a publication that was never recorded.
-- name: MarkOutboxPublished :execrows
UPDATE outbox_events
SET status = 'published', published_at = now(), attempts = attempts + 1,
    last_error = NULL, locked_until = NULL, locked_by = NULL, updated_at = now()
WHERE id = sqlc.arg(id)
  AND locked_by = sqlc.arg(owner)
  AND locked_until > now();

-- Returns the message to the pool with a backoff deadline. Transient failures
-- never exhaust a budget; they simply wait longer before the next attempt.
-- name: MarkOutboxRetryable :execrows
UPDATE outbox_events
SET status = 'pending', attempts = attempts + 1, last_error = sqlc.arg(last_error),
    next_attempt_at = now() + make_interval(secs => sqlc.arg(backoff_seconds)::float8),
    locked_until = NULL, locked_by = NULL, updated_at = now()
WHERE id = sqlc.arg(id)
  AND locked_by = sqlc.arg(owner)
  AND locked_until > now();

-- Stops retrying a message the relay can never publish, such as one whose
-- payload cannot be encoded. Reserved for errors classified as permanent;
-- broker unavailability must never land here.
-- name: MarkOutboxFailed :execrows
UPDATE outbox_events
SET status = 'failed', attempts = attempts + 1, last_error = sqlc.arg(last_error),
    locked_until = NULL, locked_by = NULL, updated_at = now()
WHERE id = sqlc.arg(id)
  AND locked_by = sqlc.arg(owner)
  AND locked_until > now();

-- Operational visibility: how much work is waiting and how old it is.
-- name: OutboxBacklog :one
SELECT count(*) AS pending,
       coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8 AS oldest_age_seconds
FROM outbox_events
WHERE status IN ('pending', 'publishing');
