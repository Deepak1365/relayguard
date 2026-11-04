# RelayGuard dev commands for Windows PowerShell.
# Usage: .\dev.ps1 <task> [path] [count]    e.g.  .\dev.ps1 send testdata\events\invoice_paid.json 2
param(
    [Parameter(Position = 0)][string]$Task = "help",
    [Parameter(Position = 1)][string]$Path,
    [Parameter(Position = 2)][int]$Count = 1
)

function Invoke-Sql([string[]]$Queries) {
    $sqlArgs = @("compose", "exec", "db", "psql", "-U", "relayguard", "-d", "relayguard", "-P", "pager=off")
    foreach ($q in $Queries) { $sqlArgs += @("-c", $q) }
    & docker @sqlArgs
}

switch ($Task) {
    "up"             { docker compose up -d }
    "down"           { docker compose down }
    "reset"          { docker compose down -v }   # deletes ALL database data
    "psql"           { docker compose exec db psql -U relayguard -d relayguard }
    "migrate"        { go run ./cmd/migrate up }
    "migrate-status" { go run ./cmd/migrate status }
    "migrate-down"   { go run ./cmd/migrate down }
    "receiver"       { go run ./cmd/receiver }
    "worker"         { go run ./cmd/worker }
    "naive"          { go run ./cmd/naive }
    "listen"         { stripe listen --all-snapshot --forward-to 127.0.0.1:8080/webhooks/stripe }
    "trigger"        { stripe trigger customer.created }
    "send" {
        if (-not $Path) { Write-Host "Usage: .\dev.ps1 send <file.json> [count]"; break }
        go run ./cmd/sendevent -file $Path -n $Count
    }
    "state" {
        Invoke-Sql @(
            "SELECT c.stripe_customer_id, c.credit_balance, count(g.id) AS grants, COALESCE(sum(g.amount), 0) AS ledger_total FROM customers c LEFT JOIN credit_grants g ON g.customer_id = c.id GROUP BY c.id ORDER BY c.id;",
            "SELECT stripe_subscription_id, status, last_event_id, last_event_created FROM subscriptions ORDER BY id;"
        )
    }
    "inbox" {
        Invoke-Sql @("SELECT id, event_id, event_type, status, attempts, left(last_error, 70) AS last_error FROM inbox ORDER BY id;")
    }
    "reset-data" {
        Invoke-Sql @("TRUNCATE inbox, subscriptions, credit_grants, customers RESTART IDENTITY;")
    }
    "test"           { go vet ./...; if ($LASTEXITCODE -eq 0) { go test ./... } }
    "build" {
        go build -o bin\worker.exe ./cmd/worker
        go build -o bin\naive.exe ./cmd/naive
        go build -o bin\chaos.exe ./cmd/chaos
        if ($LASTEXITCODE -eq 0) { Write-Host "built bin\worker.exe, bin\naive.exe, bin\chaos.exe" }
    }
    default {
        Write-Host "Tasks:"
        Write-Host "  up / down / reset      Postgres (reset DELETES data)"
        Write-Host "  psql                   SQL shell"
        Write-Host "  migrate                apply schema (also: migrate-status, migrate-down)"
        Write-Host "  receiver               RelayGuard receiver (verify + store)"
        Write-Host "  worker                 RelayGuard worker (apply events)"
        Write-Host "  naive                  NAIVE baseline receiver (same port as receiver)"
        Write-Host "  listen / trigger       Stripe CLI forwarding / test event"
        Write-Host "  send <file> [count]    sign and POST a fixture event"
        Write-Host "  state                  balances, ledger, subscriptions"
        Write-Host "  inbox                  inbox rows: status, attempts, errors"
        Write-Host "  reset-data             empty app tables and inbox (keeps schema)"
        Write-Host "  test                   go vet + go test"
        Write-Host "  build                  build worker, naive and chaos into bin\"
    }
}
