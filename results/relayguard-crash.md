# Chaos run: relayguard

- Date: 2026-11-04 18:41
- Commit: `2e1663a`
- Command: `chaos -target relayguard -seeds 1-20 -dup 0.2 -reorder 10 -supervise bin\worker.exe -crash-rate 0.1 -kill-interval 2s -out results\relayguard-crash.md`
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 20, failing seeds: 0, violations: 0, process restarts: 230 (of which hard kills: 8)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |
|---|---|---|---|---|---|---|---|
| 1 | 84 | 99 | 0 | 0 | done=82 stale=2 | 0 | - |
| 2 | 83 | 97 | 0 | 0 | done=82 stale=1 | 0 | - |
| 3 | 80 | 95 | 0 | 0 | done=79 stale=1 | 0 | - |
| 4 | 78 | 93 | 0 | 0 | done=77 stale=1 | 0 | - |
| 5 | 78 | 92 | 0 | 0 | done=75 stale=3 | 0 | - |
| 6 | 79 | 91 | 0 | 0 | done=77 stale=2 | 0 | - |
| 7 | 70 | 89 | 0 | 0 | done=70 | 0 | - |
| 8 | 71 | 88 | 0 | 0 | done=67 stale=4 | 0 | - |
| 9 | 81 | 97 | 0 | 0 | done=78 stale=3 | 0 | - |
| 10 | 89 | 109 | 0 | 0 | done=86 stale=3 | 0 | - |
| 11 | 80 | 96 | 0 | 0 | done=76 stale=4 | 0 | - |
| 12 | 85 | 105 | 0 | 0 | done=84 stale=1 | 0 | - |
| 13 | 80 | 95 | 0 | 0 | done=79 stale=1 | 0 | - |
| 14 | 83 | 94 | 0 | 0 | done=82 stale=1 | 0 | - |
| 15 | 89 | 104 | 0 | 0 | done=89 | 0 | - |
| 16 | 63 | 73 | 0 | 0 | done=62 stale=1 | 0 | - |
| 17 | 70 | 88 | 0 | 0 | done=70 | 0 | - |
| 18 | 91 | 106 | 0 | 0 | done=90 stale=1 | 0 | - |
| 19 | 68 | 86 | 0 | 0 | done=64 stale=4 | 0 | - |
| 20 | 90 | 108 | 0 | 0 | done=89 stale=1 | 0 | - |
