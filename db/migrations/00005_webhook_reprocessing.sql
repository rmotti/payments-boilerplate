-- +goose Up

-- Replays keep the original inbox and outbox rows. This preserves the provider
-- deduplication key and message id while recording that an operator explicitly
-- asked the system to try the failed work again.
ALTER TABLE webhook_events
    ADD COLUMN replay_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN last_replayed_at TIMESTAMPTZ,
    ADD CONSTRAINT webhook_events_replay_count_check CHECK (replay_count >= 0);

-- Failed events are the operational queue for manual recovery, so keep their
-- inspection scan small as the audit table grows.
CREATE INDEX webhook_events_failed_idx
    ON webhook_events (received_at DESC)
    WHERE status = 'failed';

-- +goose Down

DROP INDEX webhook_events_failed_idx;
ALTER TABLE webhook_events
    DROP CONSTRAINT webhook_events_replay_count_check,
    DROP COLUMN last_replayed_at,
    DROP COLUMN replay_count;
