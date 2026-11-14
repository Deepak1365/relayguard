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

## Steps 4, 5a, 5b (approved by the user 2026-11-12, all defaults)
User answers: Q1 -actor and -reason both required, no default. Q2 skipped is NOT replayable (dead/stale only).
Q3 step 5 split into 5a and 5b (separate commits). Q4 lock-mode switch stays in 5a. Q5 add the undone guard now (synthetic test).
Commits: step 4, then 5a, then 5b. The user pre-approved commit+push of step 4; 5a and 5b stop at checkpoint B.

### Step 4: preview (no migration)
- internal/admin/preview.go: Preview(ctx, pool, inboxID, Options{LockTimeout, StatementTimeout}) (PreviewResult, error).
  Runs the REAL app.Apply with changelog.NewApply(id, generation) in a READ COMMITTED tx that is always rolled back
  (defer tx.Rollback; no Commit anywhere in the file). SET LOCAL lock_timeout '2s', statement_timeout '5s' (Options for tests).
  Before rollback: read back change_log rows (inbox_id, generation, source='apply' ORDER BY seq) as the result; re-read inbox status.
  Errors: ErrNotFound, ErrNotPreviewable (done -> "use log <id>"), ErrBusy (55P03), ErrChanged (status changed during preview, or 23505 on change_log_apply_once).
  Status: pending/dead/stale/skipped previewable; done refused. Outcomes: apply (N changes), stale, skip, error (e.g. unknown customer, errConcurrentInsert).
  generation = 0 in step 4 (5a switches to inbox.replay_count). Output notes ids of inserted rows are provisional.
- internal/admin/log.go: Log(ctx, pool, id): all change_log rows (gen, source columns) + admin_actions rows.
- internal/admin/format.go: FormatPreview, FormatLog (pure, unit tested). Lines via changelog.Format.
- cmd/admin/main.go: `admin preview <id>`, `admin log <id>`; config via config.LoadDotEnv(".env") like the worker (never print it).
  Exit 0 when a preview completes (any outcome), non-zero for refused/busy/error.
- internal/dbtest: move invoiceObject/subscriptionObject (cmd/worker tests) and applyInbox (sim tests, as ApplyInbox) here.
- dev.ps1: build also builds bin\admin.exe; db-test adds ./internal/admin/. No dev.ps1 admin task (positional [int]$Count can't carry flags).
- Tests: admin TestFormatPreview/TestFormatLog; cmd/admin TestParseArgs; admin TestDBPreviewWritesNothing (jsonb_agg snapshot of
  customers, credit_grants, subscriptions, change_log, admin_actions, inbox identical before/after; mutation Rollback->Commit fails it);
  cmd/worker TestDBPreviewMatchesRealApply (same seq/table/op; update images equal incl row_version; inserts equal minus id/created_at/updated_at;
  mutation drop last row fails it); admin TestDBPreviewOutcomes (stale, skip, dead unknown-customer -> error then apply after customer.created, not found);
  admin TestDBPreviewRefusesDone (mutation remove check fails it); admin TestDBPreviewLockTimeout (other tx holds FOR NO KEY UPDATE on customer,
  200ms -> ErrBusy within 5s ctx; mutation remove lock_timeout fails it); sim TestDBChangeLogCheckAfterPreviews (preview every history() event
  before applying; 0 violations AND max(change_log.id) > count(*), proving gaps exist).
- Interim cost until 5a: the worker fence/claim still use FOR UPDATE, which conflicts with the preview's FK KEY SHARE on inbox (ms).

As implemented (reviewer should-fixes, 2026-11-12):
- Inbox-first lock: Preview reads the inbox row with FOR KEY SHARE before anything else (under lock_timeout). Without it, a preview of an event the worker has fenced could hold an uncommitted customer row (or a subscription pre-read lock) and then wait on the fence for its change_log FK while the worker waits on that row: 40P01, possibly aborting the worker. Test TestDBPreviewWaitsForFenceWithoutDeadlock (mutation without the lock: deadlock detected). Still harmless after 5a (fence FOR NO KEY UPDATE does not conflict with KEY SHARE).
- Error split is an allow-list: only app.ErrUnknownCustomer, app.ErrConcurrentInsert (exported for this) and JSON decode errors are OutcomeError (exit 0); everything else (timeouts, deadlocks, other constraint violations such as a duplicate customer, cancellation) is an error (exit 1). TestIsBusinessError, TestDBPreviewUnlistedFailureIsAnError.
- ErrChanged via change_log_apply_once: TestDBPreviewDetectsConcurrentApply.
- TestDBPreviewMatchesRealApply also ignores updated_at in subscription UPDATE after-images (the upsert sets now()); everything else in update images, row_version included, must match.
- Extras: TestPreviewSourceHasNoCommit (source guard), TestDBLog, TestRunUsageError; dev.ps1 build stops at the first failing binary. On success the preview's savepoint is left open so the deferred tx.Rollback is the single, tested discard point.

### Step 5a: migration 00005, replay fence, lock mode, generation-aware checker
- migrations/00005_replay.sql: inbox.replay_count INTEGER NOT NULL DEFAULT 0 CHECK (>= 0); change_log CHECK (generation >= 0), CHECK (seq >= 1). Down drops them.
- Fence token (replay_count, attempts): replay resets attempts, so attempts alone has an ABA problem.
- Worker claim CTE: FOR NO KEY UPDATE SKIP LOCKED; fence: FOR NO KEY UPDATE (resolves the step-2 lock hazard; still mutually exclusive).
  claim RETURNING attempts, replay_count -> claimToken{attempts, generation}; fence adds AND replay_count=$3; deadLetterCrashed adds AND replay_count=$4;
  changelog.NewApply(id, tok.generation). admin.Preview and dbtest.ApplyInbox use replay_count.
- Generation meaning: change_log.generation = inbox.replay_count at the fence. Older generations are always empty (replay only starts
  from dead/stale, which have 0 apply rows). Undo rows (step 6) take the generation of the apply they revert.
- Checker: checkApplyCounts joins on cl.generation = i.replay_count; apply rows with generation <> replay_count -> changelog_unexpected.
  Chain/final/attribution unchanged.
- Tests: dbtest TestDBSchemaReplay; worker TestDBKeyShareDoesNotBlockClaimOrFence (mutations: claim/fence back to FOR UPDATE);
  TestDBFenceLockBlocksReclaimMidApply; TestDBReplayFencesOldClaim (raw SQL replay in 5a; mutation drop replay_count from fence);
  TestDBDeadLetterFencedByGeneration; sim TestDBChangeLogCheckCountsCurrentGenerationOnly (synthetic); TestDBChangeLogCheckReplayedEventClean.
- Regression adds a poison smoke: chaos seeds 1-2 -poison 2 (with crash/dup/reorder/lease 1s).

As implemented (5a, 2026-11-12; reviewed and approved, reviewer follow-ups applied):
- 00005 constraint names: inbox_replay_count_nonneg, change_log_generation_nonneg, change_log_seq_positive. Down drops the two
  change_log CHECKs and the column (which drops its CHECK). Applied to the dev DB.
- Worker: claimToken{attempts, generation}; claim, process and deadLetterCrashed take/return it. Fence and dead-letter WHERE include
  replay_count. The new tests live in cmd/worker/fence_db_test.go; replays there are raw SQL (replayRaw) plus markDead for the outcome
  another worker reached meanwhile.
- Checker: the other-generation rule is separate from the current-generation comparison, so rows filed under the wrong generation give
  BOTH changelog_missing (current generation empty) and changelog_unexpected (rows elsewhere), one each per event. Generations above
  replay_count count as "other" too (<>, not <; mutation-tested). Naive runs are unaffected: cmd/chaos only calls CheckChangeLog for
  -target relayguard.
- Small deviations: dbtest.ApplyInbox also switched its inbox lock from FOR UPDATE to FOR NO KEY UPDATE to match the worker;
  TestDBHeldFenceLockBlocksReclaim's simulated fence likewise. TestDBPreviewWaitsForFenceWithoutDeadlock keeps FOR UPDATE on purpose
  (it is the conflicting case the inbox-first lock guards against); its comment and Preview's now say so.
- Extras: TestDBKeyShareDoesNotBlockClaimOrFence has two subtests (explicit FOR KEY SHARE, and an admin_actions FK insert, the real
  source); TestDBPreviewUsesReplayCount (generation = replay_count; a committed row in an older generation does not conflict, one in
  the current generation gives ErrChanged); unit test TestOtherGenerationRows. The main_db_test.go header note "fence lock not
  directly tested" is replaced: TestDBFenceLockBlocksReclaimMidApply tests it from inside a real apply (AfterGrant hook).
- Mutations run (each applied, test failed, reverted): claim FOR UPDATE SKIP LOCKED and fence FOR UPDATE -> KeyShare test; fence without
  lock -> FenceLockBlocksReclaimMidApply (-count=3); fence without replay_count, worker generation 0 -> ReplayFencesOldClaim;
  dead-letter without replay_count -> DeadLetterFencedByGeneration; checker all-generation count (2 variants), no other-generation
  rule, < instead of <> -> CountsCurrentGenerationOnly; ApplyInbox generation 0 -> ReplayedEventClean; Preview generation 0 ->
  PreviewUsesReplayCount.
- Preview vs worker after 5a: the preview no longer queues behind the worker. Its FOR KEY SHARE on the inbox row does not conflict
  with the fence's FOR NO KEY UPDATE, so it overlaps an apply in progress. That overlap is bounded by lock_timeout (it gives up with
  ErrBusy on the first key the worker holds uncommitted: the customer unique key for customer.created, the worker's
  change_log_apply_once key for invoice.paid) and by the ErrChanged checks (change_log_apply_once collision; re-read of status
  and replay_count, both at the end and after a failed apply, see below).
  Test TestDBPreviewOverlapsRealFence (worker tx = real fence lock + real app.Apply; subtests "worker inserted the customer" and
  "worker logged the grant") proves: the preview no longer blocks on the inbox lock, gives up with ErrBusy on the worker's
  uncommitted keys within lock_timeout, writes nothing, and the worker then commits the event done with exactly its change_log
  rows. Stable over -count=20. Mutation without lock_timeout -> 57014 statement timeout instead of ErrBusy, fails it. It does NOT
  prove freedom from deadlock (200ms lock_timeout < deadlock_timeout 1s, and the worker never waits on the preview); that rests on
  reasoning: the preview runs the same app.Apply, so it takes locks in the same order, and its one extra lock (inbox KEY SHARE,
  taken first) conflicts with neither the fence nor the apply. TestDBPreviewWaitsForFenceWithoutDeadlock is kept for a FOR UPDATE
  holder.
- Reviewer follow-up: the final re-read compares status AND replay_count (a replay that re-fails during the preview is dead -> dead
  in a newer generation, which status and change_log_apply_once both miss). Test TestDBPreviewDetectsReplayDuringPreview uses an
  unexported test hook Options.beforeReread to commit that change just before the re-read; mutation "status only" fails it.
- Reviewer round 2: a failed apply outside the business allow-list now also re-reads. This is the NORMAL customer.created race
  since 5a: the preview overlaps the worker, waits (well inside the 2s lock_timeout) on the worker's uncommitted customer insert,
  and gets 23505 when the worker commits (before 5a it queued behind the fence and got ErrNotPreviewable). The error branch rolls
  back the aborted savepoint, re-reads status and replay_count (shared helper stateMoved), and returns ErrChanged wrapping the
  original error if either moved; otherwise it classifies as before. If the rollback or the re-read fails, the original error is
  returned, never the re-read's. Exit code is unchanged (1, like every refusal); the message now says the event changed.
  Test TestDBPreviewWorkerCommitsDuringPreview (preview with 2s lock_timeout in a goroutine; once pg_blocking_pids shows it waiting
  on the worker, the worker marks done and commits; ErrChanged that still wraps the 23505; stable over -count=20). Mutations: no
  re-read in the error branch -> raw 23505, fails it; re-read without the savepoint rollback -> the re-read fails on the aborted
  transaction, the original 23505 is returned, fails it. TestDBPreviewUnlistedFailureIsAnError (no state change) still exits 1
  with a plain error.
- Regression (experiment-runner, 2026-11-12, uncommitted tree = not evidence, outputs in scratchpad only) on the final 5a build:
  crash smoke seeds 1-3 (-dup 0.2 -reorder 10 -crash-rate 0.1 -kill-interval 2s -lease 1s, changelog check on): 0 violations;
  poison smoke seeds 1-2 (same flags + -poison 2): 0 violations, dead=2 per seed. An earlier identical run (before the preview-only
  round-2 fix) also gave 0 violations, dead=2.

### Step 5b: replay (audited)
- admin.Replay(ctx, pool, ReplayRequest{InboxID, Actor, Reason}, Options) (ReplayResult, error). Only dead and stale; refuse done, pending,
  skipped, and any event with an admin_actions undo/done row (guard; undo arrives in step 6).
- Tx: lock_timeout 2s; SELECT inbox FOR NO KEY UPDATE; undone guard; refuse -> audit row outcome 'refused' then ErrNotReplayable;
  accept -> UPDATE status='pending', attempts=0, last_error=NULL, processed_at=NULL, next_attempt_at=now(), replay_count+1, audit 'done'
  with detail {from_status, from_attempts, from_last_error, from_processed_at, generation}. Busy -> ErrBusy, nothing written. Unknown id -> ErrNotFound, no audit.
- Input: actor and reason trimmed, non-empty, <= 200 / 1000 chars. CLI: admin replay <id> -actor <name> -reason "<text>"; warn when last_error says crashed (poison).
- Tests: TestDBReplayResetsDeadRow (mutation keep last_error fails it), TestDBReplayRefuses (pending/done/skipped; mutation widen allow-list),
  TestDBReplayRefusesUndone (synthetic; mutation remove guard), TestDBReplayValidatesInput, TestDBReplayBusy, TestDBReplayTwice,
  worker TestDBReplayedDeadEventApplies, TestDBReplayedStaleEventIsStaleAgain, TestDBReplayFencesOldClaim switched to admin.Replay.
- Optional compatibility smoke: 1 seed with bin/worker-phase4.exe -changelog-check=false against 00005.
- 00004 notes: admin_actions has no generation column (kept in detail JSON); outcome only done/refused (busy writes nothing);
  inbox status has no 'undone' (step 6 needs 00006 if it wants one).

As implemented (5b, implementer, 2026-11-12; reviewed, with the follow-ups below):
- No new migration. admin_actions columns used as in 00004; refused detail = {status, attempts, generation, refused}; done detail =
  {from_status, from_attempts, from_last_error, from_processed_at, generation (= the new replay_count)}.
- internal/admin/replay.go: Replay, ReplayRequest/Result, NormalizeReplayRequest (pure; trim, non-empty, <= 200 / 1000 runes, id > 0),
  ErrNotReplayable, ErrInvalidInput. Flow as planned: validate (no DB), lock_timeout/statement_timeout, SELECT inbox FOR NO KEY UPDATE,
  undone guard (min id of an undo row with outcome 'done'; a refused undo does not count), allow-list dead/stale, then either an audited
  refusal (committed, returns ErrNotReplayable) or ONE UPDATE (status pending, attempts 0, last_error NULL, processed_at NULL,
  next_attempt_at now(), replay_count+1) plus a 'done' audit row. The from_* values come from the locked SELECT (PG 17 UPDATE RETURNING
  has no old values); the lock guarantees nothing changed in between. Options gained an unexported test hook afterLock.
- cmd/admin: `admin replay <id> -actor <name> -reason "<text>"`. Flags go AFTER the id; any order; both required. parseArgs applies
  NormalizeReplayRequest, so missing/blank/too long flags are usage errors (exit 2) before settings or the DB are touched. Exit 0 =
  replay done; 1 = refused, busy, not found or error (a refusal still prints the REPLAY report, since it is audited). FormatReplay
  warns at two levels (review fix): strong ("will likely crash it again") only for "...last error: none recorded (the worker crashed
  or was killed while processing)", i.e. no attempt ever recorded a result; soft ("final attempt(s) ended without a result ... if it
  is a poison event it will dead-letter again") for the bare "gave up after " prefix, which a single kill or lease expiry on the last
  attempt also writes. Both texts are constants in internal/app/deadletter.go, used by the worker and by admin.CrashedLastError, so
  they cannot drift. A last_error starting "decode payload" gets a mild "will most likely fail the same way" warning.
- Review follow-ups: (1) Replay sets Outcome=refused only after the refusal's audit insert and commit succeed, and the CLI prints the
  report only when err is nil or ErrNotReplayable (replayReportable); tests TestReplayReportable and
  TestDBReplayRefusalAuditFailureReportsNoOutcome (a trigger forces the audit insert to fail). On a commit error the commit may
  still have succeeded: check `admin log <id>`. (2) the poison warning levels above. (3) setStatus now puts next_attempt_at an hour
  ahead so the "claimable at once" check can fail. (4) the undo-guard read error goes through classify.
- Lock order and deadlock reasoning (reasoning, not a test claim): Replay's only lock is the inbox row, FOR NO KEY UPDATE, taken by its
  first data statement while it holds nothing else; everything after (the undo lookup, the UPDATE, the admin_actions insert whose FK takes
  KEY SHARE on the row it already holds more strongly) needs no further lock that another transaction could hold while waiting for this
  one. A transaction that waits at most once, and only before it holds anything, cannot be in a wait cycle. Against the others:
  worker claim and fence take the same lock (FOR NO KEY UPDATE), so they are mutually exclusive with Replay: a claim SKIPs the row, a
  fence waits (bounded by the short Replay transaction; Replay's wait is bounded by lock_timeout), and the fence's own token check
  (attempts, replay_count) makes a claim taken before the replay lose afterwards. Preview takes only KEY SHARE on the inbox row, which
  does not conflict with NO KEY UPDATE, so Replay and Preview never wait for each other; the preview's re-read of status and
  replay_count turns a replay during a preview into ErrChanged. Admin undo (step 6) must follow the same rule: inbox row first.
- Tests (DB): admin TestDBReplayResetsDeadRow (dead and stale; row fields, next_attempt_at not in the future, trimmed actor/reason,
  audit detail matches the from_* values and generation = new replay_count), TestDBReplayRefuses (pending/done/skipped: ErrNotReplayable,
  inbox/app/change_log tables unchanged, exactly one 'refused' audit row), TestDBReplayRefusesUndone (synthetic undo rows; control: a
  refused undo does not block), TestDBReplayValidatesInput (snapshot of all tables unchanged), TestDBReplayNotFound (no audit),
  TestDBReplayBusy (ErrBusy within the deadline, nothing written, works after unlock), TestDBReplayTwice (replay_count 2, two done rows
  with generations 1 and 2), TestDBReplayConcurrentSecondIsRefused (first replay held inside its tx by the afterLock hook, second
  verified waiting via pg_stat_activity, then refused as pending: proves the exclusive lock between replays), TestDBPreviewAfterReplay
  UsesNewGeneration, TestDBPreviewDetectsRealReplay (real Replay committed during a preview -> ErrChanged naming generation 0 -> 1).
  Unit: TestNormalizeReplayRequest, TestReplayValidatesBeforeDB (nil pool), TestFormatReplay, TestCrashedLastError, cmd/admin
  TestParseArgs (replay cases) and TestRunUsageError (replay without -reason exits 2).
  Worker: TestDBReplayedDeadEventApplies (dead on unknown customer -> customer arrives -> real Replay -> real worker applies; 2 change_log
  rows in generation 1, seq 1..2; grant and balance exactly once), TestDBReplayedStaleEventIsStaleAgain (no new change_log rows,
  subscription unchanged, replay_count 1). TestDBReplayFencesOldClaim and TestDBDeadLetterFencedByGeneration now use admin.Replay;
  sim TestDBChangeLogCheckReplayedEventClean now uses admin.Replay too. replayRaw was deleted: admin.Replay produces every state those
  tests need. markDead stays raw SQL (the real worker only reaches "dead" after maxAttempts, and the tests need it while a claim is
  outstanding).
- Mutations (each applied, the named test failed, reverted): keep last_error, keep attempts, no replay_count bump, audit generation off
  by one -> ResetsDeadRow; allow-list widened to done / pending / skipped (three runs) and refusal audit not committed -> Refuses; undone
  guard removed, and guard counting refused undos -> RefusesUndone; validation removed -> ValidatesInput; no lock_timeout -> Busy
  (57014 instead of ErrBusy); inbox lock weakened to FOR KEY SHARE -> ConcurrentSecondIsRefused (-count=3).
- dev.ps1 unchanged: the db-test list already includes ./internal/admin.
- Review round 2 nits (lead): the decode-failure text is now app.DecodePayloadPrefix, shared by the worker and the replay warning
  (same bytes as before); the FormatReplay doc lists all warning levels; TestDBReplayRefusalAuditFailureReportsNoOutcome drops any
  leftover trigger/function first and registers its cleanup before creating them.
- Regression (experiment-runner, 2026-11-12, uncommitted tree = not evidence, outputs in scratchpad only) on the final 5b build:
  crash smoke seeds 1-3 (-dup 0.2 -reorder 10 -crash-rate 0.1 -kill-interval 2s -lease 1s, changelog check on): 0 violations;
  poison smoke seeds 1-2 (+ -poison 2): 0 violations, dead=2 per seed; compatibility smoke, seed 1 with bin/worker-phase4.exe
  against 00005 (-changelog-check=false): 0 violations.

## Step 6: undo with conflict explanations — APPROVED by the user 2026-11-12 (checkpoint A passed)
User decisions: Q1 rule B; Q2 migration 00006; Q3 delete the ledger row; Q4 allow customer.created undo when unreferenced,
with a warning; Q5 split 6a/6b, separate commits, checkpoint B each; Q6 document only. (All = the planner's defaults.)

Key finding: "strict row-version undo" read literally (current row_version == the version the event left) makes newest-first
undo impossible for any row touched twice: the BEFORE UPDATE trigger forces row_version = OLD+1, so an undo (an UPDATE) can
never restore an old version. Example: customer.created (v1), invoice.paid (v2), undo invoice (v3), undo customer.created
expects v1, sees v3 -> refused forever. Same for subscription created/updated. E4 (undo-all) would fail by design.
Proposed rule B (equally strict): an event can be undone only if every later change to its rows is accounted for in change_log
and is itself undone.

What undo reverses (reverse seq order): customer.created -> customers delete; invoice.paid -> customers update restoring the
before image (balance), then credit_grants delete; subscription insert -> delete; subscription update -> restore the before image
(status, last_event_created, last_event_id, updated_at). Restores all columns except id/row_version via jsonb_populate_record with
an explicit per-table column list (a schema test keeps it complete); the trigger bumps row_version (+1), so the chain holds.
Not undoable (audited refusal, ErrNotUndoable): pending, stale, skipped, dead, undone, done without current-generation apply rows.

Conflict rules per affected row (generation = replay_count): 1 row missing; 2 a later change_log apply entry on the row whose
event is not undone (blocks; named: change_log id, inbox id, event_id, type, generation, "undo inbox #N first"); 3 current
row_version must equal the after-image version of the last later entry (else "changed outside RelayGuard"); 4 content equals the
expected image except row_version (defense in depth); 5 customer inserts: no credit_grants/subscriptions row references the
customer (FK inserts don't bump the parent's version; otherwise 23503). Conflicts -> refused audit row with conflicts JSON,
ErrConflict, FormatUndo explanation.

Tx flow (internal/admin/undo.go): validate (as replay) -> tx with lock_timeout 2s / statement_timeout 5s -> inbox FOR NO KEY
UPDATE FIRST (afterLock hook) -> status must be done -> read current-generation apply rows -> lock app rows in reverse seq
(FOR UPDATE where deleting, FOR NO KEY UPDATE where restoring) -> conflict rules -> done audit row (id needed: undo change_log
rows require admin_action_id) -> changelog.NewUndo(inboxID, generation, actionID) records each revert (UPDATE/DELETE ... WHERE
row_version=$cur) -> inbox status 'undone' -> commit; Outcome only after commit. Errors: ErrInvalidInput, ErrNotFound (no audit),
ErrBusy (nothing written), ErrNotUndoable/ErrConflict (audited refused), other (no outcome; check admin log).
Lock risk: customer FOR UPDATE vs a worker mid-invoice holding FK KEY SHARE and upgrading; assumed safe (holder upgrade does not
queue) but UNVERIFIED -> test TestDBUndoCustomerWaitsForWorkerFK; fallback: NO KEY UPDATE + map 23503 to ErrConflict.

Migration 00006_undo.sql (new): inbox status CHECK adds 'undone'; unique index admin_actions_undo_once (inbox_id) WHERE
action='undo' AND outcome='done'. Down: drop index, undone -> done (lossy, like 00003), old CHECK. Worker needs no change (it
only claims pending). Preview must also refuse undone; checker treats undone like done for apply rows.
Interactions: later invoices apply to the restored balance; after a customer.created undo later events for that customer
dead-letter (not replayable); undoing a subscription update rolls last_event_created back, so older late events apply; events
already stale stay stale (operator may replay them).

Split: 6a = migration 00006, changelog.NewUndo (Recorder gains source + actionID), admin.Undo, FormatUndo, CLI
`admin undo <id> -actor -reason`, preview refuses undone, tests. 6b = checker: compareApplyRows treats undone as done; new
checkUndoRows (presence: missing/unexpected; pairing, restore content, action link, generation -> new kind changelog_bad_undo);
chain/final/attribution need no change (documented why); controls one per rule + mutations.
Tests (6a, each with a mutation): UndoCustomerCreated, UndoInvoicePaid, UndoSubscriptionUpdate/Insert, ConflictLaterEvent
(then newest-first succeeds), ConflictUnloggedWrite, CustomerReferenced, LogDisagreesWithRow, RefusesStatus (5 statuses),
NoChangeLogRows, ConcurrentSecondIsRefused, Busy, ValidatesInput, NotFound, RefusalAuditFailure, SchemaUndo, ColumnsMatchSchema,
ReplayRefusesRealUndo, PreviewRefusesUndone, RecorderUndo, worker UndoCustomerWaitsForWorkerFK (-count=20, no 40P01/23503),
WorkerIgnoresUndoneEvent, LaterInvoiceAfterUndo; unit FormatUndo, NormalizeUndoRequest, ParseArgs/RunUsageError/UndoReportable.
Regression per sub-step: test, db-test, migrate 00006, crash seeds 1-3, poison seeds 1-2 (dead=2), optional phase-4 compat.
E4 outline (UNDECIDED, step 7): chaos -undo-all undoes every done event newest first, asserts all succeed, app tables empty,
all undone, CheckChangeLog 0, replays refused; variant random order with retries, refusals counted verbatim.

Open questions [planner's recommended default]:
Q1 conflict rule B vs literal rule A [B]. Q2 migration 00006 (undone status + undo-once index) vs derive from admin_actions
[00006]. Q3 grant reversal: delete the ledger row vs compensating negative grant [delete]. Q4 customer.created undo: allow when
unreferenced, with a warning, vs not undoable [allow]. Q5 split 6a/6b, separate commits, checkpoint B each [yes]. Q6 stale events
left by an undone subscription event: document only vs list in FormatUndo vs auto re-queue [document only].

As implemented (6a, implementer, 2026-11-12; not yet reviewed):
- migrations/00006_undo.sql as planned (CHECK adds 'undone'; admin_actions_undo_once; lossy down). Applied to the dev DB.
- changelog: Recorder gains source + actionID; NewUndo(inboxID, generation, actionID); SourceApply/SourceUndo constants.
- internal/admin/undo.go: Undo, UndoRequest/Result, Conflict (kind, table, row, change_log id, inbox id, event, generation,
  explanation), NormalizeUndoRequest, ErrNotUndoable, ErrConflict. Validation shared with replay (normalizeAudit; ErrInvalidInput
  text is now "invalid admin request"). Flow as planned. Refinements: (1) rule 2 also requires PAIRING: a later apply entry counts
  as undone only if its event's status is 'undone' AND a later undo row with the same (inbox_id, generation) exists on that row;
  otherwise kind "unpaired" ("changed outside RelayGuard"). (2) Rules 3/4 compare against the after image of the LAST change_log
  row on the row (any event, any source), else the event's own last entry; a later delete with the row still present is rule 3.
  (3) Rule 5 uses a fixed referencedBy map (customers <- credit_grants.customer_id, subscriptions.customer_id), attributing each
  referencing row to the apply that inserted it ("undo inbox #N first") or "changed outside RelayGuard". (4) A done event whose
  apply rows include a delete is refused (the app never deletes). (5) Done audit detail {from_status, generation, changes};
  refused detail {status, generation, refused, conflicts}. (6) Content compare (rule 4) is in Go (UseNumber, DeepEqual minus
  row_version). (7) Q4 warning in UndoResult.Warnings for each deleted customer.
- Preview refuses undone with new ErrUndone. Replay unchanged (already refuses: guard + allow-list). Worker unchanged.
- CLI `admin undo <id> -actor -reason` (same flag rules as replay; undoReportable covers ErrNotUndoable and ErrConflict).
- Lock risk RESULT: TestDBUndoCustomerWaitsForWorkerFK passed -count=20 with no 40P01/23503: the worker holding KEY SHARE upgrades
  to NO KEY UPDATE ahead of the waiting FOR UPDATE, then commits; the undo then refuses with a conflict naming the invoice. The
  fallback (NO KEY UPDATE + map 23503) was NOT applied.
- Tests: 22 new DB tests (admin 18, dbtest 2: SchemaUndo, RecorderUndo; worker 2) + unit TestNormalizeUndoRequest,
  TestUndoValidatesBeforeDB, TestUndoTablesConsistent, TestFormatUndo, TestUndoReportable, TestParseArgs/TestRunUsageError (undo).
- Mutations (each applied by script, the named test failed, reverted): NewUndo generation+1 -> RecorderUndo; delete not recorded
  and status not set to undone -> UndoCustomerCreated; credit_balance dropped from restoreColumns -> InvoicePaid + ColumnsMatchSchema;
  rule 2 off -> ConflictLaterEvent; pairing off, rule 3 off, rule 1 off -> ConflictUnloggedWrite (one subtest each); rule 4 off ->
  LogDisagreesWithRow; rule 5 off -> CustomerReferenced; allow-list + stale, + pending -> RefusesStatus; refusal not committed ->
  RefusesStatus; undone guard off and inbox lock FOR KEY SHARE -> ConcurrentSecondIsRefused; no lock_timeout -> Busy; Outcome
  before the audit insert -> RefusalAuditFailure; validation off -> ValidatesInput + UndoValidatesBeforeDB; preview undone check
  off -> PreviewRefusesUndone; worker claims undone -> WorkerIgnoresUndoneEvent; FormatUndo drops conflicts -> FormatUndo;
  undoReportable without ErrConflict -> UndoReportable; parseArgs rejects undo -> ParseArgs; delete lock FOR NO KEY UPDATE ->
  UndoCustomerWaitsForWorkerFK (fails on "never waited"); forward revert order -> InvoicePaid; restore from the after image ->
  LaterInvoiceAfterUndo; referencedBy without subscriptions -> ColumnsMatchSchema + CustomerReferenced; no-rows check off ->
  NoChangeLogRows; not-found path changed -> NotFound. Replay allow-list + undone alone is NOT caught by ReplayRefusesRealUndo
  (the admin_actions guard still refuses; two independent layers); with the guard also removed it fails.
- Review round 1 fixes (2026-11-13):
  - B1 chain rule (rule 3, chain part): rules 3/4 only compared the row to the LAST entry's after image, so an unlogged write
    BETWEEN two logged changes survived (customer.created; invoice I1; outside UPDATE email; invoice I2; undo I2 restores the
    outside email; undo I1 then passed and wiped it). Now every link from the event's own first entry on must match: each
    entry's before image = the previous entry's after image (row_version and content), else changed_outside "changed outside
    RelayGuard between change_log #a and #b". Own links included (cannot break today: the apply holds the row lock).
    Regression subtests in ConflictUnloggedWrite (content write, and a no-op UPDATE that only bumps row_version); the content
    subtest FAILED before the fix (undo done) and passes after.
  - S1 UndoReplayedEvent: dead invoice -> real Replay -> done in generation 1; an older invoice is refused naming it with
    generation 1; its undo rows and audit detail carry generation 1; then the older one undoes (pairing at generation 1).
  - S2 lock order: reverse seq is deadlock-free only because every apply writes at most one pre-existing row (customer or
    subscription) and writes it last; the other entries insert the event's own rows. Documented on Undo and pinned by
    ApplyWritesExistingRowLast (all five handled event types). Chosen over locks sorted by (table rank, id): no code change, and
    the test fails the moment a new event type breaks the assumption.
  - S3 RevertRefusesStaleVersion: revert called directly with a stale rowState; UPDATE and DELETE both fail (wrapping
    ErrNoRows), nothing recorded, nothing changed.
  - Nits: Conflict.Generation is *int, omitted for references written outside RelayGuard; the delete-entry refusal names the
    first one; 00006 Down comment notes the audit guard still blocks re-undo/replay after down/up.
  - Mutations (each applied by script, named test failed, reverted): chain check off -> ConflictUnloggedWrite; chain compare
    ignoring row_version -> ConflictUnloggedWrite (no-op subtest); NewUndo(req.InboxID, 0, actionID) -> UndoReplayedEvent;
    pairing compares generation 0 -> UndoReplayedEvent; revert UPDATE without row_version guard and DELETE without it (each)
    -> RevertRefusesStaleVersion; invoice.paid updating the customer before inserting the grant -> ApplyWritesExistingRowLast.
    NOT caught: removing the break in the delete-entry loop (no test has two delete apply rows; the app never deletes).
- Limitations: credit_grants.customer_id and subscriptions.customer_id have no index, so rule 5's reference check is a full
  scan of each table while the customer row is held FOR UPDATE. Fine at this scale; an index would be needed for large tables.
- Review round 2 follow-ups (2026-11-13):
  - Chain breaks now have their own kind, ConflictChainBreak = "chain_break" (was ConflictVersion); FormatUndo prints the kind
    generically, so no format change.
  - New tests: TestDBUndoLaterInvoiceAfterUndo extended (undo inv2, then the customer: 0 customers, 0 grants, all undone) and
    TestDBUndoSubscriptionChainAcrossUndoAndReplay (create, update, stale older event, undo update, real Replay applies at
    generation 1, undo replayed, undo create: 0 subscriptions). Both passed first time; no bug found in the undo row -> later
    apply link. ApplyWritesExistingRowLast lost a redundant loop.
  - Mutations (applied by script, reverted): pairing never matches -> both new tests fail (plus 4 older); chain check skipped ->
    only ConflictUnloggedWrite (the new tests are positive paths); chain compare ignoring updated_at on one side -> the new
    subscription test fails (plus SubscriptionUpdateThenInsert). The customer-only extension is not caught by that one.
  - Limitation: after a chain break the older event can never be undone; undoing the later events cannot repair the gap. This is
    by design of the strict rule (the log no longer accounts for the row).
  - Limitation: before/after images are compared as decoded JSON, so timestamps compare as text. That relies on every session
    using the UTC time zone, pinned by SessionTimeZone and forceSessionTimeZone in internal/db/db.go (applied in Connect).

As implemented (6b, implementer, 2026-11-13; not yet reviewed):
- compareApplyRows judges 'undone' like 'done' (undo adds rows, never removes apply rows).
- internal/sim/changelog_undo_check.go: checkUndoRows (wired into checkChangeLog) loads, per in-scope event, status,
  replay_count, the done undo admin_actions row and the current-generation apply count, plus every undo row joined to the
  apply row at the mirrored position (apply seq n+1-k for undo seq k). Pure judgeUndo reports the FIRST failing rule per
  event: 1 presence (undone without undo rows -> changelog_missing; undo rows on a non-undone event, or without a done undo
  audit row -> changelog_unexpected; existing kinds, as the plan's "presence: missing/unexpected" reads); 2 action link;
  3 generation (= replay_count, frozen because undone events are not replayable); 4 pairing (count, seq 1..n, same table/row
  in reverse order); 5 restore (insert -> delete; update -> update whose after image equals the apply's before image minus
  row_version, typed compare via imagesEqualSQL). Rules 2-5 -> new kind changelog_bad_undo. Generation is checked before
  pairing so a wrong generation is named as such.
- "Chain/final/attribution need no change" HELD (checked against the code, documented on CheckChangeLog): chains read all
  sources ordered by id (an undo row chains on the row's last image; updates go through the +1 trigger; a delete is last);
  undo rows carry the undone event's inbox_id, so the scope CTE's change_log branch still finds deleted rows; final state folds
  the last entry (a delete must leave the row gone); attribution reads source='apply' only. TestDBChangeLogCheckRealUndoClean
  confirms it on real undos (0 violations after every single undo).
- Tests: unit TestJudgeUndo (one case per rule/condition), TestCompareApplyRows +4 undone cases; DB positive
  TestDBChangeLogCheckRealUndoClean (history + dead invoice replayed into generation 1; undo all five done events newest
  first with the real admin.Undo, clean after each; a refused re-undo stays clean; app tables empty, 5 undone, 7 undo rows,
  2 of them in generation 1); DB controls one per rule: UndoPresence (4 subtests after review round 1, exact full report), UndoActionLink (rows
  pointed at the event's refused re-undo audit row), UndoGeneration, UndoPairing (forward order via seq swap; ids and chains
  unchanged), UndoRestore (content, insert reverted by an update). Each asserts exactly one violation for the event with
  that rule's kind and reason; the restore controls filter to the event subject because image damage also breaks row chains.
- Mutations (applied by script, reverted): presence-missing off, status condition off, done-action condition off (originally
  panicked on the nil action in rule 2; since the lead's guard before rule 2 it is an equivalent mutation, see below), action link off, generation off, pairing table/row off -> each fails
  only its own control; pairing SQL forward order, restore compare keeping row_version, compareApplyRows without undone,
  checker not wired -> RealUndoClean and/or all controls; restore content off and insert->delete off -> their own
  subtest only; pairing count, seq 1..n and update->update off -> TestJudgeUndo only (no DB control for these three
  sub-conditions; the DB pairing and restore controls cover one condition of each rule).
- Review round 1 fixes: (a) a done undo audit row with no undo rows and status not undone is now flagged changelog_missing
  ("done undo action #N but no undo rows (status S)"), because the audit row makes Undo/Replay refuse the event forever while
  the effect stays applied; TestJudgeUndo case + 4th UndoPresence subtest (exact full report); mutation (case off) fails both.
  (b) checkUndoRows reads events and undo rows in ONE REPEATABLE READ READ ONLY transaction (an inbox row committed between
  two READ COMMITTED statements gave a nil map entry and a panic), plus a defensive error instead of a panic if an undo row's
  inbox is missing. The other CheckChangeLog reads are one statement each, so each is internally consistent; they are not
  snapshot-consistent with each other (only matters on a live system, not after the supervisor has stopped); not widened.
  (c) rule 5 skips a nil pair (unreachable after rule 4, kept as a guard). Earlier "caught by a panic" mutation (done-action condition off) only proved the test fails, not that the report is right; re-run after the fixes it STILL panicked (rule 2 dereferenced DoneAction, which rule 1 guarantees). The lead then added a
  nil guard before rule 2 that reports the same changelog_unexpected violation; with the done-action condition off the
  mutation is now equivalent (TestJudgeUndo passes with the correct report, no panic), so the checker no longer depends on
  rule order to avoid a panic. Removing rule
  4's nil-pair rejection now makes TestJudgeUndo "pairing none" fail by assertion, not by panic.
- Known limit, not checked: undo row ids increasing with seq (no correctness impact; chains already order by id).
- Not added (by scope): chaos -undo-all (E4, undecided).

## Step 7: E4 undo-all, E5 naive rerun, evidence, decision records — APPROVED by the user 2026-11-13 (all defaults)
User decisions: E4 included (newest + random variants), E5 included. Open questions all at planner defaults: seeds
R1/R2/E4a/E4b/E4c/E5 = 20/5/20/20/5/20; R1/R2 stay plain forward runs; -undo-all requires -supervise; decision records
Phase 6 only (backfill 1-5 later); results files results/phase6-* (never overwrite Phase 4/5 files); ask before each push.
Constraints: cmd/naive unchanged; every behaviour change tested; each sub-step its own commit (lead commits after user approval).

### 7a: E4 harness code (implementer Opus, reviewer, checkpoint B; CLAUDE.md Status edit goes in this commit)
Files: new internal/sim/undoall.go, undoall_test.go (unit), undoall_db_test.go; cmd/chaos/main.go + main_test.go. No migration.
UndoAll(ctx, pool, scope, mode, rng) -> (UndoReport, []Violation, error):
- Runs after the seed's forward checks (incl. CheckState vs model), same DB, before the next seed's Reset; runs even if forward
  checks found violations; own kinds undo_*. Supervised worker stays up idle (WaitIdle => pending=0); no new deliveries.
- Candidates: all done inbox rows of this seed.
- newest: order by the event's max current-generation apply change_log.id DESC (apply order, not created; differs under
  -reorder). Any refusal -> undo_refused_newest_first (with conflicts). Rule B guarantees success in this order.
- random: sort by event_id, shuffle with rand.New(rand.NewSource(seed)); passes over the remaining set: done removes,
  ErrConflict keeps + counts per kind. Pass without progress -> undo_stuck (lists remaining), stop; cap n+1 passes.
  Allowed kinds only later_event, referenced; any other -> undo_unexpected_conflict. ErrBusy/ErrNotUndoable/other -> undo_error.
  Progress argument: the remaining event with the largest apply id is always undoable.
- Probes after the loop: every undone event: second Undo -> ErrNotUndoable, Replay -> ErrNotReplayable; every stale/skipped/
  dead event: Undo -> ErrNotUndoable (no Replay probe: dead/stale are replayable by design). Mismatch -> undo_probe.
- Assertions (after probes): inbox done=0, undone = pre-phase done count, stale/skipped/dead unchanged; count(*)=0 on
  customers, credit_grants, subscriptions (undo_rows_left) plus CheckState with empty Expected; CheckChangeLog(seed) = 0;
  admin_actions: exactly one done undo per undone event; refused undo audit rows = counted refusals + probes.
- Report: console "undo: mode=X undone=N refusals=M [kind=count ...] passes=P probes=K violations=V"; RESULT gains undone=,
  undo_refusals=; markdown header line "- Undo-all: <mode> (after forward checks; asserts empty tables, all undone,
  changelog 0, probes refused)" and Undone/Refusals/Passes columns only when the flag is set.
- Flags: -undo-all ""|newest|random; rejected with -target naive, with -changelog-check=false, and without -supervise.
- Pass loop is a pure function over try func(id) (outcome, kinds, err) for unit tests.
Tests: unit (progress, stuck, disallowed kind, ErrBusy, pass cap, newest order by apply id not created, deterministic
shuffle; flag validation, header line). DB positive: TestDBUndoAllNewestClean (undoneHistory + 2 invoices applied in
reverse created order + stale subscription + dead event; 0 violations, 0 refusals, empty, probes refused);
TestDBUndoAllRandomRetries (seed whose shuffle puts customer.created first; refusals > 0, only allowed kinds, 0 violations).
DB negative controls, exact kinds: outside UPDATE customers between two invoices -> undo_unexpected_conflict(chain_break) +
undo_stuck + undo_rows_left; foreign customer row -> undo_rows_left via count (CheckState skips it); newest with reversed
order hook -> undo_refused_newest_first; status forced back to done -> undo_probe + inbox count failure.
Mutations: order by created; allow all kinds; drop count(*); skip probes; don't count refusals; no stuck detection;
CheckChangeLog not called after undo; min instead of max apply id (expected equivalent, record as such).
Regression (scratchpad): test, db-test, crash 1-3 and poison 1-2 (dead=2) with and without -undo-all newest, random on seed 1.

7a implementation notes (implementer, 2026-11-13):
- Choices where the spec was silent: extra kinds undo_inbox_counts, undo_audit, undo_pass_cap (cap separate from undo_stuck,
  so losing stuck detection cannot hide behind the cap). CheckState(empty) violations are reported as undo_rows_left (detail
  keeps the original kind); CheckChangeLog after undo keeps its kinds with an undo_ prefix (undo_changelog_chain_break), so
  results tell forward from post-undo. Probes also check the refusal came from the right status (Undo res.Status,
  Replay FromStatus): without that, the forced-done control cannot fail (the done undo audit row alone refuses both).
  Pre-phase undone events are probed too and counted in the inbox check (undone_after = undone_before + done_before); audit
  checks are deltas against a pre-phase snapshot (refused undo = loop refusals + refused undo probes, refused replay =
  refused replay probes, no new done replay, exactly one done undo per undone event). ErrNotUndoable on a done event counts
  as a refusal (it is audited); in newest mode it is undo_refused_newest_first, in random undo_error (dropped from the set).
  An undo_unexpected_conflict is reported once per event and kind, the event stays in the set. Actor "chaos-undo-all".
  CheckState gained an internal checkState(scope) so the DB tests can use dbtest ids. Newest ordering is done in Go
  (sortNewest), so it is unit-testable.
- Controls show more kinds than the plan listed, all correct: chain-break control = undo_unexpected_conflict (a_cus AND
  a_inv1: the customer's own undo reads the same broken chain) + undo_stuck + undo_rows_left + undo_inbox_counts +
  undo_changelog_chain_break; wrong-order control = undo_refused_newest_first (a_cus, a_sub_c) + undo_rows_left +
  undo_inbox_counts; forced-done control = undo_probe (x2, undo and replay probe) + undo_inbox_counts +
  undo_changelog_unexpected; foreign row = undo_rows_left (subject customers) only.
- Mutations (each applied alone, named tests run, file restored byte-identical):
  | Mutation | Result | Failing tests |
  |---|---|---|
  | order by created | caught | TestSortNewestByApplyIDNotCreated, TestDBUndoAllNewestClean |
  | allow all conflict kinds | caught | TestUndoLoopRandomDisallowedKind, TestDBUndoAllControlChainBreak |
  | drop count(*) | caught | TestDBUndoAllControlForeignRow |
  | skip probes | caught | TestDBUndoAllNewestClean, TestDBUndoAllControlForcedDone |
  | don't count refusals | caught | TestDBUndoAllRandomRetries, TestUndoLoopRandomMakesProgress |
  | no stuck detection | caught | TestUndoLoopRandomStuck, TestDBUndoAllControlChainBreak (undo_pass_cap instead) |
  | CheckChangeLog not called after undo | caught | TestDBUndoAllControlChainBreak, TestDBUndoAllControlForcedDone |
  | min instead of max apply id | equivalent (as expected) | NewestClean, RandomRetries, ControlNewestWrongOrder all pass |
- Tests: dev.ps1 test ok; dev.ps1 db-test 138 top-level tests pass (was 124; +8 sim unit, +6 sim DB); cmd/chaos +4 unit.
- 7a review round 1 fixes (2026-11-13):
  - B1: checkUndoAudit had no control. New DB controls (afterLoop hook): extra undo/refused row by an outsider ->
    exactly {undo_audit} on admin_actions (TestDBUndoAllControlAuditExtraRefusal); stray undo/done row for the stale
    event -> exactly {undo_audit (admin_actions + the stale event), undo_changelog_missing (done undo action, no undo
    rows)} (TestDBUndoAllControlAuditStrayDoneUndo). The literal "second done undo row for an undone event" cannot be
    written: admin_actions_undo_once refuses it; the control asserts that refusal first, then uses the stale event.
  - Audit totals now compare EVERY (action, outcome) key (auditDeltaViolations): any key the phase does not write must
    not change. The schema CHECKs make another key impossible today, so this rule is unit tested only
    (TestAuditDeltaViolations).
  - S1: WaitIdle timeout (pending > 0) skips the undo phase: one undo_skipped violation naming the pending count, table
    shows "skipped", console "undo: skipped ... undone=0", RESULT undone sums only seeds that ran (undoAllSkip,
    TestUndoAllSkip, TestUndoSkippedDisplay).
  - S2: TestDBUndoAllReplayedGeneration1 (dead -> admin.Replay -> applied at generation 1 -> newest): ApplyID = the
    generation-1 max, undone first, undo rows at generation 1, 0 violations. The generation filter in readUndoPhase is
    defensive: chaos never replays, and a replayed dead/stale event has no generation-0 apply rows, so the filter gives
    the same result in every state E4 can reach. E4 results must not claim to have tested it.
  - S3: classifyUndo/probeFatal: a cancelled context, or an error that is not an admin sentinel (ErrConflict,
    ErrNotUndoable, ErrBusy, ErrNotFound, ErrNotReplayable), aborts the phase with an error (chaos exits 2) instead of
    becoming undo_error/undo_probe. This narrows the spec's "other -> undo_error" to admin's sentinels; ErrBusy stays
    undo_error per the spec. Tests: TestUndoLoopFatalStops, TestClassifyUndo, TestProbeFatal.
  - NIT: header line says undo_refusals/Refusals count loop refusals only, not the probes' expected refusals.
  - Mutations (round 1), each alone, named tests, file restored byte-identical:
    | Mutation | Result | Failing tests |
    |---|---|---|
    | drop the checkUndoAudit call | caught | TestDBUndoAllControlAuditExtraRefusal, TestDBUndoAllControlAuditStrayDoneUndo |
    | audit delta over expected keys only | caught | TestAuditDeltaViolations |
    | unknown error -> undo_error (old behaviour) | caught | TestClassifyUndo |
    | no ctx check in classifyUndo | caught | TestClassifyUndo |
    | no ctx check in probeFatal | caught | TestProbeFatal |
    | loop ignores attemptFatal (random / newest) | caught | TestUndoLoopFatalStops/random, /newest |
    | undoAllSkip never skips | caught | TestUndoAllSkip, TestUndoSkippedDisplay |
    | re-run: skip probes; don't count refusals; no stuck detection | caught (same tests as before) | as in the table above |
  - Tests after round 1: go vet ok; dev.ps1 test ok; dev.ps1 db-test 145 top-level tests pass (+4 sim unit, +3 sim DB);
    cmd/chaos +2 unit; dev.ps1 build ok.
- 7a review round 2 nits (2026-11-13):
  - N1: "too few" direction of the per-event audit rule had no control (n > want left all tests green). New
    TestDBUndoAllControlAuditMissingDoneUndo: after the loop sub_u's done undo row is set to 'refused'. Exact kinds:
    undo_audit x3 (sub_u "0 done undo audit row(s), want 1"; admin_actions "3 new 'undo done', want 4"; admin_actions
    "7 new 'undo refused', want 6") + undo_changelog_unexpected (sub_u: undo rows but no done undo action).
  - N2: mutations, each alone, file restored byte-identical (sha256 checked); run over TestDBUndoAll* + TestAuditDelta*:
    | Mutation | Result | Failing tests |
    |---|---|---|
    | per-event `n != want` -> `n > want` | caught | TestDBUndoAllControlAuditMissingDoneUndo |
    | drop the per-event rule only | caught | TestDBUndoAllControlAuditStrayDoneUndo (subject assertion), TestDBUndoAllControlAuditMissingDoneUndo |
    | drop the totals rule only | caught | TestDBUndoAllControlAuditExtraRefusal, TestDBUndoAllControlAuditStrayDoneUndo, TestDBUndoAllControlAuditMissingDoneUndo |
    (Totals-drop is also caught by StrayDoneUndo and MissingDoneUndo, which assert the admin_actions subjects.)
  - N3: RESULT line of -undo-all runs now ends " undone=N undo_refusals=N undo_skipped=N" (seeds whose undo phase was
    skipped); runs without the flag are unchanged. undoResultFields, TestUndoResultFields.

### 7b: evidence (experiment-runner, reviewer on doc, checkpoint B)
Procedure: commit 7a; git status --porcelain empty (incl. untracked); no agents/processes running; migrate-status shows
00006; dev.ps1 build; go version -m shows vcs.modified=false for worker/naive; test + db-test (scratchpad); reset-data; lead
starts the RelayGuard receiver. Raw output to scratchpad, only tail/RESULT shown.
RelayGuard common flags: -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s
- R1 seeds 1-20 -> results/phase6-relayguard-crash.md
- R2 -poison 2, seeds 1-5 -> results/phase6-relayguard-poison.md
- E4a -undo-all newest, 1-20 -> results/phase6-undo-all-newest.md
- E4b -undo-all random, 1-20 -> results/phase6-undo-all-random.md
- E4c -poison 2 -undo-all newest, 1-5 -> results/phase6-undo-all-poison.md
- E5 (stop RelayGuard receiver first): -target naive -seeds 1-20 -dup 0.2 -reorder 10 -supervise bin/naive.exe
  -crash-rate 0.1 -kill-interval 2s -out results/phase6-naive-crash.md
Compat (worker-phase4) is regression only, not evidence. Then stop receiver, reset-data.
docs/phase6-results.md: commit, vcs lines, migrate-status; per experiment date, command, RESULT verbatim, link; relayguard vs
naive table noting relayguard's extra changelog_* checks and -lease 1s; E4 refusal counts verbatim (vary run to run, like
done/stale split); state that Phase 5 evidence was vcs.modified=true and this is the first clean-build evidence;
limitations (undo not crash-injected, not concurrent with live processing; undo-all is a property check; the whole-table
count(*) check assumes no real Stripe events arrive during a seed; E4 does not exercise replayed (generation >= 1)
candidates). Superseded runs
kept and labelled. Re-runs after a fix need a new commit and a clean tree.

### 7c: decision records (docs/decisions/, template 0000)
0001 change_log in the same tx + before/after images + row_version trigger; 0002 replay_count as fence generation (ABA);
0003 undo rule B + chain check (vs rule A, vs compensating events); 0004 audited refusals, outcome only after audit commit;
0005 preview = real handler + rollback. Tick ROADMAP step 7, update Status. Commit at checkpoint B.

### 7d: interview-coach pass on Phase 6 lessons (steps 1-7).

Acceptance: 7a tests pass, each control shows its exact kind, mutations recorded here, reviewer approved. 7b every results
file vcs.modified=false; numbers reported as they come; E4a 0 refusals; E4b refusals verbatim; reviewer checked doc vs raw.
Risks: naive on the 00004-00006 schema (report, never fix naive); E4b runtime unmeasured.
