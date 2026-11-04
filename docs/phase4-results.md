# Phase 4 results: chaos harness (delivery faults)

Synthetic histories generated from seeds by `cmd/chaos`, local machine, run on: YYYY-MM-DD.
Each run: 20 customers, invoices and subscription changes, compared against a reference model.
Worker crashes are not injected yet (part 2).

| Scenario | Flags | Seeds | RelayGuard violations | Naive violations |
|---|---|---|---|---|
| No faults (harness sanity check) | none | 1-5 | | |
| Duplicates | `-dup 0.2` | 1-5 | | |
| Reordering | `-reorder 10` | 1-5 | | |
| Both | `-dup 0.2 -reorder 10` | 1-5 | | |

## Notes

- "No faults" should give 0 for BOTH targets. If the naive receiver showed violations without
  faults, the harness or the model would be wrong.
- Violations are counted per customer or subscription that ends up wrong, not per event.

## Sample output

```text

RESULT target=naive seed=1 dup=0.20 reorder=10 violations=19 elapsed=165ms
RESULT target=naive seed=2 dup=0.20 reorder=10 violations=13 elapsed=158ms
RESULT target=naive seed=3 dup=0.20 reorder=10 violations=12 elapsed=149ms
RESULT target=naive seed=4 dup=0.20 reorder=10 violations=11 elapsed=146ms
RESULT target=naive seed=5 dup=0.20 reorder=10 violations=7 elapsed=136ms

```
