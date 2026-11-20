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
        # change_log, admin_actions and outbox reference inbox, so they must go in the
        # same TRUNCATE. Wiping the admin audit here is a deliberate choice:
        # its rows point at inbox ids that RESTART IDENTITY reuses.
        Invoke-Sql @("TRUNCATE outbox, change_log, admin_actions, inbox, subscriptions, credit_grants, customers RESTART IDENTITY;")
    }
    "test"           { go vet ./...; if ($LASTEXITCODE -eq 0) { go test ./... }; exit $LASTEXITCODE }
    "db-test" {
        # DB integration tests against the dev database. They TRUNCATE the
        # inbox, app, change_log, admin_actions and outbox tables (and refuse if the inbox has rows they did not
        # create). Uses DATABASE_URL if set, otherwise the non-secret dev URL
        # from .env.example. The URL is never printed.
        # Before running: stop any worker (it polls and could claim the test
        # rows), never run during an experiment, and make sure the inbox holds
        # only evt_dbtest_ rows (or is empty).
        if (-not $env:DATABASE_URL) {
            $env:DATABASE_URL = "postgres://relayguard:relayguard@localhost:5433/relayguard?sslmode=disable"
        }
        $env:RELAYGUARD_DB_TESTS = "1"
        # -p 1: the packages share the same tables, so they must not run in
        # parallel or one package's TRUNCATE wipes another's rows mid-test.
        go test -count=1 -p 1 -v ./cmd/worker/ ./internal/dbtest/ ./internal/sim/ ./internal/admin/ ./internal/adminhttp/
        exit $LASTEXITCODE
    }
    "build" {
        # Stop at the first failure: checking only the last exit code would
        # report success after an earlier binary failed to build.
        foreach ($name in @("worker", "naive", "chaos", "admin")) {
            go build -o "bin\$name.exe" "./cmd/$name"
            if ($LASTEXITCODE -ne 0) { Write-Host "build of bin\$name.exe failed"; exit $LASTEXITCODE }
        }
        Write-Host "built bin\worker.exe, bin\naive.exe, bin\chaos.exe, bin\admin.exe"
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
        Write-Host "  reset-data             empty app tables, inbox, change_log, admin_actions, outbox (keeps schema)"
        Write-Host "  test                   go vet + go test"
        Write-Host "  db-test                DB integration tests (TRUNCATES inbox, app, change_log, admin_actions, outbox)"
        Write-Host "  build                  build worker, naive, chaos and admin into bin\"
    }
}
