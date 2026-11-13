package sim

// DB tests for the undo part of the change_log checker (step 6b): a positive
// test with the real admin.Undo, and helpers for the negative controls in
// changelog_undo_controls_db_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// mustUndo undoes inbox id with the real admin.Undo and returns its audit row.
func mustUndo(t *testing.T, pool *pgxpool.Pool, id int64) int64 {
	t.Helper()
	res, err := admin.Undo(context.Background(), pool,
		admin.UndoRequest{InboxID: id, Actor: "dbtest", Reason: "checker test"}, admin.Options{})
	if err != nil || res.Outcome != admin.UndoDone {
		t.Fatalf("undo %d: %+v, %v", id, res, err)
	}
	return res.ActionID
}

// undoneFixture is history "a" with two events undone by the real Undo:
// the newest subscription update (one undo row: an update) and the invoice
// (two undo rows: the customer balance restore, then the grant delete).
type undoneFixture struct {
	fixture
	subU                 int64 // inbox id of a_sub_u
	invAction, subAction int64 // their done undo audit rows
}

func undoneHistory(t *testing.T, pool *pgxpool.Pool) undoneFixture {
	t.Helper()
	u := undoneFixture{fixture: history(t, pool, "a")}
	u.subU = inboxIDOf(t, pool, "a_sub_u")
	u.subAction = mustUndo(t, pool, u.subU)
	u.invAction = mustUndo(t, pool, u.invoice)
	wantViolations(t, check(t, pool, "a")) // clean before any damage
	return u
}

// undoViolations keeps only what checkUndoRows reports for event suffix: any
// kind, subject = the event. Image damage in the restore controls also
// (correctly) breaks the chain or final state of the ROW; those have row
// subjects and are not what the control tests.
func undoViolations(vs []Violation, suffix string) []Violation {
	var out []Violation
	for _, v := range vs {
		if v.Subject == dbtest.EventPrefix+suffix {
			out = append(out, v)
		}
	}
	return out
}

// The realistic path: every event of a history is undone newest first with
// the real admin.Undo, including an invoice that was dead, replayed and
// applied in generation 1. The checker must be clean after EVERY undo (each
// partial state is a real state an operator can leave), and at the end the
// app rows must be gone and the undo rows really there in the right
// generations, so "clean" cannot mean "not looked at".
func TestDBChangeLogCheckRealUndoClean(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	f := history(t, pool, "a")

	late := dbtest.InsertEvent(t, pool, "a_inv_late", "invoice.paid", dbtest.InvoiceObject("in_dbtest_a_late", f.customer, 3000))
	mustExec(t, pool, `UPDATE inbox SET status = 'dead', attempts = 8, last_error = 'synthetic', processed_at = now() WHERE id = $1`, late)
	if res, err := admin.Replay(ctx, pool, admin.ReplayRequest{InboxID: late, Actor: "dbtest", Reason: "checker test"}, admin.Options{}); err != nil || res.Outcome != admin.ReplayDone {
		t.Fatalf("replay: %+v, %v", res, err)
	}
	if got := dbtest.ApplyInbox(t, pool, late); got != "done" {
		t.Fatalf("replayed invoice: %s, want done", got)
	}
	wantViolations(t, check(t, pool, "a"))

	// The customer goes last: rule 5 refuses it while grants or a
	// subscription still reference it.
	newestFirst := []int64{late, inboxIDOf(t, pool, "a_sub_u"), inboxIDOf(t, pool, "a_sub_c"), f.invoice, inboxIDOf(t, pool, "a_cus")}
	for _, id := range newestFirst {
		mustUndo(t, pool, id)
		wantViolations(t, check(t, pool, "a"))
	}

	// A refused undo (already undone) writes an audit row but no change_log
	// rows; still clean.
	if _, err := admin.Undo(ctx, pool, admin.UndoRequest{InboxID: late, Actor: "dbtest", Reason: "again"}, admin.Options{}); !errors.Is(err, admin.ErrNotUndoable) {
		t.Fatalf("second undo: %v, want ErrNotUndoable", err)
	}
	wantViolations(t, check(t, pool, "a"))

	for _, table := range []string{"customers", "credit_grants", "subscriptions"} {
		if n := dbtest.Count(t, pool, table); n != 0 {
			t.Fatalf("%s: %d rows after undoing everything, want 0", table, n)
		}
	}
	var undone, undoRows, lateGen1 int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM inbox WHERE status = 'undone'),
		       (SELECT count(*) FROM change_log WHERE source = 'undo'),
		       (SELECT count(*) FROM change_log WHERE source = 'undo' AND inbox_id = $1 AND generation = 1)`,
		late).Scan(&undone, &undoRows, &lateGen1); err != nil {
		t.Fatal(err)
	}
	// Undo rows: late invoice 2, sub_u 1, sub_c 1, invoice 2, customer 1.
	if undone != 5 || undoRows != 7 || lateGen1 != 2 {
		t.Fatalf("undone %d, undo rows %d, late invoice undo rows in generation 1: %d; want 5, 7, 2", undone, undoRows, lateGen1)
	}
}
