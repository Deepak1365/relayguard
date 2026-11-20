package sim

// DB test for Reset, which every chaos run (both targets) calls first.
// Skipped unless DB tests are enabled; see internal/dbtest.

import (
	"context"
	"testing"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// Once change_log and admin_actions reference inbox, a TRUNCATE that names
// inbox but not them is refused by Postgres, and every chaos run would fail
// at its first step. Reset must wipe all of them together.
func TestDBResetWithChangeLogRows(t *testing.T) {
	pool := dbtest.Pool(t)
	inboxID := dbtest.InsertEvent(t, pool, "sim_reset_1", "customer.created", dbtest.CustomerObject("cus_dbtest_reset"))
	dbtest.InsertAuditRows(t, pool, inboxID)

	if err := Reset(context.Background(), pool); err != nil {
		t.Fatalf("Reset with change_log/admin_actions rows present: %v", err)
	}
	for _, table := range []string{"change_log", "admin_actions", "inbox", "customers", "credit_grants", "subscriptions"} {
		if n := dbtest.Count(t, pool, table); n != 0 {
			t.Fatalf("%s after Reset: %d rows, want 0", table, n)
		}
	}
}

// outbox references inbox too: Reset must name it, or every chaos run fails at
// its first step once an outbox row exists.
func TestDBResetWithOutboxRows(t *testing.T) {
	pool := dbtest.Pool(t)
	id := dbtest.InsertEvent(t, pool, "sim_reset_ob", "customer.created", dbtest.CustomerObject("cus_dbtest_reset_ob"))
	if got := dbtest.ApplyInbox(t, pool, id); got != "done" {
		t.Fatalf("apply: %s", got)
	}
	if n := dbtest.Count(t, pool, "outbox"); n != 1 {
		t.Fatalf("outbox before Reset: %d rows, want 1 (the welcome email)", n)
	}

	if err := Reset(context.Background(), pool); err != nil {
		t.Fatalf("Reset with outbox rows present: %v", err)
	}
	for _, table := range []string{"outbox", "change_log", "inbox", "customers"} {
		if n := dbtest.Count(t, pool, table); n != 0 {
			t.Fatalf("%s after Reset: %d rows, want 0", table, n)
		}
	}
}
