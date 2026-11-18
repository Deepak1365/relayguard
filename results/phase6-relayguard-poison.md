# Chaos run: relayguard

- Date: 2026-11-13 20:41
- Commit: `6c5a66c`
- Command: `chaos -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-5 -poison 2 -out results/phase6-relayguard-poison.md`
- Worker lease: 1s, poison events per seed: 2
- Change log check: on (changelog_* violation kinds)
- Worker binary: vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c vcs.modified=false
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 5, failing seeds: 0, violations: 0, process restarts: 161 (of which hard kills: 20)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |
|---|---|---|---|---|---|---|---|
| 1 | 84 | 101 | 0 | 0 | dead=2 done=80 stale=4 | 0 | - |
| 2 | 83 | 99 | 0 | 0 | dead=2 done=80 stale=3 | 0 | - |
| 3 | 80 | 97 | 0 | 0 | dead=2 done=75 stale=5 | 0 | - |
| 4 | 78 | 95 | 0 | 0 | dead=2 done=76 stale=2 | 0 | - |
| 5 | 78 | 94 | 0 | 0 | dead=2 done=70 stale=8 | 0 | - |
