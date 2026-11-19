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
crashes, claim with lease + poison dead-lettering). Phase 6 steps 1-5b pushed (fe27b99, d2e3cf3, b56731c, 052c0f5,
425f62d, dcde809), plus 9959dce (agent model tiers + token discipline). Step 6 pushed: 370f3eb, 14ae77f, b4c9405.
Step 6 (undo) plan APPROVED 2026-11-12 (Q1-Q6 = planner defaults, recorded in docs/phase6-plan.md).

Handoff (2026-11-13): Phase 6 step 6 DONE and pushed: 370f3eb (hook + plan), 14ae77f (6a undo), b4c9405 (6b checker);
main level with origin. This Status edit is committed with 7a (step 7's first commit).
- Step 7 plan APPROVED 2026-11-13 (all defaults; E4 and E5 INCLUDED; full plan in docs/phase6-plan.md "## Step 7").
  7a (chaos -undo-all newest|random, internal/sim/undoall.go) done: 2 review rounds + nits, APPROVED; 146 top-level DB
  tests; mutations in the plan. Smoke7a (scratchpad, not evidence): crash 1-3, poison 1-2 (dead=2), both also with
  -undo-all newest (0 refusals), random seed 1 (refusals=39, passes=3), all 0 violations. 7a COMMITTED 6c5a66c,
  NOT pushed (user: push later together with 7b).
- Handoff (2026-11-14): 7b evidence RUN from clean 6c5a66c (all 4 binaries vcs.modified=false): R1 crash 1-20, R2
  poison 1-5, E4a undo-all newest 1-20 (undone=1534 refusals=0), E4b random 1-20 (undone=1530 refusals=814), E4c
  poison+newest 1-5, all 0 violations; E5 naive 1-20: failing_seeds=20 violations=402. UNCOMMITTED, on purpose:
  docs/phase6-results.md + results/phase6-{relayguard-crash,relayguard-poison,undo-all-newest,undo-all-random,
  undo-all-poison,naive-crash}.md. Doc review round 1 CHANGES REQUIRED (comparison caveats, E4 framing) -> fixed;
  round 2 CHANGES REQUIRED (1 blocking: "first vcs.modified=false" was false, Phase 5 had it for worker-phase4.exe;
  plus wording) -> fixed by the lead; round 3 (2026-11-18) found the same "first" claim still at lines 8 and 336
  (a superseded Phase 5 E2 main-tree worker also had false, phase5:434) -> fixed by the lead with the reviewer's
  wording; all other checks (numbers vs 7b_times.txt/RESULT lines, changelog_* caveat, citations) passed. Raw outputs (7b_*.txt, phase6-results.old):
  C:\Users\adabs\AppData\Local\Temp\claude\bae3b7b563753f5f\8920b4f6-0d4c-4479-8d2e-ad5ca1948b13\scratchpad
  Dev DB reset, no processes running. 7b COMMITTED 7068ef2 and PUSHED with 6c5a66c (2026-11-18); main level with
  origin. 7c (2026-11-18): docs/decisions/0001-0005 written (implementer sonnet), review CHANGES REQUIRED
  (2 blocking: 0001 date, 0003 overclaim/citations; 4 nits on untested deadlock claims) -> fixed by the lead with
  the reviewer's wording. Options not in the plan are labelled as the author's reasoning. 7c committed f955677 and
  pushed. 7d (2026-11-18): Phase 6 section in docs/interview-notes.md (interview-coach sonnet), review CHANGES
  REQUIRED (1 blocking: retries/gave_up paraphrase inverted; changelog_* caveat in the 30-second version; 2 citation
  nits) -> fixed by the lead. PHASE 6 DONE: 7d committed ac5c427 and pushed.
- Phase 7 (2026-11-18): plan APPROVED (all defaults Q1-Q7), docs/phase7-plan.md. 7a done (implementer sonnet):
  internal/admin/list.go, Log one-snapshot tx (mutation tx->pool fails TestDBLogIsOneSnapshot), reportable.go moved
  from cmd/admin. Review r1 CHANGES REQUIRED (unattributed negative controls; cursor id<$1 with MaxInt64) -> fixed,
  mutations (a)-(d) each fail list_db_test.go:284; r2 APPROVED. 114 TestDB* pass (156 top-level tests in the DB
  packages, was 146). Do NOT merge the simple-CASE WHEN branches (Postgres rejects WHEN 'a','b'; r2 nit ignored).
  Docker Desktop lives at %LOCALAPPDATA%\Programs\DockerDesktop (start it if port 5433 refuses).
  7a COMMITTED 3c525e5 and PUSHED (main level with origin). /verify before the commit: test ok (10 pkgs), build ok,
  chaos seeds 1-3 crash smoke `RESULT target=relayguard seeds=3 failing_seeds=0 violations=0 restarts=38 hard_kills=5`
  (scratchpad, not evidence); receiver stopped, reset-data done.
- Handoff (2026-11-18, session 2): 7b (HTTP API + `admin serve`) IMPLEMENTED by a sonnet implementer, NOT yet
  reviewed, NOT committed. Its report (verified by it; vet, gofmt, test, db-test, build ok; reset-data run):
  files internal/adminhttp/{server,handlers,errors}.go + addr_test.go + handlers_db_test.go; cmd/admin/serve.go +
  serve_test.go (main.go only dispatches); dev.ps1 db-test now includes ./internal/adminhttp/ (not in the plan).
  Routes as the plan; unattributed = GET /api/events/unattributed (separate route); GET / is a text placeholder.
  Tests: TestParseAddr, TestSecurityHeadersOnEveryResponse, TestParseServeArgs, TestDBReplayOK,
  TestDBReplayRefusedIsAudited, TestDBInvalidInput400NoAudit, TestDBNotFound404, TestDBGetOnPostRoute405,
  TestDBCrossSitePost403, TestDBWrongHost403, TestDBWrongContentType415, TestDBBodyTooLarge413, TestDBUndoConflict409,
  TestDBPreviewAndReads, TestDBBusy503 (-count=20: 20/20), TestDBInternalErrorNotLeaked. Counts (reviewer-verified): 171 top-level / 127 TestDB*
  across the 5 DB packages incl. internal/adminhttp (worker 26, dbtest 7, sim 56, admin 67, adminhttp 15 of which 13 TestDB*).
  Mutations (each restored): no Host check -> addr_test.go:60 + handlers_db_test.go:260; no CrossOriginProtection ->
  :234; no content-type check -> :284; result always returned -> :422 and :461.
  Implementer decisions for the reviewer to judge: only literal 127.0.0.1:<port> accepted (::1 and `localhost`
  refused as bind addresses); 413 for oversized bodies, 400 for unknown JSON fields; preview requires JSON content type
  though it reads no body; admin result structs have no JSON tags (Go field names like InboxID, Conflicts; Conflict
  fields snake_case) -> consider adding json tags (in 7b fix round or 7c) so the UI gets consistent keys; ErrChanged
  message may include wrapped apply error text (check against "500 does not leak" intent).
  Handoff (2026-11-18, session 3): review r1 CHANGES REQUIRED (1 blocking: untested not_undoable/undone/changed
  mappings; should-fix: raw pg text in 409 changed/503 busy, JSON shape -> DTOs; 5 nits) -> ALL FIXED by a sonnet
  implementer (dto.go, errors_test.go, TestDBUndoTwiceAndPreviewUndone, TestDBWireKeysOnRealResponses; 8 mutations
  each fail a named test; 129 TestDB* pass: worker 24, dbtest 7, sim 31, admin 52, adminhttp 15; adminhttp 21 top-level).
  Lead added the 7b implementation note in docs/phase7-plan.md.
- 2026-11-19: review r2 APPROVED (all r1 findings fixed; reviewer re-ran 2 mutations). Its should-fix done by the lead:
  mux 404 code is now no_route (handler 404 stays not_found), TestDBNotFound404 checks code+message (mutation
  "jsonErrors ignores Content-Type" fails it at handlers_db_test.go:208). Open r2 nits for 7c: a mounted FileServer's
  plain-text 404s will become JSON no_route (decide deliberately); rawJSON maps invalid JSON to null silently.
  /verify: test ok (11 pkgs), build ok (now also bin/admin.exe), db-test 129 TestDB* pass, chaos seeds 1-3
  `RESULT target=relayguard seeds=3 failing_seeds=0 violations=0 restarts=38 hard_kills=5` (scratchpad, not
  evidence). Note: run reset-data BEFORE db-test after a chaos run (the truncate guard refuses foreign rows).
  7b COMMITTED 9cb1405 and PUSHED (main level with origin).
- Handoff (2026-11-19, session 4): 7c (UI) IMPLEMENTED (sonnet), review r1 APPROVED (Opus, no blocking), all 5 nits
  FIXED (sonnet): numeric-only #/event ids, "."/".." customer ids refused, el() attribute allow-list (throws) +
  TestElAttributeAllowList, more banned sinks, actor maxlength 200, paging null guard, nav re-click refresh, Loading...,
  timeline "newest arrival first". Files (UNCOMMITTED): internal/adminhttp/server.go (M), static.go, static_test.go,
  static/{index.html,app.js,style.css}. GET /{$} index; GET /static/{name} only the 3 embedded names (explicit
  content-type map; else JSON no_route 404, a lead decision). adminhttp 27 top-level tests; TestDB* 129 unchanged.
  vet/gofmt/test ok after the nits; db-test last ran green BEFORE the nits (nits touched only app.js + static_test.go).
  OPEN acceptance (plan 7c): manual click-through. USER DECISION: the lead AUTOMATES it (headless Edge via the
  DevTools protocol, driven by a node 24 script in the scratchpad (built-in WebSocket, no deps); not evidence).
  Plan for it: dev.ps1 build; reset-data; receiver in background; chaos poison run (e.g. -seeds 1 -poison 2 with
  -supervise bin/worker.exe; it leaves the last seed's ~80 rows incl. dead=2, enough for "Older" paging); stop the
  receiver; bin/admin.exe serve (default 127.0.0.1:8090) in background; msedge --headless --remote-debugging-port.
  Steps: dead list -> event; Preview; Replay (actor+reason) -> ok notice + re-read; Replay again -> refusal with
  action #; Undo a done event (newest for its customer) -> ok; Undo again -> not_undoable; Undo an older event of a
  customer with newer changes -> conflict list; buttons disabled in flight; "Older" paging; #/event/abc -> Unknown
  page; check no console errors. UI hooks: buttons "Preview apply (writes nothing)", "Replay", "Undo", "Older";
  inputs #actor, #reason; notices div.notice.{ok,error,...}. Stop every process by PID; reset-data after.
  EXACT NEXT STEP: that click-through, then fix anything found (re-review if behaviour changes), /verify (reset-data
  BEFORE db-test), checkpoint B commit of 7c (user OK), ask push, then 7d (evidence, screenshots, results doc, 0006,
  interview notes). No agents or project processes running at handoff; Postgres container up.
- 2026-11-19 (session 5): click-through DONE (scratchpad clickthrough.mjs + ct_run2.txt + shots/, not evidence):
  29/29 checks pass on chaos seed 1 poison 2 data (86 rows). It found 1 UI bug, fixed by the lead: undoNotice said
  "event status now <d.status>" but UndoResult.Status is the status BEFORE the undo (showed "now done"); now
  "was <status>, generation N; event status now undone" (mirrors FormatUndo). Only expected 409/404 network logs,
  no JS errors. Headless Edge children survive taskkill /T: stop them by PID matched on the edgeprof command line.
  dev.ps1 test ok; dev DB reset. NEXT: user decides re-review of the one-line fix, then /verify, checkpoint B.
- Model policy ADOPTED by the user 2026-11-14 (see Token discipline, Agent models). Conserve tokens wherever possible.
- Incident (2026-11-13): the experiment-runner ran `taskkill /IM python.exe` to kill a stray interactive python it
  started (it said it was the only python.exe). Told the user. Brief runners: never kill by image name.
- Results docs comparing relayguard vs naive must say relayguard runs carry
  extra changelog_* checks. Evidence only from a clean tree (git status empty, so vcs.modified=false). Then interview notes
  for Phase 6 steps 1-6 (lessons listed below, plus 6a/6b: chain check found by review; audit row vs data consistency;
  checker must not panic on corrupt state; single-snapshot reads).
- Known, not a bug: the done/stale split varies run to run under crashes (docs/phase5-results.md:739).
- 6b: internal/sim/changelog_undo_check.go (checkUndoRows: presence -> changelog_missing/unexpected, incl. a done undo
  audit row with nothing reverted; action link, generation, pairing k<->n+1-k, restore -> changelog_bad_undo; one
  violation per event, first failing rule; both reads in one REPEATABLE READ READ ONLY tx). compareApplyRows treats
  undone as done. Chain/final/attribution checks verified to need no change. One DB control per rule plus
  TestDBChangeLogCheckRealUndoClean. 124 top-level tests in the DB packages pass. Regression smoke6b (scratchpad, not
  evidence): crash 1-3, poison 1-2 (dead=2), compat seed 1, all 0 violations. Dev DB empty.
- 6a: migration 00006 (status 'undone', unique index admin_actions_undo_once), changelog.NewUndo (source + action id),
  internal/admin/undo.go (rule B conflicts 1-5 plus a chain check: every later change_log entry's before image must
  equal the previous after image incl. row_version -> kind chain_break; reverse-seq locking, pinned by
  TestDBApplyWritesExistingRowLast), FormatUndo, CLI `admin undo <id> -actor -reason`, preview refuses undone (ErrUndone).
  Review round 1 found BLOCKING: an unlogged write between two logged entries was silently wiped after undoing the
  later event (fixed by the chain check, test reproduces it). Round 2 APPROVED; follow-ups added (undo across undo->apply
  links, subscription chain across undo+replay at generation 1). 117 top-level tests in the DB packages pass (91 TestDB*).
  Mutations listed in the plan note. FK-wait test 20/20 (fallback not needed).
  Regression on the dirty tree (scratchpad smoke6a, not evidence): crash seeds 1-3, poison 1-2 (dead=2 each), compat
  seed 1 with bin/worker-phase4.exe, all 0 violations. Dev DB empty (reset-data).
- Next: commit 6a (user OK), push 370f3eb + 6a (user OK), then 6b (harness checker) with a fresh implementer.
- User preference (2026-11-13): quality over speed; keep the full loop. The user hibernates the laptop at times:
  they will say so, and the lead then stops at a checkpoint (sleep mid-agent stalls the stream).

Handoff (2026-11-12, still relevant below):
- Earlier: Phase 6 step 5b. internal/admin/replay.go: Replay (dead/stale only; one tx with lock_timeout;
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
- Step 6 plan key finding: literal strict row-version undo cannot undo newest-first because the trigger bumps
  row_version, hence rule B (approved). Step 7 (evidence) needs approval too. Results docs that compare relayguard vs naive must say relayguard
  runs carry extra changelog_* checks.
- For step 6 (from reviews): undo must take the inbox row lock (FOR NO KEY UPDATE) FIRST, like Replay, or a
  concurrent undo could commit after Replay's undone guard has read. Undo rows take the generation of the
  apply they revert. bin/worker-phase4.exe ignores replay_count: do not use it once replays exist.
- Decisions not written elsewhere: commit and push each step separately; stop at checkpoint B before commit and
  before push. The user lets the lead pick routine commands/options without asking (still ask for deletions,
  pushes, and irreversible operations). The user does not start worker/receiver: the lead starts the receiver
  in the background for chaos runs and stops it afterwards. Regression smokes on a dirty tree go to the
  scratchpad, not results/ (evidence only from a clean tree, step 7). /verify's chaos step needs no rerun when
  the same smoke already ran on the same build. The user wants a handoff when context reaches ~140k (hook enforces it).
- User decisions (also in the plan): strict row-version undo (refuse if changed since; in practice newest
  first per customer); row_version via a BEFORE UPDATE trigger; reset-data also wipes admin_actions; undone
  events are not replayable; previews are not audited; no change_log retention for now. Optional step-7
  experiments E4 (undo-all property run) and E5 (naive rerun) are UNDECIDED: ask the user at step 7.
  Keep the Phase 4 worktree C:/dev/relayguard-phase4 and bin/worker-phase4.exe.
- The context-handoff hook (.claude/hooks/context-handoff.ps1, settings.json UserPromptSubmit + PostToolUse) is in 370f3eb.
  Keep the tree clean before any evidence build: untracked files set vcs.modified=true.
- Interview notes for steps 1-5 are deferred to the end of Phase 6 (lessons so far: lock strength vs FK
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
- Session handoff at ~140k context: .claude/hooks/context-handoff.ps1 (UserPromptSubmit + PostToolUse) injects a
  reminder; then update Status, tick ROADMAP, show the diff, and give the user the new-session prompt it contains.
  Threshold override: env RELAYGUARD_HANDOFF_TOKENS.
- Agent models: planner and reviewer inherit (Opus); implementer, experiment-runner and interview-coach sonnet.
  The lead overrides to Opus only for real concurrency/locking/correctness design, stating the reason; never for
  small follow-ups (nits, wording, one test). Small text edits: the lead does them directly, no agent (user, 2026-11-14).

## Team workflow
The main session is the lead. It delegates to project subagents in .claude/agents/:
planner (plan, read-only) -> user approves -> implementer (code + tests) -> reviewer (read-only review)
-> experiment-runner (runs experiments, records results verbatim) -> reviewer (checks results doc)
-> interview-coach (updates docs/interview-notes.md). Use /next-phase to run this loop and /verify for a quick health check.
