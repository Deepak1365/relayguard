# RelayGuard roadmap

Each phase is done only when its acceptance criteria are met AND the evidence is committed.

## Done
- [x] Phase 1: schema, migrations, naive baseline, 4 baseline failures recorded (docs/baseline-failures.md)
- [x] Phase 2: signed intake, durable inbox, single-transaction worker, retries (docs/phase2-results.md)
- [x] Phase 3: ordering for subscription events, stale status (docs/phase3-results.md)
- [x] Phase 4: seeded chaos harness, crash injection, supervisor, batch results (docs/phase4-results.md, results/)
- [x] Phase 5: claim with lease, fencing, poison dead-lettering (docs/phase5-results.md, results/relayguard-crash-lease.md,
      results/relayguard-poison.md). Before: poison invoice stuck pending/0 attempts across 99 crashes, later event blocked.
      After: invoice dead after 8 attempts, later event done. Chaos: 0 violations (seeds 1-20 with lease; seeds 1-5 with poison, dead=2 each).
      Open questions recorded in the doc: a poison event still blocks later events until it is dead (claim ORDER BY id, restart > lease;
      unmeasured); fencing not exercised with concurrent workers (Phase 10).

## Phase 6: change log, preview, replay, undo
- Record before/after row images for every applied event (change_log table, written in the same transaction).
- Preview: run an event's handler in a transaction that is rolled back; return the would-be changes.
- Replay: re-queue a dead or stale event deliberately (audited).
- Undo: reverse an applied event only if the affected rows are unchanged since (row versions); otherwise refuse with a conflict explanation.
- Acceptance: unit/integration tests for preview, replay, undo-success and undo-conflict; chaos regression still 0 violations; results doc.
- Status: DONE (2026-11-18). Approved plan: docs/phase6-plan.md (7 steps, one commit each). Evidence:
  docs/phase6-results.md, results/phase6-*.md; decisions: docs/decisions/0001-0005.
  - [x] Step 1: migration 00004 (change_log, admin_actions, row_version trigger), TRUNCATE sites, internal/dbtest (fe27b99)
  - [x] Step 2: change capture in internal/app + worker, subprocess crash test (review fix: FOR NO KEY UPDATE pre-reads)
  - [x] Step 3: CheckChangeLog in the harness + negative controls (incl. per-rule attribution controls, -changelog-check flag)
  - [x] Step 4: preview (internal/admin, cmd/admin preview/log; inbox-first KEY SHARE lock avoids a worker deadlock)
  - [x] Step 5a: migration 00005 (replay_count, CHECKs), fence token (replay_count, attempts), claim/fence FOR NO KEY UPDATE, generation-aware checker
        (review fixes: preview re-reads status+replay_count at the end and after a failed apply -> ErrChanged)
  - [x] Step 5b: admin replay (dead/stale only, audited, -actor/-reason required, undone guard)
  - [x] Step 6: undo with conflict explanations (plan approved 2026-11-12: rule B, migration 00006, Q1-Q6 in the plan)
    - [x] 6a: migration 00006 (undone status, undo-once index), admin undo with rule-B conflicts incl. chain check,
          FormatUndo, CLI `admin undo`, preview refuses undone (2 review rounds; review found the unlogged-write chain hole)
    - [x] 6b: harness checker for undo rows (checkUndoRows, changelog_bad_undo) + one negative control per rule
          (2 review rounds; review added the "done undo audit row but nothing reverted" case and a single-snapshot read)
  - [x] Step 7: evidence runs from a clean tree, docs/phase6-results.md, decision records
    - [x] 7a: chaos -undo-all newest|random (E4 harness), 2 review rounds (6c5a66c)
    - [x] 7b: evidence R1/R2/E4a/E4b/E4c/E5 run from clean 6c5a66c, docs/phase6-results.md (3 doc review rounds)
    - [x] 7c: decision records 0001-0005 (docs/decisions/; 1 review round, fixes by the lead)
    - [x] 7d: interview notes for Phase 6 (docs/interview-notes.md; 1 review round, fixes by the lead)

## Phase 7: dashboard
- Small web UI (served by a Go admin API on localhost): event timeline per customer, failed/dead/stale events,
  preview / replay / undo buttons, audit log.
- Acceptance: works end to end on fixtures; screenshots/GIF for README; admin API has tests.
- Status: IN PROGRESS. Approved plan: docs/phase7-plan.md (2026-11-18, Q1-Q7 defaults; 4 steps, one commit each).
  - [x] 7a: read API (ListEvents/CustomerTimeline/ListUnattributed/ListActions, keyset paging), admin.Log in one
        REPEATABLE READ READ ONLY snapshot, ReplayReportable/UndoReportable shared with the CLI (2 review rounds)
  - [x] 7b: HTTP API + `admin serve` (127.0.0.1:8090, Host allow-list, CrossOriginProtection, error mapping,
        JSON DTOs; 2 review rounds)
  - [ ] 7c: UI (embedded vanilla JS)
  - [ ] 7d: evidence from a clean tree, screenshots, docs/phase7-results.md, decision 0006, interview notes

## Phase 8: CI that re-proves the claims
- GitHub Actions with a Postgres service: migrations, go test, and a short chaos run (e.g. seeds 1-3 with crashes) on every push.
- Acceptance: green badge in README; CI fails if the harness reports any violation.

## Phase 9: transactional outbox for side effects
- Emails / external calls recorded in an outbox table in the same transaction, sent by a relay with idempotency keys.
- Acceptance: harness counts side effects at the outbox consumer; 0 duplicates under crashes vs naive.

## Phase 10 (stretch): scaling and load
- Multiple workers (SKIP LOCKED), per-entity ordering under concurrency, throughput and p50/p99 latency with k6 or a Go load generator on stated hardware.

## Final polish
- README: problem, architecture diagram, quick start, results table with links, limitations, demo GIF.
- 2-minute demo script. Resume bullets using ONLY measured numbers.
