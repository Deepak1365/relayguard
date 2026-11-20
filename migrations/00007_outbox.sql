-- Phase 9 step 9a: transactional outbox for side effects (emails, API calls).
-- A row is written by app.Apply inside the same transaction and savepoint as
-- the event's effect, so it commits, rolls back or vanishes in a crash exactly
-- with that effect. A relay (later step) delivers pending rows at least once.
--
-- outbox_once: an inbox row has at most one committed apply (replay accepts
-- only dead/stale events, which never committed one; undone is terminal), so
-- (inbox_id, kind) identifies a message without a generation. The database
-- enforces it even if the application's reasoning were ever wrong.
--
-- idempotency_key is the business identity rg:stripe:<event_id>:<kind>, not
-- the row id, because ids restart after TRUNCATE ... RESTART IDENTITY.

-- +goose Up
CREATE TABLE outbox (
    id              BIGSERIAL   PRIMARY KEY,
    inbox_id        BIGINT      NOT NULL REFERENCES inbox(id),
    generation      INT         NOT NULL,
    kind            TEXT        NOT NULL
        CHECK (kind IN ('welcome_email', 'receipt_email', 'subscription_notice')),
    idempotency_key TEXT        NOT NULL UNIQUE,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sent', 'dead')),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ,
    CONSTRAINT outbox_once UNIQUE (inbox_id, kind),
    CHECK ((status = 'sent') = (sent_at IS NOT NULL))
);

-- The relay's claim scans only undelivered rows, oldest due first.
CREATE INDEX outbox_pending_idx ON outbox (next_attempt_at, id) WHERE status = 'pending';

-- +goose Down
DROP TABLE outbox;
