-- +goose Up

-- Outbox of messages the relay still has to publish. A row is written in the
-- same transaction as the webhook_event that produced it, which is what closes
-- the gap between committing to PostgreSQL and publishing to RabbitMQ.
--
-- The row carries no provider payload: the message published from it references
-- the webhook_event, and the consumer reads the canonical copy from the inbox.
-- See docs/decisions/0011-webhook-reception-and-outbox.md.
CREATE TABLE outbox_events (
    -- Also travels as the published messageId, so the consumer deduplicates on
    -- the same value the relay can be asked about.
    id               TEXT        PRIMARY KEY,
    webhook_event_id TEXT        NOT NULL,
    -- Domain vocabulary, not the provider's event name. Keeping the message
    -- provider-neutral is what lets a second provider reuse the same contract.
    event_type       TEXT        NOT NULL,
    schema_version   INTEGER     NOT NULL,
    routing_key      TEXT        NOT NULL,
    correlation_id   TEXT        NOT NULL,
    occurred_at      TIMESTAMPTZ NOT NULL,
    status           TEXT        NOT NULL,
    attempts         INTEGER     NOT NULL DEFAULT 0,
    published_at     TIMESTAMPTZ,
    last_error       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT outbox_events_webhook_event_fkey
        FOREIGN KEY (webhook_event_id) REFERENCES webhook_events (id),
    CONSTRAINT outbox_events_status_check
        CHECK (status IN ('pending', 'published', 'failed')),
    CONSTRAINT outbox_events_attempts_check CHECK (attempts >= 0),
    CONSTRAINT outbox_events_schema_version_check CHECK (schema_version > 0),
    -- A published row must record when, and an unpublished one must not claim
    -- a publication instant.
    CONSTRAINT outbox_events_published_at_check
        CHECK ((status = 'published') = (published_at IS NOT NULL))
);

-- One message per received event. The inbox already rejects a redelivered
-- event, but this makes a second message impossible even if that changes.
CREATE UNIQUE INDEX outbox_events_webhook_event_id_key
    ON outbox_events (webhook_event_id);

-- The relay polls the oldest unpublished rows and skips locked ones. Published
-- rows are history and must not be walked.
CREATE INDEX outbox_events_unpublished_idx
    ON outbox_events (created_at)
    WHERE status <> 'published';

-- +goose Down

DROP TABLE outbox_events;
