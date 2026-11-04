-- Durable inbox: every verified webhook is stored here before any processing.

-- +goose Up
CREATE TABLE inbox (
    id              BIGSERIAL PRIMARY KEY,
    provider        TEXT        NOT NULL,
    event_id        TEXT        NOT NULL,
    event_type      TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'done', 'skipped', 'dead')),
    attempts        INTEGER     NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at    TIMESTAMPTZ,
    -- Idempotency key: a redelivered event can never create a second row.
    UNIQUE (provider, event_id)
);

-- Lets the worker find ready events quickly without scanning finished ones.
CREATE INDEX inbox_ready_idx ON inbox (next_attempt_at, id) WHERE status = 'pending';

-- +goose Down
DROP TABLE inbox;
