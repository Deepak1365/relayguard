# 0004: Admin refusals are audited, and an outcome is reported only after its audit row commits

Status: Accepted (Phase 6, step 5b, commit dcde809; applied again to undo in 14ae77f)
Date: 2026-11-12 (steps 4, 5a, 5b approved by the user, docs/phase6-plan.md:128)

## Context
Replay and undo change production data on an operator's say-so. Two questions: what happens when the request
is refused (event not dead/stale, event already undone, conflicts), and when may the CLI tell the operator what
happened. An operator who reads "refused, audited" must be able to trust that the audit row exists, and an
operator who reads "replayed" must be able to trust the replay committed.

## Options considered
1. **Audit only successful actions.** Pro: simpler, fewer rows. Con: refusals are exactly where an operator
   probes state ("why can't I undo this?"), and a missing record of attempts hides misuse and confusion.
2. **Audit refusals in a separate transaction after the main one rolled back.** Con: two transactions, a gap
   in which the process can die between them, and the refusal could be recorded against state that has moved.
3. **Audit refusals inside the same locked transaction and commit it (chosen).** The refusal row is written
   while the inbox row lock is held, so it describes the state the decision was based on. Pro: one transaction,
   one commit. Con: a refusal is a committed write, so it needs the same lock and failure handling as a success.
4. **Report the outcome as soon as it is decided versus after commit.** Chosen: after commit. A review
   found that setting `Outcome = refused` before the audit insert committed would let the CLI claim an audit
   that failed (plan :263-266).

## Decision
- Replay (internal/admin/replay.go): validate input before touching the DB; take the inbox row FOR NO KEY
  UPDATE; on refusal insert an `admin_actions` row with outcome `refused` and a detail of status, attempts,
  generation and reason, commit, and only then set `Outcome = ReplayRefused` and return ErrNotReplayable
  (replay.go:178-192). On success one UPDATE plus a `done` audit row with the `from_*` values, then commit,
  then `Outcome = ReplayDone` (replay.go:221-224).
- Undo does the same with ErrNotUndoable/ErrConflict, with the conflicts as JSON in the refused row
  (plan :327-328, :334-335; internal/admin/undo.go:176-179).
- The CLI prints a report only when the error is nil or a reportable refusal (`replayReportable`,
  `undoReportable`). If the audit insert or commit fails, the report says nothing. The remedy is
  `admin log <id>`, because a commit error may still have succeeded (doc comment, internal/admin/replay.go:118; plan :265-266).
- Not audited on purpose: unknown id (no event to attach to), busy (nothing was written), and previews
  (user decision Q4, plan :4).
- Tests: TestDBReplayRefuses (exactly one `refused` audit row, tables unchanged),
  TestDBReplayRefusalAuditFailureReportsNoOutcome (a trigger forces the audit insert to fail),
  TestDBUndoRefusalAuditFailure, TestReplayReportable, TestUndoReportable. Mutations: refusal not committed
  fails TestDBReplayRefuses; outcome set before the audit insert fails TestDBUndoRefusalAuditFailure
  (plan :295, :390-391).

## Consequences
- The audit trail answers "who asked for what, why, and what happened", including refusals, and the unique index
  `admin_actions_undo_once` (migration 00006) lets the guard against a second undo or a replay of an undone event
  rest on the same table. A refused undo does not count as "undone" (plan :279-280).
- Cost: every refusal path writes and commits. A refusal under heavy contention waits for the same inbox lock
  as a success, bounded by lock_timeout (2s), then returns ErrBusy with nothing written.
- Limitation: if the process dies after commit but before printing, the operator sees no report although the
  action happened. The documented remedy is `admin log <id>`. This is a known edge, not eliminated.
- Limitation: actor and reason are free text from the command line (trimmed, non-empty, at most 200 and 1000
  characters). There is no authentication; the audit is only as honest as the operator's flags.
- Not measured: audit write volume or latency; no experiment covers it.
- Revisit: authenticated actors, and an audit table that cannot be updated or deleted by the same role that
  writes it.
