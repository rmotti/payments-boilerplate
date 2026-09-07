-- Inserting the event is what deduplicates a redelivery: the unique index on
-- (provider, provider_event_id) turns the second arrival into zero rows, and
-- the caller learns it must not produce an outbox message.
-- name: InsertWebhookEvent :one
INSERT INTO webhook_events (
    id, provider, provider_event_id, event_type, raw_payload, payload, status, received_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $8
)
ON CONFLICT (provider, provider_event_id) DO NOTHING
RETURNING id;

-- Written in the same transaction as the event above.
-- name: InsertOutboxEvent :exec
INSERT INTO outbox_events (
    id, webhook_event_id, event_type, schema_version, routing_key,
    correlation_id, occurred_at, status, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $9
);
