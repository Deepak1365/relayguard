# Chaos run: relayguard

- Date: 2026-11-13 20:45
- Commit: `6c5a66c`
- Command: `chaos -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-20 -undo-all random -out results/phase6-undo-all-random.md`
- Worker lease: 1s, poison events per seed: 0
- Change log check: on (changelog_* violation kinds)
- Undo-all: random (after forward checks; asserts empty tables, all undone, changelog 0, probes refused; undo_refusals and Refusals count refusals in the undo loop only, not the probes' expected refusals)
- Worker binary: vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c vcs.modified=false
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 20, failing seeds: 0, violations: 0, process restarts: 260 (of which hard kills: 34)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Undone | Refusals | Passes | Violations | Kinds |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 84 | 99 | 0 | 0 | done=80 stale=4 | 80 | 39 | 3 | 0 | - |
| 2 | 83 | 97 | 0 | 0 | done=81 stale=2 | 81 | 43 | 3 | 0 | - |
| 3 | 80 | 95 | 0 | 0 | done=79 stale=1 | 79 | 40 | 3 | 0 | - |
| 4 | 78 | 93 | 0 | 0 | done=74 stale=4 | 74 | 45 | 3 | 0 | - |
| 5 | 78 | 92 | 0 | 0 | done=73 stale=5 | 73 | 38 | 3 | 0 | - |
| 6 | 79 | 91 | 0 | 0 | done=77 stale=2 | 77 | 47 | 4 | 0 | - |
| 7 | 70 | 89 | 0 | 0 | done=68 stale=2 | 68 | 33 | 3 | 0 | - |
| 8 | 71 | 88 | 0 | 0 | done=67 stale=4 | 67 | 36 | 4 | 0 | - |
| 9 | 81 | 97 | 0 | 0 | done=75 stale=6 | 75 | 44 | 4 | 0 | - |
| 10 | 89 | 109 | 0 | 0 | done=85 stale=4 | 85 | 43 | 3 | 0 | - |
| 11 | 80 | 96 | 0 | 0 | done=76 stale=4 | 76 | 41 | 3 | 0 | - |
| 12 | 85 | 105 | 0 | 0 | done=82 stale=3 | 82 | 52 | 4 | 0 | - |
| 13 | 80 | 95 | 0 | 0 | done=76 stale=4 | 76 | 32 | 3 | 0 | - |
| 14 | 83 | 94 | 0 | 0 | done=79 stale=4 | 79 | 45 | 3 | 0 | - |
| 15 | 89 | 104 | 0 | 0 | done=86 stale=3 | 86 | 54 | 4 | 0 | - |
| 16 | 63 | 73 | 0 | 0 | done=62 stale=1 | 62 | 25 | 3 | 0 | - |
| 17 | 70 | 88 | 0 | 0 | done=70 | 70 | 26 | 3 | 0 | - |
| 18 | 91 | 106 | 0 | 0 | done=90 stale=1 | 90 | 49 | 3 | 0 | - |
| 19 | 68 | 86 | 0 | 0 | done=62 stale=6 | 62 | 27 | 3 | 0 | - |
| 20 | 90 | 108 | 0 | 0 | done=88 stale=2 | 88 | 55 | 4 | 0 | - |
