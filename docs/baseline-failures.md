# Baseline failures: naive receiver

Synthetic fixtures (`testdata/events/`), local machine, run on: 2026-11-04.
Each section shows the actual `.\dev.ps1 state` output after the experiment.

| # | Scenario | Correct outcome | Observed (naive) |
|---|---|---|---|
| 1 | invoice.paid delivered twice | balance 20, 1 grant, ledger 20 | balance 40, 2 grants, ledger 40 |
| 2 | crash between writes, then provider retry | balance 20, 1 grant, ledger 20 | balance 20, 2 grants, ledger 40 (inconsistent) |
| 3 | invoice.paid arrives before customer.created | balance 20 | balance 0; event acknowledged and lost |
| 4 | older "active" event arrives after "canceled" | status canceled | status active |

## 1. Duplicate delivery

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |             40 |      2 |           40
(1 row)
```

## 2. Crash mid-processing

After the crash and the provider's retry:

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |             20 |      2 |           40
(1 row)
```

## 3. Out-of-order: missing prerequisite

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)
```

## 4. Out-of-order: stale overwrite

```text
 stripe_customer_id | credit_balance | grants | ledger_total
--------------------+----------------+--------+--------------
 cus_fx_1           |              0 |      0 |            0
(1 row)

 stripe_subscription_id | status |          updated_at
------------------------+--------+-------------------------------
 sub_fx_1               | active | 2026-11-04 16:35:05.241843+00
(1 row)
```
