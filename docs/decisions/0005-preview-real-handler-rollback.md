# 0005: Preview runs the real apply code inside a transaction that is always rolled back

Status: Accepted (Phase 6, step 4, commit 052c0f5; lock and re-read refinements in 425f62d)
Date: 2026-11-12 (steps 4, 5a, 5b approved by the user, docs/phase6-plan.md:128)

## Context
Before replaying a dead or stale event, or looking at a pending one, an operator wants to know what it would do:
which rows change, and whether it would be stale, skipped or fail. The answer must be correct for the current
database state, and showing it must write nothing.

## Options considered
1. **A separate "dry-run" implementation that computes the changes.** Pro: no database writes at all. Con: a
   second copy of the business rules (internal/app) that can drift, and it cannot show trigger-set values such
   as row_version or generated ids.
2. **Run the real handler and roll back (chosen).** Pro: the preview cannot disagree with the real apply
   because it is the same code, same SQL, same triggers. Con: it takes real locks, and sequence-generated ids
   are consumed, so printed ids of inserted rows are provisional (plan :140).
3. **Preview against a copy or snapshot of the database.** Not weighed in the plan. Con: operationally heavy for
   a single-database project.

## Decision
`admin.Preview` (internal/admin/preview.go) opens a READ COMMITTED transaction whose only ending is a deferred
`Rollback`; the file never calls Commit, and a source-guard test enforces it
(preview.go:97-106, TestPreviewSourceHasNoCommit). It runs the real `app.Apply` with a change recorder, reads the
change_log rows it produced, then discards everything. TestDBPreviewWritesNothing snapshots all tables before
and after; TestDBPreviewMatchesRealApply checks preview rows equal the real apply's (update images including
row_version equal; inserts equal except id and timestamps).

Review forced three refinements:
- **Inbox row first, FOR KEY SHARE.** Without it, a preview of an event the worker had fenced could hold an
  uncommitted row and then wait on the fence for its change_log FK while the worker waited on that row:
  deadlock (40P01). TestDBPreviewWaitsForFenceWithoutDeadlock fails without the lock (preview.go:116-133; plan
  :157). That risk was against the pre-5a FOR UPDATE fence; since 0002 the lock order is kept as a guard
  (plan :157).
- **Overlap with the worker is expected after 0002.** KEY SHARE does not conflict with the fence's NO KEY
  UPDATE, so a preview can run during an apply. It is bounded by a 2s lock_timeout (ErrBusy) and by
  `stateMoved`, which re-reads status and replay_count at the end and after a failed apply and returns
  ErrChanged (preview.go:193-232, :237-240). Tests: TestDBPreviewDetectsReplayDuringPreview,
  TestDBPreviewWorkerCommitsDuringPreview (the latter stable over -count=20, plan :222-223).
- **Allow-list for "business" outcomes.** Only unknown customer, a concurrent insert and payload decode errors
  show as an outcome; everything else (timeouts, deadlocks, other constraint errors) is an error and exits
  non-zero (plan :158).

## Consequences
- The preview is truthful by construction, and a preview never leaves rows behind. The checker confirms the
  gap-leaving effect is harmless: TestDBChangeLogCheckAfterPreviews requires 0 violations while
  max(change_log.id) > count(*) (plan :152-153).
- Cost: previews are not free for the database. They consume sequence values, hold row locks briefly, and
  can lose a race with the worker (ErrBusy, ErrChanged), in which case the operator reruns it.
- Limitation, stated in the plan: the absence of deadlock between a preview and a live worker after 0002 rests
  on lock-order reasoning, not on a test; TestDBPreviewOverlapsRealFence proves ErrBusy and that the worker still
  commits, but with a 200ms lock_timeout, below deadlock_timeout (plan :208-212).
- Not measured: preview latency or its effect on worker throughput; no experiment exists. The chaos runs in
  docs/phase6-results.md do not exercise the preview command.
- Revisit: a read-only replica for previews if lock contention ever matters, and an index or cache if
  previews of long histories get slow.
