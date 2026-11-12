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
crashes, claim with lease + poison dead-lettering). CI green on e674670.

Handoff (2026-11-12):
- Just finished: Phase 6 steps 2 and 3 of 7. Step 2 (d2e3cf3, pushed, CI green): change capture in
  app.Apply/worker, FOR NO KEY UPDATE pre-reads (review found a deadlock with FOR UPDATE), sessions forced to UTC.
  Step 3: sim.CheckChangeLog in chaos (-target relayguard; flag -changelog-check=false for pre-phase-6 workers
  such as bin/worker-phase4.exe). Kinds: changelog_missing/unexpected/chain_break/final_mismatch/misattributed.
  Every rule has a negative control proven by mutation. Smokes on the uncommitted tree (not evidence):
  seeds 1-5 crash+dup+reorder+lease 1s: 0 violations; seeds 1-2 with -poison 1: 0 violations, dead=1 each.
  Migration 00004 is applied to the dev DB.
- In progress: Phase 6 (change log, preview, replay, undo). The approved plan is docs/phase6-plan.md, which
  includes the reviewer notes for later steps. Steps 1-3 are done; steps 4-7 NEED USER APPROVAL before starting.
  Results docs that compare relayguard vs naive must say relayguard runs carry extra changelog_* checks.
- User decisions (also in the plan): strict row-version undo (refuse if changed since; in practice newest
  first per customer); row_version via a BEFORE UPDATE trigger; reset-data also wipes admin_actions; undone
  events are not replayable; previews are not audited; no change_log retention for now. Optional step-7
  experiments E4 (undo-all property run) and E5 (naive rerun) are UNDECIDED: ask the user at step 7.
  Keep the Phase 4 worktree C:/dev/relayguard-phase4 and bin/worker-phase4.exe.
- Uncommitted: nothing (step 3 committed together with this handoff).
  Keep the tree clean before any evidence build: untracked files set vcs.modified=true.
- Next step: ask the user to approve Phase 6 step 4 (preview: internal/admin, cmd/admin preview/log).
  Interview notes for steps 1-3 are deferred to the end of Phase 6 (lessons so far: lock strength vs FK
  KEY SHARE; repeat tests whose bug depends on random map order; one control per rule, because rules that
  report the same kind mask each other; check attribution, not just counts). Step 5 must decide the inbox
  fence lock (see the plan's "Step 2 review" note) and make CheckChangeLog count only the current generation.
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
