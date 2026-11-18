# Chaos run: relayguard

- Date: 2026-11-13 20:46
- Commit: `6c5a66c`
- Command: `chaos -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-5 -poison 2 -undo-all newest -out results/phase6-undo-all-poison.md`
- Worker lease: 1s, poison events per seed: 2
- Change log check: on (changelog_* violation kinds)
- Undo-all: newest (after forward checks; asserts empty tables, all undone, changelog 0, probes refused; undo_refusals and Refusals count refusals in the undo loop only, not the probes' expected refusals)
- Worker binary: vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c vcs.modified=false
- Data: synthetic seeded histories on a local machine (not production traffic)

**Seeds: 5, failing seeds: 0, violations: 0, process restarts: 164 (of which hard kills: 20)**

| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Undone | Refusals | Passes | Violations | Kinds |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 84 | 101 | 0 | 0 | dead=2 done=80 stale=4 | 80 | 0 | 1 | 0 | - |
| 2 | 83 | 99 | 0 | 0 | dead=2 done=79 stale=4 | 79 | 0 | 1 | 0 | - |
| 3 | 80 | 97 | 0 | 0 | dead=2 done=78 stale=2 | 78 | 0 | 1 | 0 | - |
| 4 | 78 | 95 | 0 | 0 | dead=2 done=76 stale=2 | 76 | 0 | 1 | 0 | - |
| 5 | 78 | 94 | 0 | 0 | dead=2 done=70 stale=8 | 70 | 0 | 1 | 0 | - |
