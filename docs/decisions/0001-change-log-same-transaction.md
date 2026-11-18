# 0001: Record every applied change in change_log, in the same transaction, with before/after images and a row_version trigger

Status: Accepted (Phase 6, steps 1-2, commits fe27b99 and d2e3cf3)
Date: 2026-11-11 (plan approved by the user, docs/phase6-plan.md:1-5; fe27b99 dated 2026-11-11, d2e3cf3 dated 2026-11-12)

## Context
Phase 6 adds operator tools: preview, replay and undo of webhook effects. Undo needs to know exactly what an
event wrote and what each row looked like before and after, and it needs to detect that a row changed
afterwards. The worker already applies each event in one transaction (internal/app via cmd/worker, with the
effect inside a savepoint), so any record of the effect must not be able to disagree with the effect itself.

## Options considered
1. **Write change_log rows through the same transaction as the effect (chosen).**
   Pro: the log commits or rolls back with the effect, so a crash between "applied" and "logged" cannot
   exist. Con: every write path in internal/app must call the recorder; a forgotten path is an unlogged write.
2. **Log asynchronously, or from a separate worker step.** Not recorded as a weighed option in the plan; listed
   here as the obvious alternative. Pro: no extra work in the hot transaction. Con: a crash leaves applied
   changes with no log, which defeats undo. Rejected for that reason (my reasoning, not a quote from the plan).
3. **Images built in Go versus images taken from the database.** Chosen: the database. Images come from
   `to_jsonb(row)` in the writing statement (`INSERT ... RETURNING`, or a `SELECT ... FOR NO KEY UPDATE`
   just before an `UPDATE ... RETURNING`), so they include defaults and trigger-set values
   (internal/app/apply.go:5-9, :66, :141-148; plan :101-102). Con: JSON timestamps depend on session time zone,
   so Connect pins the session to UTC (plan :107, :435-436; SessionTimeZone in internal/db/db.go).
4. **Version counter: app-managed versus a BEFORE UPDATE trigger.** The user chose the trigger (plan :3,
   "Q1"). Pro: no write path can forget to bump it, including raw SQL. Con: the trigger bumps on every
   UPDATE, even no-ops, so the recorder must log every UPDATE it runs (plan :92), and an undo, being an UPDATE,
   also bumps the version (see 0003).

## Decision
- Migration migrations/00004_change_log.sql (new file; 00001-00003 untouched) adds `row_version` to
  customers, credit_grants and subscriptions, the `bump_row_version()` BEFORE UPDATE triggers, `admin_actions`
  and `change_log` (before_image, after_image, op, seq, generation, source, admin_action_id), with CHECK
  constraints tying insert/delete to NULL images.
- `change_log_apply_once` is a unique index on (inbox_id, generation, seq) for apply rows, so the same
  event cannot be logged twice for one generation (TestDBChangeLogApplyOnceIndex).
- The recorder (internal/changelog) inserts each row immediately on the same pgx.Tx, so a failed apply that
  rolls back its savepoint also rolls back its log rows (TestDBApplyErrorAfterWriteRollsBackChangeLog,
  TestDBCrashMidApplyLeavesNoChangeLog).
- Row pre-reads use FOR NO KEY UPDATE, not FOR UPDATE: a review argued (by reasoning; no test reproduces the deadlock) that FOR UPDATE conflicts with the FOR KEY
  SHARE that FK checks hold on the customer row and can deadlock two workers on one customer (plan :102).

## Consequences
- Easier: preview, undo and the checker all work from one source of truth. The chaos checker re-derives
  expected rows per event type independently of internal/app and compares chain, final state and attribution
  (plan :110-126, commit b56731c). TestDBChangeLogCheckDetectsUnloggedWrite proves the checker flags an
  unlogged write.
- Evidence: R1 (crash, seeds 1-20) ended `RESULT target=relayguard seeds=20 failing_seeds=0 violations=0
  restarts=257 hard_kills=36` (docs/phase6-results.md:99). RelayGuard runs carry extra changelog_* checks that the
  naive baseline does not, so the 0-versus-402 comparison (docs/phase6-results.md:309-315) holds RelayGuard to
  more checks, not fewer.
- Cost: one extra insert per written row, inside the event's transaction. Not measured; no throughput
  experiment exists for it.
- Limitations: change_log has no retention (plan :4, Q5). The checker compares images through
  `jsonb_populate_record`, which ignores unknown keys and turns missing keys into NULL, so a future migration
  that adds a column must backfill the images or teach the checker (plan :126). A stale subscription upsert
  that matches `WHERE false` fires no trigger and logs nothing, which is intended (plan :93).
- Revisit: retention or archiving; whether capture should move to database triggers so that a new write path
  cannot skip logging (today it is protected only by the checker and tests).
