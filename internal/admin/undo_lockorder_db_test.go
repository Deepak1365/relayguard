package admin

// Undo locks the event's rows in reverse seq order. That is deadlock-free
// only because of a property of internal/app, checked here for every event
// type it handles: an apply writes at most one row that existed before it
// (a customer or a subscription, the rows later events also lock), and that
// row comes LAST in its change_log; every earlier entry inserts a row of the
// event's own. So the undo's first lock is the only contended one, and it
// holds nothing else while it waits. If a new event type wrote an existing
// row first (or two existing rows), this fails and the lock order needs a
// fresh argument (or locks sorted by table rank and id).

import (
	"testing"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

func TestDBApplyWritesExistingRowLast(t *testing.T) {
	pool := dbtest.Pool(t)
	sub := dbtest.SubscriptionObject("sub_dbtest_lo", testCustomer, "active")
	events := []struct {
		name string
		id   func() int64
	}{
		{"customer.created", func() int64 { return appliedCustomer(t, pool) }},
		{"invoice.paid", func() int64 { return appliedInvoice(t, pool, "lo_inv", 2000) }},
		{"customer.subscription.created", func() int64 {
			return appliedEvent(t, pool, "lo_sc", "customer.subscription.created", dbtest.DefaultCreated, sub)
		}},
		{"customer.subscription.updated", func() int64 {
			return appliedEvent(t, pool, "lo_su", "customer.subscription.updated", dbtest.DefaultCreated+10, sub)
		}},
		{"customer.subscription.deleted", func() int64 {
			return appliedEvent(t, pool, "lo_sd", "customer.subscription.deleted", dbtest.DefaultCreated+20, sub)
		}},
	}
	for _, ev := range events {
		cl := dbtest.ChangeLog(t, pool, ev.id())
		if len(cl) == 0 {
			t.Fatalf("%s: no change_log rows", ev.name)
		}
		for i, c := range cl[:len(cl)-1] {
			if c.Op != "insert" {
				t.Fatalf("%s: change_log entry %d of %d is %s %s %d; only the last entry may write an existing row",
					ev.name, i+1, len(cl), c.Op, c.Table, c.RowID)
			}
		}
	}
}
