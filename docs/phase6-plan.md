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

## Later steps (NOT now): 6 undo, 7 evidence.
Constraints: cmd/naive unchanged; every behaviour change tested; each step its own commit (lead commits after user approval).
