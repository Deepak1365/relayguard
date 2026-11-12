# Phase 3 results: ordering for state-overwriting events

Synthetic fixtures (`testdata/events/`), signed with the local webhook secret, local machine, run on: 2026-11-04.
Start each experiment with `.\dev.ps1 reset-data`.

| # | Scenario | Correct outcome | Naive | Phase 2 | Phase 3 |
|---|---|---|---|---|---|
| 4 | older "active" arrives after "canceled" | status canceled | status active | status active | status canceled ✅ (older event marked `stale`; output in section 4) |
| 5 | in-order: "active" then "canceled" (regression check) | status canceled | not recorded | not recorded | status canceled ✅ (output in section 5) |
| 1-3 | rerun earlier experiments (regression check) | as before | see [phase 2](phase2-results.md) | ✅ | not recorded (no output was pasted) |

Provenance (added 2026-11-11 during an audit): the Phase 3 cells above were empty until then and were
filled only from the outputs pasted below. Cells with no pasted output are marked "not recorded".
The exact commands and the commit each run used were not recorded at the time. This doc was
first committed in `9c24065`.

## 4. Stale overwrite

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 stripe_subscription_id |  status  |    last_event_id    | last_event_created
------------------------+----------+---------------------+--------------------
 sub_fx_1               | canceled | evt_fx_sub_canceled |         1793800300
(1 row)

PS C:\dev\relayguard> .\dev.ps1 inbox
 id |      event_id       |          event_type           | status | attempts |                               last_error
----+---------------------+-------------------------------+--------+----------+------------------------------------------------------------------------
  1 | evt_fx_customer_1   | customer.created              | done   |        1 |
  2 | evt_fx_sub_canceled | customer.subscription.deleted | done   |        1 |
  3 | evt_fx_sub_active   | customer.subscription.updated | stale  |        1 | stale event: sub_fx_1 already reflects evt_fx_sub_canceled (created 17
(3 rows)
```

## 5. In-order delivery still applies

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 stripe_subscription_id |  status  |    last_event_id    | last_event_created
------------------------+----------+---------------------+--------------------
 sub_fx_1               | canceled | evt_fx_sub_canceled |         1793800300
(1 row)

```

## Known limitations

- Ordering uses Stripe's `created` timestamp, which has 1-second resolution: two events for the
  same subscription in the same second cannot be ordered, and the later-processed one wins.
  A stronger alternative is to re-fetch the object's current state from Stripe's API when the event
  arrives, at the cost of an API call per event and a dependency on Stripe being reachable.
