# Phase 8 plan: CI that re-proves the claims

Planner (Opus) draft 2026-11-19. APPROVED by the user 2026-11-19: all defaults Q1-Q10.

Goal: today the crash-safety claims are re-checked only when someone runs `dev.ps1 db-test` and a chaos smoke by
hand on the Windows dev machine. Phase 8 makes every push re-run the DB tests and a short crash/poison chaos run
against real Postgres 17, red on any harness violation, and red if the checks silently stopped checking.

## Starting point (cited)
- `.github/workflows/ci.yml:1-14`: one job `test` on ubuntu-latest: checkout@v4 (tag, not SHA), `bash
  scripts/secret-scan.sh`, setup-go@v5 (`go-version-file: go.mod`, Go 1.26.0, go.mod:3), `go vet ./...`,
  `go test ./...`. No `permissions:`, no Postgres, no chaos. Without `RELAYGUARD_DB_TESTS=1` every DB test skips
  (internal/dbtest/dbtest.go:73-74), so today's green runs (checked: 192baae and the 4 before it, success) skip all
  TestDB*.
- `.gitattributes`: `* text=auto eol=lf`, so `gofmt -l` on Linux sees LF files.
- DB tests: all go through `dbtest.Pool` (its `t.Skip`, dbtest.go:74, is the only `t.Skip` in the repo);
  cross-process advisory lock (dbtest.go:55-70); `Truncate` refuses foreign inbox rows (dbtest.go:118-139).
  Chaos `sim.Reset` truncates the same tables without a guard (internal/sim/check.go:24-25).
- `dev.ps1 db-test` (dev.ps1:56-62) uses a hand-kept list of 5 packages with `-p 1`; that list drifted once.
- Settings: binaries call `config.LoadDotEnv(".env")`; a missing file is fine and real env vars win
  (internal/config/dotenv.go:15-19, 37). CI needs no .env.
- Receiver refuses an empty secret or the placeholder `whsec_replace_me` (cmd/receiver/main.go:38-41); chaos needs
  `STRIPE_WEBHOOK_SECRET` to sign (cmd/chaos/main.go:94-97); the naive receiver does not read it.
- Chaos exit codes: 0 no violations, 1 violations (main.go:210-213), 2 setup/run error (main.go:131-132,
  150-152), BUT `log.Fatal` (flags, DB connect, missing secret; main.go:76-101) also exits 1. Exit 1 alone does
  not mean "violations found". The RESULT line (main.go:183-184) prints only after all seeds ran.
- Portability: the supervisor uses `exec.Command` and `Process.Kill`, nothing Windows-specific
  (internal/sim/supervise.go:54, 93, 106); faults exit via `os.Exit(1)` (internal/fault/fault.go:74); `.exe` in
  cmd/chaos/main_test.go:41-45 are flag values only; supervisor log is `bin/supervised.log` (main.go:110).
  `dev.ps1 build` does not build the receiver (dev.ps1:68).

## Design
**Runner: `ubuntu-24.04`, plain bash, no pwsh.** Service containers run only on Linux runners. Linux needs only
binary names without `.exe` and a receiver binary. Rejected: dev.ps1 under pwsh on Linux (it uses `docker compose
exec` and hard-codes `bin\*.exe`); a Go-native CI driver (more code than ~40 lines YAML + one gate script).
Pinned 24.04 so an image switch does not change results under us (Q2).

**Two parallel jobs, each with its own Postgres 17 service.** `test`: secret scan, gofmt, vet, migrate, all Go
tests incl. DB tests. `chaos`: migrate, build, chaos runs. Separate databases: DB-test TRUNCATE and chaos Reset
never share tables; the truncate guard can never refuse a leftover chaos inbox. Cost: a second container start.
Rejected: one job, db-test then chaos (slower wall time, order-coupled) (Q3).

**Postgres service:** `postgres:17` (as docker-compose.yml:4), same non-secret credentials, `ports: 5433:5432`, so
`DATABASE_URL` equals the documented dev URL (.env.example:2). Health: `pg_isready -U relayguard -d relayguard`,
interval 5s, timeout 3s, 10 retries. URL in job `env:`, not a GitHub secret (throwaway container).

**Webhook secret: generated per run, never a real one, no GitHub secrets.**
`s="whsec_ci_$(openssl rand -hex 24)"; echo "::add-mask::$s"; echo "STRIPE_WEBHOOK_SECRET=$s" >> "$GITHUB_ENV"`.
Receiver and chaos inherit it. Rejected: a fixed literal in YAML (keeps secret-scan meaningful, scripts/secret-scan.sh:22).

**Test job (fail fast):** checkout; `bash scripts/secret-scan.sh`; setup-go; `test -z "$(gofmt -l .)"` (print
the list if not); `go vet ./...`; `go run ./cmd/migrate up` then `status` (logged);
`RELAYGUARD_DB_TESTS=1 go test -count=1 -p 1 -v ./... 2>&1 | tee ci-out/go-test.log` (`set -o pipefail`);
**skip guard:** fail if the log has the dbtest skip message or no `--- PASS: TestDB` line. `./...` so no DB
package is left out; `-p 1` as dev.ps1:60-61. No fixed test-count floor.

**Chaos job:** checkout, setup-go; migrate up; generate secret; `go build -o bin/<name> ./cmd/<name>` for
receiver, worker, naive, chaos; start `./bin/receiver > ci-out/receiver.log 2>&1 &` (save PID; chaos waits up
to 15s for /healthz, main.go:130);
- R1 crash (flags of results/phase6-relayguard-crash.md:5, seeds 1-3):
  `./bin/chaos -target relayguard -seeds 1-3 -dup 0.2 -reorder 10 -supervise bin/worker -crash-rate 0.1 -kill-interval 2s -lease 1s -out ci-out/relayguard-crash.md`
- R2 poison: same flags with `-seeds 1 -poison 2 -out ci-out/relayguard-poison.md` (Q4)
- stop the receiver (`kill $PID`), freeing 8080
- N1 negative control (flags of results/phase6-naive-crash.md:5):
  `./bin/chaos -target naive -seeds 1-3 -dup 0.2 -reorder 10 -supervise bin/naive -crash-rate 0.1 -kill-interval 2s -out ci-out/naive-crash.md`
- upload `ci-out/` and `bin/supervised.log`, `if: always()`, `retention-days: 14`.
Each chaos command tees to `ci-out/*.log`, captures its own exit code (`set +e` or `|| code=$?` under bash -e
-o pipefail), then calls the gate.

**Gate `scripts/ci-chaos-gate.sh <clean|violations> <expected_seeds> <exit_code> <log>`.** `clean` passes only
if exit 0 AND exactly one `RESULT target=relayguard` line containing `seeds=<n> failing_seeds=0 violations=0`.
`violations` passes only if exit 1 AND exactly one `RESULT target=naive seeds=<n>` line with violations >= 1.
This closes: a `log.Fatal` (exit 1, no RESULT) counting as "naive failed as expected"; a future change exiting 0
after zero seeds. The naive control needs >= 1 violation, not 3 failing seeds (E5 had 20/20 failing,
results/phase6-naive-crash.md:11, but requiring all seeds is a flakiness risk the claim does not need).
Rejected: a `-expect` flag in cmd/chaos (would change exit-code meaning that earlier results docs rely on).

**Why the negative control.** "CI fails if the harness reports any violation" (ROADMAP) is necessary, not
sufficient: a checker broken into always reporting 0 keeps CI green. The naive run proves on every push that the
checker still sees the failures it exists to find. cmd/naive is unchanged (rule 4).

**Flakiness policy.** Gate only on `failing_seeds`, `violations`, `seeds`, exit code; never on restarts, hard
kills or the done/stale split (known variation, docs/phase5-results.md:738-741). No automatic retries, no
`continue-on-error` on chaos steps: a red run is investigated and recorded, not re-run until green. Timeouts:
chaos caps each seed's wait at 3 min (main.go:66); `timeout-minutes: 20` per job as a backstop (a cap, not a
measurement; real durations recorded in 8c). Concurrency: `group: ci-${{ github.ref }}`,
`cancel-in-progress: ${{ github.ref != 'refs/heads/main' }}`.

**Hardening.** Top-level `permissions: contents: read`. Every `uses:` pinned to a full commit SHA with a
`# vX.Y.Z` comment (checkout, setup-go, upload-artifact), resolved with
`gh api repos/actions/<name>/git/ref/tags/<tag>`, never guessed. Image `postgres:17` tag (Q5). Triggers: `push`
(all branches, for the red-proof branches), `pull_request`, `workflow_dispatch`.

**Not evidence, a regression signal.** CI builds from an exact commit but on GitHub-hosted hardware with
wall-clock kill timing; its counts are not comparable to local results and go in no results table. Recorded in 8c:
proof that the gates work (run URLs, commits, conclusions, RESULT lines verbatim from logs; artifacts expire). The
badge means "re-checked on every push", not "here are the numbers".

## Steps (one commit each; checkpoint B before each commit and push)
### 8a: test job with Postgres (implementer sonnet)
ci.yml: permissions, concurrency, triggers, SHA pins, Postgres service, gofmt, migrate, DB tests, skip guard,
upload `ci-out/go-test.log` on failure.
Acceptance: (1) main run green; log shows `migrate status` all applied and `--- PASS: TestDB` lines, no skip
message. (2) Red proof: throwaway branch `ci-red/8a-db` with a mutation a DB test is known to catch (e.g. remove
CrossOriginProtection -> handlers_db_test.go:234); the test job must go red at the go-test step. (3) Guard proof:
branch `ci-red/8a-skip` removes `RELAYGUARD_DB_TESTS=1`; red at the skip guard. Branches never merged; push and
delete each with the user's OK (rule 6).

### 8b: chaos job + gate script (implementer sonnet)
scripts/ci-chaos-gate.sh; scripts/ci-chaos-gate-test.sh running the gate on canned logs in
scripts/testdata/ci-gate/ (labelled SYNTHETIC): clean pass; violations>0 under clean; seeds mismatch; missing
RESULT with exit 1 (Fatal) under violations; naive with violations=0; valid naive control. It also runs as the
chaos job's first step. ci.yml gains the `chaos` job.
Acceptance: (1) gate self-test passes locally (Git Bash) and in CI. (2) On main: R1 and R2 clean, N1 has
violations, job green. (3) Red proof A: `ci-red/8b-app` app mutation (e.g. credit grant applied twice); R1 red
with violations (8a test job may also go red; noted). (4) Red proof B: `ci-red/8b-checker` checker no-op
(`sim.CheckState` returns none); R1 stays green and N1 must go red.

### 8c: badge, evidence, decision (lead; experiment-runner sonnet for the record; interview-coach sonnet)
README badge (`actions/workflows/ci.yml/badge.svg?branch=main`, linked) plus one line on what CI re-checks.
docs/phase8-results.md: per proof run (8a green, 8a-db, 8a-skip, 8b green, 8b-app, 8b-checker): date, branch,
commit, run URL, conclusion, failing step, RESULT lines verbatim, job durations as GitHub reports them; states
GitHub-hosted runs are not comparable to local evidence. docs/decisions/0007-ci.md (Q8). Interview notes. ROADMAP.
Acceptance: badge green on main; evidence review APPROVED.

8a implementation note (2026-11-19, review r1 APPROVED): pins checkout v7.0.1 3d3c42e5aac5ba805825da76410c181273ba90b1,
setup-go v7.1.0 d8cbb8045d7f43706b9177cceb36c1f91a6644d5, upload-artifact v7.0.2 cf430e030ddbb5b0abf93d22962f4752f3646cd9
(reviewer re-resolved all three). Deviation: concurrency group is `ci-${{ github.ref == 'refs/heads/main' &&
github.sha || github.ref }}` because with one group per ref a queued main run can be replaced by a newer one
(only one pending run per group); per-SHA groups on main make "every main commit gets a verdict" true. Upload has
`if-no-files-found: ignore`. Known, for 8c/0007: CI uses Go 1.26.0 from go.mod (no toolchain line) while local
builds used go1.27.4; record the first real job durations against the 20-min cap.

Regression per step: `/verify` locally (reset-data before db-test). The workflow itself becomes the new regression.

## Risks
- Timing-sensitive DB tests (TestDBBusy503, lock_timeout tests) were validated on the dev machine; shared 2-vCPU
  runners may expose flakiness: investigate, record, fix the test's assumption, no retries.
- `WaitIdle` timeouts on a slow runner show as `unprocessed_event`; record and judge; raise `-timeout` only with a
  stated reason.
- Linux has never run the DB tests or chaos: a first red may be a real portability finding; report it.
- SHA pins go stale (Q6). Wall-clock kill timing: a green run is one sample, not proof of absence.

## Decisions (approved 2026-11-19, all defaults)
- Q1 Linux only, no Windows job. Q2 `ubuntu-24.04`. Q3 two parallel jobs, own Postgres each.
- Q4 chaos: crash seeds 1-3, poison seed 1, naive control seeds 1-3; no undo-all (add later only if job time allows).
- Q5 `postgres:17` tag, not digest. Q6 no Dependabot (bump pins manually in Final polish). Q7 no `-race` in Phase 8.
- Q8 short decision record 0007. Q9 repo is PUBLIC (checked); no secrets; Actions enabled, workflow permissions
  Read; no branch protection. Q10 `ci-red/*` proof branches, pushed and deleted with the user's OK each time.

## Likely interview questions
- How do you know CI is actually checking anything? (skip guard; naive negative control; red-proof runs)
- Why not retry flaky chaos runs? Why is the exit code alone not enough? (`log.Fatal` also exits 1)
- Why separate databases per job rather than ordering? Where does the webhook secret come from in CI?
- Why pin actions to SHAs, and what does `contents: read` prevent? Are CI numbers evidence? How would you run
  this on Windows?
