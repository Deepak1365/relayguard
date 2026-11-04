# RelayGuard: notes for Claude Code

## What this project is
RelayGuard is a crash-safe inbound webhook processor (Go + Postgres) built as a portfolio project.
Goal: apply each webhook's effect exactly once, even through crashes, duplicates, and out-of-order delivery,
and prove it with measured experiments against a deliberately naive baseline.

## Environment
- Native Windows, PowerShell. Project lives at C:\dev\relayguard.
- Postgres 17 runs in Docker (docker-compose.yml), exposed on localhost:5433.
- Settings come from .env (loaded by internal/config). Never print, log, or commit .env or the whsec_ secret.
- All common tasks go through dev.ps1. Run `.\dev.ps1` with no arguments to list them.

## Layout
- cmd/receiver: smoke-test receiver (will become the real RelayGuard receiver in phase 2)
- cmd/naive: the BASELINE receiver. Its flaws are deliberate. Do NOT fix or refactor it.
- cmd/migrate + migrations/: goose migrations, embedded in the binary
- testdata/events/: synthetic Stripe-format fixtures (ids start with evt_fx_); README lists correct outcomes
- docs/baseline-failures.md: recorded results of the baseline experiments

## Rules
- Ask before running anything destructive: `.\dev.ps1 reset` (deletes the database volume), `migrate-down`,
  `git push --force`, or deleting files. `.\dev.ps1 reset-data` is fine during experiments.
- The receiver and `stripe listen` are long-running; the user usually runs them in their own terminals.
  Check `/healthz` on 127.0.0.1:8080 before sending events instead of starting a second copy.
- Report experiment results exactly as observed. Label synthetic results as synthetic. Never invent numbers.
- Explain what each change does and why, so the user can discuss it in interviews.
- Commit in small, descriptive steps; show the diff summary before committing.
