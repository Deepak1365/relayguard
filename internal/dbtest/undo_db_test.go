package dbtest

// Schema tests for migration 00006 and the undo Recorder. Skipped unless DB
// tests are enabled (see the package comment).

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Deepak1365/relayguard/internal/changelog"
)

// 00006: 'undone' is a valid status, anything else still is not; one event
// can have at most one DONE undo (refused ones are unlimited).
func TestDBSchemaUndo(t *testing.T) {
	pool := Pool(t)
	ctx := context.Background()
	id := InsertEvent(t, pool, "schema_undo", "customer.created", CustomerObject("cus_dbtest_su"))

	if _, err := pool.Exec(ctx, `UPDATE inbox SET status = 'undone' WHERE id = $1`, id); err != nil {
		t.Fatalf("status undone: %v", err)
	}
	_, err := pool.Exec(ctx, `UPDATE inbox SET status = 'reverted' WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "inbox_status_check" {
		t.Fatalf("status reverted: err=%v, want inbox_status_check violation", err)
	}

	audit := func(action, outcome string) error {
		_, err := pool.Exec(ctx, `INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
			VALUES ($2, $1, 'dbtest', 'schema', $3)`, id, action, outcome)
		return err
	}
	for i, a := range [][2]string{{"undo", "refused"}, {"undo", "refused"}, {"undo", "done"}, {"replay", "done"}, {"replay", "done"}} {
		if err := audit(a[0], a[1]); err != nil {
			t.Fatalf("audit row %d %v: %v", i, a, err)
		}
	}
	err = audit("undo", "done")
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "admin_actions_undo_once" {
		t.Fatalf("second done undo: err=%v, want admin_actions_undo_once violation", err)
	}
}

// NewUndo writes source 'undo' rows tied to the admin action, in the
// generation given (the reverted apply's), numbered from 1, alongside the
// apply rows of the same generation without colliding with them.
func TestDBRecorderUndo(t *testing.T) {
	pool := Pool(t)
	ctx := context.Background()
	id := InsertEvent(t, pool, "rec_undo", "customer.created", CustomerObject("cus_dbtest_ru"))
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
		VALUES ('undo', $1, 'dbtest', 'recorder', 'done') RETURNING id`, id).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	img := []byte(`{"id": 1}`)
	if err := changelog.NewApply(id, 2).Record(ctx, tx, changelog.Change{Table: "customers", RowID: 1, Op: changelog.OpInsert, After: img}); err != nil {
		t.Fatalf("apply row: %v", err)
	}
	rec := changelog.NewUndo(id, 2, actionID)
	for _, c := range []changelog.Change{
		{Table: "customers", RowID: 1, Op: changelog.OpUpdate, Before: img, After: img},
		{Table: "customers", RowID: 1, Op: changelog.OpDelete, Before: img},
	} {
		if err := rec.Record(ctx, tx, c); err != nil {
			t.Fatalf("undo row: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	cl := ChangeLog(t, pool, id)
	if len(cl) != 3 || cl[0].Source != "apply" || cl[0].AdminActionID != nil {
		t.Fatalf("change_log %+v, want an apply row then 2 undo rows", cl)
	}
	for i, r := range cl[1:] {
		if r.Source != "undo" || r.AdminActionID == nil || *r.AdminActionID != actionID || r.Generation != 2 || r.Seq != i+1 {
			t.Fatalf("undo row %+v, want source undo, action %d, generation 2, seq %d", r, actionID, i+1)
		}
	}
}
