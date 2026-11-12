-- Phase 6: change capture and admin audit.
-- Every row the worker (or an admin undo) writes is recorded with before/after
-- images in the same transaction. row_version lets undo detect later changes.

-- +goose Up
ALTER TABLE customers     ADD COLUMN row_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE credit_grants ADD COLUMN row_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE subscriptions ADD COLUMN row_version BIGINT NOT NULL DEFAULT 1;

-- +goose StatementBegin
CREATE FUNCTION bump_row_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER customers_row_version     BEFORE UPDATE ON customers
    FOR EACH ROW EXECUTE FUNCTION bump_row_version();
CREATE TRIGGER credit_grants_row_version BEFORE UPDATE ON credit_grants
    FOR EACH ROW EXECUTE FUNCTION bump_row_version();
CREATE TRIGGER subscriptions_row_version BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION bump_row_version();

CREATE TABLE admin_actions (
    id         BIGSERIAL   PRIMARY KEY,
    action     TEXT        NOT NULL CHECK (action IN ('replay', 'undo')),
    inbox_id   BIGINT      NOT NULL REFERENCES inbox(id),
    actor      TEXT        NOT NULL,
    reason     TEXT        NOT NULL,
    outcome    TEXT        NOT NULL CHECK (outcome IN ('done', 'refused')),
    detail     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX admin_actions_inbox_idx ON admin_actions (inbox_id, id);

CREATE TABLE change_log (
    id              BIGSERIAL   PRIMARY KEY,
    inbox_id        BIGINT      NOT NULL REFERENCES inbox(id),
    generation      INTEGER     NOT NULL,  -- inbox.replay_count when written (0 before any replay; replay_count arrives in 00005, step 5)
    source          TEXT        NOT NULL CHECK (source IN ('apply', 'undo')),
    admin_action_id BIGINT      REFERENCES admin_actions(id),
    seq             INTEGER     NOT NULL,
    table_name      TEXT        NOT NULL CHECK (table_name IN ('customers', 'credit_grants', 'subscriptions')),
    row_id          BIGINT      NOT NULL,
    op              TEXT        NOT NULL CHECK (op IN ('insert', 'update', 'delete')),
    before_image    JSONB,
    after_image     JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((op = 'insert') = (before_image IS NULL)),
    CHECK ((op = 'delete') = (after_image IS NULL)),
    CHECK ((source = 'undo') = (admin_action_id IS NOT NULL))
);
CREATE UNIQUE INDEX change_log_apply_once ON change_log (inbox_id, generation, seq) WHERE source = 'apply';
CREATE UNIQUE INDEX change_log_undo_once  ON change_log (admin_action_id, seq)      WHERE source = 'undo';
CREATE INDEX change_log_row_idx   ON change_log (table_name, row_id, id);
CREATE INDEX change_log_inbox_idx ON change_log (inbox_id, id);

-- +goose Down
DROP TABLE change_log;
DROP TABLE admin_actions;
DROP TRIGGER subscriptions_row_version ON subscriptions;
DROP TRIGGER credit_grants_row_version ON credit_grants;
DROP TRIGGER customers_row_version ON customers;
DROP FUNCTION bump_row_version();
ALTER TABLE subscriptions DROP COLUMN row_version;
ALTER TABLE credit_grants DROP COLUMN row_version;
ALTER TABLE customers     DROP COLUMN row_version;
