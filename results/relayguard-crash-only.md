# Chaos run: relayguard

- Date: 2026-11-04 18:44
- Commit: `2e1663a`
- Command: `chaos -target relayguard -seeds 1-20 -supervise bin\worker.exe -crash-rate 0.1 -kill-interval 2s -out results\relayguard-crash-only.md`
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 20, failing seeds: 0, violations: 0, process restarts: 246 (of which hard kills: 9)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |
|---|---|---|---|---|---|---|---|
| 1 | 84 | 84 | 0 | 0 | done=84 | 0 | - |
| 2 | 83 | 83 | 0 | 0 | done=83 | 0 | - |
| 3 | 80 | 80 | 0 | 0 | done=80 | 0 | - |
| 4 | 78 | 78 | 0 | 0 | done=78 | 0 | - |
| 5 | 78 | 78 | 0 | 0 | done=78 | 0 | - |
| 6 | 79 | 79 | 0 | 0 | done=79 | 0 | - |
| 7 | 70 | 70 | 0 | 0 | done=70 | 0 | - |
| 8 | 71 | 71 | 0 | 0 | done=71 | 0 | - |
| 9 | 81 | 81 | 0 | 0 | done=81 | 0 | - |
| 10 | 89 | 89 | 0 | 0 | done=89 | 0 | - |
| 11 | 80 | 80 | 0 | 0 | done=80 | 0 | - |
| 12 | 85 | 85 | 0 | 0 | done=85 | 0 | - |
| 13 | 80 | 80 | 0 | 0 | done=80 | 0 | - |
| 14 | 83 | 83 | 0 | 0 | done=83 | 0 | - |
| 15 | 89 | 89 | 0 | 0 | done=89 | 0 | - |
| 16 | 63 | 63 | 0 | 0 | done=63 | 0 | - |
| 17 | 70 | 70 | 0 | 0 | done=70 | 0 | - |
| 18 | 91 | 91 | 0 | 0 | done=91 | 0 | - |
| 19 | 68 | 68 | 0 | 0 | done=68 | 0 | - |
| 20 | 90 | 90 | 0 | 0 | done=90 | 0 | - |
