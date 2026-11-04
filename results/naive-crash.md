# Chaos run: naive

- Date: 2026-11-04 18:42
- Commit: `2e1663a`
- Command: `chaos -target naive -seeds 1-20 -dup 0.2 -reorder 10 -supervise bin\naive.exe -crash-rate 0.1 -kill-interval 2s -out results\naive-crash.md`
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 20, failing seeds: 20, violations: 406, process restarts: 283 (of which hard kills: 15)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |
|---|---|---|---|---|---|---|---|
| 1 | 84 | 99 | 18 | 3 | - | 25 | ledger_mismatch=3 wrong_balance=10 wrong_grant_count=10 wrong_subscription_status=2 |
| 2 | 83 | 97 | 24 | 3 | - | 27 | ledger_mismatch=6 wrong_balance=9 wrong_grant_count=10 wrong_subscription_status=2 |
| 3 | 80 | 95 | 23 | 5 | - | 18 | ledger_mismatch=3 wrong_balance=6 wrong_grant_count=8 wrong_subscription_status=1 |
| 4 | 78 | 93 | 27 | 6 | - | 16 | ledger_mismatch=2 wrong_balance=6 wrong_grant_count=6 wrong_subscription_status=2 |
| 5 | 78 | 92 | 31 | 6 | - | 11 | ledger_mismatch=3 missing_subscription=1 wrong_balance=3 wrong_grant_count=3 wrong_subscription_status=1 |
| 6 | 79 | 91 | 19 | 4 | - | 23 | ledger_mismatch=4 wrong_balance=9 wrong_grant_count=10 |
| 7 | 70 | 89 | 43 | 9 | - | 20 | ledger_mismatch=2 wrong_balance=9 wrong_grant_count=9 |
| 8 | 71 | 88 | 27 | 6 | - | 17 | ledger_mismatch=3 wrong_balance=6 wrong_grant_count=6 wrong_subscription_status=2 |
| 9 | 81 | 97 | 45 | 9 | - | 23 | ledger_mismatch=4 wrong_balance=7 wrong_grant_count=9 wrong_subscription_status=3 |
| 10 | 89 | 109 | 42 | 10 | - | 22 | ledger_mismatch=3 wrong_balance=9 wrong_grant_count=10 |
| 11 | 80 | 96 | 25 | 4 | - | 18 | ledger_mismatch=2 wrong_balance=7 wrong_grant_count=7 wrong_subscription_status=2 |
| 12 | 85 | 105 | 29 | 6 | - | 28 | ledger_mismatch=4 wrong_balance=12 wrong_grant_count=11 wrong_subscription_status=1 |
| 13 | 80 | 95 | 29 | 6 | - | 20 | ledger_mismatch=4 wrong_balance=6 wrong_grant_count=7 wrong_subscription_status=3 |
| 14 | 83 | 94 | 28 | 7 | - | 11 | ledger_mismatch=4 wrong_balance=3 wrong_grant_count=4 |
| 15 | 89 | 104 | 12 | 2 | - | 18 | ledger_mismatch=1 wrong_balance=9 wrong_grant_count=7 wrong_subscription_status=1 |
| 16 | 63 | 73 | 26 | 5 | - | 20 | ledger_mismatch=5 wrong_balance=6 wrong_grant_count=9 |
| 17 | 70 | 88 | 34 | 9 | - | 16 | ledger_mismatch=2 wrong_balance=6 wrong_grant_count=6 wrong_subscription_status=2 |
| 18 | 91 | 106 | 41 | 8 | - | 24 | ledger_mismatch=4 wrong_balance=8 wrong_grant_count=11 wrong_subscription_status=1 |
| 19 | 68 | 86 | 30 | 7 | - | 21 | ledger_mismatch=3 wrong_balance=8 wrong_grant_count=8 wrong_subscription_status=2 |
| 20 | 90 | 108 | 35 | 6 | - | 28 | ledger_mismatch=5 wrong_balance=10 wrong_grant_count=11 wrong_subscription_status=2 |
