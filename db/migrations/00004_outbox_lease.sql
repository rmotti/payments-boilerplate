-- +goose Up

-- Publishing to a broker is network I/O, and holding a database transaction
-- open across it couples the lock duration to how slow the broker is. Worse,
-- a batch that outlives the transaction timeout rolls back confirmations that
-- already happened, so messages the broker accepted get published again.
--
-- The lease replaces the held lock: a short transaction claims messages and
-- stamps a deadline, the publish happens with no transaction open, and a second
-- short transaction records the outcome. There is no renewal; a relay that dies
-- mid-publish simply never settles the message, and once the deadline passes
-- another instance may take it over.
--
-- Every deadline is computed by now() inside the database, never by a relay
-- process. With several instances, PostgreSQL is the only clock they share.
ALTER TABLE outbox_events
    ADD COLUMN locked_until    TIMESTAMPTZ,
    ADD COLUMN locked_by       TEXT,
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- 'publishing' means a relay holds a lease on the row. It is not a terminal
-- state: an expired lease returns the message to the pool.
ALTER TABLE outbox_events DROP CONSTRAINT outbox_events_status_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_status_check
    CHECK (status IN ('pending', 'publishing', 'published', 'failed'));

-- A leased row must carry both its holder and its deadline, and a row without
-- a lease must claim neither. Settling checks locked_by, so a half-populated
-- lease would let the wrong process settle the message.
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_lease_check
    CHECK ((status = 'publishing')
           = (locked_until IS NOT NULL AND locked_by IS NOT NULL));

DROP INDEX outbox_events_unpublished_idx;

-- The relay looks for work that is due: pending rows past their backoff, and
-- leased rows whose holder died. Published and failed rows are history.
CREATE INDEX outbox_events_claimable_idx
    ON outbox_events (next_attempt_at)
    WHERE status IN ('pending', 'publishing');

-- +goose Down

DROP INDEX outbox_events_claimable_idx;

CREATE INDEX outbox_events_unpublished_idx
    ON outbox_events (created_at)
    WHERE status <> 'published';

ALTER TABLE outbox_events DROP CONSTRAINT outbox_events_lease_check;
ALTER TABLE outbox_events DROP CONSTRAINT outbox_events_status_check;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_events_status_check
    CHECK (status IN ('pending', 'published', 'failed'));

ALTER TABLE outbox_events
    DROP COLUMN next_attempt_at,
    DROP COLUMN locked_by,
    DROP COLUMN locked_until;
