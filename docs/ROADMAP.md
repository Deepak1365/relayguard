# RelayGuard roadmap

Each phase is done only when its acceptance criteria are met AND the evidence is committed.

## Done
- [x] Phase 1: schema, migrations, naive baseline, 4 baseline failures recorded (docs/baseline-failures.md)
- [x] Phase 2: signed intake, durable inbox, single-transaction worker, retries (docs/phase2-results.md)
- [x] Phase 3: ordering for subscription events, stale status (docs/phase3-results.md)
- [x] Phase 4: seeded chaos harness, crash injection, supervisor, batch results (docs/phase4-results.md, results/)

## Phase 5: poison events (claim with lease)  [IN PROGRESS]
Code delivered (cmd/worker claim/process, FAULT_POISON, chaos -poison/-lease). Remaining:
- [ ] "Before" evidence with the OLD worker (the Phase 4 worker, built from commit 7b7e855 in a worktree):
      invoice crashes the worker every time -> stays pending with attempts 0, later event blocked.
      Requires the RelayGuard receiver (not naive) on 8080.
- [ ] Unblock, `dev.ps1 test`, `dev.ps1 build`.
- [ ] "After" evidence with the new worker (WORKER_CRASH_AFTER_GRANT=1, WORKER_LEASE=1s in a restart loop):
      later event done, invoice dead after 8 attempts with a reason.
- [ ] Regression: chaos seeds 1-20, -dup 0.2 -reorder 10, crash-rate 0.1, kill-interval 2s, -lease 1s -> results/relayguard-crash-lease.md, expect 0 violations.
- [ ] Poison: same (including -lease 1s) + -poison 2, seeds 1-5 -> results/relayguard-poison.md, expect 0 violations and dead=2 per seed.
- [ ] docs/phase5-results.md filled in; commit.

## Phase 6: change log, preview, replay, undo
- Record before/after row images for every applied event (change_log table, written in the same transaction).
- Preview: run an event's handler in a transaction that is rolled back; return the would-be changes.
- Replay: re-queue a dead or stale event deliberately (audited).
- Undo: reverse an applied event only if the affected rows are unchanged since (row versions); otherwise refuse with a conflict explanation.
- Acceptance: unit/integration tests for preview, replay, undo-success and undo-conflict; chaos regression still 0 violations; results doc.

## Phase 7: dashboard
- Small web UI (served by a Go admin API on localhost): event timeline per customer, failed/dead/stale events,
  preview / replay / undo buttons, audit log.
- Acceptance: works end to end on fixtures; screenshots/GIF for README; admin API has tests.

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
