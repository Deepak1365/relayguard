-- The demo "subscription app" that webhooks update.
-- Rule: each paid invoice grants 1 credit per dollar paid (amount_paid / 100).

-- +goose Up
CREATE TABLE customers (
    id                 BIGSERIAL PRIMARY KEY,
    stripe_customer_id TEXT        NOT NULL UNIQUE,
    email              TEXT,
    credit_balance     BIGINT      NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A ledger of every credit grant. stripe_invoice_id is intentionally NOT unique:
-- many real side effects (emails, counters, API calls) have no natural key,
-- so RelayGuard must prevent duplicates at the event level, not rely on table constraints.
CREATE TABLE credit_grants (
    id                BIGSERIAL PRIMARY KEY,
    customer_id       BIGINT      NOT NULL REFERENCES customers(id),
    stripe_invoice_id TEXT        NOT NULL,
    amount            BIGINT      NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE subscriptions (
    id                     BIGSERIAL PRIMARY KEY,
    stripe_subscription_id TEXT        NOT NULL UNIQUE,
    customer_id            BIGINT      NOT NULL REFERENCES customers(id),
    status                 TEXT        NOT NULL,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE subscriptions;
DROP TABLE credit_grants;
DROP TABLE customers;
