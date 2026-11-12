package dbtest

// Schema tests for migration 00004. Skipped unless DB tests are enabled (see
// the package comment); run them through `dev.ps1 db-test`.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Deepak1365/relayguard/internal/db"
)

// The undo path (phase 6, step 6) refuses when a row's row_version moved
// since the change it wants to revert. That only works if EVERY update bumps
// it, including ones that try to set row_version themselves and ones that
// change nothing, so the trigger, not application code, owns the column.
func TestDBRowVersionTrigger(t *testing.T) {
	pool := Pool(t)
	ctx := context.Background()

	var customerID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (stripe_customer_id, email) VALUES ('cus_dbtest_rv', 'rv@example.test')
		RETURNING id`).Scan(&customerID); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	var grantID, subID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO credit_grants (customer_id, stripe_invoice_id, amount) VALUES ($1, 'in_dbtest_rv', 20)
		RETURNING id`, customerID).Scan(&grantID); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO subscriptions (stripe_subscription_id, customer_id, status) VALUES ('sub_dbtest_rv', $1, 'active')
		RETURNING id`, customerID).Scan(&subID); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}

	// table and the SQL are literals from this test, never input.
	cases := []struct {
		table    string
		id       int64
		change   string // a real change
		forced   string // tries to set row_version directly
		noChange string // writes the same value back
	}{
		{"customers", customerID,
			`UPDATE customers SET credit_balance = credit_balance + 20 WHERE id = $1`,
			`UPDATE customers SET row_version = 100 WHERE id = $1`,
			`UPDATE customers SET email = email WHERE id = $1`},
		{"credit_grants", grantID,
			`UPDATE credit_grants SET amount = amount + 1 WHERE id = $1`,
			`UPDATE credit_grants SET row_version = 100 WHERE id = $1`,
			`UPDATE credit_grants SET amount = amount WHERE id = $1`},
		{"subscriptions", subID,
			`UPDATE subscriptions SET status = 'canceled' WHERE id = $1`,
			`UPDATE subscriptions SET row_version = 100 WHERE id = $1`,
			`UPDATE subscriptions SET status = status WHERE id = $1`},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			version := func() int64 {
				t.Helper()
				var v int64
				if err := pool.QueryRow(ctx, `SELECT row_version FROM `+c.table+` WHERE id = $1`, c.id).Scan(&v); err != nil {
					t.Fatalf("read row_version: %v", err)
				}
				return v
			}
			exec := func(sql string) {
				t.Helper()
				if _, err := pool.Exec(ctx, sql, c.id); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}

			if v := version(); v != 1 {
				t.Fatalf("after INSERT: row_version %d, want 1 (column default)", v)
			}
			exec(c.change)
			if v := version(); v != 2 {
				t.Fatalf("after a real UPDATE: row_version %d, want 2", v)
			}
			exec(c.forced)
			if v := version(); v != 3 {
				t.Fatalf("after UPDATE SET row_version = 100: row_version %d, want 3 (trigger must override)", v)
			}
			exec(c.noChange)
			if v := version(); v != 4 {
				t.Fatalf("after a no-op UPDATE: row_version %d, want 4 (every UPDATE bumps)", v)
			}
		})
	}
}

// change_log and admin_actions reference inbox; Truncate must still empty
// everything (and reset ids) while such rows exist.
func TestDBTruncateWithChangeLogRows(t *testing.T) {
	pool := Pool(t)
	inboxID := InsertEvent(t, pool, "fk_truncate_1", "customer.created", CustomerObject("cus_dbtest_fk"))
	InsertAuditRows(t, pool, inboxID)
	if n := Count(t, pool, "change_log"); n != 2 {
		t.Fatalf("change_log before truncate: %d, want 2", n)
	}

	Truncate(t, pool)

	for _, table := range []string{"change_log", "admin_actions", "inbox", "customers", "credit_grants", "subscriptions"} {
		if n := Count(t, pool, table); n != 0 {
			t.Fatalf("%s after truncate: %d rows, want 0", table, n)
		}
	}
	// RESTART IDENTITY: the next inbox row starts at 1 again.
	if id := InsertEvent(t, pool, "fk_truncate_2", "customer.created", CustomerObject("cus_dbtest_fk2")); id != 1 {
		t.Fatalf("inbox id after truncate: %d, want 1 (identity restarted)", id)
	}
}

// db.Connect must end up with a UTC session even when the URL asks for
// another timezone, in any spelling of the parameter name: change_log images
// render timestamptz in the session timezone (see db.SessionTimeZone).
func TestDBConnectForcesUTC(t *testing.T) {
	Pool(t) // skip unless DB tests are enabled; holds the test lock
	ctx := context.Background()
	base := os.Getenv("DATABASE_URL")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	// Without the case-insensitive override, a mixed-case key leaves two
	// timezone settings in the startup message, and which one Postgres keeps
	// follows Go's random map order (roughly a coin flip per connection). So
	// each spelling gets several fresh connections; missing the bug in all of
	// them has probability about 2^-attempts.
	const attempts = 16
	for _, param := range []string{"timezone", "TimeZone", "TIMEZONE"} {
		t.Run(param, func(t *testing.T) {
			for i := 0; i < attempts; i++ {
				pool, err := db.Connect(ctx, base+sep+param+"=Europe/Berlin")
				if err != nil {
					t.Fatalf("connect: %v", err)
				}
				var tz string
				err = pool.QueryRow(ctx, `SHOW TimeZone`).Scan(&tz)
				pool.Close()
				if err != nil {
					t.Fatal(err)
				}
				if tz != db.SessionTimeZone {
					t.Fatalf("URL with %s=Europe/Berlin, connection %d: session TimeZone %q, want %q",
						param, i+1, tz, db.SessionTimeZone)
				}
			}
		})
	}
}

// Pool must hold testLockKey for the whole test, so another package's DB test
// (in another process) waits instead of truncating these tables mid-test. A
// pool connection is a different session, so its try-lock must fail.
func TestDBPoolHoldsTestLock(t *testing.T) {
	pool := Pool(t)
	var got bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_try_advisory_lock($1)`, testLockKey).Scan(&got); err != nil {
		t.Fatalf("try advisory lock: %v", err)
	}
	if got {
		// Release it again so this session does not keep it after the test.
		// Best effort: the test is failing either way.
		_, _ = pool.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, testLockKey)
		t.Fatal("another session took the DB test lock while a test held it: tests are not serialised")
	}
}
