package admin

// DB tests for Undo's conflict rules (rule B, rules 1-5). Each refusal must
// be audited, change nothing else, and name the right cause.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// appTables are the tables a refused undo must leave unchanged.
var appTables = []string{"inbox", "customers", "credit_grants", "subscriptions", "change_log"}

func tablesSnapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, tb := range appTables {
		m[tb] = tableJSON(t, pool, tb)
	}
	return m
}

// wantConflict runs an undo that must be refused with ErrConflict, changing
// nothing but adding one refused audit row whose detail lists the conflicts.
// It returns the conflicts.
func wantConflict(t *testing.T, pool *pgxpool.Pool, id int64) []Conflict {
	t.Helper()
	before := tablesSnapshot(t, pool)
	nActs := len(actions(t, pool, id))
	res, err := Undo(context.Background(), pool, undoReq(id), Options{})
	if !errors.Is(err, ErrConflict) || res.Outcome != UndoRefused || res.ActionID == 0 || len(res.Conflicts) == 0 {
		t.Fatalf("undo inbox %d: %+v, %v; want ErrConflict with conflicts", id, res, err)
	}
	for tb, s := range tablesSnapshot(t, pool) {
		if s != before[tb] {
			t.Fatalf("refused undo changed %s:\nbefore %s\nafter  %s", tb, before[tb], s)
		}
	}
	acts := actions(t, pool, id)
	if len(acts) != nActs+1 {
		t.Fatalf("audit rows %+v, want one more", acts)
	}
	a := acts[len(acts)-1]
	if a.ID != res.ActionID || a.Action != "undo" || a.Outcome != "refused" ||
		!strings.Contains(a.Detail, `"conflicts": [{`) || !strings.Contains(a.Detail, res.Conflicts[0].Kind) {
		t.Fatalf("audit row %+v, want a refused undo with the conflicts in its detail", a)
	}
	return res.Conflicts
}

func hasConflict(cs []Conflict, kind, table string, inboxID int64, text string) bool {
	for _, c := range cs {
		if c.Kind == kind && c.Table == table && c.InboxID == inboxID && strings.Contains(c.Explanation, text) {
			return true
		}
	}
	return false
}

// Rule 2 and the rule-B order: an older invoice cannot be undone while a
// newer one on the same customer is not undone; the refusal names the newer
// one. Newest first then succeeds all the way down to customer.created,
// leaving the app tables empty.
func TestDBUndoConflictLaterEvent(t *testing.T) {
	pool := dbtest.Pool(t)
	cus := appliedCustomer(t, pool)
	inv1 := appliedInvoice(t, pool, "cf_1", 2000)
	inv2 := appliedInvoice(t, pool, "cf_2", 3000)

	cs := wantConflict(t, pool, inv1)
	if !hasConflict(cs, ConflictLaterEvent, "customers", inv2, fmt.Sprintf("undo inbox #%d first", inv2)) {
		t.Fatalf("conflicts %+v, want inbox #%d named as later event on customers", cs, inv2)
	}
	cs = wantConflict(t, pool, cus)
	if !hasConflict(cs, ConflictLaterEvent, "customers", inv1, "") || !hasConflict(cs, ConflictReferenced, "customers", inv2, "") {
		t.Fatalf("conflicts %+v, want both invoices named for customer.created", cs)
	}

	for _, id := range []int64{inv2, inv1, cus} {
		mustUndo(t, pool, id)
	}
	for _, tb := range []string{"customers", "credit_grants", "subscriptions"} {
		if n := dbtest.Count(t, pool, tb); n != 0 {
			t.Fatalf("%s: %d rows after undoing everything newest first, want 0", tb, n)
		}
	}
}

// Rules 1 and 3, and rule 2's pairing: writes the change_log does not
// account for are "changed outside RelayGuard". Each case breaks one thing.
func TestDBUndoConflictUnloggedWrite(t *testing.T) {
	cases := []struct {
		name, kind, table string
		tamper            func(t *testing.T, pool *pgxpool.Pool, inv, later int64)
	}{
		{"unlogged update", ConflictVersion, "customers", func(t *testing.T, pool *pgxpool.Pool, inv, later int64) {
			exec(t, pool, `UPDATE customers SET email = 'other@example.test'`)
		}},
		{"row deleted", ConflictMissing, "credit_grants", func(t *testing.T, pool *pgxpool.Pool, inv, later int64) {
			exec(t, pool, `DELETE FROM credit_grants`)
		}},
		{"later event marked undone without undo rows", ConflictUnpaired, "customers",
			func(t *testing.T, pool *pgxpool.Pool, inv, later int64) {
				exec(t, pool, `UPDATE inbox SET status = 'undone' WHERE id = $1`, later)
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := dbtest.Pool(t)
			appliedCustomer(t, pool)
			inv := appliedInvoice(t, pool, "uw_1", 2000)
			var later int64
			if c.kind == ConflictUnpaired {
				later = appliedInvoice(t, pool, "uw_2", 1000)
			}
			c.tamper(t, pool, inv, later)
			cs := wantConflict(t, pool, inv)
			found := false
			for _, x := range cs {
				found = found || (x.Kind == c.kind && x.Table == c.table && strings.Contains(x.Explanation, "outside RelayGuard"))
			}
			if !found {
				t.Fatalf("conflicts %+v, want %s on %s saying changed outside RelayGuard", cs, c.kind, c.table)
			}
		})
	}

	// An unlogged write BETWEEN two logged changes. Comparing the row only to
	// the last entry's after image misses it once the later event is undone
	// (the row is back to the later event's before image, which includes the
	// outside write), and undoing the older event would silently wipe it. The
	// chain check (each entry's before image = the previous entry's after
	// image) catches it. The no-op variant changes only row_version: strict
	// like rule 3, it is still a write the log does not account for.
	for _, between := range []struct{ name, sql, email string }{
		{"unlogged write between logged changes", `UPDATE customers SET email = 'outside@example.test'`, "outside@example.test"},
		{"unlogged no-op update between logged changes", `UPDATE customers SET email = email`, ""},
	} {
		t.Run(between.name, func(t *testing.T) { undoAcrossUnloggedWrite(t, between.sql, between.email) })
	}
}

// undoAcrossUnloggedWrite runs the steps above with one outside write.
func undoAcrossUnloggedWrite(t *testing.T, outside, wantEmail string) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	inv1 := appliedInvoice(t, pool, "uw_c1", 2000)
	var original string
	if err := pool.QueryRow(context.Background(), `SELECT email FROM customers`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if wantEmail == "" {
		wantEmail = original
	}
	exec(t, pool, outside)
	inv2 := appliedInvoice(t, pool, "uw_c2", 1000)
	cs := wantConflict(t, pool, inv1)
	if !hasConflict(cs, ConflictLaterEvent, "customers", inv2, fmt.Sprintf("undo inbox #%d first", inv2)) {
		t.Fatalf("conflicts %+v, want inbox #%d named as later event", cs, inv2)
	}
	mustUndo(t, pool, inv2)
	cs = wantConflict(t, pool, inv1)
	if len(cs) != 1 || !hasConflict(cs, ConflictChainBreak, "customers", 0, "changed outside RelayGuard between change_log #") {
		t.Fatalf("conflicts %+v, want exactly one chain break on customers", cs)
	}
	var email string
	if err := pool.QueryRow(context.Background(), `SELECT email FROM customers`).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != wantEmail {
		t.Fatalf("email %q, want %q (the outside write kept)", email, wantEmail)
	}
}

// Rule 5: a subscription references the customer without bumping its
// row_version, so only the reference check stops the delete (else 23503).
// A reference no event inserted is "changed outside RelayGuard".
func TestDBUndoCustomerReferenced(t *testing.T) {
	pool := dbtest.Pool(t)
	cus := appliedCustomer(t, pool)
	sub := appliedEvent(t, pool, "ref_sub", "customer.subscription.created", dbtest.DefaultCreated,
		dbtest.SubscriptionObject("sub_dbtest_ref", testCustomer, "active"))
	cs := wantConflict(t, pool, cus)
	if len(cs) != 1 || !hasConflict(cs, ConflictReferenced, "customers", sub, fmt.Sprintf("undo inbox #%d first", sub)) {
		t.Fatalf("conflicts %+v, want exactly the subscription reference naming inbox #%d", cs, sub)
	}
	mustUndo(t, pool, sub)
	exec(t, pool, `INSERT INTO credit_grants (customer_id, stripe_invoice_id, amount) SELECT id, 'in_manual', 1 FROM customers`)
	cs = wantConflict(t, pool, cus)
	if len(cs) != 1 || !hasConflict(cs, ConflictReferenced, "customers", 0, "outside RelayGuard") {
		t.Fatalf("conflicts %+v, want the unlogged grant reported as changed outside RelayGuard", cs)
	}
}

// Rule 4 (defense in depth): row_version matches but the content differs
// from what the log says. Only reachable by editing the log or by a write
// that bypasses the trigger; here the log's after image is edited.
func TestDBUndoLogDisagreesWithRow(t *testing.T) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	inv := appliedInvoice(t, pool, "ld_1", 2000)
	exec(t, pool, `UPDATE change_log SET after_image = jsonb_set(after_image, '{credit_balance}', '99')
		WHERE inbox_id = $1 AND table_name = 'customers'`, inv)
	cs := wantConflict(t, pool, inv)
	if len(cs) != 1 || cs[0].Kind != ConflictContent || cs[0].Table != "customers" {
		t.Fatalf("conflicts %+v, want exactly one content_mismatch on customers", cs)
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
