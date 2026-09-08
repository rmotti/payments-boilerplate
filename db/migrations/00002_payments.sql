-- +goose Up

-- Orders hold the commercial intent. Amount and currency are computed by the
-- server from the product catalog and never accepted from the client.
CREATE TABLE orders (
    id              TEXT        PRIMARY KEY,
    status          TEXT        NOT NULL,
    amount          BIGINT      NOT NULL,
    currency        TEXT        NOT NULL,
    product_id      TEXT        NOT NULL,
    quantity        INTEGER     NOT NULL,
    idempotency_key TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT orders_status_check
        CHECK (status IN ('pending', 'paid', 'cancelled', 'expired')),
    CONSTRAINT orders_amount_check CHECK (amount > 0),
    CONSTRAINT orders_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT orders_quantity_check CHECK (quantity > 0),
    -- Redundant on its own, since id is already unique. It exists so payments
    -- can point a composite foreign key at the priced order.
    CONSTRAINT orders_id_amount_currency_key UNIQUE (id, amount, currency)
);

-- Replaying POST /v1/orders with the same Idempotency-Key must not create a
-- second order.
CREATE UNIQUE INDEX orders_idempotency_key_key ON orders (idempotency_key);

-- Payments hold the consolidated financial state of an order.
CREATE TABLE payments (
    id         TEXT        PRIMARY KEY,
    order_id   TEXT        NOT NULL,
    provider   TEXT        NOT NULL,
    status     TEXT        NOT NULL,
    amount     BIGINT      NOT NULL,
    currency   TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Amount and currency are a snapshot of the order and must match it. The
    -- composite reference also blocks repricing the order while a payment
    -- exists, which is what makes the snapshot immutable on both sides.
    CONSTRAINT payments_order_fkey
        FOREIGN KEY (order_id, amount, currency)
        REFERENCES orders (id, amount, currency),
    CONSTRAINT payments_status_check
        CHECK (status IN (
            'pending',
            'processing',
            'succeeded',
            'failed',
            'cancelled',
            'partially_refunded',
            'refunded'
        )),
    CONSTRAINT payments_amount_check CHECK (amount > 0),
    CONSTRAINT payments_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    -- Lets attempts anchor their provider to this payment.
    CONSTRAINT payments_id_provider_key UNIQUE (id, provider)
);

-- An order accumulates payments over time: a rejected card leaves a failed row
-- behind and the customer tries again under a new one.
CREATE INDEX payments_order_id_idx ON payments (order_id);

-- At most one payment per order may be live or settled. Failed and cancelled
-- rows are history and do not block a retry, but a second payment can never be
-- opened next to one that is still running or already charged. This is the
-- database-level barrier against double charging.
CREATE UNIQUE INDEX payments_order_id_active_key
    ON payments (order_id)
    WHERE status NOT IN ('failed', 'cancelled');

-- Attempts record each call made to the provider under a payment, including
-- the idempotency key sent remotely and the identifiers it returned.
CREATE TABLE payment_attempts (
    id                         TEXT        PRIMARY KEY,
    payment_id                 TEXT        NOT NULL,
    provider                   TEXT        NOT NULL,
    status                     TEXT        NOT NULL,
    idempotency_key            TEXT        NOT NULL,
    provider_session_id        TEXT,
    provider_payment_intent_id TEXT,
    checkout_url               TEXT,
    expires_at                 TIMESTAMPTZ,
    failure_code               TEXT,
    failure_message            TEXT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- An attempt always talks to the same provider as its payment.
    CONSTRAINT payment_attempts_payment_fkey
        FOREIGN KEY (payment_id, provider) REFERENCES payments (id, provider),
    CONSTRAINT payment_attempts_status_check
        CHECK (status IN (
            'created',
            'pending',
            'succeeded',
            'failed',
            'expired',
            'cancelled'
        ))
);

-- Replaying POST /v1/orders/{orderId}/checkout with the same Idempotency-Key
-- must return the existing attempt instead of opening a second session.
CREATE UNIQUE INDEX payment_attempts_idempotency_key_key
    ON payment_attempts (idempotency_key);

-- Two concurrent checkout requests carrying different keys would otherwise open
-- two provider sessions for one payment, and the customer could pay both. Only
-- one attempt per payment may be open or settled; dead attempts step aside so a
-- retry stays possible.
CREATE UNIQUE INDEX payment_attempts_payment_id_active_key
    ON payment_attempts (payment_id)
    WHERE status NOT IN ('failed', 'expired', 'cancelled');

-- Incoming events carry the provider session id, so it has to resolve to at
-- most one attempt.
CREATE UNIQUE INDEX payment_attempts_provider_session_id_key
    ON payment_attempts (provider, provider_session_id)
    WHERE provider_session_id IS NOT NULL;

CREATE INDEX payment_attempts_payment_id_idx ON payment_attempts (payment_id);

-- Durable inbox of provider events. Rows are written inside the webhook
-- request, before any business effect is produced.
CREATE TABLE webhook_events (
    id                TEXT        PRIMARY KEY,
    provider          TEXT        NOT NULL,
    provider_event_id TEXT        NOT NULL,
    event_type        TEXT        NOT NULL,
    -- The exact request bytes whose signature was verified. jsonb reorders keys
    -- and drops formatting, so it cannot preserve the body as received. The
    -- signature header is not stored, therefore this column is not independent
    -- cryptographic proof and cannot by itself reverify the signature later.
    raw_payload       BYTEA       NOT NULL,
    -- The same event parsed, for querying and reprocessing.
    payload           JSONB       NOT NULL,
    status            TEXT        NOT NULL,
    attempts          INTEGER     NOT NULL DEFAULT 0,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    last_error        TEXT,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT webhook_events_status_check
        CHECK (status IN (
            'pending',
            'processing',
            'processed',
            'failed',
            'skipped'
        )),
    CONSTRAINT webhook_events_attempts_check CHECK (attempts >= 0)
);

-- The provider event id is what makes a redelivered event a no-op.
CREATE UNIQUE INDEX webhook_events_provider_event_id_key
    ON webhook_events (provider, provider_event_id);

-- Supports scanning for work still to be done without walking processed rows.
CREATE INDEX webhook_events_pending_idx
    ON webhook_events (received_at)
    WHERE status IN ('pending', 'failed');

-- +goose Down

DROP TABLE webhook_events;
DROP TABLE payment_attempts;
DROP TABLE payments;
DROP TABLE orders;
