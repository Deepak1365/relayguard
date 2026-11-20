# Phase 8 results: CI (steps 8a and 8b proof runs)

Date written: 2026-11-20. Source: GitHub Actions runs of Deepak1365/relayguard, read with `gh` on 2026-11-20 (no local experiments were run for this file).
Run URL pattern: https://github.com/Deepak1365/relayguard/actions/runs/<id>
Full logs were saved with `gh run view <id> --repo Deepak1365/relayguard --log` to the session scratchpad (`run_<id>.log`, plus `meta_<id>.json` from
`gh run view <id> --json databaseId,headBranch,headSha,conclusion,createdAt,startedAt,updatedAt,event,jobs`). Scratchpad files are not committed; GitHub
logs and artifacts expire, so the verbatim lines below are the durable record.

## What these runs are, and are not

- CI runs are a **regression signal**, not evidence in the sense of the Phase 5/6/7 results docs. They build an exact commit, but on GitHub-hosted
  `ubuntu-24.04` runners (the "before" run: `ubuntu-26.04`), with Go taken from `go.mod` (`go 1.26.0`, log: `go version go1.26.0 linux/amd64`) whereas the
  local runs used go1.27.4 (docs/phase8-plan.md:131-132), and
  the chaos supervisor's crash and kill timing is wall-clock. Their counts (restarts, hard_kills, violations, test counts) are **not comparable** to local
  results and go in no results table.
- A green chaos run is **one sample**, not proof of absence of bugs.
- Caveat required whenever relayguard and naive appear together: the relayguard runs carry extra `changelog_*` checks that the naive run does not.
  The R1/R2 (relayguard) and N1 (naive) lines below are therefore not a like-for-like comparison of the same checker.
- The mutations on the `ci-red/*` branches are quoted verbatim from `git show <sha>` (2026-11-20). The branches are deleted and the commits are
  unreachable, so they may be garbage-collected; the changed lines are quoted in each section for that reason. The runs themselves remain in Actions history.
- Per-job durations are `startedAt`/`completedAt` exactly as `gh run view --json jobs` reports them. The "seconds" column is my subtraction of those two
  timestamps. The workflow cap is `timeout-minutes: 20` per job (1200 s), a backstop and not a measurement (docs/phase8-plan.md:85-86).
  Every job below finished far under the cap (largest: 69 s).

### Count command for top-level DB test passes

Counted only inside the step "Go tests (including DB tests)" of the `test` job, because the "Skip guard" step echoes two matches from its own source text,
and subtests are indented (so the pattern, which requires the text to follow the timestamp directly, matches top-level tests only):

```text
awk -F'\t' '$1=="test" && $2 ~ /^Go tests/' run_<id>.log | grep -c -- 'Z --- PASS: TestDB'
```

The same command with `'Z --- FAIL: '`, `'Z --- SKIP'` and `'Z --- PASS: '` gave the FAIL, SKIP and all-top-level-PASS counts. Counts are of log lines, taken
by me from the saved logs. This matches CLAUDE.md Status for 0ca60ff and b20348a (129 TestDB, 0 SKIP). The 130 for 446218e is counted only here.

## Summary table

No numbers from these runs are comparable to local evidence. "TestDB pass" is the top-level count described above.

| Run | Purpose | Expected | Observed |
| --- | --- | --- | --- |
| 44654164320 main 192baae | "Before": pre-Phase-8 workflow | green, DB tests skipped | success; `go test ./...` without `-v`, no Postgres service; DB packages (cmd/worker, internal/admin, internal/adminhttp, internal/dbtest, internal/sim) ok in 0.006-0.023 s (see section) |
| 44674527638 main 0ca60ff | 8a green | green, DB tests run, no skip | success; 129 TestDB pass, 0 FAIL, 0 SKIP |
| 44676583712 ci-red/8a-db 8e2c0e7 | 8a red proof (CrossOriginProtection removed) | red at Go tests | failure at "Go tests (including DB tests)"; TestDBCrossSitePost403 FAIL; 128 TestDB pass, 1 FAIL |
| 44676583762 ci-red/8a-skip 737c9db | 8a guard proof (`RELAYGUARD_DB_TESTS=1` removed) | red at Skip guard | failure at "Skip guard (DB tests really ran)"; Go tests step green with 121 SKIP |
| 44766247687 main b20348a | 8b green | both jobs green; R1/R2 clean; N1 violations | success; R1 and R2 gate PASS (clean); N1 gate PASS (violations); 129 TestDB pass |
| 44774125059 ci-red/8b-app 9b2a718 | 8b red proof A (app mutation) | R1 red with violations | chaos job failed at R1 (`failing_seeds=3 violations=144`); test job also red (31 top-level TestDB FAIL) |
| 44774125003 ci-red/8b-checker eb06742 | 8b red proof B (CheckState returns nothing) | R1 green, N1 red | test job green (129 TestDB pass, 0 FAIL); R1 and R2 green; N1 red (`violations=0`) |
| 44782779433 main 446218e | Adds TestDBCheckStateRules (closes gap found by 8b-checker) | both jobs green | success; 130 TestDB pass, 0 FAIL, 0 SKIP; gates as in b20348a |

## Per-run sections

### 1. Before: 44654164320, main, 192baae (pre-Phase-8 workflow)

- Date: 2026-11-20 (createdAt 2026-11-20T01:30:55Z, event push). Branch main. Commit 192baaedc01384fee8fd7b1a0ef26037ed7f2e50.
  URL: https://github.com/Deepak1365/relayguard/actions/runs/44654164320
- Conclusion: success.

| Job | Conclusion | startedAt | completedAt | seconds |
| --- | --- | --- | --- | --- |
| test | success | 2026-11-20T01:30:58Z | 2026-11-20T01:31:22Z | 24 |

- First non-success step: none.
- The workflow at 192baae (`git show 192baae:.github/workflows/ci.yml`) was: trigger `[push, pull_request]`, `runs-on: ubuntu-latest`, steps secret scan,
  setup-go, `go vet ./...`, `go test ./...`. No Postgres service, no `RELAYGUARD_DB_TESTS`, no `-v`. The runner image in this run was `ubuntu-26.04`
  (the later, pinned runs use `ubuntu-24.04`).
- **What the log does and does not show.** Because `go test` ran without `-v`, this log contains no `--- SKIP` lines and no skip message:
  `grep -c -- '--- SKIP'` on the whole log gives 0, and 0 `--- PASS: TestDB` lines. The skip is therefore not displayed here; it is inferred from
  the workflow (no `RELAYGUARD_DB_TESTS=1` and no database) and from `internal/dbtest/dbtest.go:74`, which calls `t.Skip(...)` in that case. The log's
  per-package lines show the DB packages finishing in milliseconds, consistent with that (verbatim):

```text
test	UNKNOWN STEP	2026-11-20T01:31:17.9179260Z ok  	github.com/Deepak1365/relayguard/cmd/admin	0.007s
test	UNKNOWN STEP	2026-11-20T01:31:17.9644829Z ok  	github.com/Deepak1365/relayguard/cmd/chaos	0.010s
test	UNKNOWN STEP	2026-11-20T01:31:18.5569053Z ok  	github.com/Deepak1365/relayguard/cmd/worker	0.006s
test	UNKNOWN STEP	2026-11-20T01:31:18.5816495Z ok  	github.com/Deepak1365/relayguard/internal/admin	0.009s
test	UNKNOWN STEP	2026-11-20T01:31:19.2617134Z ok  	github.com/Deepak1365/relayguard/internal/adminhttp	0.023s
test	UNKNOWN STEP	2026-11-20T01:31:19.2651608Z ok  	github.com/Deepak1365/relayguard/internal/app	0.006s
test	UNKNOWN STEP	2026-11-20T01:31:19.2672614Z ok  	github.com/Deepak1365/relayguard/internal/changelog	0.005s
test	UNKNOWN STEP	2026-11-20T01:31:19.2725773Z ok  	github.com/Deepak1365/relayguard/internal/db	0.005s
test	UNKNOWN STEP	2026-11-20T01:31:19.6348644Z ok  	github.com/Deepak1365/relayguard/internal/dbtest	0.013s
test	UNKNOWN STEP	2026-11-20T01:31:20.0272665Z ok  	github.com/Deepak1365/relayguard/internal/sim	0.010s
test	UNKNOWN STEP	2026-11-20T01:31:20.0274412Z ok  	github.com/Deepak1365/relayguard/internal/stripesig	0.003s
```

  (Step names show `UNKNOWN STEP` because gh could not map log lines to steps for this run. These are the 11 `ok` lines of the run.)
- For what a skipped DB test looks like in a log, section 4 below (run 44676583762, 8a-skip) shows 121 `--- SKIP` lines with the dbtest skip message. That is a different run
  (`-v` was on there); I am not claiming the "before" run printed the same lines.
- RESULT / GATE lines: none (no chaos job existed).

### 2. 8a green: 44674527638, main, 0ca60ff

- Date: 2026-11-20 (createdAt 2026-11-20T04:27:32Z, push). Branch main. Commit 0ca60ff66e98d7bc49bc3bf2628d440f062e9d76.
  URL: https://github.com/Deepak1365/relayguard/actions/runs/44674527638
- Conclusion: success.

| Job | Conclusion | startedAt | completedAt | seconds |
| --- | --- | --- | --- | --- |
| test | success | 2026-11-20T04:27:35Z | 2026-11-20T04:28:43Z | 68 |

- First non-success step: none.
- Go tests step: 129 `--- PASS: TestDB`, 0 `--- FAIL: `, 0 `--- SKIP`, 206 top-level `--- PASS: ` of any name, 11 `ok` package lines.
- Migrate step (verbatim tail):

```text
2026/11/20 04:28:17 goose: successfully migrated database to version: 6
2026/11/20 04:28:18     Applied At                  Migration
2026/11/20 04:28:18     =======================================
2026/11/20 04:28:18     Fri Nov 20 04:28:17 2026 -- 00001_create_app_tables.sql
2026/11/20 04:28:18     Fri Nov 20 04:28:17 2026 -- 00002_create_inbox.sql
2026/11/20 04:28:18     Fri Nov 20 04:28:17 2026 -- 00003_subscription_ordering.sql
2026/11/20 04:28:18     Fri Nov 20 04:28:17 2026 -- 00004_change_log.sql
2026/11/20 04:28:18     Fri Nov 20 04:28:17 2026 -- 00005_replay.sql
2026/11/20 04:28:18     Fri Nov 20 04:28:17 2026 -- 00006_undo.sql
```

- RESULT / GATE lines: none (the chaos job arrives in 8b).

### 3. 8a red proof, DB test: 44676583712, ci-red/8a-db, 8e2c0e7

- Date: 2026-11-20 (createdAt 2026-11-20T04:46:03Z, push). Branch ci-red/8a-db. Commit 8e2c0e7844608e0a37520c80d421685855ce7f80.
  URL: https://github.com/Deepak1365/relayguard/actions/runs/44676583712
- Mutation (verbatim from `git show 8e2c0e7`): `CrossOriginProtection` removed from the admin HTTP server.

```text
-	h = cop.Handler(h)
+	_ = cop // CI RED PROOF: CrossOriginProtection removed; TestDBCrossSitePost403 must fail
```

- Conclusion: failure.

| Job | Conclusion | startedAt | completedAt | seconds |
| --- | --- | --- | --- | --- |
| test | failure | 2026-11-20T04:46:06Z | 2026-11-20T04:46:50Z | 44 |

- First non-success step: "Go tests (including DB tests)" (failure).
- Go tests step: 128 `--- PASS: TestDB`, 1 `--- FAIL: `, 0 `--- SKIP`, 205 top-level `--- PASS: `. 10 `ok` lines and 1 `FAIL` package line.
- Selected FAIL lines (verbatim; the first is truncated here, the full line is in the log; the step also has a second bare `FAIL` line and a
  `FAIL` package line for internal/adminhttp):

```text
    handlers_db_test.go:243: status 200, want 403; body {"inbox_id":1,"event_id":"evt_dbtest_csrf","event_type":"invoice.paid","outcome":"done","reason":"","action_id":1,"from_status":"dead","from_attempts":8,"from_last_error":"un
--- FAIL: TestDBCrossSitePost403 (0.03s)
FAIL
```

- RESULT / GATE lines: none.

### 4. 8a guard proof: 44676583762, ci-red/8a-skip, 737c9db

- Date: 2026-11-20 (createdAt 2026-11-20T04:46:03Z, push). Branch ci-red/8a-skip. Commit 737c9dbc83bf5e1a6ec7e6ba11a7888d1acc0500.
  URL: https://github.com/Deepak1365/relayguard/actions/runs/44676583762
- Change (verbatim from `git show 737c9db`): `RELAYGUARD_DB_TESTS=1` removed from the go test command.

```text
-          RELAYGUARD_DB_TESTS=1 go test -count=1 -p 1 -v ./... 2>&1 | tee ci-out/go-test.log
+          go test -count=1 -p 1 -v ./... 2>&1 | tee ci-out/go-test.log
```

- Conclusion: failure.

| Job | Conclusion | startedAt | completedAt | seconds |
| --- | --- | --- | --- | --- |
| test | failure | 2026-11-20T04:46:07Z | 2026-11-20T04:46:40Z | 33 |

- First non-success step: "Skip guard (DB tests really ran)" (failure). The "Go tests (including DB tests)" step itself concluded success.
- Go tests step: 8 `--- PASS: TestDB`, 0 `--- FAIL: `, 121 `--- SKIP`, 85 top-level `--- PASS: `. The 8 top-level TestDB passes
  (TestDBKeyShareDoesNotBlockClaimOrFence, TestDBPreviewOverlapsRealFence, TestDBReplayResetsDeadRow, TestDBUndoConflictUnloggedWrite,
  TestDBChangeLogCheckAttributionRules, TestDBChangeLogCheckCountsCurrentGenerationOnly, TestDBChangeLogCheckUndoPresence,
  TestDBChangeLogCheckUndoRestore, all 0.00s) are parents of table-driven subtests that each skipped; Go reports a parent as PASS when its subtests
  skip. Counting indented lines too, the step has 149 `--- SKIP` lines. This is why the guard looks for the skip message, not only for a `PASS: TestDB` line.
- Sample skip lines, and the guard message (verbatim, log text after the timestamp):

```text
    changelog_db_test.go:100: set RELAYGUARD_DB_TESTS=1 and DATABASE_URL to run database tests (they truncate tables)
--- SKIP: TestDBApplyWritesChangeLog (0.00s)
    changelog_db_test.go:175: set RELAYGUARD_DB_TESTS=1 and DATABASE_URL to run database tests (they truncate tables)
--- SKIP: TestDBZeroCreditInvoiceLogsUpdate (0.00s)
DB tests were skipped (dbtest skip message found in the log).
##[error]Process completed with exit code 1.
```

- RESULT / GATE lines: none.

### 5. 8b green: 44766247687, main, b20348a

- Date: 2026-11-20 (createdAt 2026-11-20T15:44:09Z, push). Branch main. Commit b20348a76437b18468ea8c5f8b1f16663dac98f0.
  URL: https://github.com/Deepak1365/relayguard/actions/runs/44766247687
- Conclusion: success.

| Job | Conclusion | startedAt | completedAt | seconds |
| --- | --- | --- | --- | --- |
| chaos | success | 2026-11-20T15:44:13Z | 2026-11-20T15:45:22Z | 69 |
| test | success | 2026-11-20T15:44:13Z | 2026-11-20T15:44:58Z | 45 |

- First non-success step: none in either job.
- Test job, Go tests step: 129 `--- PASS: TestDB`, 0 `--- FAIL: `, 0 `--- SKIP`, 206 top-level `--- PASS: `.
- RESULT and GATE lines (verbatim, step name then text; the first column is the job):

```text
chaos	R1 crash run (RelayGuard, must be clean) RESULT target=relayguard seeds=3 failing_seeds=0 violations=0 restarts=35 hard_kills=4
chaos	R1 crash run (RelayGuard, must be clean) GATE PASS (clean): exit 0, seeds=3 failing_seeds=0 violations=0
chaos	R2 poison run (RelayGuard, must be clean) RESULT target=relayguard seeds=1 failing_seeds=0 violations=0 restarts=28 hard_kills=2
chaos	R2 poison run (RelayGuard, must be clean) GATE PASS (clean): exit 0, seeds=1 failing_seeds=0 violations=0
chaos	N1 negative control (naive, must report violations) RESULT target=naive seeds=3 failing_seeds=3 violations=60 restarts=35 hard_kills=1
chaos	N1 negative control (naive, must report violations) GATE PASS (violations): exit 1, naive seeds=3 violations=60 (checker still sees the flaws)
```

- Gate self-test step (synthetic canned logs in scripts/testdata/ci-gate/, so these `RESULT` strings are SYNTHETIC test input, not chaos output). Its log
  lines also match `GATE`/`RESULT` greps; the five lines that mention a RESULT are, verbatim:

```text
chaos	Gate self-test ok   two RESULT lines (rejected:  (clean): expected exactly one 'RESULT target=relayguard' line in scripts/testdata/ci-gate/two-results.txt, found 2)
chaos	Gate self-test ok   no RESULT, exit 0 (rejected:  (clean): expected exactly one 'RESULT target=relayguard' line in scripts/testdata/ci-gate/fatal-no-result.txt, found 0)
chaos	Gate self-test ok   missing RESULT with exit 1 (log.Fatal) under violations (rejected:  (violations): expected exactly one 'RESULT target=naive' line in scripts/testdata/ci-gate/fatal-no-result.txt, found 0)
chaos	Gate self-test ok   relayguard RESULT under violations mode (rejected:  (violations): expected exactly one 'RESULT target=naive' line in scripts/testdata/ci-gate/clean-pass.txt, found 0)
chaos	Gate self-test ok   duplicate violations= field (last would win) (rejected:  (clean): duplicate violations= field: RESULT target=relayguard seeds=3 failing_seeds=0 violations=5 restarts=38 hard_kills=5 violations=0)
```

  (The same five lines appear in the logs of runs 6, 7 and 8; they are not repeated below.)
- Caveat: R1/R2 (relayguard, with `changelog_*` checks) and N1 (naive, without them) are different checker configurations, so N1's `violations=60` is not a
  like-for-like number against R1's `violations=0`. Within this run the purpose of N1 is only "the checker still reports flaws in the naive baseline".

### 6. 8b red proof A, app mutation: 44774125059, ci-red/8b-app, 9b2a718

- Date: 2026-11-20 (createdAt 2026-11-20T16:30:48Z, push). Branch ci-red/8b-app. Commit 9b2a718135e95a55ad16272b7f25e91e2c8c82bd.
  URL: https://github.com/Deepak1365/relayguard/actions/runs/44774125059
- Mutation (verbatim from `git show 9b2a718`, internal/app/apply.go):

```text
-		credits := inv.AmountPaid / 100
+		credits := inv.AmountPaid / 100 * 2 // RED PROOF: deliberate app bug (double credit)
```

- Conclusion: failure.

| Job | Conclusion | startedAt | completedAt | seconds |
| --- | --- | --- | --- | --- |
| test | failure | 2026-11-20T16:30:52Z | 2026-11-20T16:31:42Z | 50 |
| chaos | failure | 2026-11-20T16:30:52Z | 2026-11-20T16:31:40Z | 48 |

- First non-success step, test job: "Go tests (including DB tests)" (failure). Chaos job: "R1 crash run (RelayGuard, must be clean)" (failure).
  In the chaos job, R2 and "Stop RelayGuard receiver" and N1 concluded `skipped` (they follow the failed R1 step), and "Upload chaos output" ran and succeeded.
  So no R2 and no N1 result exists for this run. (The plan, docs/phase8-plan.md:116, anticipated that the test job may also go red.)
- Test job, Go tests step: 98 `--- PASS: TestDB`, 31 `--- FAIL: TestDB` (31 top-level `--- FAIL: ` in total), 0 `--- SKIP`, 175 top-level `--- PASS: `.
  First six FAIL lines (verbatim), from 31:

```text
--- FAIL: TestDBApplyWritesChangeLog (0.03s)
--- FAIL: TestDBReplayedDeadEventApplies (0.03s)
--- FAIL: TestDBUndoLaterInvoiceAfterUndo (0.03s)
--- FAIL: TestDBChangeLogCheckCleanHistory (0.04s)
--- FAIL: TestDBChangeLogCheckDetectsUnloggedWrite (0.04s)
--- FAIL: TestDBChangeLogCheckDetectsBrokenImageLink (0.03s)
```

- Chaos RESULT and GATE lines (verbatim):

```text
chaos	R1 crash run (RelayGuard, must be clean) RESULT target=relayguard seeds=3 failing_seeds=3 violations=144 restarts=33 hard_kills=3
chaos	R1 crash run (RelayGuard, must be clean) GATE FAIL (clean): chaos exit code was 1, expected 0
```

### 7. 8b red proof B, checker no-op: 44774125003, ci-red/8b-checker, eb06742

- Date: 2026-11-20 (createdAt 2026-11-20T16:30:48Z, push). Branch ci-red/8b-checker. Commit eb0674230e5253b450aa043a098ada71c9b09b5d.
  URL: https://github.com/Deepak1365/relayguard/actions/runs/44774125003
- Mutation (verbatim from `git show eb06742`): `sim.CheckState` returns `nil, 0, nil`.

```text
-	return checkState(ctx, pool, seedScope(seed), exp)
+	return nil, 0, nil // RED PROOF: deliberate checker no-op
```

- Conclusion: failure.

| Job | Conclusion | startedAt | completedAt | seconds |
| --- | --- | --- | --- | --- |
| test | success | 2026-11-20T16:30:52Z | 2026-11-20T16:31:39Z | 47 |
| chaos | failure | 2026-11-20T16:30:52Z | 2026-11-20T16:31:55Z | 63 |

- First non-success step: test job none; chaos job "N1 negative control (naive, must report violations)" (failure).
- Test job, Go tests step: 129 `--- PASS: TestDB`, 0 `--- FAIL: `, 0 `--- SKIP`, 206 top-level `--- PASS: `. **The test job stayed green**: no Go test
  exercised `sim.CheckState` directly. The commit message of 446218e states the same ("No Go test called CheckState").
- Chaos RESULT and GATE lines (verbatim):

```text
chaos	R1 crash run (RelayGuard, must be clean) RESULT target=relayguard seeds=3 failing_seeds=0 violations=0 restarts=32 hard_kills=2
chaos	R1 crash run (RelayGuard, must be clean) GATE PASS (clean): exit 0, seeds=3 failing_seeds=0 violations=0
chaos	R2 poison run (RelayGuard, must be clean) RESULT target=relayguard seeds=1 failing_seeds=0 violations=0 restarts=28 hard_kills=2
chaos	R2 poison run (RelayGuard, must be clean) GATE PASS (clean): exit 0, seeds=1 failing_seeds=0 violations=0
chaos	N1 negative control (naive, must report violations) RESULT target=naive seeds=3 failing_seeds=0 violations=0 restarts=35 hard_kills=1
chaos	N1 negative control (naive, must report violations) GATE FAIL (violations): chaos exit code was 0, expected 1
```

- Interpretation, as the plan intended: R1 and R2 are green here only because the broken checker reports nothing, so green R1/R2 would be
  worthless without N1. N1 (naive, which must show violations) is what went red. This is the reason the negative control exists.

### 8. After the gap fix: 44782779433, main, 446218e

- Date: 2026-11-20 (createdAt 2026-11-20T17:23:07Z, push). Branch main. Commit 446218e652b6392cc53c3ef6ad07f8aefb12a168
  ("Phase 8: DB test for sim.CheckState (gap found by red proof ci-red/8b-checker)"; adds TestDBCheckStateRules).
  URL: https://github.com/Deepak1365/relayguard/actions/runs/44782779433
- Conclusion: success.

| Job | Conclusion | startedAt | completedAt | seconds |
| --- | --- | --- | --- | --- |
| test | success | 2026-11-20T17:23:10Z | 2026-11-20T17:23:49Z | 39 |
| chaos | success | 2026-11-20T17:23:10Z | 2026-11-20T17:24:11Z | 61 |

- First non-success step: none in either job.
- Test job, Go tests step: 130 `--- PASS: TestDB`, 0 `--- FAIL: `, 0 `--- SKIP`, 207 top-level `--- PASS: `. One more than b20348a (129), consistent with the one added
  test. The new test's line (verbatim):

```text
--- PASS: TestDBCheckStateRules (0.07s)
```

- Chaos RESULT and GATE lines (verbatim):

```text
chaos	R1 crash run (RelayGuard, must be clean) RESULT target=relayguard seeds=3 failing_seeds=0 violations=0 restarts=32 hard_kills=2
chaos	R1 crash run (RelayGuard, must be clean) GATE PASS (clean): exit 0, seeds=3 failing_seeds=0 violations=0
chaos	R2 poison run (RelayGuard, must be clean) RESULT target=relayguard seeds=1 failing_seeds=0 violations=0 restarts=26 hard_kills=2
chaos	R2 poison run (RelayGuard, must be clean) GATE PASS (clean): exit 0, seeds=1 failing_seeds=0 violations=0
chaos	N1 negative control (naive, must report violations) RESULT target=naive seeds=3 failing_seeds=3 violations=62 restarts=35 hard_kills=0
chaos	N1 negative control (naive, must report violations) GATE PASS (violations): exit 1, naive seeds=3 violations=62 (checker still sees the flaws)
```

- Note: this run shows that the green-run gap is closed only in the sense that a test now exists. Whether TestDBCheckStateRules fails under the `ci-red/8b-checker`
  mutation was **not re-run in CI**: no red-proof run exists for it. The 446218e commit message says six checker mutations each fail it (locally, not recorded here).

## Observations and unexpected things

- The same chaos configuration gave different counts in the three runs where R1 was green (sections 5, 7, 8): R1 restarts 35, 32, 32 and hard_kills 4, 2, 2;
  R2 restarts 28, 28, 26. In the two runs with a working checker (sections 5 and 8) N1 violations were 60 and 62 and N1 hard_kills 1 and 0. That is consistent with run-to-run variation of wall-clock kill timing, and is one reason these numbers go in no table.
- Run 6 (8b-app): the test job also went red, as the plan allowed. Run 7 (8b-checker): the test job did NOT catch the mutation; that gap was real and led to 446218e.
- The "before" run does not display the skip (no `-v`); see section 1. Its runner image was `ubuntu-26.04` (`ubuntu-latest` at the time), later runs `ubuntu-24.04`.
- In run 6 the chaos job's R2 and N1 were skipped after R1 failed, so that run provides no information about them.
- In run 4 (8a-skip) the Go tests step itself is green with 121 skipped tests; only the Skip guard step catches it. That is the case the guard exists for.
- Largest job duration among all 8 runs: 69 s (chaos, run 5), versus the 1200 s `timeout-minutes` cap. Chaos still has its own per-seed cap of 3 minutes (cmd/chaos/main.go:66,
  per docs/phase8-plan.md:85); no run came near either.
