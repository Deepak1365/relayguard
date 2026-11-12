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
