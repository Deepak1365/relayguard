# RelayGuard dev commands for Windows PowerShell.
# Usage: .\dev.ps1 <task> [path] [count]    e.g.  .\dev.ps1 send testdata\events\invoice_paid.json 2
param(
    [Parameter(Position = 0)][string]$Task = "help",
    [Parameter(Position = 1)][string]$Path,
    [Parameter(Position = 2)][int]$Count = 1
)

$Url = "http://127.0.0.1:8080/webhooks/stripe"

switch ($Task) {
    "up"             { docker compose up -d }
    "down"           { docker compose down }
    "reset"          { docker compose down -v }   # deletes ALL database data
    "psql"           { docker compose exec db psql -U relayguard -d relayguard }
    "migrate"        { go run ./cmd/migrate up }
    "migrate-status" { go run ./cmd/migrate status }
    "migrate-down"   { go run ./cmd/migrate down }
    "receiver"       { go run ./cmd/receiver }
    "naive"          { go run ./cmd/naive }
    "listen"         { stripe listen --all-snapshot --forward-to 127.0.0.1:8080/webhooks/stripe }
    "trigger"        { stripe trigger customer.created }
    "send" {
        if (-not $Path) { Write-Host "Usage: .\dev.ps1 send <file.json> [count]"; break }
        for ($i = 1; $i -le $Count; $i++) {
            try {
                $r = Invoke-WebRequest -Method Post -Uri $Url -ContentType "application/json" -InFile $Path -UseBasicParsing
                Write-Host "attempt $i -> HTTP $($r.StatusCode)"
            } catch {
                if ($_.Exception.Response) {
                    Write-Host "attempt $i -> HTTP $($_.Exception.Response.StatusCode.value__)"
                } else {
                    Write-Host "attempt $i -> no response ($($_.Exception.Message))"
                }
            }
        }
    }
    "state" {
        docker compose exec db psql -U relayguard -d relayguard -P pager=off `
            -c "SELECT c.stripe_customer_id, c.credit_balance, count(g.id) AS grants, COALESCE(sum(g.amount), 0) AS ledger_total FROM customers c LEFT JOIN credit_grants g ON g.customer_id = c.id GROUP BY c.id ORDER BY c.id;" `
            -c "SELECT stripe_subscription_id, status, updated_at FROM subscriptions ORDER BY id;"
    }
    "reset-data" {
        docker compose exec db psql -U relayguard -d relayguard -c "TRUNCATE subscriptions, credit_grants, customers RESTART IDENTITY;"
    }
    "test"           { go vet ./...; if ($LASTEXITCODE -eq 0) { go test ./... } }
    default {
        Write-Host "Tasks:"
        Write-Host "  up / down / reset      Postgres (reset DELETES data)"
        Write-Host "  psql                   SQL shell"
        Write-Host "  migrate                apply schema (also: migrate-status, migrate-down)"
        Write-Host "  receiver               smoke-test receiver"
        Write-Host "  naive                  NAIVE baseline receiver"
        Write-Host "  listen / trigger       Stripe CLI forwarding / test event"
        Write-Host "  send <file> [count]    POST a fixture event, optionally several times"
        Write-Host "  state                  show balances, ledger and subscriptions"
        Write-Host "  reset-data             empty the app tables (keeps schema)"
        Write-Host "  test                   go vet + go test"
    }
}
