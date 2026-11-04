-- Ordering for state-overwriting events: remember which event a subscription
-- row reflects, and let the inbox record events that arrived too late to apply.

-- +goose Up
ALTER TABLE subscriptions
    ADD COLUMN last_event_created BIGINT NOT NULL DEFAULT 0,  -- Stripe "created", Unix seconds
    ADD COLUMN last_event_id      TEXT;

ALTER TABLE inbox DROP CONSTRAINT inbox_status_check;
ALTER TABLE inbox ADD CONSTRAINT inbox_status_check
    CHECK (status IN ('pending', 'done', 'skipped', 'dead', 'stale'));

-- +goose Down
UPDATE inbox SET status = 'done' WHERE status = 'stale';
ALTER TABLE inbox DROP CONSTRAINT inbox_status_check;
ALTER TABLE inbox ADD CONSTRAINT inbox_status_check
    CHECK (status IN ('pending', 'done', 'skipped', 'dead'));

ALTER TABLE subscriptions
    DROP COLUMN last_event_id,
    DROP COLUMN last_event_created;
