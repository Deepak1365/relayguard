# RelayGuard

Crash-safe processing for inbound webhooks: each event's effect is applied exactly once,
even through crashes, duplicates, and out-of-order delivery.

> Status: setup / phase 1. Nothing below is measured yet.

## Quick start (Windows PowerShell)

```powershell
Copy-Item .env.example .env
.\dev.ps1 up          # start Postgres in Docker
.\dev.ps1 receiver    # terminal 1
.\dev.ps1 listen      # terminal 2 (copy the whsec_ secret into .env)
.\dev.ps1 trigger     # terminal 3: send a test event
```

On macOS/Linux the same commands work directly: `docker compose up -d`, `go run ./cmd/receiver`, and so on.

## Layout

- `cmd/` runnable programs (receiver, later worker and chaos harness)
- `internal/` application code
- `migrations/` SQL schema changes
- `docs/decisions/` short notes on design decisions
- `dashboard/` React UI (added later)
