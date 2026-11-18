# Phase 6 results: step 7b evidence (change log, replay, undo)

Local machine. Synthetic seeded histories (not production traffic). All numbers below are pasted
verbatim from the raw run output or the results files linked per experiment. The raw stdout/stderr of
each run was kept in the session scratchpad (not committed); the committed evidence is the
`results/phase6-*.md` files.

All main-tree binaries used here (bin/worker.exe, bin/naive.exe, bin/chaos.exe) record
`vcs.modified=false`. The final Phase 5 runs used main-tree binaries with `vcs.modified=true`
(docs/phase5-results.md:143). In Phase 5, `false` appears only for the Phase 4 compatibility binary
bin/worker-phase4.exe, built from a clean worktree (docs/phase5-results.md:32), and for an earlier
bin/worker.exe in a superseded E2 section (docs/phase5-results.md:148, :434). Results from Phases 1-4
record no vcs line.

## Tree and binary state

- Date: 2026-11-13
- HEAD: `6c5a66cecd5cfb15b1e760984f0d2055b768ec5c` (Phase 6 step 7a: chaos -undo-all property check)
- `git status --porcelain` was empty before the runs (checked by the lead). No `results/phase6-*` files existed before the runs.
- `dev.ps1 build` output: `built bin\worker.exe, bin\naive.exe, bin\chaos.exe, bin\admin.exe`
- `dev.ps1 test` and `dev.ps1 db-test` exited 0 (reported by the lead, before the runs), then `reset-data`.
- The RelayGuard receiver ran in the background on 127.0.0.1:8080 for R1-E4c (started by the lead,
  healthz `ok`) and was stopped by the lead before E5.

Binary build info (read-only `go version -m`, run after the experiments, no rebuild in between):

```text
$ go version -m bin/worker.exe | grep vcs
	build	vcs=git
	build	vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c
	build	vcs.time=2026-11-14T01:31:29Z
	build	vcs.modified=false
$ go version -m bin/naive.exe | grep vcs
	build	vcs=git
	build	vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c
	build	vcs.time=2026-11-14T01:31:29Z
	build	vcs.modified=false
$ go version -m bin/chaos.exe | grep vcs
	build	vcs=git
	build	vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c
	build	vcs.time=2026-11-14T01:31:29Z
	build	vcs.modified=false
$ go version -m bin/admin.exe | grep vcs
	build	vcs=git
	build	vcs.revision=6c5a66cecd5cfb15b1e760984f0d2055b768ec5c
	build	vcs.time=2026-11-14T01:31:29Z
	build	vcs.modified=false
```

`vcs.time` is the commit time in UTC (`git log -1 --format=%ci 6c5a66c` prints `2026-11-13 20:31:29 -0500`),
not the build time.

migrate-status (lead, 2026-11-13 20:33:45):

```text
2026/11/13 20:33:45     Applied At                  Migration
2026/11/13 20:33:45     =======================================
2026/11/13 20:33:45     Wed Nov  4 16:27:32 2026 -- 00001_create_app_tables.sql
2026/11/13 20:33:45     Wed Nov  4 17:33:09 2026 -- 00002_create_inbox.sql
2026/11/13 20:33:45     Wed Nov  4 21:02:03 2026 -- 00003_subscription_ordering.sql
2026/11/13 20:33:45     Thu Nov 12 02:55:39 2026 -- 00004_change_log.sql
2026/11/13 20:33:45     Thu Nov 12 09:47:16 2026 -- 00005_replay.sql
2026/11/13 20:33:45     Thu Nov 12 21:02:46 2026 -- 00006_undo.sql
```

## Common flags

RelayGuard runs: `-target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s`.
Experiments ran sequentially from `c:\dev\relayguard`. After each of R1-E4c a `tasklist` check showed no
worker, naive or chaos process left running.

## R1: RelayGuard crash run, seeds 1-20

- Date: 2026-11-13, start 20:38:49, end 20:40:30, exit code 0
- Command: `./bin/chaos.exe -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-20 -out results/phase6-relayguard-crash.md`
- Results file: [results/phase6-relayguard-crash.md](../results/phase6-relayguard-crash.md)

```text
seed=1    events=84   deliveries=99   provider_retries=0   gave_up=0   inbox[done=81 stale=3] violations=0 (9.403s)
seed=2    events=83   deliveries=97   provider_retries=0   gave_up=0   inbox[done=80 stale=3] violations=0 (5.875s)
seed=3    events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=78 stale=2] violations=0 (2.871s)
seed=4    events=78   deliveries=93   provider_retries=0   gave_up=0   inbox[done=73 stale=5] violations=0 (4.876s)
seed=5    events=78   deliveries=92   provider_retries=0   gave_up=0   inbox[done=74 stale=4] violations=0 (4.63s)
seed=6    events=79   deliveries=91   provider_retries=0   gave_up=0   inbox[done=76 stale=3] violations=0 (5.632s)
seed=7    events=70   deliveries=89   provider_retries=0   gave_up=0   inbox[done=69 stale=1] violations=0 (5.377s)
seed=8    events=71   deliveries=88   provider_retries=0   gave_up=0   inbox[done=65 stale=6] violations=0 (5.871s)
seed=9    events=81   deliveries=97   provider_retries=0   gave_up=0   inbox[done=73 stale=8] violations=0 (5.919s)
seed=10   events=89   deliveries=109  provider_retries=0   gave_up=0   inbox[done=81 stale=8] violations=0 (3.664s)
seed=11   events=80   deliveries=96   provider_retries=0   gave_up=0   inbox[done=74 stale=6] violations=0 (4.879s)
seed=12   events=85   deliveries=105  provider_retries=0   gave_up=0   inbox[done=83 stale=2] violations=0 (3.63s)
seed=13   events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=79 stale=1] violations=0 (4.383s)
seed=14   events=83   deliveries=94   provider_retries=0   gave_up=0   inbox[done=80 stale=3] violations=0 (4.21s)
seed=15   events=89   deliveries=104  provider_retries=0   gave_up=0   inbox[done=87 stale=2] violations=0 (3.137s)
seed=16   events=63   deliveries=73   provider_retries=0   gave_up=0   inbox[done=61 stale=2] violations=0 (3.875s)
seed=17   events=70   deliveries=88   provider_retries=0   gave_up=0   inbox[done=70] violations=0 (4.442s)
seed=18   events=91   deliveries=106  provider_retries=0   gave_up=0   inbox[done=87 stale=4] violations=0 (7.458s)
seed=19   events=68   deliveries=86   provider_retries=0   gave_up=0   inbox[done=60 stale=8] violations=0 (4.152s)
seed=20   events=90   deliveries=108  provider_retries=0   gave_up=0   inbox[done=90] violations=0 (5.197s)
RESULT target=relayguard seeds=20 failing_seeds=0 violations=0 restarts=257 hard_kills=36
```

## R2: RelayGuard poison run, seeds 1-5, 2 poison events per seed

- Date: 2026-11-13, start 20:40:31, end 20:41:29, exit code 0
- Command: `./bin/chaos.exe -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-5 -poison 2 -out results/phase6-relayguard-poison.md`
- Results file: [results/phase6-relayguard-poison.md](../results/phase6-relayguard-poison.md)

```text
seed=1    events=84   deliveries=101  provider_retries=0   gave_up=0   inbox[dead=2 done=80 stale=4] violations=0 (10.948s)
seed=2    events=83   deliveries=99   provider_retries=0   gave_up=0   inbox[dead=2 done=80 stale=3] violations=0 (12.166s)
seed=3    events=80   deliveries=97   provider_retries=0   gave_up=0   inbox[dead=2 done=75 stale=5] violations=0 (10.643s)
seed=4    events=78   deliveries=95   provider_retries=0   gave_up=0   inbox[dead=2 done=76 stale=2] violations=0 (11.66s)
seed=5    events=78   deliveries=94   provider_retries=0   gave_up=0   inbox[dead=2 done=70 stale=8] violations=0 (12.156s)
RESULT target=relayguard seeds=5 failing_seeds=0 violations=0 restarts=161 hard_kills=20
```

Every seed has `dead=2`.

## What E4 (undo-all) asserts

After the forward checks (it runs even if they found violations; it is skipped, as `undo_skipped`, only
if events are still pending), the harness undoes events (`-undo-all newest|random`). The results-file
header states, verbatim: `asserts empty tables, all undone, changelog 0, probes refused; undo_refusals and Refusals count refusals in the undo loop only, not the probes' expected refusals`.
The code (checkUndoAudit, internal/sim/undoall.go) also checks that `admin_actions` holds exactly the
expected audit rows: one done undo per undone event, one refused row per audited refusal, no other change.
The probes are a second undo and a replay of each undone event, plus an undo of each stale, skipped or
dead event; hence probes = 2 x undone + non-done events. It is a property check, not an operator workflow.
In every seed of E4a, E4b and E4c, `undone` equals the `done` count.

## E4a: undo-all newest, seeds 1-20

- Date: 2026-11-13, start 20:41:29, end 20:43:19, exit code 0
- Command: `./bin/chaos.exe -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-20 -undo-all newest -out results/phase6-undo-all-newest.md`
- Results file: [results/phase6-undo-all-newest.md](../results/phase6-undo-all-newest.md)

```text
seed=1    events=84   deliveries=99   provider_retries=0   gave_up=0   inbox[done=81 stale=3] violations=0 (4.393s)
    undo: mode=newest undone=81 refusals=0 passes=1 probes=165 violations=0
seed=2    events=83   deliveries=97   provider_retries=0   gave_up=0   inbox[done=82 stale=1] violations=0 (6.16s)
    undo: mode=newest undone=82 refusals=0 passes=1 probes=165 violations=0
seed=3    events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=78 stale=2] violations=0 (6.06s)
    undo: mode=newest undone=78 refusals=0 passes=1 probes=158 violations=0
seed=4    events=78   deliveries=93   provider_retries=0   gave_up=0   inbox[done=76 stale=2] violations=0 (4.134s)
    undo: mode=newest undone=76 refusals=0 passes=1 probes=154 violations=0
seed=5    events=78   deliveries=92   provider_retries=0   gave_up=0   inbox[done=72 stale=6] violations=0 (5.187s)
    undo: mode=newest undone=72 refusals=0 passes=1 probes=150 violations=0
seed=6    events=79   deliveries=91   provider_retries=0   gave_up=0   inbox[done=76 stale=3] violations=0 (6.869s)
    undo: mode=newest undone=76 refusals=0 passes=1 probes=155 violations=0
seed=7    events=70   deliveries=89   provider_retries=0   gave_up=0   inbox[done=67 stale=3] violations=0 (4.812s)
    undo: mode=newest undone=67 refusals=0 passes=1 probes=137 violations=0
seed=8    events=71   deliveries=88   provider_retries=0   gave_up=0   inbox[done=66 stale=5] violations=0 (6.59s)
    undo: mode=newest undone=66 refusals=0 passes=1 probes=137 violations=0
seed=9    events=81   deliveries=97   provider_retries=0   gave_up=0   inbox[done=76 stale=5] violations=0 (4.64s)
    undo: mode=newest undone=76 refusals=0 passes=1 probes=157 violations=0
seed=10   events=89   deliveries=109  provider_retries=0   gave_up=0   inbox[done=83 stale=6] violations=0 (5.37s)
    undo: mode=newest undone=83 refusals=0 passes=1 probes=172 violations=0
seed=11   events=80   deliveries=96   provider_retries=0   gave_up=0   inbox[done=76 stale=4] violations=0 (5.561s)
    undo: mode=newest undone=76 refusals=0 passes=1 probes=156 violations=0
seed=12   events=85   deliveries=105  provider_retries=0   gave_up=0   inbox[done=84 stale=1] violations=0 (5.482s)
    undo: mode=newest undone=84 refusals=0 passes=1 probes=169 violations=0
seed=13   events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=78 stale=2] violations=0 (6.336s)
    undo: mode=newest undone=78 refusals=0 passes=1 probes=158 violations=0
seed=14   events=83   deliveries=94   provider_retries=0   gave_up=0   inbox[done=81 stale=2] violations=0 (3.9s)
    undo: mode=newest undone=81 refusals=0 passes=1 probes=164 violations=0
seed=15   events=89   deliveries=104  provider_retries=0   gave_up=0   inbox[done=89] violations=0 (4.802s)
    undo: mode=newest undone=89 refusals=0 passes=1 probes=178 violations=0
seed=16   events=63   deliveries=73   provider_retries=0   gave_up=0   inbox[done=62 stale=1] violations=0 (4.382s)
    undo: mode=newest undone=62 refusals=0 passes=1 probes=125 violations=0
seed=17   events=70   deliveries=88   provider_retries=0   gave_up=0   inbox[done=70] violations=0 (4.695s)
    undo: mode=newest undone=70 refusals=0 passes=1 probes=140 violations=0
seed=18   events=91   deliveries=106  provider_retries=0   gave_up=0   inbox[done=85 stale=6] violations=0 (6.193s)
    undo: mode=newest undone=85 refusals=0 passes=1 probes=176 violations=0
seed=19   events=68   deliveries=86   provider_retries=0   gave_up=0   inbox[done=63 stale=5] violations=0 (7.66s)
    undo: mode=newest undone=63 refusals=0 passes=1 probes=131 violations=0
seed=20   events=90   deliveries=108  provider_retries=0   gave_up=0   inbox[done=89 stale=1] violations=0 (6.438s)
    undo: mode=newest undone=89 refusals=0 passes=1 probes=179 violations=0
RESULT target=relayguard seeds=20 failing_seeds=0 violations=0 restarts=277 hard_kills=40 undone=1534 undo_refusals=0 undo_skipped=0
```

0 refusals in all 20 seeds, 1 pass each.

## E4b: undo-all random, seeds 1-20

- Date: 2026-11-13, start 20:43:20, end 20:45:03, exit code 0
- Command: `./bin/chaos.exe -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-20 -undo-all random -out results/phase6-undo-all-random.md`
- Results file: [results/phase6-undo-all-random.md](../results/phase6-undo-all-random.md)

```text
seed=1    events=84   deliveries=99   provider_retries=0   gave_up=0   inbox[done=80 stale=4] violations=0 (4.688s)
    undo: mode=random undone=80 refusals=39 later_event=43 referenced=38 passes=3 probes=164 violations=0
seed=2    events=83   deliveries=97   provider_retries=0   gave_up=0   inbox[done=81 stale=2] violations=0 (7.312s)
    undo: mode=random undone=81 refusals=43 later_event=50 referenced=43 passes=3 probes=164 violations=0
seed=3    events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=79 stale=1] violations=0 (4.752s)
    undo: mode=random undone=79 refusals=40 later_event=51 referenced=38 passes=3 probes=159 violations=0
seed=4    events=78   deliveries=93   provider_retries=0   gave_up=0   inbox[done=74 stale=4] violations=0 (4.45s)
    undo: mode=random undone=74 refusals=45 later_event=55 referenced=36 passes=3 probes=152 violations=0
seed=5    events=78   deliveries=92   provider_retries=0   gave_up=0   inbox[done=73 stale=5] violations=0 (3.969s)
    undo: mode=random undone=73 refusals=38 later_event=46 referenced=33 passes=3 probes=151 violations=0
seed=6    events=79   deliveries=91   provider_retries=0   gave_up=0   inbox[done=77 stale=2] violations=0 (4.379s)
    undo: mode=random undone=77 refusals=47 later_event=55 referenced=42 passes=4 probes=156 violations=0
seed=7    events=70   deliveries=89   provider_retries=0   gave_up=0   inbox[done=68 stale=2] violations=0 (5.8s)
    undo: mode=random undone=68 refusals=33 later_event=43 referenced=29 passes=3 probes=138 violations=0
seed=8    events=71   deliveries=88   provider_retries=0   gave_up=0   inbox[done=67 stale=4] violations=0 (5.337s)
    undo: mode=random undone=67 refusals=36 later_event=39 referenced=37 passes=4 probes=138 violations=0
seed=9    events=81   deliveries=97   provider_retries=0   gave_up=0   inbox[done=75 stale=6] violations=0 (5.406s)
    undo: mode=random undone=75 refusals=44 later_event=50 referenced=38 passes=4 probes=156 violations=0
seed=10   events=89   deliveries=109  provider_retries=0   gave_up=0   inbox[done=85 stale=4] violations=0 (5.355s)
    undo: mode=random undone=85 refusals=43 later_event=54 referenced=44 passes=3 probes=174 violations=0
seed=11   events=80   deliveries=96   provider_retries=0   gave_up=0   inbox[done=76 stale=4] violations=0 (5.473s)
    undo: mode=random undone=76 refusals=41 later_event=57 referenced=46 passes=3 probes=156 violations=0
seed=12   events=85   deliveries=105  provider_retries=0   gave_up=0   inbox[done=82 stale=3] violations=0 (5.068s)
    undo: mode=random undone=82 refusals=52 later_event=64 referenced=42 passes=4 probes=167 violations=0
seed=13   events=80   deliveries=95   provider_retries=0   gave_up=0   inbox[done=76 stale=4] violations=0 (4.875s)
    undo: mode=random undone=76 refusals=32 later_event=28 referenced=27 passes=3 probes=156 violations=0
seed=14   events=83   deliveries=94   provider_retries=0   gave_up=0   inbox[done=79 stale=4] violations=0 (4.728s)
    undo: mode=random undone=79 refusals=45 later_event=41 referenced=37 passes=3 probes=162 violations=0
seed=15   events=89   deliveries=104  provider_retries=0   gave_up=0   inbox[done=86 stale=3] violations=0 (6.31s)
    undo: mode=random undone=86 refusals=54 later_event=66 referenced=50 passes=4 probes=175 violations=0
seed=16   events=63   deliveries=73   provider_retries=0   gave_up=0   inbox[done=62 stale=1] violations=0 (4s)
    undo: mode=random undone=62 refusals=25 later_event=34 referenced=26 passes=3 probes=125 violations=0
seed=17   events=70   deliveries=88   provider_retries=0   gave_up=0   inbox[done=70] violations=0 (5.8s)
    undo: mode=random undone=70 refusals=26 later_event=35 referenced=30 passes=3 probes=140 violations=0
seed=18   events=91   deliveries=106  provider_retries=0   gave_up=0   inbox[done=90 stale=1] violations=0 (5.388s)
    undo: mode=random undone=90 refusals=49 later_event=60 referenced=46 passes=3 probes=181 violations=0
seed=19   events=68   deliveries=86   provider_retries=0   gave_up=0   inbox[done=62 stale=6] violations=0 (4.751s)
    undo: mode=random undone=62 refusals=27 later_event=28 referenced=26 passes=3 probes=130 violations=0
seed=20   events=90   deliveries=108  provider_retries=0   gave_up=0   inbox[done=88 stale=2] violations=0 (4.898s)
    undo: mode=random undone=88 refusals=55 later_event=71 referenced=49 passes=4 probes=178 violations=0
RESULT target=relayguard seeds=20 failing_seeds=0 violations=0 restarts=260 hard_kills=34 undone=1530 undo_refusals=814 undo_skipped=0
```

The per-kind counts (`later_event=`, `referenced=`) count conflicts, so they can sum to more than
`refusals=`, since one refusal can list several conflicts. Refusal counts are expected to vary run to
run: the random order is fixed by the seed, but the set of done events depends on the forward run's
done/stale split, which varies; E4b was run once.

## E4c: undo-all newest with poison, seeds 1-5, 2 poison events per seed

- Date: 2026-11-13, start 20:45:03, end 20:46:03, exit code 0
- Command: `./bin/chaos.exe -target relayguard -dup 0.2 -reorder 10 -supervise bin/worker.exe -crash-rate 0.1 -kill-interval 2s -lease 1s -seeds 1-5 -poison 2 -undo-all newest -out results/phase6-undo-all-poison.md`
- Results file: [results/phase6-undo-all-poison.md](../results/phase6-undo-all-poison.md)

```text
seed=1    events=84   deliveries=101  provider_retries=0   gave_up=0   inbox[dead=2 done=80 stale=4] violations=0 (11.069s)
    undo: mode=newest undone=80 refusals=0 passes=1 probes=166 violations=0
seed=2    events=83   deliveries=99   provider_retries=0   gave_up=0   inbox[dead=2 done=79 stale=4] violations=0 (12.76s)
    undo: mode=newest undone=79 refusals=0 passes=1 probes=164 violations=0
seed=3    events=80   deliveries=97   provider_retries=0   gave_up=0   inbox[dead=2 done=78 stale=2] violations=0 (10.667s)
    undo: mode=newest undone=78 refusals=0 passes=1 probes=160 violations=0
seed=4    events=78   deliveries=95   provider_retries=0   gave_up=0   inbox[dead=2 done=76 stale=2] violations=0 (11.891s)
    undo: mode=newest undone=76 refusals=0 passes=1 probes=156 violations=0
seed=5    events=78   deliveries=94   provider_retries=0   gave_up=0   inbox[dead=2 done=70 stale=8] violations=0 (13.216s)
    undo: mode=newest undone=70 refusals=0 passes=1 probes=150 violations=0
RESULT target=relayguard seeds=5 failing_seeds=0 violations=0 restarts=164 hard_kills=20 undone=383 undo_refusals=0 undo_skipped=0
```

Every seed has `dead=2`; the `undone` count equals the `done` count in each seed.

## E5: naive baseline, seeds 1-20

- Date: 2026-11-13, start 21:47:52, end 21:49:01, exit code 1 (violations found; expected for the baseline)
- Command: `./bin/chaos.exe -target naive -seeds 1-20 -dup 0.2 -reorder 10 -supervise bin/naive.exe -crash-rate 0.1 -kill-interval 2s -out results/phase6-naive-crash.md`
- Results file: [results/phase6-naive-crash.md](../results/phase6-naive-crash.md)
- The RelayGuard receiver was stopped first by the lead (healthz unreachable, port 8080 free, no relayguard
  processes: reported by the lead). Before the run the experiment-runner also saw no worker/naive/chaos
  process and curl exit 7 (not captured in the committed output). Afterwards no naive.exe or chaos.exe was running and
  nothing listened on port 8080.
- The gap between E4c end (20:46:03) and E5 start (21:47:52) is hand-off time between the RelayGuard runs
  and E5 (the lead stopped the RelayGuard receiver and confirmed port 8080 free); no other runs happened in
  between (reported by the lead). No run was superseded; each experiment ran once.
- naive ran on the 00001-00006 schema without failing outright.

```text
seed=1    events=84   deliveries=99   provider_retries=18  gave_up=3   violations=25 (2.594s)
seed=2    events=83   deliveries=97   provider_retries=24  gave_up=3   violations=27 (3.534s)
seed=3    events=80   deliveries=95   provider_retries=23  gave_up=5   violations=18 (2.286s)
seed=4    events=78   deliveries=93   provider_retries=27  gave_up=6   violations=16 (2.495s)
seed=5    events=78   deliveries=92   provider_retries=29  gave_up=6   violations=16 (3.168s)
seed=6    events=79   deliveries=91   provider_retries=19  gave_up=4   violations=19 (2.255s)
seed=7    events=70   deliveries=89   provider_retries=46  gave_up=10  violations=24 (4.769s)
seed=8    events=71   deliveries=88   provider_retries=32  gave_up=7   violations=13 (3.102s)
seed=9    events=81   deliveries=97   provider_retries=44  gave_up=10  violations=17 (4.315s)
seed=10   events=89   deliveries=109  provider_retries=35  gave_up=8   violations=24 (4.499s)
seed=11   events=80   deliveries=96   provider_retries=35  gave_up=8   violations=24 (4.746s)
seed=12   events=85   deliveries=105  provider_retries=30  gave_up=6   violations=25 (3.216s)
seed=13   events=80   deliveries=95   provider_retries=30  gave_up=7   violations=12 (2.809s)
seed=14   events=83   deliveries=94   provider_retries=28  gave_up=7   violations=10 (2.352s)
seed=15   events=89   deliveries=104  provider_retries=12  gave_up=2   violations=21 (1.708s)
seed=16   events=63   deliveries=73   provider_retries=27  gave_up=5   violations=19 (3.876s)
seed=17   events=70   deliveries=88   provider_retries=32  gave_up=7   violations=17 (3.011s)
seed=18   events=91   deliveries=106  provider_retries=41  gave_up=8   violations=26 (5.112s)
seed=19   events=68   deliveries=86   provider_retries=26  gave_up=5   violations=24 (2.564s)
seed=20   events=90   deliveries=108  provider_retries=34  gave_up=6   violations=25 (4.354s)
RESULT target=naive seeds=20 failing_seeds=20 violations=402 restarts=276 hard_kills=12
```

Per-kind violation totals, summed from the Kinds column of the results file (they add up to 402) with:

`grep -E '^\| [0-9]+ ' results/phase6-naive-crash.md | awk -F'|' '{n=split($9,a," "); for(i=1;i<=n;i++){split(a[i],kv,"="); t[kv[1]]+=kv[2]}} END{for(k in t) print k"="t[k]}' | sort`

```text
ledger_mismatch=61
missing_subscription=1
wrong_balance=149
wrong_grant_count=164
wrong_subscription_status=27
```

## RelayGuard vs naive (R1 vs E5)

Same seeds (1-20), `-dup 0.2`, `-reorder 10`, `-crash-rate 0.1`, `-kill-interval 2s`.
Differences: `-supervise bin/worker.exe` (R1) vs `-supervise bin/naive.exe` (E5); RelayGuard runs use
`-lease 1s` and carry extra `changelog_*` checks; naive has neither (its results file says "Change log
check: n/a"). The checks are therefore not identical: RelayGuard is held to more checks and still has 0
violations.

| | R1 relayguard | E5 naive |
|---|---|---|
| failing seeds | 0 of 20 | 20 of 20 |
| violations | 0 | 402 |
| restarts | 257 | 276 |
| hard kills | 36 | 12 |
| provider retries / gave up | 0 in every seed | 592 provider retries, 123 gave up (sum over 20 seeds) |
| exit code | 0 | 1 |

Only the worker was crash-injected on the RelayGuard side; the receiver was not, so RelayGuard shows no
provider retries by construction (internal/sim/deliver.go counts a retry only on a non-2xx reply or
network error). Naive's gave_up includes already-applied events whose resends were rejected (as analysed in Phase 4). Restarts and
hard kills are of different processes and scale with run duration under `-kill-interval 2s`. Violations
are the real measure. The retry, gave_up and violations caveats were also stated in docs/phase4-results.md:57-61.

## Notes

- The done/stale split under crashes varies run to run (see docs/phase5-results.md:739), and the E4b
  refusal counts are expected to vary with it (see E4b). Neither is a violation by itself. R1, E4a and E4b
  ran the same seeds 1-20 with the same forward flags (E4a/E4b add -undo-all) but their Inbox columns
  differ (compare the results files); restart and hard-kill counts differ as well.
- All main-tree binaries in this evidence record vcs.modified=false. The final Phase 5 runs recorded
  vcs.modified=true (docs/phase5-results.md:143); in Phase 5, false appears only for the compatibility
  binary bin/worker-phase4.exe (docs/phase5-results.md:32) and for an earlier bin/worker.exe in a
  superseded E2 section (docs/phase5-results.md:434). Phases 1-4 record no vcs line.

## Limitations

- Undo is not crash-injected and not concurrent with live processing.
- undo-all is a property check, not an operator workflow.
- E4 never replays events, so the generation filter in undo-all ordering is not exercised by E4 (only by
  `TestDBUndoAllReplayedGeneration1`).
- The whole-table count check assumes no real Stripe events arrive during a seed.
- Each experiment was run once; numbers are from that single run.
