# Chaos run: relayguard

- Date: 2026-11-13 20:40
- Commit: `6c5a66c`
- Command: `chaos -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-20 -out results/phase6-relayguard-crash.md`
- Worker lease: 1s, poison events per seed: 0
- Change log check: on (changelog_* violation kinds)
- Worker binary: vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c vcs.modified=false
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 20, failing seeds: 0, violations: 0, process restarts: 257 (of which hard kills: 36)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |
|---|---|---|---|---|---|---|---|
| 1 | 84 | 99 | 0 | 0 | done=81 stale=3 | 0 | - |
| 2 | 83 | 97 | 0 | 0 | done=80 stale=3 | 0 | - |
| 3 | 80 | 95 | 0 | 0 | done=78 stale=2 | 0 | - |
| 4 | 78 | 93 | 0 | 0 | done=73 stale=5 | 0 | - |
| 5 | 78 | 92 | 0 | 0 | done=74 stale=4 | 0 | - |
| 6 | 79 | 91 | 0 | 0 | done=76 stale=3 | 0 | - |
| 7 | 70 | 89 | 0 | 0 | done=69 stale=1 | 0 | - |
| 8 | 71 | 88 | 0 | 0 | done=65 stale=6 | 0 | - |
| 9 | 81 | 97 | 0 | 0 | done=73 stale=8 | 0 | - |
| 10 | 89 | 109 | 0 | 0 | done=81 stale=8 | 0 | - |
| 11 | 80 | 96 | 0 | 0 | done=74 stale=6 | 0 | - |
| 12 | 85 | 105 | 0 | 0 | done=83 stale=2 | 0 | - |
| 13 | 80 | 95 | 0 | 0 | done=79 stale=1 | 0 | - |
| 14 | 83 | 94 | 0 | 0 | done=80 stale=3 | 0 | - |
| 15 | 89 | 104 | 0 | 0 | done=87 stale=2 | 0 | - |
| 16 | 63 | 73 | 0 | 0 | done=61 stale=2 | 0 | - |
| 17 | 70 | 88 | 0 | 0 | done=70 | 0 | - |
| 18 | 91 | 106 | 0 | 0 | done=87 stale=4 | 0 | - |
| 19 | 68 | 86 | 0 | 0 | done=60 stale=8 | 0 | - |
| 20 | 90 | 108 | 0 | 0 | done=90 | 0 | - |
