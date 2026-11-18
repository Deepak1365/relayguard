# Chaos run: naive

- Date: 2026-11-13 21:49
- Commit: `6c5a66c`
- Command: `chaos -target naive -seeds 1-20 -dup 0.2 -reorder 10 -supervise bin/naive.exe -crash-rate 0.1 -kill-interval 2s -out results/phase6-naive-crash.md`
- Worker lease: n/a (worker not supervised by the harness), poison events per seed: 0
- Change log check: n/a (the naive receiver writes no change_log)
- Worker binary: vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c vcs.modified=false
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 20, failing seeds: 20, violations: 402, process restarts: 276 (of which hard kills: 12)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |
|---|---|---|---|---|---|---|---|
| 1 | 84 | 99 | 18 | 3 | - | 25 | ledger_mismatch=3 wrong_balance=10 wrong_grant_count=10 wrong_subscription_status=2 |
| 2 | 83 | 97 | 24 | 3 | - | 27 | ledger_mismatch=6 wrong_balance=9 wrong_grant_count=10 wrong_subscription_status=2 |
| 3 | 80 | 95 | 23 | 5 | - | 18 | ledger_mismatch=3 wrong_balance=6 wrong_grant_count=8 wrong_subscription_status=1 |
| 4 | 78 | 93 | 27 | 6 | - | 16 | ledger_mismatch=2 wrong_balance=6 wrong_grant_count=6 wrong_subscription_status=2 |
| 5 | 78 | 92 | 29 | 6 | - | 16 | ledger_mismatch=4 missing_subscription=1 wrong_balance=3 wrong_grant_count=6 wrong_subscription_status=2 |
| 6 | 79 | 91 | 19 | 4 | - | 19 | ledger_mismatch=3 wrong_balance=8 wrong_grant_count=8 |
| 7 | 70 | 89 | 46 | 10 | - | 24 | ledger_mismatch=3 wrong_balance=10 wrong_grant_count=10 wrong_subscription_status=1 |
| 8 | 71 | 88 | 32 | 7 | - | 13 | ledger_mismatch=3 wrong_balance=4 wrong_grant_count=5 wrong_subscription_status=1 |
| 9 | 81 | 97 | 44 | 10 | - | 17 | ledger_mismatch=4 wrong_balance=5 wrong_grant_count=6 wrong_subscription_status=2 |
| 10 | 89 | 109 | 35 | 8 | - | 24 | ledger_mismatch=4 wrong_balance=9 wrong_grant_count=10 wrong_subscription_status=1 |
| 11 | 80 | 96 | 35 | 8 | - | 24 | ledger_mismatch=3 wrong_balance=9 wrong_grant_count=10 wrong_subscription_status=2 |
| 12 | 85 | 105 | 30 | 6 | - | 25 | ledger_mismatch=2 wrong_balance=12 wrong_grant_count=10 wrong_subscription_status=1 |
| 13 | 80 | 95 | 30 | 7 | - | 12 | ledger_mismatch=2 wrong_balance=3 wrong_grant_count=5 wrong_subscription_status=2 |
| 14 | 83 | 94 | 28 | 7 | - | 10 | ledger_mismatch=1 wrong_balance=4 wrong_grant_count=5 |
| 15 | 89 | 104 | 12 | 2 | - | 21 | ledger_mismatch=2 wrong_balance=10 wrong_grant_count=8 wrong_subscription_status=1 |
| 16 | 63 | 73 | 27 | 5 | - | 19 | ledger_mismatch=3 wrong_balance=8 wrong_grant_count=8 |
| 17 | 70 | 88 | 32 | 7 | - | 17 | ledger_mismatch=2 wrong_balance=7 wrong_grant_count=7 wrong_subscription_status=1 |
| 18 | 91 | 106 | 41 | 8 | - | 26 | ledger_mismatch=5 wrong_balance=8 wrong_grant_count=12 wrong_subscription_status=1 |
| 19 | 68 | 86 | 26 | 5 | - | 24 | ledger_mismatch=3 wrong_balance=8 wrong_grant_count=9 wrong_subscription_status=4 |
| 20 | 90 | 108 | 34 | 6 | - | 25 | ledger_mismatch=3 wrong_balance=10 wrong_grant_count=11 wrong_subscription_status=1 |
