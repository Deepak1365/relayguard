package admin

// revert's WHERE row_version = <version read under the lock> is unreachable
// through Undo (the lock keeps the version), so it is tested directly: with
// a stale rowState both the restoring UPDATE and the DELETE must fail loudly
// (not match zero rows and carry on), write nothing, and record nothing.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

func TestDBRevertRefusesStaleVersion(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	inv := appliedInvoice(t, pool, "rv_stale", 2000)
	before := tablesSnapshot(t, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	entries, err := readApplyEntries(ctx, tx, inv, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := lockRows(ctx, tx, entries)
	if err != nil {
		t.Fatal(err)
	}
	// admin_action_id is only written by Record, which must not be reached.
	rec := changelog.NewUndo(inv, 0, 1)
	for _, e := range entries {
		r := rows[rowKey(e.table, e.rowID)]
		r.version-- // as if the row had moved on since it was read
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = revert(ctx, sp, rec, e, r)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("revert %s %s %d at a stale row_version: %v, want an error wrapping ErrNoRows", e.op, e.table, e.rowID, err)
		}
		if err := sp.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		r.version++
		if !r.exists {
			t.Fatalf("%s %d: rowState marked gone after a failed revert", e.table, e.rowID)
		}
	}
	// Nothing changed inside the transaction either (read through it).
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM change_log WHERE source = 'undo'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d undo change_log rows after failed reverts, want 0", n)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for tb, s := range tablesSnapshot(t, pool) {
		if s != before[tb] {
			t.Fatalf("failed reverts changed %s", tb)
		}
	}
}
