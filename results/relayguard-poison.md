# Chaos run: relayguard

- Date: 2026-11-11 19:43
- Commit: `2c708e6`
- Command: `chaos -target relayguard -seeds 1-5 -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -poison 2 -out results/relayguard-poison.md`
- Worker lease: 1s, poison events per seed: 2
- Worker binary: vcs.revision=2c708e68ffec82261c38c1c191b2333a09474c58 vcs.modified=true
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 5, failing seeds: 0, violations: 0, process restarts: 155 (of which hard kills: 21)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |
|---|---|---|---|---|---|---|---|
| 1 | 84 | 101 | 0 | 0 | dead=2 done=81 stale=3 | 0 | - |
| 2 | 83 | 99 | 0 | 0 | dead=2 done=81 stale=2 | 0 | - |
| 3 | 80 | 97 | 0 | 0 | dead=2 done=78 stale=2 | 0 | - |
| 4 | 78 | 95 | 0 | 0 | dead=2 done=75 stale=3 | 0 | - |
| 5 | 78 | 94 | 0 | 0 | dead=2 done=73 stale=5 | 0 | - |
