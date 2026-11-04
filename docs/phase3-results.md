# Phase 3 results: ordering for state-overwriting events

Synthetic fixtures (`testdata/events/`), signed with the local webhook secret, local machine, run on: YYYY-MM-DD.
Start each experiment with `.\dev.ps1 reset-data`.

| # | Scenario | Correct outcome | Naive | Phase 2 | Phase 3 |
|---|---|---|---|---|---|
| 4 | older "active" arrives after "canceled" | status canceled | status active | status active | |
| 5 | in-order: "active" then "canceled" (regression check) | status canceled | | | |
| 1-3 | rerun earlier experiments (regression check) | as before | | ✅ | |

## 4. Stale overwrite

```text
paste .\dev.ps1 state and .\dev.ps1 inbox output, plus the worker's "stale" log line
```

## 5. In-order delivery still applies

```text
paste output
```

## Known limitations

- Ordering uses Stripe's `created` timestamp, which has 1-second resolution: two events for the
  same subscription in the same second cannot be ordered, and the later-processed one wins.
  A stronger alternative is to re-fetch the object's current state from Stripe's API when the event
  arrives, at the cost of an API call per event and a dependency on Stripe being reachable.
