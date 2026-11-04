# Chaos run: naive

- Date: 2026-11-04 18:45
- Commit: `2e1663a`
- Command: `chaos -target naive -seeds 1-20 -supervise bin\naive.exe -crash-rate 0.1 -kill-interval 2s -out results\naive-crash-only.md`
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 20, failing seeds: 20, violations: 226, process restarts: 230 (of which hard kills: 9)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |
|---|---|---|---|---|---|---|---|
| 1 | 84 | 84 | 12 | 1 | - | 10 | ledger_mismatch=3 wrong_balance=2 wrong_grant_count=5 |
| 2 | 83 | 83 | 14 | 1 | - | 9 | ledger_mismatch=3 wrong_balance=2 wrong_grant_count=4 |
| 3 | 80 | 80 | 18 | 4 | - | 9 | ledger_mismatch=3 wrong_balance=2 wrong_grant_count=4 |
| 4 | 78 | 78 | 12 | 1 | - | 12 | ledger_mismatch=4 wrong_balance=3 wrong_grant_count=5 |
| 5 | 78 | 78 | 13 | 2 | - | 9 | ledger_mismatch=2 wrong_balance=1 wrong_grant_count=3 wrong_subscription_status=3 |
| 6 | 79 | 79 | 11 | 0 | - | 13 | ledger_mismatch=2 wrong_balance=4 wrong_grant_count=5 wrong_subscription_status=2 |
| 7 | 70 | 70 | 13 | 3 | - | 6 | ledger_mismatch=1 wrong_balance=2 wrong_grant_count=3 |
| 8 | 71 | 71 | 17 | 3 | - | 8 | ledger_mismatch=3 wrong_balance=2 wrong_grant_count=3 |
| 9 | 81 | 81 | 19 | 2 | - | 14 | ledger_mismatch=3 wrong_balance=3 wrong_grant_count=6 wrong_subscription_status=2 |
| 10 | 89 | 89 | 17 | 1 | - | 15 | ledger_mismatch=5 wrong_balance=2 wrong_grant_count=7 wrong_subscription_status=1 |
| 11 | 80 | 80 | 17 | 2 | - | 15 | ledger_mismatch=2 wrong_balance=5 wrong_grant_count=7 wrong_subscription_status=1 |
| 12 | 85 | 85 | 19 | 1 | - | 18 | ledger_mismatch=5 wrong_balance=4 wrong_grant_count=7 wrong_subscription_status=2 |
| 13 | 80 | 80 | 15 | 2 | - | 12 | ledger_mismatch=2 wrong_balance=2 wrong_grant_count=4 wrong_subscription_status=4 |
| 14 | 83 | 83 | 9 | 1 | - | 9 | ledger_mismatch=2 wrong_balance=2 wrong_grant_count=4 wrong_subscription_status=1 |
| 15 | 89 | 89 | 24 | 3 | - | 15 | ledger_mismatch=5 wrong_balance=3 wrong_grant_count=7 |
| 16 | 63 | 63 | 7 | 0 | - | 10 | ledger_mismatch=4 wrong_balance=1 wrong_grant_count=5 |
| 17 | 70 | 70 | 5 | 0 | - | 7 | ledger_mismatch=2 wrong_balance=2 wrong_grant_count=3 |
| 18 | 91 | 91 | 10 | 1 | - | 10 | ledger_mismatch=3 wrong_balance=2 wrong_grant_count=5 |
| 19 | 68 | 68 | 17 | 2 | - | 11 | ledger_mismatch=1 wrong_balance=4 wrong_grant_count=4 wrong_subscription_status=2 |
| 20 | 90 | 90 | 20 | 3 | - | 14 | ledger_mismatch=4 wrong_balance=3 wrong_grant_count=6 wrong_subscription_status=1 |
