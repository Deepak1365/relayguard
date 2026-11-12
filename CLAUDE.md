# RelayGuard: project guide for Claude Code

## What this is
RelayGuard is a crash-safe inbound webhook processor (Go + Postgres), built as a portfolio project
for SWE job applications. Goal: apply each webhook's effect exactly once, even through crashes,
duplicates, and out-of-order delivery, and PROVE it with measured, reproducible experiments against
a deliberately naive baseline. Honest evidence matters more than features.

## Environment (Windows, native)
- Project: C:\dev\relayguard. Postgres 17 in Docker (docker-compose.yml), host port 5433.
- Settings come from .env (loaded by internal/config). Never read, print, log or commit .env or the whsec_ secret.
- Local dev DATABASE_URL (non-secret, same as .env.example): postgres://relayguard:relayguard@localhost:5433/relayguard?sslmode=disable
  Use it for DB tests (`dev.ps1 db-test`). .env itself stays off-limits because of the webhook secret.
  Never run the DB tests while an experiment is running: they truncate the same tables.
- Claude Code's shell is Git Bash. Run project tasks through PowerShell:
  `powershell.exe -NoProfile -ExecutionPolicy Bypass -File dev.ps1 <task> [args]`
  Tasks: up, down, psql, migrate, migrate-status, receiver, worker, naive, send <file> [n], state, inbox,
  reset-data, test, db-test, build. Run with no task to list them.
- Binaries built by `dev.ps1 build`: bin/worker.exe, bin/naive.exe, bin/chaos.exe.
- Port 8080 is shared by the RelayGuard receiver and the naive receiver. Only one may run at a time.
  Startup lines tell them apart: "RelayGuard receiver listening" vs "NAIVE receiver listening".

## Architecture
- cmd/receiver: verifies the Stripe signature, stores the raw event in `inbox` (unique on provider+event_id), returns 200.
- cmd/worker: CLAIM (own statement: attempts+1, lease via next_attempt_at) then PROCESS (one transaction:
  fencing check, apply via internal/app inside a savepoint, mark done/skipped/stale; retries with backoff;
  dead after 8 attempts). Fault points: after_grant, before_commit; FAULT_POISON for test.poison events.
- internal/app: business rules (customers, credit_grants ledger, subscriptions with last_event_created ordering).
- cmd/naive: the BASELINE receiver. Its flaws are deliberate. Never fix or refactor it. Only fault-injection points may be added.
- internal/stripesig: signing/verification (unit tested). internal/fault: crash injection.
- internal/sim + cmd/chaos: seeded generator, reference model, delivery plan (dup/reorder/poison), supervisor
  (crash/kill/restart), invariant checker, markdown results (-out results/*.md).
- migrations/: goose, embedded. docs/: per-phase results and decisions. results/: chaos run outputs (committed).

## Status
Phases 1-5 complete and pushed (baseline, inbox + transactional worker, ordering, chaos harness with
crashes, claim with lease + poison dead-lettering). Phase 6 steps 1-4 pushed (fe27b99, d2e3cf3, b56731c, step 4 commit).

Handoff (2026-11-12, end of session):
- Just finished: Phase 6 step 4 (preview). internal/admin (Preview, Log, formatters), cmd/admin (`preview <id>`,
  `log <id>`), bin/admin.exe built by dev.ps1 build, db-test includes ./internal/admin/. Preview runs the real
  app.Apply in an always-rolled-back tx, refuses done events, lock_timeout 2s / statement_timeout 5s, takes
  FOR KEY SHARE on the inbox row FIRST (review found a preview/worker deadlock otherwise), business-error
  allow-list (app.ErrUnknownCustomer, app.ErrConcurrentInsert now exported, decode errors) -> exit 0, anything
  else exit 1. Reviewed and approved; 72 DB tests pass; 3-seed chaos smoke 0 violations (not evidence).
  Earlier this session: step 2 (d2e3cf3) change capture; step 3 (b56731c) sim.CheckChangeLog with kinds
  changelog_missing/unexpected/chain_break/final_mismatch/misattributed and chaos flag -changelog-check.
  Migration 00004 is applied to the dev DB; the dev DB is empty (reset-data after the smoke).
- In progress: Phase 6. Steps 4, 5a and 5b are APPROVED (docs/phase6-plan.md "Steps 4, 5a, 5b", with the
  user's answers Q1-Q5). Step 5a has NOT been started. Steps 6-7 still need user approval.
  Results docs that compare relayguard vs naive must say relayguard runs carry extra changelog_* checks.
- Decisions this session not written elsewhere: commit and push each step separately (4, then 5a, then 5b);
  the user pre-approved step 4's push only, so 5a and 5b stop at checkpoint B before commit/push. The user
  lets the lead pick routine commands/options without asking (still ask for deletions, pushes not
  pre-approved, and irreversible operations). The user does not need to start worker/receiver: the lead
  starts the receiver in the background for chaos runs and stops it afterwards.
- User decisions (also in the plan): strict row-version undo (refuse if changed since; in practice newest
  first per customer); row_version via a BEFORE UPDATE trigger; reset-data also wipes admin_actions; undone
  events are not replayable; previews are not audited; no change_log retention for now. Optional step-7
  experiments E4 (undo-all property run) and E5 (naive rerun) are UNDECIDED: ask the user at step 7.
  Keep the Phase 4 worktree C:/dev/relayguard-phase4 and bin/worker-phase4.exe.
- Uncommitted: nothing (step 4 committed together with this handoff).
  Keep the tree clean before any evidence build: untracked files set vcs.modified=true.
- Next step: Phase 6 step 5a via /next-phase (already approved): migration 00005 (inbox.replay_count,
  CHECK generation >= 0, seq >= 1), fence token (replay_count, attempts), claim and fence FOR NO KEY UPDATE,
  deadLetterCrashed fenced by generation, Preview/ApplyInbox use replay_count, CheckChangeLog counts only the
  current generation. Tests and mutations as listed in the plan; regression adds a poison smoke (-poison 2).
  Running the new migration on the dev DB is part of 5a (00005 is new, so rule 6 does not block it).
  Interview notes for steps 1-5 are deferred to the end of Phase 6 (lessons so far: lock strength vs FK
  KEY SHARE; consistent lock order prevents deadlocks (preview inbox-first); repeat tests whose bug depends
  on random map order; one control per rule, because rules that report the same kind mask each other;
  check attribution, not just counts; preview = real code + rollback, not a simulation).
- Notes: gh is at "C:\Program Files\GitHub CLI\gh.exe" if it is not on PATH. Background agents survive
  /clear, so check for running agents and tasklist before builds or experiments.

## Rules (all agents)
1. Never run `dev.ps1 reset` or `docker compose down -v` (deletes the database). `dev.ps1 reset-data` is fine.
2. Never touch .env. Never commit secrets.
3. Never invent, round or "clean up" measured numbers. Results docs contain real outputs, pasted verbatim,
   with date, command and git commit. Label synthetic data as synthetic. Unexpected results are reported, not hidden.
4. Keep cmd/naive's flaws intact.
5. Every behaviour change needs a test or a harness experiment that proves it, plus a regression run.
6. Ask the user before: git push, deleting files, changing migrations that already ran, or anything irreversible.
7. Explain changes so the user can defend them in an interview: what, why, tradeoffs, alternatives.
8. Small, descriptive commits.

## Team workflow
The main session is the lead. It delegates to project subagents in .claude/agents/:
planner (plan, read-only) -> user approves -> implementer (code + tests) -> reviewer (read-only review)
-> experiment-runner (runs experiments, records results verbatim) -> reviewer (checks results doc)
-> interview-coach (updates docs/interview-notes.md). Use /next-phase to run this loop and /verify for a quick health check.
