# Phase 9 plan: transactional outbox for side effects

Planner (Opus) draft 2026-11-20. APPROVED by the user 2026-11-20: Q2-Q10 = defaults; Q1 = yes (option 1: gated
keyless naive sender, an explicit user-approved exception to rule 4, recorded in CLAUDE.md).

Goal: today the worker commits state exactly once but has no external side effects. A real system sends
emails and calls APIs; that is where "exactly once" usually breaks (the send is outside the DB, so a crash
between send and commit loses or duplicates it). Phase 9 records each side effect in an `outbox` table in the
same fenced transaction as the apply; a relay delivers it at least once with a stable idempotency key to a
fake consumer that dedupes on that key. The harness counts raw deliveries and distinct effects there.

## Roadmap corrections
1. "0 duplicates" means 0 duplicate *effects* at a consumer that honours idempotency keys. Raw duplicate
   deliveries (crash after send, lost ack) are expected and measured. Every doc states this definition.
2. "vs naive": cmd/naive has no side effects (cmd/naive/main.go:115-175). Q1 approved a gated keyless sender.

## Starting point (cited)
- Worker tx: fence cmd/worker/main.go:229-240; apply in a savepoint :255-263; `before_commit` :270; finish/commit
  :313-323. Anything written inside the savepoint shares the effect's fate: stale/skipped/failed roll it back
  (:274-306); lost fence writes nothing (:234-236).
- Apply call sites (4): cmd/worker/main.go:263, internal/admin/preview.go:168, internal/dbtest/apply.go:59,
  internal/admin/preview_db_test.go:398. Preview runs the real Apply then rolls back (preview.go:106, 199-214).
- At most one committed apply per inbox row ever: Replay accepts only dead/stale (internal/admin/replay.go:97-98),
  neither has a committed apply; undone is terminal (migrations/00006_undo.sql:2-3). So outbox can be unique on
  (inbox_id, kind) without a generation in the key.
- Undo reverts change_log images only (internal/admin/undo.go); it cannot unsend an email.
- TRUNCATE sites that must add `outbox` (FK to inbox): internal/sim/check.go:25, internal/dbtest/dbtest.go:42,
  dev.ps1:45; Reset test internal/sim/reset_db_test.go:24.
- Harness: one Supervisor per run (cmd/chaos/main.go:104-120); FAULT_SEED = Seed*100000+starts
  (internal/sim/supervise.go:57), so a second supervisor needs an offset. RESULT line main.go:183-184; optional
  fields appended per mode like undoResultFields (:179-182). WaitIdle looks at inbox only (check.go:49-64).
- Fault package: named points, os.Exit(1) on hit (internal/fault/fault.go:65-76).
- CI: chaos job builds 4 binaries (ci.yml:132-137), runs R1, R2, N1 (:142-175); gate parses fields by name.
- 8b lesson: every new checker entry point needs a DB test that calls it.

## Design

### Effects (Q2: all three). Payloads built at enqueue time, so every retry sends identical bytes.
| Event | Kind | Payload | Expected |
| --- | --- | --- | --- |
| customer.created | welcome_email | to, customer id | one per customer, from the model |
| invoice.paid | receipt_email | to, invoice id, credits | one per invoice, from the model |
| customer.subscription.* | subscription_notice | to, sub id, status | only when the upsert wrote; never for stale |

### Schema: migration 00007_outbox.sql (new)
`outbox(id BIGSERIAL PK, inbox_id BIGINT NOT NULL REFERENCES inbox(id), generation INT NOT NULL, kind TEXT CHECK
IN (3 kinds), idempotency_key TEXT NOT NULL UNIQUE, payload JSONB NOT NULL, status TEXT NOT NULL DEFAULT 'pending'
CHECK IN ('pending','sent','dead'), attempts INT NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT
now(), last_error TEXT, created_at, sent_at, CHECK ((status='sent') = (sent_at IS NOT NULL)))`;
`UNIQUE (inbox_id, kind)` named outbox_once; partial index `(next_attempt_at, id) WHERE status='pending'`.
Down: DROP TABLE outbox.
Idempotency key: `rg:stripe:<event_id>:<kind>` (business identity, not outbox.id: ids restart after RESTART
IDENTITY and a real provider would answer a new message with an old key's response). No generation in the key
(at-most-one-apply argument); revisit if done-replay is ever allowed (record in 0008).

### Enqueue
New internal/outbox: Message, Writer (`outbox.NewWriter(inboxID, generation)`), `Enqueue(ctx, tx, kind, eventID,
payload)`. app.Apply gains `out *outbox.Writer`, called after the last state write of each branch, inside the
caller's savepoint. nil writer = error (like the nil recorder, apply.go:51-55). Rejected: worker enqueues after
Apply (preview/dbtest would diverge); reading ids from the changelog Recorder (couples concerns). Apply's doc
comment (apply.go:5-9) gains the exception: outbox rows are append-only facts, not undoable state, not in
change_log (change_log scope is the 3 app tables, 00004:45).

### Relay: cmd/relay, one process, polling (Opus implementer)
- Claim: own committed statement like the worker (main.go:188-214): attempts+1, lease via next_attempt_at,
  FOR NO KEY UPDATE SKIP LOCKED; returns token (id, attempts). attempts > 8 -> dead, fenced by the token.
- Send: POST SINK_URL, header Idempotency-Key, body = stored payload, timeout RELAY_SEND_TIMEOUT. Startup refuses
  lease <= timeout. Fault points before_send, after_send (the key one).
- 2xx: `UPDATE ... SET status='sent', sent_at=now() WHERE id=$1 AND status='pending'`, deliberately NOT
  attempts-fenced (confirmed delivery is a fact; the key makes the other send harmless).
- 5xx/409/429/timeout: retry with the worker's backoff; other 4xx permanent -> dead (Q7). Both fenced by
  `attempts=$tok AND status='pending'`. Dead after 8 attempts.
- Rejected: tx + row lock held across HTTP (holds a connection during I/O; crash after send still rolls back).
  Rejected: LISTEN/NOTIFY, CDC (more parts; mention in 0008).
- Known, documented: a row can be dead although the sink has the effect; checker reports `outbox_dead`, never
  "not delivered".

### Fake consumer: internal/sink (SYNTHETIC), in-process in cmd/chaos (Q4)
http.Handler + mutex, per-seed Reset(). Records every raw delivery (key or none, body, time) and one effect per
key (first body). Same key + same body -> 200 (replayed); same key + different body -> 422, recorded as conflict.
No key -> new effect every time. Seeded faults: `-sink-fail-rate` (503 before recording), `-sink-lost-ack-rate`
(record, then 500). Survives relay crashes. Effects attributed to (event_id, kind) from the body.

### Harness (cmd/chaos + internal/sim)
Flags: `-outbox` (sink on `-sink-addr 127.0.0.1:8091`, SINK_URL passed to supervised processes),
`-supervise-relay bin/relay.exe` (relayguard only), `-sink-fail-rate`, `-sink-lost-ack-rate`; -crash-rate,
-kill-interval, -lease (RELAY_LEASE) apply to both supervisors. Supervisor gains SeedOffset; relay log
bin/relay-supervised.log. WaitIdle then WaitOutboxIdle (no pending rows, same -timeout); then sink snapshot.
RESULT gains, only with -outbox: `outbox_rows= raw_deliveries= effects= duplicates_absorbed= relay_restarts=
relay_kills=`; results file gains an "Outbox check" meta line/columns only in that mode.

### Checker (internal/sim/outbox_check.go); one violation kind per rule, one control per rule
DB side (on relayguard runs even without -outbox; one REPEATABLE READ READ ONLY tx):
- outbox_missing: no row for an expected welcome/receipt (model), or for a subscription event with inbox done/undone.
- outbox_unexpected: row whose event is stale/skipped/dead, or not in the history (poison included).
- outbox_bad_generation: generation differs from that inbox row's apply change_log generation.
- outbox_not_sent (with -outbox): still pending after the wait. outbox_dead (with -outbox): ended dead.
Sink side (-outbox): effect_missing (expected fact without effect, or a sent row's key absent);
effect_duplicate (>1 distinct effect per (event_id, kind)); effect_unexpected (effect for stale/skipped/dead/
unknown event); effect_conflict (same key, different bodies).
Measured, not judged: raw deliveries, distinct effects, absorbed = sum over keys of (raw - 1).
Honesty: expected subscription notices come from the system's own inbox status (circular). Mitigation: model
cross-check that the true-order last event of each subscription has a notice; notices <= subscription events.
Labelled weaker than the model-derived welcome/receipt checks.
Naive target: welcome/receipt rules apply unchanged; for notices only effect_duplicate (naive has no stale). Labelled.
Undo (Q3): never touches the outbox, no compensation; with -undo-all, snapshot (outbox rows, sink effects) before
the undo phase and compare after: undo_outbox_changed. Replay: stale row replayed then applied enqueues at
generation 1 (DB test); dead poison never enqueues.

### Naive (Q1 = yes, rule-4 exception approved by the user 2026-11-20)
When SINK_URL is set, naive POSTs each message with NO idempotency key after its writes, in the handler (tutorial
pattern). Send errors logged and ignored, 200 still returned (Q5). The send sits before the existing
before_response fault point (naive main.go:109), so a crash there -> resend -> second email; no new fault point.
SINK_URL unset -> behaviour unchanged (Phase 1-8 results and CI N1 keep their meaning). Every existing flaw stays.

## Steps (one commit each; checkpoint B before each commit and push)

### 9a: schema + enqueue (implementer sonnet; reviewer Opus)
Changes: migrations/00007_outbox.sql, internal/outbox/, app.Apply signature + 4 call sites, 3 TRUNCATE sites +
reset_db_test, dev.ps1 db-test and reset-data help text.
DB tests: TestDBOutboxEnqueuedWithApply (one row per kind; key, generation 0, payload); TestDBOutboxRolledBackOnFailedApply
(AfterGrant error -> retry scheduled, 0 rows); TestDBOutboxNoneForStale; TestDBOutboxNoneForSkipped;
TestDBOutboxNoneForDecodeDead; TestDBOutboxNoneOnLostFence; TestDBOutboxCrashBeforeCommit (child process,
before_commit always -> 0 rows; after retry exactly 1); TestDBOutboxOncePerEventKind; TestDBReplayStaleEnqueuesAtGeneration1;
TestDBPreviewLeavesNoOutbox; TestDBUndoLeavesOutboxUntouched; TestDBResetWithOutboxRows.
Mutations (each fails a named test): enqueue via pool not tx; enqueue before the stale decision; drop outbox_once;
omit outbox from Reset.
Acceptance: vet, gofmt, test, db-test (-p 1) green; smoke (scratchpad) crash seeds 1-3 + poison seed 1, 0 violations.
9a implementation note (2026-11-20): TestDBOutboxRolledBackOnFailedApply was renamed TestDBOutboxNoneOnFailedApply
(review r1 should-fix): AfterGrant fires before the receipt enqueue, so the test proves "a failed apply leaves no
row, the retry exactly one", not rollback of a written row (covered by TestDBOutboxCrashBeforeCommit and
TestDBPreviewLeavesNoOutbox). Mutation "enqueue before the stale decision" first passed (the worker's savepoint
rollback hid it); TestDBOutboxNoneForStale now calls app.Apply directly and checks Writer.Len() and rows in the tx.

### 9b: relay + sink (implementer OPUS: relay state machine; fenced vs unfenced updates, claim-time dead-lettering,
lease vs send timeout, "dead but delivered"; the race class earlier phases got wrong first, 5a ABA, 6a chain-break)
Changes: cmd/relay, internal/sink, dev.ps1 build (+ bin/relay.exe).
Tests: sink unit (dedupe; conflict 422; keyless counted each time; fail-rate; lost-ack; concurrent same-key -> 1
effect under -race). Relay DB tests against an httptest sink: TestDBRelaySendsAndMarksSent, TestDBRelayRetryOn503,
TestDBRelayDeadAfterMaxAttempts, TestDBRelayPermanent4xxDead, TestDBRelayCrashAfterSendResendsSameKey (child,
after_send always: sink raw=2, effects=1, row sent, attempts=2), TestDBRelayStaleFailureFenced,
TestDBRelayClaimDeadLettersOverMax, TestRelayRefusesLeaseNotAboveTimeout.
Mutations: random key per attempt (fails crash test); unfenced failure update (fails stale-failure test); mark-sent before send.

### 9c: harness + checker (sonnet with the rule table verbatim; Opus if the lead changes any rule, or for the
expected-set and undo-snapshot logic)
Changes: cmd/chaos flags, second supervisor with SeedOffset, WaitOutboxIdle, RESULT/markdown fields, sim/outbox_check.go.
Tests: unit tests with canned data, one control per kind; TestDBCheckOutboxRules (DB control per DB-side rule;
checker actually called); cmd/chaos flag validation (-supervise-relay needs -outbox and relayguard), RESULT format
unchanged without -outbox.
Smoke (scratchpad): `chaos -target relayguard -seeds 1-3 -dup 0.2 -reorder 10 -supervise bin/worker.exe
-supervise-relay bin/relay.exe -outbox -crash-rate 0.1 -kill-interval 2s -lease 1s -sink-lost-ack-rate 0.1
-sink-fail-rate 0.1`. Hypothesis: 0 violations, duplicates_absorbed > 0 (if 0, report: duplicate path not exercised).

### 9d: naive sender (sonnet)
Changes: cmd/naive (gated sender only), outbox rules for the naive target.
Tests: httptest unit: with SINK_URL sends once per apply, no key; unset -> no HTTP call; existing flaws intact
(doc comment naive main.go:1-10).
Smoke: naive seeds 1-3 with -outbox, hypothesis effect_duplicate > 0; N1 without -outbox still reports violations.

### 9e: CI (sonnet)
ci.yml builds relay; new step R3: `chaos -target relayguard -seeds 1-2 -outbox -supervise bin/worker
-supervise-relay bin/relay -dup 0.2 -reorder 10 -crash-rate 0.1 -kill-interval 2s -lease 1s -sink-lost-ack-rate 0.1
-sink-fail-rate 0.1 -out ci-out/relayguard-outbox.md`, gated `clean 2`; upload bin/relay-supervised.log; N1
unchanged (Q6); gate self-test gains a SYNTHETIC canned log with outbox fields.
Acceptance: main green; red proof ci-red/9e-key (random key per attempt): R3 red with effect_duplicate
(push/delete needs the user's OK).

### 9f: evidence + docs (experiment-runner sonnet, reviewer, interview-coach sonnet)
Clean tree (worker, relay, naive, chaos vcs.modified=false); reset-data before each run; never alongside db-test.
Runs (Q9): O1 crash+outbox seeds 1-20 (9c flags) -> results/phase9-outbox-crash.md; O2 poison+outbox seeds 1-5
(-poison 2; dead=2, no effects for them); O3 outbox + -undo-all newest seeds 1-5 (undo_outbox_changed=0);
O4 lost-ack only, no crashes, seeds 1-5; N9 naive + -outbox seeds 1-20 (O1 flags minus relay/poison);
regression R1/R2 seeds 1-3 to the scratchpad.
docs/phase9-results.md caveats: sink synthetic and honours keys; "0 duplicates" = 0 duplicate effects, raw
measured; relayguard runs carry extra changelog_* and outbox_* checks; naive sends without a key, so the gap
combines inbox dedupe + outbox + keys and does not isolate the key; subscription-notice expectation circular;
real providers expire keys (TTL).
docs/decisions/0008-transactional-outbox.md: alternatives (send in tx, send after commit, 2PC, CDC, LISTEN/NOTIFY,
key from outbox.id); undo cannot unsend. README Phase 9 section + results line; interview notes; ROADMAP ticks.

## Risks
Two supervisors double processes and kill noise (wall time unmeasured). Backoff 1s-60s with sink failures can
stretch runs; 3-min -timeout per seed is the backstop, timeouts reported. outbox_dead under 8 consecutive faults
possible: report it. Apply signature change touches preview: preview DB tests stay green apart from the new argument.

## Approved answers
Q1 yes (gated keyless naive send on SINK_URL; rule-4 exception). Q2 all three kinds. Q3 no compensation, undo
leaves outbox alone (compensation -> Final polish). Q4 sink in-process in cmd/chaos only. Q5 naive logs send
error, still 200. Q6 CI adds R3 only, N1 unchanged. Q7 4xx other than 409/429 permanent -> dead. Q8 no preview
outbox display in Phase 9. Q9 O1/N9 20 seeds, O2-O4 5. Q10 keep sent rows (no retention), documented.

## Likely interview questions
Send inside the tx or after commit? What "exactly once" means (at-least-once + consumer dedupe on a stable key =
effectively-once effects); provider without keys? Why key = event_id+kind, not outbox.id/UUID; key TTL vs retry
window. Why mark-sent unfenced, failure fenced. Relay crash after send walkthrough. Undo after the email went out.
Replay vs outbox; why (inbox_id, kind) unique without generation. Polling vs LISTEN/NOTIFY vs CDC; several relays
(SKIP LOCKED, Phase 10); ordering (none). How do you know the checker catches a duplicate (naive run + red proof).
Is the naive comparison fair (keyless; caveat in the results doc).
