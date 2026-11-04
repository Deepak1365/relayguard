# Baseline failures: naive receiver

Synthetic fixtures (`testdata/events/`), local machine, run on: YYYY-MM-DD.
Paste the actual `.\dev.ps1 state` output under each experiment.

| # | Scenario | Correct outcome | Observed (naive) |
|---|---|---|---|
| 1 | invoice.paid delivered twice | balance 20, 1 grant, ledger 20 | |
| 2 | crash between writes, then provider retry | balance 20, 1 grant, ledger 20 | |
| 3 | invoice.paid arrives before customer.created | balance 20 | |
| 4 | older "active" event arrives after "canceled" | status canceled | |

## 1. Duplicate delivery
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |             40 |      2 |           40
(1 row)

 stripe_subscription_id | status | updated_at
------------------------+--------+------------
(0 rows)

## 2. Crash mid-processing
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |             20 |      2 |           40
(1 row)

 stripe_subscription_id | status | updated_at
------------------------+--------+------------
(0 rows)

## 3. Out-of-order: missing prerequisite
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 stripe_subscription_id | status | updated_at
------------------------+--------+------------
(0 rows)

## 4. Out-of-order: stale overwrite
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 stripe_subscription_id | status |          updated_at
------------------------+--------+-------------------------------
 sub_fx_1               | active | 2026-11-04 16:35:05.241843+00
(1 row)
