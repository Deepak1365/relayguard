# Phase 2 results: inbox + single-transaction worker

Synthetic fixtures (`testdata/events/`), signed with the local webhook secret, local machine, run on: 2026-11-04.
Naive results are from [baseline-failures.md](baseline-failures.md).

| # | Scenario | Correct outcome | Naive | RelayGuard (phase 2) |
|---|---|---|---|---|
| 1 | invoice.paid delivered twice | balance 20, 1 grant, ledger 20 | balance 40, 2 grants, ledger 40 | balance 20, 1 grant, ledger 20 ✅ |
| 2 | worker crash mid-transaction | balance 20, 1 grant, ledger 20 | balance 20, 2 grants, ledger 40 | rolled back, then balance 20, 1 grant, ledger 20 after restart ✅ |
| 3 | invoice.paid arrives before customer.created | balance 20 | balance 0; event lost | balance 20 after retry ✅ |
| 4 | older "active" event arrives after "canceled" | status canceled | status active | status active ❌ (ordering fixed in phase 3) |

## 1. Duplicate delivery

The second delivery was rejected at the inbox by the unique key on `(provider, event_id)`.

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |             20 |      1 |           20
(1 row)

 id |     event_id      |    event_type    | status | attempts | last_error
----+-------------------+------------------+--------+----------+------------
  1 | evt_fx_customer_1 | customer.created | done   |        1 |
  2 | evt_fx_invoice_1  | invoice.paid     | done   |        1 |
(2 rows)
```

## 2. Crash mid-transaction

Right after the crash: the half-finished grant was rolled back, and the invoice is still pending.

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 id |     event_id      |    event_type    | status  | attempts | last_error
----+-------------------+------------------+---------+----------+------------
  1 | evt_fx_customer_1 | customer.created | done    |        1 |
  2 | evt_fx_invoice_1  | invoice.paid     | pending |        0 |
(2 rows)
```

After restarting the worker (no resend from the provider was needed):

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |             20 |      1 |           20
(1 row)

 id |     event_id      |    event_type    | status | attempts | last_error
----+-------------------+------------------+--------+----------+------------
  1 | evt_fx_customer_1 | customer.created | done   |        1 |
  2 | evt_fx_invoice_1  | invoice.paid     | done   |        1 |
(2 rows)
```

Worker log:

```text
12:47:01 applied evt_fx_customer_1 (customer.created)
12:47:01 FAULT: crashing mid-transaction while applying evt_fx_invoice_1
12:48:28 RelayGuard worker started
12:48:28 applied evt_fx_invoice_1 (invoice.paid)
```

## 3. Missing prerequisite

The invoice failed with "unknown customer", was retried with backoff, and applied once the customer existed.

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |             20 |      1 |           20
(1 row)
```

Worker log and inbox:

```text

Inbox (the invoice needed 2 attempts):

 id |     event_id      |    event_type    | status | attempts | last_error
----+-------------------+------------------+--------+----------+------------
  1 | evt_fx_invoice_1  | invoice.paid     | done   |        2 |
  2 | evt_fx_customer_1 | customer.created | done   |        1 |
(2 rows)

```

## 4. Stale overwrite (still failing)

Both events succeed, so retries don't help; they are applied in arrival order. Fixed in phase 3.

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |             20 |      1 |           20
(1 row)

 stripe_subscription_id | status |          updated_at
------------------------+--------+------------------------------
 sub_fx_1               | active | 2026-11-04 18:16:45.83667+00
(1 row)
```

## Known limitations

- **Poison events:** if an event crashes the worker process (instead of returning an error), the rollback also undoes the `attempts` increment, so the event is retried forever and never reaches the dead-letter limit. Experiment 2 shows this: the invoice ended with `attempts = 1` despite two tries.
- **Ordering:** events that each succeed are applied in arrival order (experiment 4).
- These are synthetic results from hand-written fixtures on one machine, not production traffic.
