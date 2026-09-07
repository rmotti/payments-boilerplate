-- Lists inbox entries without returning either stored payload. Provider
-- payloads may contain customer data; the operational API exposes only the
-- metadata needed to diagnose delivery and processing.
-- name: ListWebhookEvents :many
SELECT e.id, e.provider, e.provider_event_id, e.event_type, e.status,
       e.attempts, e.received_at, e.processed_at, e.last_error, e.updated_at,
       e.replay_count, e.last_replayed_at,
       o.id AS outbox_id, o.status AS outbox_status,
       o.attempts AS outbox_attempts, o.published_at AS outbox_published_at,
       o.last_error AS outbox_last_error, o.next_attempt_at AS outbox_next_attempt_at
FROM webhook_events e
LEFT JOIN outbox_events o ON o.webhook_event_id = e.id
WHERE sqlc.arg(status)::text = '' OR e.status = sqlc.arg(status)::text
ORDER BY e.received_at DESC, e.id DESC
LIMIT sqlc.arg(result_limit);

-- name: GetWebhookEvent :one
SELECT e.id, e.provider, e.provider_event_id, e.event_type, e.status,
       e.attempts, e.received_at, e.processed_at, e.last_error, e.updated_at,
       e.replay_count, e.last_replayed_at,
       o.id AS outbox_id, o.status AS outbox_status,
       o.attempts AS outbox_attempts, o.published_at AS outbox_published_at,
       o.last_error AS outbox_last_error, o.next_attempt_at AS outbox_next_attempt_at
FROM webhook_events e
LEFT JOIN outbox_events o ON o.webhook_event_id = e.id
WHERE e.id = sqlc.arg(id);

-- Both rows are locked before replay eligibility is checked. Concurrent
-- requests therefore cannot count or enqueue the same replay twice.
-- name: LockWebhookEventForReplay :one
SELECT e.status AS event_status, o.status AS outbox_status
FROM webhook_events e
JOIN outbox_events o ON o.webhook_event_id = e.id
WHERE e.id = sqlc.arg(id)
FOR UPDATE OF e, o;

-- name: ResetWebhookEventForReplay :execrows
UPDATE webhook_events
SET status = CASE WHEN status = 'failed' THEN 'pending' ELSE status END,
    attempts = CASE WHEN status = 'failed' THEN 0 ELSE attempts END,
    processed_at = CASE WHEN status = 'failed' THEN NULL ELSE processed_at END,
    last_error = CASE WHEN status = 'failed' THEN NULL ELSE last_error END,
    replay_count = replay_count + 1,
    last_replayed_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND status IN ('pending', 'failed');

-- Reuse the original outbox message instead of creating a second row. The
-- inbox makes a repeated broker delivery harmless, while keeping one message
-- id per provider event makes replay auditable and deterministic.
-- name: ResetOutboxForReplay :execrows
UPDATE outbox_events
SET status = 'pending', attempts = 0, published_at = NULL, last_error = NULL,
    locked_until = NULL, locked_by = NULL, next_attempt_at = now(), updated_at = now()
WHERE webhook_event_id = sqlc.arg(webhook_event_id)
  AND status IN ('published', 'failed');
