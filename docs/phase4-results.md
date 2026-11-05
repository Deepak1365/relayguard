# Phase 4 results: chaos harness

Synthetic histories generated from seeds by `cmd/chaos`, local machine, run on: 2026-11-04.
Each seed: 20 customers with invoices and subscription changes; the database is compared
against a reference model computed from the true history. These are not production results.

## Part 2: worker crashes (20 seeds each)

The harness ran the worker (or the naive receiver) itself, crashed it at fault points with
probability 0.1, and hard-killed it at random moments (`-kill-interval 2s`).

| Scenario | RelayGuard | Naive |
|---|---|---|
| Duplicates + reordering + crashes (`-dup 0.2 -reorder 10`) | **0 violations**, 0/20 seeds failing, 230 restarts (8 hard kills) | **406 violations**, 20/20 seeds failing, 283 restarts (15 hard kills) |
| Crashes only | **0 violations**, 0/20 seeds failing, 246 restarts (9 hard kills) | **226 violations**, 20/20 seeds failing, 230 restarts (9 hard kills) |

Full per-seed tables, each with its git commit and exact command:

- [results/relayguard-crash.md](../results/relayguard-crash.md)
- [results/naive-crash.md](../results/naive-crash.md)
- [results/relayguard-crash-only.md](../results/relayguard-crash-only.md)
- [results/naive-crash-only.md](../results/naive-crash-only.md)

The supervised processes' log (not committed) showed crashes at every fault point: worker
`after_grant` and `before_commit`, naive `after_grant` and `before_response`. It showed no
worker errors and no dead-lettered events.

## Part 1: delivery faults only, no crashes (seeds 1-5)

| Scenario | Flags | RelayGuard violations | Naive violations |
|---|---|---|---|
| No faults (harness sanity check) | none | 0 (0/5 seeds failing) | 0 (0/5 seeds failing) |
| Duplicates | `-dup 0.2` | 0 (0/5) | 48 (5/5 seeds failing; per seed: 16, 6, 12, 10, 4) |
| Reordering | `-reorder 10` | 0 (0/5) | 8 (3/5 seeds failing; per seed: 3, 2, 0, 0, 3) |
| Both | `-dup 0.2 -reorder 10` | not run separately; see part 2 (0 with crashes added) | 62 (5/5 seeds failing; per seed: 19, 13, 12, 11, 7) |

"No faults" gave 0 for both targets, as it must: the harness does not invent failures.
Reordering alone hurt the naive receiver less often than duplicates (two seeds came out correct),
because only some reorderings land an event before its prerequisite or an older status after a newer one.

## Notes and limitations

- Violations count customers or subscriptions that end up wrong, not individual events.
- Only the worker was crashed on the RelayGuard side; the receiver was not fault-injected,
  which is why RelayGuard shows no provider retries.
- Naive's "gave up" deliveries are not all lost events: many are resent `customer.created`
  events that had already been applied before a crash but could not be acknowledged
  (duplicate-key errors). The violation count is the real measure.
- Event timestamps are generated one second apart, so same-second ordering
  (a documented limitation) is not exercised.
