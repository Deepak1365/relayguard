# RelayGuard dev commands for Windows PowerShell.
# Usage: .\dev.ps1 <task>    e.g.  .\dev.ps1 up
param([Parameter(Position = 0)][string]$Task = "help")

switch ($Task) {
    "up"       { docker compose up -d }
    "down"     { docker compose down }
    "reset"    { docker compose down -v }   # deletes the database data
    "psql"     { docker compose exec db psql -U relayguard -d relayguard }
    "receiver" { go run ./cmd/receiver }
    "listen"   { stripe listen --forward-to 127.0.0.1:8080/webhooks/stripe }
    "trigger"  { stripe trigger payment_intent.succeeded }
    "test"     { go vet ./...; if ($LASTEXITCODE -eq 0) { go test ./... } }
    default {
        Write-Host "Tasks:"
        Write-Host "  up        start Postgres (Docker)"
        Write-Host "  down      stop Postgres, keep data"
        Write-Host "  reset     stop Postgres and DELETE its data"
        Write-Host "  psql      open a SQL shell in the database"
        Write-Host "  receiver  run the webhook receiver"
        Write-Host "  listen    forward Stripe test events to the receiver"
        Write-Host "  trigger   send a sample Stripe test event"
        Write-Host "  test      go vet + go test"
    }
}
