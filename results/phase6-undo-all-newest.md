# Chaos run: relayguard

- Date: 2026-11-13 20:43
- Commit: `6c5a66c`
- Command: `chaos -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-20 -undo-all newest -out results/phase6-undo-all-newest.md`
- Worker lease: 1s, poison events per seed: 0
- Change log check: on (changelog_* violation kinds)
- Undo-all: newest (after forward checks; asserts empty tables, all undone, changelog 0, probes refused; undo_refusals and Refusals count refusals in the undo loop only, not the probes' expected refusals)
- Worker binary: vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c vcs.modified=false
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 20, failing seeds: 0, violations: 0, process restarts: 277 (of which hard kills: 40)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Undone | Refusals | Passes | Violations | Kinds |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 84 | 99 | 0 | 0 | done=81 stale=3 | 81 | 0 | 1 | 0 | - |
| 2 | 83 | 97 | 0 | 0 | done=82 stale=1 | 82 | 0 | 1 | 0 | - |
| 3 | 80 | 95 | 0 | 0 | done=78 stale=2 | 78 | 0 | 1 | 0 | - |
| 4 | 78 | 93 | 0 | 0 | done=76 stale=2 | 76 | 0 | 1 | 0 | - |
| 5 | 78 | 92 | 0 | 0 | done=72 stale=6 | 72 | 0 | 1 | 0 | - |
| 6 | 79 | 91 | 0 | 0 | done=76 stale=3 | 76 | 0 | 1 | 0 | - |
| 7 | 70 | 89 | 0 | 0 | done=67 stale=3 | 67 | 0 | 1 | 0 | - |
| 8 | 71 | 88 | 0 | 0 | done=66 stale=5 | 66 | 0 | 1 | 0 | - |
| 9 | 81 | 97 | 0 | 0 | done=76 stale=5 | 76 | 0 | 1 | 0 | - |
| 10 | 89 | 109 | 0 | 0 | done=83 stale=6 | 83 | 0 | 1 | 0 | - |
| 11 | 80 | 96 | 0 | 0 | done=76 stale=4 | 76 | 0 | 1 | 0 | - |
| 12 | 85 | 105 | 0 | 0 | done=84 stale=1 | 84 | 0 | 1 | 0 | - |
| 13 | 80 | 95 | 0 | 0 | done=78 stale=2 | 78 | 0 | 1 | 0 | - |
| 14 | 83 | 94 | 0 | 0 | done=81 stale=2 | 81 | 0 | 1 | 0 | - |
| 15 | 89 | 104 | 0 | 0 | done=89 | 89 | 0 | 1 | 0 | - |
| 16 | 63 | 73 | 0 | 0 | done=62 stale=1 | 62 | 0 | 1 | 0 | - |
| 17 | 70 | 88 | 0 | 0 | done=70 | 70 | 0 | 1 | 0 | - |
| 18 | 91 | 106 | 0 | 0 | done=85 stale=6 | 85 | 0 | 1 | 0 | - |
| 19 | 68 | 86 | 0 | 0 | done=63 stale=5 | 63 | 0 | 1 | 0 | - |
| 20 | 90 | 108 | 0 | 0 | done=89 stale=1 | 89 | 0 | 1 | 0 | - |
