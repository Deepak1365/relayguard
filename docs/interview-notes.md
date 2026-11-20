# RelayGuard interview notes

Notes for explaining each phase in a SWE interview. Every number here is quoted from a results
file in this repo, with the file named next to it. If a number is not in a results file, it is
not in these notes.

---

## Phase 5: claim with lease, fencing, poison dead-lettering

Sources: `docs/phase5-results.md` (evidence, limitations, tradeoffs), `cmd/worker/main.go`
(`claim`, `process`, `deadLetterCrashed`), `cmd/worker/main_db_test.go`, `docs/ROADMAP.md`.
Code change: commit `975ef44`. Results: commit `fe6b30a`. The evidence binaries were built at
HEAD `2c708e6` (per `docs/phase5-results.md`, "Tree and binary state for all evidence runs").

### 30-second version

"In Phase 4 the worker incremented an event's attempt counter inside the same transaction that
applied the event. If an event crashed the worker every time (a poison event), the crash rolled
back the counter too, so the event never reached the dead-letter limit and the worker kept
picking it up forever. Everything queued behind it was blocked. I reproduced that: 99 crash
cycles, and the invoice was still `pending` with `attempts` 0, and the event behind it never ran.

The fix splits the worker into two steps. CLAIM is its own statement that commits immediately: it
does `attempts + 1` and sets a lease by pushing `next_attempt_at` forward. PROCESS is one
transaction. It first re-checks that its claim is still current (a fencing check on `attempts`),
then applies the effect. Because the attempt is committed before any risky work, a crashing event
still uses up its attempts. Once a claim sees more than 8 attempts, the worker marks the row
`dead` with a reason instead of running it again. After the fix, the same scenario ends with the
invoice `dead` after 8 attempts and the next event `done`. The chaos regression and the poison runs
both report 0 violations."

### The problem (E1, before)

- Phase 4 worker built from `7b7e855` in a worktree, `WORKER_CRASH_AFTER_GRANT=1`. Three events
  were sent: a customer, an invoice that crashes the worker, and a subscription update behind it.
- Result, from `docs/phase5-results.md` section "E1": "99 cycles, all exit code 1". Inbox after the
  loop: customer `done` / 1, invoice `pending` / 0, subscription `pending` / 0. Customer `cus_fx_1`
  has balance 0, 0 grants, ledger 0, and there are no subscription rows.
- The doc labels the cause as interpretation, not a separate measurement: the attempt counter is
  rolled back with each crashed transaction.

### The fix (code, `cmd/worker/main.go`)

- `claim` is one autocommitted statement:
  `WITH next AS (SELECT id FROM inbox WHERE status = 'pending' AND next_attempt_at <= now() ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED) UPDATE inbox ... SET attempts = attempts + 1, next_attempt_at = now() + lease ... RETURNING id, attempts`.
- `processOne`: if the claimed `attempts > maxAttempts` (8), it calls `deadLetterCrashed` and does
  not run the event again.
- `deadLetterCrashed`: `UPDATE ... SET status = 'dead', attempts = 8, last_error = 'gave up after 8 attempts; last error: ...'`
  `WHERE id = $1 AND status = 'pending' AND attempts = $3`. The guard on `attempts` is a fence, so a
  worker that stalled and lost its claim cannot dead-letter a row someone else re-claimed. It
  records `maxAttempts`, not `claimedAttempts - 1`, because a claim past 8 never runs the event,
  and one such claim can be lost (a kill before the UPDATE). `claimedAttempts - 1` could then
  overstate the count.
- `process` fencing: `SELECT payload FROM inbox WHERE id = $1 AND status = 'pending' AND attempts = $2 FOR UPDATE`.
  If no row comes back, the claim is stale and the worker logs "lost claim" and does nothing. The
  `FOR UPDATE` row lock is held until commit, so `claim`'s `SKIP LOCKED` cannot re-claim the row
  mid-apply, even if the lease runs out.
- Lease: `defaultLease = 10s`, can be overridden with `WORKER_LEASE` (the harness uses `1s`).
- Ordinary apply errors still go through the Phase 2 path: savepoint rollback, backoff
  (1s, 2s, 4s ... capped at 1 minute), and `dead` on attempt 8 with the real error.

### Evidence (quoted from `docs/phase5-results.md`; verbatim stdout is in that doc)

| Run | What | Result as recorded |
|---|---|---|
| E1 (before) | Phase 4 worker, crashing invoice | 99 cycles, all exit code 1. Invoice `pending`, `attempts` 0. Subscription `pending` |
| E2 (after) | Phase 5 worker, same scenario, `WORKER_LEASE=1s` | 9 cycles. Cycles 1-8 ended with `FAULT: crashing at after_grant` and exit code 1. In cycle 9 the worker logged `DEAD inbox row 2 after 8 attempts (possible poison event)`, then `applied evt_fx_sub_active`. Final inbox: customer `done`/1, invoice `dead`/8, subscription `done`/1 |
| E3 (regression) | `-dup 0.2 -reorder 10 -crash-rate 0.1 -kill-interval 2s -lease 1s`, seeds 1-20 | `RESULT target=relayguard seeds=20 failing_seeds=0 violations=0 restarts=272 hard_kills=30`. No dead or pending rows |
| E4 (poison) | same flags + `-poison 2`, seeds 1-5 | `RESULT target=relayguard seeds=5 failing_seeds=0 violations=0 restarts=155 hard_kills=21`. `dead=2` in every seed |

Also from the doc:
- E2's full `last_error` for the invoice: "gave up after 8 attempts; last error: none recorded
  (the worker crashed or was killed while processing)".
- E4 log: 80 `POISON` lines = 5 seeds x 2 poison events x 8 attempts.
- E3 vs Phase 4 (`results/relayguard-crash.md`): Events and Deliveries match for all 20 seeds.
- Results files: `results/relayguard-crash-lease.md` (E3), `results/relayguard-poison.md` (E4).
  A superseded E3 run (`results/relayguard-crash-lease-unattended.md`, 273 restarts, 32 hard
  kills, 0 violations) is kept but is not the evidence.
- E3b (crash-only regression) and E5 (Phase 4 worker with poison under the harness) were **not
  run**. Do not claim them.
- DB tests covering lease, fencing and dead-lettering exist in `cmd/worker/main_db_test.go`, for
  example `TestDBLeaseExpiryAndFencing`, `TestDBHeldFenceLockBlocksReclaim` and
  `TestDBDeadLetterRespectsReclaim`. The Phase 5 results doc says `db-test` was not run in its
  recorded test run. Only `dev.ps1 test` was recorded, with exit status 0.

### Likely interview questions

1. **Why did a poison event never get dead-lettered before?**
   The attempt counter was updated in the same transaction as the work. The crash killed the
   process before commit, so Postgres rolled the counter back along with everything else. From
   the database's view the event had never been tried. E1 shows it: 99 crash cycles, `attempts` 0.

2. **Why not just catch the error and increment the counter?**
   Because a crash is not an error you get to catch. `os.Exit`, an OOM kill or a power loss runs
   no handler. The only safe approach is to commit "I am about to try this" *before* the risky
   work. That is the CLAIM statement. It is the same idea as write-ahead logging: record the
   intent durably first.

3. **What is the lease for, and what is fencing?**
   The lease (`next_attempt_at = now() + lease`) hides a claimed row from other claims until it
   expires, so a crashed worker's row becomes claimable again without anyone having to clean up.
   Fencing deals with a worker that is slow, not dead. Each claim bumps `attempts`, and `process`
   only continues `WHERE attempts = <my claim>`. A worker holding an older claim finds no row and
   backs off. `attempts` doubles as the fencing token, so no extra column is needed.

4. **What if the lease expires while a worker is still applying the event?**
   Then the `SELECT ... FOR UPDATE` in the fencing check has already locked the row, and `claim`
   uses `FOR UPDATE SKIP LOCKED`, so nobody can re-claim it until the transaction ends.
   `TestDBHeldFenceLockBlocksReclaim` checks this with a held lock and a 5 s timeout. If a re-claim
   wins the race *before* the first worker takes the lock, the fence rejects the old worker
   (`TestDBLeaseExpiryAndFencing`: the stale worker writes 0 customers, the new one writes 1). Be
   honest that these tests run two worker objects one after the other in one process. They are not
   concurrent processes.

5. **Why does `deadLetterCrashed` write 8, not `claimedAttempts - 1`?**
   Only claims 1-8 ever run the event. Claim 9 and later only lead to dead-lettering. If a worker
   is killed between claim 9 and the UPDATE, claim 10 does the dead-lettering, and
   `claimedAttempts - 1` would report 9 runs that never happened. Writing `maxAttempts` keeps the
   count true. `TestDBDeadLetterRespectsReclaim` covers this case.

6. **How do you know it works?**
   Before and after on the same scenario: E1 is stuck after 99 cycles, and in E2 the invoice is
   `dead` after 8 attempts and the event behind it is `done`. Then the chaos harness: the 20-seed
   regression with crashes, duplicates and reordering has 0 violations, and the 5-seed poison run
   has 0 violations with `dead=2` in every seed. The harness's invariant checker compares the final
   database state against a seeded reference model (`internal/sim`). (Before an interview, read the
   checker so you can list exactly which invariants it covers.)

7. **Does a poison event still block other events?**
   Yes, until it is dead. In E2 the subscription was applied only in cycle 9, after `DEAD inbox row 2`.
   The doc's explanation is marked "not measured": the claim orders by `id`, and the restart
   interval (cycle starts 1.26-1.34 s apart) was longer than the 1 s lease. So each restarted
   worker found the invoice's lease already expired and claimed it again before the subscription.
   With a lease longer than the restart time, I expect later events to overtake it, but I have not
   run that.

8. **Your logs show 9 DEAD lines but the harness found 10 dead rows. Explain.**
   In E4 seed 4, only one `DEAD inbox row` line was logged, but the harness reported `dead=2`.
   `deadLetterCrashed` commits the UPDATE first and logs afterwards. The doc's unverified
   explanation is that a random hard kill landed between the commit and the log write. The
   workers started at relative lines 823 and 826 ended with no output, which fits. The lesson:
   the inbox table is the source of truth, and logs can undercount.

### Tradeoffs to state openly

- **One extra database round trip per event** (claim, then process).
- **Lease wait after a crash.** A crashed event is retried only when its lease expires: 10 s by
  default, and the harness uses 1 s to keep runs short. The lease is a fixed delay, not an
  exponential backoff, so a crash-looping event is retried at the same pace every time.
- **The lease length is a guess about processing time.** If it is too short, rows get re-claimed
  while still in progress (fencing makes that safe, but it wastes attempts). If it is too long,
  recovery after a crash is slower.
- **An attempt is used even when the crash was not the event's fault.** A random kill
  during an innocent event also uses up one of its 8 attempts. A healthy event that is unlucky
  enough could in principle be dead-lettered. That was not observed: E3 has 0 dead rows.

### Weak spots an interviewer may probe (admit them, do not defend them)

- **A poison event still blocks later events until it is dead.** It is not isolated, only
  bounded. The likely cause is `ORDER BY id` plus a restart interval longer than the lease. This
  is unmeasured (see Q7).
- **Fencing was not tested with concurrent workers.** Only one worker process ran at a time, both
  in the restart loop and under `-supervise`. The code comment above the fencing query says "Not
  directly tested". Concurrency is planned for Phase 10.
- **The poison sample is small:** 5 seeds, 10 poison events in total.
- **The poison is simulated.** With `FAULT_POISON=1`, `test.poison` events call `os.Exit(1)`.
  Real poison, such as panics, hangs, OOM or handler bugs, was not tested. A *hang* is
  especially relevant: nothing here kills a worker that hangs while holding the row lock.
- **The DEAD log line can undercount** (E4 seed 4). Use the inbox, not the log.
- **Stale rose compared with Phase 4, and the cause is not explained.** E3 totals: Phase 4
  done=1558 stale=34, Phase 5 done=1534 stale=58. Stale was higher in 16 seeds, equal in 4 and
  lower in none. The superseded run also had more stale rows (done=1531 stale=61). The checker
  found 0 violations, so the final state was still correct. But the split varies from run to run
  under crashes and has not been explained. If asked: "My hypothesis, which I have not tested, is
  that the lease delays a crashed event's retry, so a newer event for the same subscription
  commits first and the older one is then correctly judged stale. I'd test it by varying
  `-lease` with fixed seeds."
- **Worker-start lines are fewer than restarts + 1** in all three chaos runs, for example 268 vs
  273 in E3. This was not investigated. The likely reason is kills during startup.

### Alternatives worth naming

- **Separate attempts table** (append-only `inbox_attempts` rows written in their own
  transaction). It gives a full attempt history with timestamps for debugging, but needs an extra
  write and a join or count to decide on dead-lettering. The single counter is simpler and enough
  for a limit.
- **Postgres advisory locks** (`pg_try_advisory_lock(id)`) instead of a lease. A session lock is
  released automatically when the connection dies, so there is no lease to wait out after a crash.
  The cost: it is tied to the connection (awkward with poolers like PgBouncer in transaction mode),
  it is not durable, and on its own it does not count attempts. It would still need the committed
  counter.
- **Per-entity queues / partitioned ordering** (claim the oldest ready event *per customer or
  subscription*, not globally by `id`). A poison event would block only its own entity, not
  everything. More complex, and it ties into Phase 10's per-entity ordering under concurrency.
- **Exponential lease / crash backoff** (`lease * 2^(attempts-1)`, capped). A crash-looping event
  would back off and give other events room, at the cost of slower recovery for an event that
  crashed once by bad luck.
- **Heartbeat lease renewal** for long-running handlers instead of one fixed lease.
- **Off-the-shelf queues** (SQS visibility timeout + redrive to a DLQ, or Kafka with a
  dead-letter topic) do the same claim/lease/DLQ pattern as a managed service. Building it on
  Postgres keeps the inbox, the effects and the status update in one transaction.

### "Tell me about a bug you found"

"My Phase 4 worker passed every chaos run, but I suspected it had a blind spot. Its retry limit
was enforced by a counter updated *inside* the processing transaction. So I built an event that
crashes the worker every time and ran the old binary, built from the old commit, in a restart
loop. After 99 crashes the event still showed `attempts` 0 and was `pending`. The subscription
event behind it never ran, and the dead-letter limit was never reached. The rollback that made
the worker crash-safe also erased the evidence that it had tried. The fix was to move the attempt
into a separate CLAIM statement that commits first, with a lease and a fencing check so a slow
worker can't act on an outdated claim. Same scenario afterwards: dead after 8 attempts with a
reason, and the next event processed. The full 20-seed chaos regression still had 0 violations.
What I'd still flag: the poison event blocks the queue until it dies, and I haven't tested
fencing with truly concurrent workers yet."

---

## Phase 6: change log, preview, replay, undo, and clean-tree evidence

Sources: `docs/decisions/0001` to `0005` (decisions and tradeoffs), `docs/phase6-results.md`
(evidence), `docs/phase6-plan.md` (review findings, mutations), `CLAUDE.md` Status (lessons).
Commits: change log `fe27b99` and `d2e3cf3`; preview `052c0f5`; replay fence `425f62d`; replay
`dcde809`; undo `14ae77f`; undo checker `b4c9405`; harness `6c5a66c`; evidence `7068ef2`;
decision records `f955677`. Evidence binaries were built from clean `6c5a66c`
(`docs/phase6-results.md:8-12`).

### 30-second version

"Phase 5 made event processing crash-safe. Phase 6 gives an operator safe tools on top of it:
preview what an event would do, replay a dead event, and undo an applied one. All three rest on a
`change_log` table. The worker writes it in the same transaction as the effect, with before and
after row images taken from the database. So the log can never disagree with the data.

Preview runs the real apply code inside a transaction that is always rolled back. Replay resets a
dead or stale event, and I changed the worker's fence token to (replay_count, attempts) so a stalled
old worker can't sneak past after a reset. Undo restores before-images, but only if every later
change to those rows is logged and itself undone. Every refusal is audited. I checked it with the
chaos harness: crash runs, poison runs, and an 'undo everything' property run, all with 0
violations, against a naive baseline that fails all 20 seeds (RelayGuard runs also carry extra changelog_* checks the
naive run does not). The limits: undo was never crash-injected or
run against live concurrent workers, and E4 never replays events."

### Evidence (quoted from `docs/phase6-results.md`; each experiment ran once)

| Run | RESULT line (verbatim) | Line |
|---|---|---|
| R1 crash, seeds 1-20 | `failing_seeds=0 violations=0 restarts=257 hard_kills=36` | :99 |
| R2 poison, seeds 1-5 | `failing_seeds=0 violations=0 restarts=161 hard_kills=20` | :114 |
| E4a undo-all newest, 1-20 | `violations=0 ... undone=1534 undo_refusals=0 undo_skipped=0` | :177 |
| E4b undo-all random, 1-20 | `violations=0 ... undone=1530 undo_refusals=814 undo_skipped=0` | :229 |
| E4c poison + newest, 1-5 | `violations=0 ... undone=383 undo_refusals=0 undo_skipped=0` | :254 |
| E5 naive crash, 1-20 | `failing_seeds=20 violations=402 restarts=276 hard_kills=12` | :294 |

Comparison caveat (`docs/phase6-results.md:309-315`): RelayGuard runs carry extra `changelog_*`
checks and use `-lease 1s`. The naive run has neither, so the checks are not identical. RelayGuard
is held to more checks and still has 0 violations. Restart and hard-kill counts are of different
processes and are not a measure; violations are.

### Likely interview questions

1. **Why write the change log in the same transaction as the effect?** (decision 0001)
   If it were a separate step, a crash between "applied" and "logged" would leave a change undo
   can't see. In one transaction the log commits or rolls back with the effect. The tradeoff is
   that every write path must call the recorder, and a forgotten path is an unlogged write.
   Today that is caught by the checker and tests (`TestDBChangeLogCheckDetectsUnloggedWrite`), not
   by construction. Alternative: database triggers for capture, listed in 0001 as a revisit. Cost
   of the extra insert: not measured.

2. **Why take images from the database instead of building them in Go?** (0001)
   `to_jsonb(row)` in the writing statement includes defaults and trigger-set values such as
   `row_version`. Tradeoff: JSON timestamps depend on the session time zone, so the connection pins
   UTC. Image comparison relies on that.

3. **Why does preview run the real handler and roll back instead of simulating?** (0005)
   A simulation is a second copy of the business rules that can drift, and it can't show
   trigger-set values. Real code plus rollback can't disagree with the real apply. Costs: it takes
   real locks, consumes sequence values (printed ids of inserts are provisional), and can lose a
   race with the worker (`ErrBusy`, `ErrChanged`). The file never calls Commit, and
   `TestPreviewSourceHasNoCommit` enforces that. `TestDBPreviewWritesNothing` snapshots tables.

4. **Why did replay change the fence token? What is the ABA problem?** (0002, commit `425f62d`)
   Replay resets `attempts` to 0. A worker that claimed with `attempts = 1` and stalled could wake
   after a replay and a second claim had brought `attempts` back to 1, and pass the newer claim's
   fence. A counter that resets can repeat a value. Fix: a monotonic `replay_count`, fenced as
   `(replay_count, attempts)`. It doubles as `change_log.generation`. Proof is targeted tests
   (`TestDBReplayFencesOldClaim`; the mutation that drops replay_count from the fence fails it),
   not a chaos run, because chaos runs never replay. Tradeoff: one more column and predicate in
   the claim, fence and dead-letter statements.

5. **You changed FOR UPDATE to FOR NO KEY UPDATE. Why?** (0001, 0002; plan :96)
   Foreign keys take FOR KEY SHARE on the parent row. `change_log` and `admin_actions` point at
   `inbox`, so an admin transaction holds KEY SHARE on the inbox row, and a FOR UPDATE fence would
   queue behind it. FOR NO KEY UPDATE still excludes other claims, fences and replays. The
   customer-row version of the deadlock was argued by a reviewer, and 0001 says no test reproduces
   it. `TestDBKeyShareDoesNotBlockClaimOrFence` pins the inbox side; the plan's mutation
   (claim/fence back to FOR UPDATE) fails it (plan :173).

6. **A weaker lock changes who overlaps whom. What did you re-check?** (0005, plan :200-212)
   With NO KEY UPDATE, preview (KEY SHARE) no longer queues behind the worker, so it can overlap an
   apply in progress. I re-checked each "can't happen" race. Preview is bounded by a 2s
   `lock_timeout` and by `stateMoved`, which re-reads status and replay_count and returns
   `ErrChanged`. Be honest: freedom from deadlock between preview and a live worker rests on
   lock-order reasoning, not a test. Preview takes the inbox row first, and the inbox-first order
   is kept as a guard.

7. **Why rule B for undo and not strict row-version equality?** (0003)
   The user's first choice was strict row-version. The planner found it makes newest-first undo
   impossible: the undo is an UPDATE, so the trigger bumps the version, and the next older undo sees
   a version it doesn't expect (customer v1, invoice v2, undo makes v3, undo of customer expects v1,
   refused forever). Rule B: an event is undoable only if every later change to its rows is logged
   and itself undone. Same strictness about unknown writes, but undo's own bump is explained by its
   log row. Alternative: compensating events. A new forward event would collide with the
   subscription ordering rule, so I chose delete-and-restore from before-images.

8. **Why audit refusals, and why report an outcome only after commit?** (0004)
   Refusals are where an operator probes state, and hiding them hides confusion or misuse. The
   refusal row is written inside the same locked transaction and committed, so it describes the
   state the decision was based on. A review found that setting `Outcome = refused` before the audit
   insert committed would let the CLI claim an audit that failed. The CLI now prints nothing if the
   audit insert or commit fails, and points to `admin log <id>`. Tests:
   `TestDBReplayRefusalAuditFailureReportsNoOutcome` forces the insert to fail with a trigger.
   Known edge: dying after commit but before printing leaves the operator with no report.

9. **What does the E4b result of 814 refusals mean? Is that a failure?**
   No. In E4b, undo is attempted in random order with retries. Each refusal is a legitimate
   "undo the later event first" or "referenced" answer, and every done event was eventually
   undone (undone=1530; per-seed undone equals done, `docs/phase6-results.md:189-228`). The refusal
   count varies run to run because the done/stale split varies under crashes
   (`docs/phase6-results.md:232-235`, `:334-337`). Compare E4a, newest first: 0 refusals.

10. **Why only claim evidence from a clean tree?**
    A binary built from a dirty tree records `vcs.modified=true`, so the result can't be tied to
    a commit. All four binaries for these runs record `vcs.modified=false`, built from `6c5a66c`
    (`docs/phase6-results.md:19`, `:26-48`, `:338-342`). I also keep the tree clean, since untracked files
    set the flag too.

### More engineering lessons (short answers)

- **One control per rule.** If two checker rules report the same violation kind, one masks the
  other, and a test can pass for the wrong rule. Each rule has its own failing control, and a mutation
  check showed the row_version +1 rule would otherwise mask the image comparison (plan :122).
- **Check attribution, not just counts.** The checker compares chain, final state and which event
  wrote each row, not only row counts (0001 consequences).
- **A test must be able to fail for the claim it makes.** A 40P01 (deadlock) assertion under a 200ms
  `lock_timeout` could not fail, because the timeout fires before `deadlock_timeout` (1s). The plan
  states that test does not prove freedom from deadlock (plan :208-209). It proves `ErrBusy` and that
  the worker still commits.
- **Re-read state after a failed statement.** After a failed apply the transaction is aborted, so
  preview rolls back to the savepoint and then re-reads status and replay_count (plan :219-224). A
  mutation that re-reads without the rollback fails, because the re-read hits the aborted transaction.
- **Shared constants between a free-text writer and its parser.** The worker writes `last_error`
  text and the admin reader recognises it, so both use constants in `internal/app/deadletter.go`
  (`GaveUpPrefix`, `NoResultRecorded`, `DecodePayloadPrefix`) and can't drift (plan :262, :298).
- **A heuristic warning must not claim more than its signal supports.** A `last_error` starting with
  "decode payload" gets only a mild "will most likely fail the same way" warning (plan :262).
- **Repeat tests whose outcome depends on random ordering.** Concurrency-sensitive DB tests were
  run with `-count=20` (for example `TestDBPreviewWorkerCommitsDuringPreview`, and
  `TestDBUndoCustomerWaitsForWorkerFK` with no 40P01/23503, plan :353, :381). I could not find a
  written example of a specific Go map-order bug in the docs, so don't claim one in an interview.
- **Checker must not panic on corrupt state, and reads one snapshot.** `checkUndoRows` reads events
  and undo rows in one REPEATABLE READ READ ONLY transaction. Two READ COMMITTED reads let an inbox
  row commit between them, which gave a nil map entry and a panic (plan :470-471). It also returns
  an error instead of panicking if an undo row is malformed.
- **Audit row versus data consistency.** The checker flags a done undo audit row with nothing
  reverted (`changelog_missing`/`unexpected`), so an audit row that claims an action the data doesn't show is caught.

### Hard follow-up questions

1. **"Did you test concurrent workers?"** No. Fencing tests simulate a stalled claim with a second
   connection (0002). The chaos harness crashes and restarts a supervised worker, but does not run
   several at once. The preview-versus-worker overlap and the undo lock order are argued from lock
   ordering. `TestDBUndoCustomerWaitsForWorkerFK` (-count=20) is the closest test, and
   `TestDBApplyWritesExistingRowLast` pins the assumption the undo lock-order argument rests on.

2. **"Does the undo evidence cover replays and crashes?"** No. E4 never replays events, so
   the generation filter in undo-all ordering is covered only by `TestDBUndoAllReplayedGeneration1`.
   Undo was not crash-injected and not run against live processing
   (`docs/phase6-results.md:343-349`). The undo-all run is a property check on synthetic seeds,
   not an operator workflow. Each experiment ran once.

3. **"Is 0 versus 402 a fair comparison?"** Mostly, with caveats. Same seeds and flags, but
   RelayGuard runs `-lease 1s` and carries extra `changelog_*` checks. The naive run says
   "Change log check: n/a". Restart and hard-kill counts (257 vs 276, 36 vs 12) are different
   processes and not a measure. RelayGuard shows 0 provider retries by construction: only its worker
   was crash-injected, not its receiver. Naive's 592 provider retries and 123 gave up (sum over 20 seeds)
   include already-applied events whose resends were rejected (`docs/phase6-results.md:323-331`).

### Tradeoffs and limitations to admit openly

- `change_log` has no retention, and undo needs the history. A future column needs a backfill or
  a checker update (0001).
- After a chain break the older event can never be undone (by design, 0003). Undo rule 5 scans
  `credit_grants` and `subscriptions` unindexed while the customer row is locked (plan :421-422).
  The customer-delete safety check covers only the references in the fixed `referencedBy` map.
- One mutation is not caught: removing the `break` in the delete-entry loop, because the app never
  deletes (plan :420).
- Actor and reason are free text. There is no authentication, so the audit is only as honest as
  the operator's flags (0004).
- `bin/worker-phase4.exe` ignores replay_count and must not be used once replays exist (0002).
- Not measured: change-log write cost, preview latency, audit volume, multi-worker behaviour.

### "Tell me about a bug you found"

"I built undo, and a review found a hole I'd missed. My conflict rules compared the row only with
the last logged change. Take this sequence: customer created, invoice I1, then someone edits the
email directly with an outside UPDATE, then invoice I2. I undo I2, which restores an image that
already contains the outside edit. Then I undo I1, and every check passes, and it silently wipes
the outside edit. Each later before and after image carries the outside write forward, so a
comparison at the end can't see it. The fix is a chain check: every link from the event's own entry
onward must have `before_image == previous after_image`, including row_version, otherwise the
conflict is `chain_break` and undo refuses. The regression test
(`TestDBUndoConflictUnloggedWrite`) failed before the fix on the content case
(`docs/decisions/0003`, plan :399-405). A similar thing happened in review of the evidence: a
sentence in the results doc said this run was the 'first' with `vcs.modified=false`, which was
false, because Phase 5 had it for `worker-phase4.exe`. It was corrected to what the Phase 5 doc shows
(`docs/phase6-results.md:338-341`)."

---

## Phase 7: local admin dashboard (read API, HTTP admin API, embedded UI)

Sources: `docs/phase7-plan.md` (decisions Q1-Q7 at :141-149, safety at :49-63),
`docs/decisions/0006-dashboard-localhost-security.md`, `docs/phase7-results.md` and
`results/phase7-dashboard-smoke.md` (evidence), and the code cited inline. Commits: read API
`3c525e5`; HTTP API and `admin serve` `9cb1405`; UI `0b42f07`. The evidence run used binaries
from clean `0b42f07` (`docs/phase7-results.md:3`, `:9-10`). The decision record 0006 and the
evidence docs are committed with this section (Phase 7 step 7d).

### 30-second version

"Phase 6 gave operators replay and undo as a CLI. Phase 7 puts a small dashboard on top: lists of
failed and dead events, a customer timeline, an event log, and buttons for preview, replay and
undo. It is a Go HTTP API (`admin serve`) plus a vanilla-JS page embedded in the binary. It calls
the same audited admin functions as the CLI, so there is no second implementation of replay or undo.

The interesting part is security. There is no login, so the threat is not the network, it is the
operator's own browser: any web page can make it send requests to localhost, and DNS rebinding can
make a hostile page look same-origin. So the server binds only 127.0.0.1, checks the Host header,
uses Go's cross-origin protection, accepts only JSON POSTs, caps bodies, and sets CSP and nosniff.
The bind, Host, cross-origin, content-type and body-limit layers each have a handler-level test that
pairs refused requests with an allowed control; the detached-context behaviour has no test. The evidence is one scripted run
on five synthetic fixture events, not a load or security test, and `actor` is self-declared."

### Evidence (from `docs/phase7-results.md`; one run, synthetic fixtures)

- Binaries from clean `0b42f07`, `vcs.modified=false` (`phase7-results.md:9-10`).
- Preview of the dead event: HTTP 200, outcome "skip", zero change_log and admin_actions rows (`:32`).
- Replay of a done event: HTTP 409 `not_replayable`, audited as action #1 (`:33`).
- Undo of #5 worked; a second undo was 409 `not_undoable`, audited (`:34-35`). Undo of #1 was 409
  `conflict` with 3 conflicts, audited, state unchanged (`:36`).
- Poison event reached dead in 11.7971117 s with `WORKER_LEASE=1s`, one run. With the default 10 s
  lease it would be much longer and that was not measured (`:21-25`).
- Limitations stated there: one run, 5 inbox rows, one operator; the worker was stopped before
  `admin serve`; "not a load, concurrency or security test" (`:47-50`). Screenshots show fixture data (`:41`).

### Likely interview questions

1. **It only listens on localhost, so why all the web security?** (0006:6-12)
   Localhost stops other machines, not other tabs. Any site can make the browser send a request to
   `127.0.0.1` (CSRF), and a hostile name that resolves to 127.0.0.1 makes the attacker's page
   same-origin (DNS rebinding). So the layers are: literal `127.0.0.1:<port>` bind, Host allow-list,
   `CrossOriginProtection`, JSON-only POST, 16 KiB body cap with strict decoding, CSP and nosniff
   (0006:33-59). Tests are handler-level with a control request each, not a browser experiment (0006:72-75, :78-80).

2. **Why check Host if you already check Origin?** (plan :52-53; 0006:29-30, :40-42)
   After a rebind the attacker's page is same-origin, so Origin or Sec-Fetch-Site looks fine. The
   Host header still carries the attacker's hostname, so an allow-list of `127.0.0.1:<port>` and
   `localhost:<port>` refuses it, for GETs too. `TestDBWrongHost403` covers a wrong name, wrong port,
   no port, empty and a suffix trick, with allowed controls (`handlers_db_test.go:260`).

3. **Why is preview a POST if it changes nothing?** (Q6, plan :57, :148)
   It takes row locks (it runs the real apply and rolls back), and browsers can prefetch GETs. A
   prefetch must not take locks on production rows. Being a POST also puts it behind the same
   cross-origin and content-type layers. It is not audited (plan :57).

4. **Why keyset paging and not OFFSET?** (`internal/admin/list.go:4-6`, `:91-94`, `:174`)
   The worker moves rows between statuses while someone pages. With OFFSET the pages shift under the
   reader. Keyset uses `id < $1` with the last id as cursor, so a page boundary is stable. Each list
   is one statement, so one page is one snapshot. Cost: ordering is by id only, and no expression
   index was added for the timeline (Q3, plan :145); it is only to be added if measured slow.

5. **What is the "one snapshot" fix in `admin.Log`?** (`internal/admin/log.go:50-54`, plan Q5 :147)
   `Log` makes three reads: inbox row, change log, audit actions. Between them a replay or worker
   commit could show a status that disagrees with the rows beside it. They now run in one
   REPEATABLE READ READ ONLY transaction. `TestDBLogIsOneSnapshot` commits a real replay between the
   reads and expects the old snapshot (`list_db_test.go:359-383`).

6. **What do errors look like to the client, and why DTOs?** (`dto.go:3-8`; plan :110-114)
   Stable codes such as `not_replayable`, `not_undoable`, `conflict`, `no_route`. 409 changed and 503
   busy send fixed texts; the wrapped Postgres or apply error is only logged, like 500 (plan :110-111;
   `TestFailHidesWrappedDetail`, `errors_test.go:51`). The admin result structs have no JSON tags, so
   encoding them directly would give Go field names, base64 images and double-encoded text. DTOs fix
   snake_case keys in one place; `TestDBWireKeysOnRealResponses` checks keys on real responses
   (`handlers_db_test.go:511`), so the wire format is a tested contract.

7. **What happens if the browser disconnects mid-undo?** (`handlers.go:15-20`; plan :58-60)
   The admin call runs on `context.WithoutCancel(r.Context())` plus a 30 s timeout, so a closed tab
   cannot cancel a replay or undo half way. The UI re-reads the event log after an error (plan :59-60).
   Admit it: 0006 says no test exercises a client disconnect (0006:67-69).

8. **How does the UI avoid XSS from payloads?** (0006:64-66; `static_test.go:94`, `:107`)
   Payloads, actor, reason and errors are rendered with `textContent` through one `el()` helper that
   has an attribute allow-list and throws on anything else. `TestUISourceHasNoMarkupOrCodeSinks`
   scans the source for banned sinks and `TestElAttributeAllowList` pins the helper. CSP is the
   second line of defence.

9. **Why no auth, and why no SPA framework?** (0006:15-17, :23-26, :76-77)
   For one operator on one machine a token adds secret handling without stopping a local process
   that can already read it (author's reasoning in 0006, not in the plan). Authentication is out of
   scope in the plan (:63-64). A framework adds a build step for six views (also author's reasoning);
   the plan chose embedded plain HTML/JS (plan :25). Before exposing it beyond localhost: per-user
   authentication, TLS, a configured Host list, a CSRF/CORS re-review, a rate limit, and an
   append-only audit table (author's reasoning, 0006:84-87).

10. **How do you avoid showing an audit id that was never committed?** (`internal/admin/replay.go:115-119`, `:178-192`; `reportable.go:5-18`)
   The audit insert's `RETURNING id` fills `ActionID` before the commit, so the id exists in memory
   even if the commit then fails. `Outcome` is set only after the commit succeeds (replay.go:178-179,
   :192). Only success and the audited refusals (`ErrNotReplayable`; `ErrNotUndoable` and `ErrConflict`
   for undo) count as reportable. `ReplayReportable` and `UndoReportable` are shared by the CLI and the
   HTTP API (`cmd/admin/main.go:167`, `:178`; `internal/adminhttp/handlers.go:206`, `:226`). For any
   other error the handler drops the result body (`errors.go:117-119`), so the UI never sees an
   `action_id`. Tests: `TestDBBusy503` and `TestDBInternalErrorNotLeaked` assert that the error response
   carries no result (`handlers_db_test.go:430`, `:474`). After a commit error the commit may still
   have succeeded, so the operator checks `admin log <id>` (replay.go:118-119).

### Tradeoffs and limitations to admit openly

- No authentication: anyone with a shell or browser on this machine can read everything and replay
  or undo. `actor` is self-declared, so the audit is only as honest as what was typed (0006:76-77).
- No penetration test and no browser-based rebinding experiment; the security tests are
  handler-level (0006:78-80). The evidence is one run on 5 synthetic events, no read or action ran
  during worker activity (`phase7-results.md:49-50`).
- The detached-context behaviour has no test (0006:67-69).
- Bind is stricter than planned: `::1` and other 127.x addresses are refused, which may surprise an
  IPv6-only setup (0006:81-83, plan :107-109).
- A source scan for banned sinks is a heuristic, not a proof (author's reasoning); CSP is the backstop (0006:66).
- The console showed a 404 whose URL was not recorded; a missing favicon is a plausible but
  unverified cause (`phase7-results.md:44-45`).
- Before exposing it beyond localhost (author's reasoning, 0006:84-87): real per-user authentication,
  TLS, a Host list from configuration, a CSRF/CORS re-review, a rate limit, an audit table the writing
  role cannot alter.

### "Tell me about a bug you found"

"I automated a click-through of the UI with headless Edge, driving it over the DevTools protocol.
It found a real bug. After a successful undo the notice said the event status was 'now' the old
value, for example 'now done'. The cause was a semantics mismatch: the API's `UndoResult.Status` is
the inbox status before the undo (`internal/admin/undo.go:73`), and the UI printed it as the current
status. The fix prints 'was <status>, generation N; event status now undone', matching the CLI's
`FormatUndo` (`internal/adminhttp/static/app.js:316-321`, comment at :318). Lesson: a field name
that is true in one layer can mislead in another, and rendering real responses in a real browser
catches that where unit tests did not." Note for honesty: the click-through notes were in a scratch
area and are not evidence; the 7c commit message (`0b42f07`) records "29 checks, not evidence". The committed trace is that commit message,
the code comment and the fixed text.

### What I'd do next

- Real authentication with per-user identities, then an audit table the writing role cannot change
  (0006:84-87).
- A test for client disconnect during undo, and a browser-based rebinding check.
- Measure the timeline query before adding an index (Q3).
- Run the dashboard against live workers, since the evidence run stopped the worker first
  (`phase7-results.md:49-50`).

---

## Phase 8: CI that re-checks the claims

Sources: `docs/phase8-results.md` (8 CI runs; read with `gh` on 2026-11-20, `:3`),
`docs/decisions/0007-ci.md`, `docs/phase8-plan.md`, `.github/workflows/ci.yml`,
`scripts/ci-chaos-gate.sh`, `scripts/ci-chaos-gate-test.sh`, `internal/sim/check_db_test.go`.
Commits: 8a `0ca60ff`, 8b `b20348a`, CheckState test `446218e`. Commit-message claims are labelled as
such. The numbers below are CI numbers: a regression signal, not evidence.

### 30-second version

"Phases 1-7 produced evidence that I ran by hand on my machine. Phase 8 puts a version of those
checks in GitHub Actions so a later commit cannot quietly break them. The `test` job runs every Go
test with the Postgres tests switched on. The `chaos` job runs the crash and poison experiments
against RelayGuard, which must come out clean, and the same experiment against the deliberately
naive receiver, which must still report violations.

The point of the work was making sure green means something. The old workflow was green while,
as far as I can tell, every database test was skipped. So there is a guard for skips, a gate script
that reads the chaos RESULT line instead of trusting the exit code, and a negative control. Each guard
was shown to go red on a throwaway branch. That is how I found that nothing tested the checker itself.
Caveats: CI is a regression signal, not evidence: it runs on different hardware and a different Go
version from my local evidence, a green chaos run is one sample, and the naive run does not carry the `changelog_*` checks that the RelayGuard
runs do, so those two are not like-for-like."

### Evidence (from `docs/phase8-results.md`; CI runs, a regression signal, not evidence)

- Before Phase 8: run 44654164320 on `192baae` was green. `go test` ran without `-v` and without a
  database, so its log shows no skip lines. The skip is inferred from the workflow and from
  `internal/dbtest/dbtest.go:74` (`results:65-71`).
- 8a green, `0ca60ff`: 129 top-level `TestDB*` pass, 0 FAIL, 0 SKIP (`results:103`).
- Red proof 8a-skip: with `RELAYGUARD_DB_TESTS=1` removed, the "Go tests" step stayed green with 121
  top-level tests skipped (149 `--- SKIP` lines counting subtests) and only the "Skip guard" step went
  red (`results:167-172`).
- Red proof 8a-db: with `CrossOriginProtection` removed, `TestDBCrossSitePost403` failed, 128 pass and
  1 FAIL (`results:124-138`).
- 8b green, `b20348a` (`results:202-207`): R1 `RESULT target=relayguard seeds=3 failing_seeds=0
  violations=0 restarts=35 hard_kills=4`; R2 `seeds=1 ... violations=0`; N1 `RESULT target=naive
  seeds=3 failing_seeds=3 violations=60`; all three gates PASS. (N1 vs R1 is not like-for-like: only the
  RelayGuard runs carry `changelog_*` checks, `results:17-18`.)
- Red proof 8b-app (doubled credit): R1 `failing_seeds=3 violations=144`, GATE FAIL; the test job also
  went red with 31 failing top-level `TestDB*` (`results:246`, `:261-262`).
- Red proof 8b-checker (`CheckState` returns `nil, 0, nil`): test job green with 129 pass, R1 and R2
  green, N1 `violations=0` and `GATE FAIL (violations): chaos exit code was 0, expected 1`
  (`results:284`, `:289-294`).
- After the fix, `446218e`: 130 `TestDB*` pass, both jobs green (`results:313`). N1 gave
  `violations=62` there, versus 60 earlier (`results:327`, `:336-337`).
- Largest job was 69 s against a 1200 s timeout cap (`results:342`).
- Stated limits: runner `ubuntu-24.04`, Go 1.26.0 from `go.mod`, local runs used go1.27.4; kill timing is
  wall-clock; counts go in no results table (`results:11-15`). The branches are deleted and the runs
  are only in Actions history (`results:19-20`).

### Likely interview questions

1. **Your CI was green before. What did Phase 8 change?** (`results:65-71`; 0007:10-13)
   The workflow ran `go test ./...` with no database and no `RELAYGUARD_DB_TESTS`, so every `TestDB*`
   skipped itself (`dbtest.go:73-75`). I should be careful: that is inferred, because that run had no
   `-v` and its log has no skip lines. Now the `test` job has a Postgres 17 service, sets the flag, runs
   `-v`, and a guard step fails if the skip message appears or no `--- PASS: TestDB` line does
   (`ci.yml:61-77`).

2. **Why a guard script for skips? Doesn't a skipped test show up?** (`results:168-172`, `:341`)
   It looks green. In red proof 8a-skip the test step concluded success with 121 skipped top-level tests. Also, 8
   top-level `TestDB*` showed PASS in that run: they are table-driven parents whose subtests all
   skipped, and Go reports such a parent as PASS. So the guard greps for the skip message and not only
   for a PASS line. Lesson: count what ran, not what did not fail.

3. **Why not just fail CI on the chaos exit code?** (`ci-chaos-gate.sh:5-8`; 0007:14-16, :20)
   `chaos` exits 1 both for "violations found" and for `log.Fatal` (bad flag, missing secret, DB down).
   For the naive control exit 1 is the expected result, so exit 1 alone cannot tell "the checker saw
   the flaws" from "nothing ran". The gate also requires exactly one `RESULT target=<t>` line, the
   expected `seeds=`, and `failing_seeds=0 violations=0` (or `violations >= 1` for naive)
   (`ci-chaos-gate.sh:10-14`, `:43-46`, `:67-75`). I rejected adding an `-expect` flag to `chaos`
   because it would change the exit-code meaning earlier results docs rely on (0007:21-22).

4. **How do you know the gate itself works?** (`ci-chaos-gate-test.sh:2-4`, `:27`; `ci.yml:120-122`)
   It has a self-test run as the chaos job's first step, on canned logs in `scripts/testdata/ci-gate/`.
   Those logs are SYNTHETIC, and the script checks each starts with a `SYNTHETIC` line. Every failure
   case is asserted too, because a gate that cannot fail proves nothing. One case, a repeated
   `violations=` field where the last value would win, is rejected too: the gate fails on duplicate
   keys (`ci-chaos-gate.sh:49-60`; case at `ci-chaos-gate-test.sh:44`; also stated in the b20348a
   commit message).

5. **Why run the naive receiver in CI at all?** (0007:38-39; `ci.yml:168-170`)
   It is a negative control. If the checker were broken and always reported 0 violations, R1 and R2
   would stay green. N1 must report violations, so a blind checker turns it red. `cmd/naive` is unchanged.

6. **What did the red proofs show?** (0007:40-42; `results:19-20`)
   Each guard was made to fail on a throwaway `ci-red/*` branch: 8a-db and 8a-skip for the test job,
   8b-app and 8b-checker for the chaos job. They were pushed, then deleted with my OK, and never merged.
   Only the red run shows a guard can fail; a green run does not. The mutated lines are quoted in the
   results doc because the commits are unreachable and may be garbage-collected.

7. **Why can't you compare the CI numbers to your local results?** (`results:11-15`, `:336-337`)
   Different OS and Go version (GitHub `ubuntu-24.04` and go1.26.0, versus Windows and go1.27.4 locally),
   and the supervisor's kill timing is wall-clock. Across the three runs where R1 was green, restarts
   were 35, 32, 32 and hard_kills 4, 2, 2. So CI only asks "still clean, and the checker still sees the
   naive flaws". Also, N1 versus R1 is not like-for-like: the RelayGuard runs carry extra `changelog_*`
   checks and the naive run does not (`results:17-18`, `:222-223`).

8. **Is a green chaos run proof that there are no bugs?** (`results:16`; 0007:54-55)
   No. It is one sample of a randomized, timing-dependent run, with 3 crash seeds and 1 poison seed
   (0007:35-37). Failures are investigated and recorded, not retried until green (0007:27-29).

### Tradeoffs and limitations to admit openly

- CI numbers are a regression signal, not evidence, and not comparable to local runs (`results:11-15`).
- No `-race` in CI (plan Q7, `phase8-plan.md:147`), no Dependabot, so the SHA-pinned actions go stale
  until I bump them by hand (plan Q6, `:147`; 0007:58).
- Undo-all property runs (E4 from Phase 6) are not in CI; only crash seeds 1-3, poison seed 1 and
  naive seeds 1-3 are (plan Q4, `:146`; 0007:58-59).
- `TestDBCheckStateRules` was not re-run as a CI red proof. Reading the test, it calls the exported
  `CheckState` (`check_db_test.go:127`), so the 8b-checker break should fail it. The `446218e` commit
  message says six checker mutations each fail it; those were local runs, not recorded in a results
  doc (`results:331-332`; 0007:47-52).
- Linux-only CI: a failure there would be a portability finding, not noise (0007:56-57).
- The "before" skip is inferred, not displayed in its log (`results:68-71`).
- The self-test uses synthetic logs, so it shows the gate logic works, not that real chaos output
  always has the shape the gate expects. The gate did parse the real RESULT lines in the 4 runs that
  had a chaos job (`results` sections 5-8), but that is a sample, not a guarantee.

### "Tell me about a bug you found"

"Phase 8 had a gap, found by deliberately breaking things. To check my chaos gate I made a throwaway
branch where `sim.CheckState`, the model check every chaos run ends with, returns `nil, 0, nil`. The
`test` job stayed green with 129 database tests passing, and R1 and R2 stayed green, because a checker
that reports nothing looks exactly like a correct system. Only the naive negative control went red: it
reported `violations=0` and the gate failed with 'chaos exit code was 0, expected 1'
(`results:284`, `:289-294`). The cause was that no Go test called `CheckState`, so the thing I relied on to
check correctness was itself untested (header comment, `check_db_test.go:3-10`). I added
`TestDBCheckStateRules`: each case changes either the stored rows or the expected state, asserts the
exact violations, and sits next to a clean positive control, so both a silent checker and an
everything-is-wrong checker fail (`check_db_test.go:6-10`). Honest status: I did not push a new red
proof to confirm CI now fails under that mutation; I checked it by reading that the test calls the
exported function; the `446218e` commit message also reports six local checker mutations failing it,
which is not recorded in a results doc (`results:331-332`).
Lesson: the negative control was worth its cost in that one run, and a check that cannot fail is the
first thing to test."

### What I'd do next

- Push a red proof for `TestDBCheckStateRules` so the fix is shown in CI, not only argued.
- Add Dependabot or a scheduled bump for the SHA pins (0007:58).
- Consider `-race` and the undo-all property run if job time allows (0007:58-59).
- Keep the CI-versus-local caveat in any future table; do not mix the numbers.
