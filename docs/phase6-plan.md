# Phase 6 plan (approved by the user 2026-11-11) — steps 1-3 in scope now

User decisions: strict row-version undo (Q2), row_version via BEFORE UPDATE trigger (Q1),
reset-data also wipes admin_actions (Q6), undone events not replayable (Q3), previews not audited (Q4),
no change_log retention for now (Q5). Optional experiments E4/E5: undecided (step 7).

## Step 1: migration 00004, all TRUNCATE sites, shared DB-test helpers
New file migrations/00004_change_log.sql (do NOT edit 00001-00003):

```sql
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
```
(Verify table/column names against 00001-00003 before using; adjust if the real names differ and report it.)

TRUNCATE sites that must include change_log, admin_actions (FKs to inbox make TRUNCATE inbox fail otherwise):
- internal/sim/check.go Reset (~line 22; used by every chaos run incl. naive)
- dev.ps1 reset-data (~line 42) — also wipes admin_actions (user decision)
- cmd/worker/main_db_test.go truncate helper (~line 90)
Move DB-test helpers into internal/dbtest/ so cmd/worker, internal/admin, internal/sim tests can share them.
dev.ps1 db-test becomes: go test -count=1 -p 1 -v over the DB-test packages (-p 1: packages share tables).
Acceptance: migrate-status shows 00004 applied; existing DB tests pass; reset-data works; a 1-seed chaos smoke completes (not evidence).

## Reviewer notes from step 1 (apply in later steps)
- The trigger bumps row_version on EVERY UPDATE, including no-ops (e.g. invoice.paid with amount_paid < 100 -> 0 credits, credit_balance + 0). So step 2's recorder MUST log every UPDATE it executes, even if nothing changed; add a DB test for a 0-credit invoice. Step 3's "+1 per update" chain check depends on this.
- Stale subscription upsert (ON CONFLICT DO UPDATE ... WHERE false) does not fire the BEFORE UPDATE trigger: no bump, no log row. Matches the plan.
- generation is NOT NULL with no default: step 2 writes 0 explicitly.
- Decide before step 5 what generation means on undo rows (generation reverted vs current replay_count); unique index covers apply rows only.
- Step 2 review (lock hazard for steps 5-6): change_log.inbox_id and admin_actions.inbox_id are FKs to inbox, so any admin replay/undo transaction that inserts them holds FOR KEY SHARE on that inbox row until commit. The worker fence (cmd/worker/main.go, SELECT ... FOR UPDATE on inbox) would then block behind it, and the claim's SKIP LOCKED would skip the row. Decide deliberately in step 5 (option: fence with FOR NO KEY UPDATE, still exclusive against other fences/claims) and test it.
- No BEFORE INSERT trigger: INSERTs could set row_version freely. App never does; step 3 chain check should require first version = 1. Do not edit 00004 (already applied).
- Optional in 00005: CHECK (generation >= 0), CHECK (seq >= 1).

## Step 2: capture in internal/app + worker
- New package internal/changelog: Change type, Recorder (INSERTs each change_log row immediately through the same pgx.Tx, i.e. the savepoint in process), pure diff formatting.
- app.Apply gets the recorder. Images via SQL: INSERT ... RETURNING to_jsonb(t.*); UPDATE: SELECT to_jsonb(c.*) ... FOR NO KEY UPDATE first, then UPDATE ... RETURNING to_jsonb(c.*) (review fix: FOR UPDATE conflicts with the FOR KEY SHARE that FK checks from credit_grants/subscriptions inserts hold on the customer row -> deadlock between two workers on one customer; FOR NO KEY UPDATE is what a plain UPDATE takes anyway. Step 6 undo must use the same lock); subscription upsert: pre-SELECT FOR NO KEY UPDATE, then upsert RETURNING (xmax = 0) AS inserted, to_jsonb(subscriptions.*); if pre-SELECT found none but upsert hit conflict -> return error (retry), don't log a wrong before image.
- Writing as it goes: the grant's log row must exist before the AfterGrant hook fires.
- Hooks.AfterGrant becomes func(string) error so tests can fail an apply after a logged write (covers the savepoint path main_db_test.go ~370 says is not exercised).
- Stale/skipped/lost claim: no rows (savepoint rolled back / fence fails before Apply).
- generation = 0 for now (replay_count comes in step 5).
- R3: to_jsonb(timestamptz) depends on session TimeZone; compare via jsonb_populate_record or set TimeZone=UTC in internal/db.
Tests (DB): TestDBApplyWritesChangeLog (customer.created 1 insert; invoice.paid 2 rows: grant insert seq1 then customer update balance 0->20 and row_version 1->2; subscription insert then update), TestDBStaleAndSkippedWriteNoChangeLog, TestDBApplyErrorAfterWriteRollsBackChangeLog (AfterGrant returns error -> 0 change_log, 0 grants, pending, last_error set), TestDBCrashMidApplyLeavesNoChangeLog (re-exec own test binary as child with fault.Always("after_grant"): exit 1, change_log 0, grants 0, balance 0, pending attempts=1; then expireLease + normal processOne -> done, exactly 2 change_log rows), TestDBChangeLogApplyOnceIndex, TestDBRowVersionTrigger.

## Step 3: harness CheckChangeLog (relayguard target only; naive skipped by design)
For this seed's inbox rows (event_id prefix evt_s<seed>_):
1. Count by status: done -> exact expected apply rows (customer.created 1, invoice.paid 2, subscription events 1) — mapping lives in the checker, independent of internal/app; stale/skipped/dead/pending -> 0.
2. Chain per (table_name,row_id) ordered by id: first is insert; each before_image equals previous after_image; row_version +1 per update.
3. Fold == final state: last after_image equals current row (compare with jsonb_populate_record(NULL::<table>, after_image) IS NOT DISTINCT FROM row, not JSON text); every app row has >=1 entry.
New violation kinds: changelog_missing, changelog_unexpected, changelog_chain_break, changelog_final_mismatch. cmd/chaos calls it for -target relayguard.
Negative control test (internal/sim): TestDBChangeLogCheckDetectsUnloggedWrite — raw UPDATE produces changelog_chain_break and changelog_final_mismatch.
Acceptance: control fails as designed; 1-seed smoke reports 0.

As implemented (deviations accepted by the lead/reviewer, 2026-11-12):
- Two-write negative control: one raw UPDATE cannot produce both kinds. If a logged write follows it, that write's before image no longer chains (changelog_chain_break); if it is the row's last write, the log no longer folds to the stored row (changelog_final_mismatch). TestDBChangeLogCheckDetectsUnloggedWrite therefore does an unlogged customer update followed by a logged invoice, plus an unlogged subscription update at the end, and asserts exactly those two violations.
- The count check also compares the tables in seq order and requires seq 1..n without gaps (changelog_unexpected).
- Extra controls: broken image link with row_version intact (a mutation check showed the +1 rule otherwise masks the image comparison), first row_version != 1, cross-scope damage, seq gap.
- Attribution (reviewer should-fix): new kind changelog_misattributed. Each apply row's content must belong to its event: row_id = image id; customer.created -> payload customer; grant -> payload invoice id and amount_paid/100; invoice customer update -> payload customer, the grant's customer_id, balance change = grant amount; subscription rows -> payload subscription and last_event_id = event id. Control: TestDBChangeLogCheckDetectsSwappedEvents.
- cmd/chaos flag -changelog-check (default true; relayguard only). Set it to false for workers built before phase 6 (e.g. bin/worker-phase4.exe), which write no change_log. The results meta line records on / off (flag) / n/a (naive).
- Step 5 follow-up: the count check counts all generations together; with replay it must count the current generation only.
- jsonb_populate_record ignores extra keys and turns missing keys into NULL: a future migration that adds a column must backfill change_log images or teach the checker the column (comment in internal/sim/changelog_check.go).

## Later steps (NOT now): 4 preview, 5 migration 00005 + replay fence (attempts, replay_count) + replay, 6 undo, 7 evidence.
Constraints: cmd/naive unchanged; every behaviour change tested; each step its own commit (lead commits after user approval).
