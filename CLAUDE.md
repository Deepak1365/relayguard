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
crashes, claim with lease + poison dead-lettering). Phase 6 steps 1-5a pushed (fe27b99, d2e3cf3, b56731c, 052c0f5,
425f62d). 9959dce (agent model tiers + token discipline) committed, not pushed: it goes up together with 5b.
Step 5b implemented, reviewed and approved, awaiting the user's OK to commit (then push both).

Handoff (2026-11-12):
- Just finished: Phase 6 step 5b. internal/admin/replay.go: Replay (dead/stale only; one tx with lock_timeout;
  SELECT inbox FOR NO KEY UPDATE; undone guard; refusals audited 'refused' and committed -> ErrNotReplayable;
  accept = one UPDATE resetting status/attempts/last_error/processed_at/next_attempt_at and bumping
  replay_count, plus a 'done' audit row with from_* detail and generation; input validated before the DB;
  Outcome set only after commit). CLI `admin replay <id> -actor <name> -reason "<text>"` (flags after the id;
  report printed only for success/refusal). Poison warning in two levels from shared constants in
  internal/app/deadletter.go (GaveUpPrefix, NoResultRecorded, DecodePayloadPrefix), so the worker text and the
  admin reader cannot drift. replayRaw removed; worker/sim tests use the real Replay. 2 review rounds; 87 DB
  tests pass; 15 mutations caught (plan note). Regression on the final build (uncommitted, not evidence): crash
  seeds 1-3, poison seeds 1-2 (dead=2 each) and compatibility seed 1 with bin/worker-phase4.exe all 0
  violations. Dev DB empty (reset-data after the smoke).
  Earlier this session: 5a (425f62d) replay_count fence token, FOR NO KEY UPDATE claim/fence, generation-aware
  checker, preview re-reads status+replay_count.
- In progress: Phase 6. Steps 6 (undo with conflict explanations) and 7 (evidence) need user approval: start
  step 6 with the planner and checkpoint A. Results docs that compare relayguard vs naive must say relayguard
  runs carry extra changelog_* checks.
- For step 6 (from reviews): undo must take the inbox row lock (FOR NO KEY UPDATE) FIRST, like Replay, or a
  concurrent undo could commit after Replay's undone guard has read. Undo rows take the generation of the
  apply they revert. bin/worker-phase4.exe ignores replay_count: do not use it once replays exist.
- Decisions not written elsewhere: commit and push each step separately; stop at checkpoint B before commit and
  before push. The user lets the lead pick routine commands/options without asking (still ask for deletions,
  pushes, and irreversible operations). The user does not start worker/receiver: the lead starts the receiver
  in the background for chaos runs and stops it afterwards. Regression smokes on a dirty tree go to the
  scratchpad, not results/ (evidence only from a clean tree, step 7). /verify's chaos step needs no rerun when
  the same smoke already ran on the same build.
- User decisions (also in the plan): strict row-version undo (refuse if changed since; in practice newest
  first per customer); row_version via a BEFORE UPDATE trigger; reset-data also wipes admin_actions; undone
  events are not replayable; previews are not audited; no change_log retention for now. Optional step-7
  experiments E4 (undo-all property run) and E5 (naive rerun) are UNDECIDED: ask the user at step 7.
  Keep the Phase 4 worktree C:/dev/relayguard-phase4 and bin/worker-phase4.exe.
- Uncommitted: step 5b (incl. untracked internal/admin/replay*.go and internal/app/deadletter.go).
  Keep the tree clean before any evidence build: untracked files set vcs.modified=true.
- Next step: after the 5b commit and the push of 9959dce + 5b, plan Phase 6 step 6 (planner -> user approval).
  Interview notes for steps 1-5 are deferred to the end of Phase 6 (lessons so far: lock strength vs FK
  KEY SHARE; consistent lock order prevents deadlocks (preview inbox-first); repeat tests whose bug depends
  on random map order; one control per rule, because rules that report the same kind mask each other;
  check attribution, not just counts; preview = real code + rollback, not a simulation; 5a: ABA on a reset
  counter needs a monotonic generation in the fence token; weaker lock modes change who overlaps whom, so
  re-check every "can't happen" race (preview now overlaps the worker); a test must be able to fail for the
  claim it makes (40P01 assert under a 200ms lock_timeout could not); re-read state after a failed statement
  via savepoint rollback; 5b: audit honesty (report an outcome only after its audit row commits); refusals
  are audited too; shared constants between a free-text writer and its parser; a heuristic warning must not
  claim more than its signal supports).
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

## Token discipline (all agents)
- Send long outputs to files (scratchpad, or results/ for evidence) and show only summaries: tail, grep/Select-String, RESULT lines.
- Review git diffs instead of reading whole files.
- Never print whole logs (bin/supervised.log, receiver logs, go test -v output).
- Prefer a direct request to the lead over /next-phase for small tasks.
- Lead: give each step a fresh subagent with a tight brief instead of resuming one agent across steps (its context keeps growing).
- Agent models: planner and reviewer inherit (Opus); implementer, experiment-runner and interview-coach sonnet.
  The lead may override per call for a hard step (e.g. Opus implementer for concurrency/locking work).

## Team workflow
The main session is the lead. It delegates to project subagents in .claude/agents/:
planner (plan, read-only) -> user approves -> implementer (code + tests) -> reviewer (read-only review)
-> experiment-runner (runs experiments, records results verbatim) -> reviewer (checks results doc)
-> interview-coach (updates docs/interview-notes.md). Use /next-phase to run this loop and /verify for a quick health check.
