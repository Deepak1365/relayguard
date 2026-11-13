package admin

// DB tests for Undo's locking, plus the schema guard for its column lists.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// Two undos of the same event at once: the inbox row lock makes the second
// wait, then see 'undone' and refuse. Deterministic via the afterLock hook,
// as in TestDBReplayConcurrentSecondIsRefused. FOR KEY SHARE instead would
// let both pass the checks; the second would then fail on its own (row_version
// moved, or admin_actions_undo_once) with a plain error instead of an audited
// refusal.
func TestDBUndoConcurrentSecondIsRefused(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	inv := appliedInvoice(t, pool, "uc", 2000)

	locked, release := make(chan struct{}), make(chan struct{})
	type outcome struct {
		res UndoResult
		err error
	}
	first, second := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		res, err := Undo(ctx, pool, undoReq(inv), Options{afterLock: func() { close(locked); <-release }})
		first <- outcome{res, err}
	}()
	<-locked
	go func() {
		res, err := Undo(ctx, pool, UndoRequest{InboxID: inv, Actor: "bob", Reason: "me too"}, Options{})
		second <- outcome{res, err}
	}()
	blocked := false
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%FROM inbox WHERE id = $1%FOR NO KEY UPDATE%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			blocked = true
			break
		}
	}
	close(release)
	o1, o2 := <-first, <-second
	if !blocked {
		t.Error("the second undo never waited for the first one's row lock")
	}
	if o1.err != nil || o1.res.Outcome != UndoDone {
		t.Fatalf("first undo: %+v, %v", o1.res, o1.err)
	}
	if !errors.Is(o2.err, ErrNotUndoable) || !strings.Contains(o2.res.Reason, "already undone") {
		t.Fatalf("second undo: %+v, %v; want ErrNotUndoable (already undone)", o2.res, o2.err)
	}
	acts := actions(t, pool, inv)
	if len(acts) != 2 || acts[0].Outcome != "done" || acts[0].Actor != "alice" || acts[1].Outcome != "refused" || acts[1].Actor != "bob" {
		t.Fatalf("audit rows %+v, want alice done then bob refused", acts)
	}
	if n := dbtest.Count(t, pool, "credit_grants"); n != 0 {
		t.Fatalf("%d grants, want 0", n)
	}
}

// A worker (or admin) holds the inbox row: ErrBusy within lock_timeout,
// nothing written, and the undo works once the lock is gone.
func TestDBUndoBusy(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	inv := appliedInvoice(t, pool, "ub", 2000)
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) // before the cleanup Truncate
	if _, err := other.Exec(ctx, `SELECT 1 FROM inbox WHERE id = $1 FOR NO KEY UPDATE`, inv); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, pool)
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = Undo(rctx, pool, undoReq(inv), Options{LockTimeout: 200 * time.Millisecond, StatementTimeout: 3 * time.Second})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrBusy) || elapsed > 2*time.Second {
		t.Fatalf("undo against a locked row: err %v after %s, want ErrBusy after about 200ms", err, elapsed)
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, pool); after != before {
		t.Fatalf("a busy undo wrote something.\nbefore:\n%s\nafter:\n%s", before, after)
	}
	mustUndo(t, pool, inv)
}

// restoreColumns must list every column but id and row_version, and
// referencedBy every foreign key between the app tables; a column missing
// from the list would silently not be restored, a missing foreign key would
// turn rule 5 into a 23503 at delete time.
func TestDBUndoColumnsMatchSchema(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	for table, cols := range restoreColumns {
		rows, err := pool.Query(ctx, `
			SELECT column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name NOT IN ('id', 'row_version')
			ORDER BY column_name`, table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			got = append(got, c)
		}
		rows.Close()
		want := append([]string(nil), cols...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: schema columns %v, restoreColumns %v", table, got, want)
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT confrelid::regclass::text, conrelid::regclass::text, a.attname
		FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND confrelid::regclass::text IN ('customers', 'credit_grants', 'subscriptions')
		ORDER BY 1, 2, 3`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][]fkRef{}
	for rows.Next() {
		var parent string
		var r fkRef
		if err := rows.Scan(&parent, &r.table, &r.column); err != nil {
			t.Fatal(err)
		}
		got[parent] = append(got[parent], r)
	}
	if len(got) != len(referencedBy) {
		t.Fatalf("foreign keys %v, referencedBy %v", got, referencedBy)
	}
	for parent, refs := range referencedBy {
		if len(got[parent]) != len(refs) {
			t.Fatalf("foreign keys to %s: %v, referencedBy %v", parent, got[parent], refs)
		}
		for i := range refs {
			if got[parent][i] != refs[i] {
				t.Errorf("foreign keys to %s: %v, referencedBy %v", parent, got[parent], refs)
			}
		}
	}
}
