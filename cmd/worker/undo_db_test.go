package main

// Worker-side tests for admin undo (phase 6 step 6a). Same rules as
// main_db_test.go: skipped unless DB tests are enabled.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/app"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

func undoReq(id int64) admin.UndoRequest {
	return admin.UndoRequest{InboxID: id, Actor: "dbtest", Reason: "worker test"}
}

// The lock risk named in the plan. A worker paying an invoice holds FOR KEY
// SHARE on the customer (the credit_grants foreign key) and then upgrades to
// FOR NO KEY UPDATE (addCredits). An undo of customer.created wants FOR
// UPDATE on that customer, which conflicts with the KEY SHARE, so it waits.
// If the worker's upgrade queued behind the waiting undo, the two would
// deadlock (40P01 after deadlock_timeout, 1s, which is below the undo's 2s
// lock_timeout, so a deadlock would show up as 40P01, not as ErrBusy).
// Postgres lets a transaction that already holds a lock on the tuple go ahead
// of waiters, so the worker finishes, and the undo then sees the invoice and
// refuses with a conflict (no 23503 either: rule 5 sees the grant first).
// Run with -count=20.
func TestDBUndoCustomerWaitsForWorkerFK(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := newTestWorker(pool)
	cus := dbtest.InsertEvent(t, pool, "ufk_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_ufk"))
	processN(t, w, 1)
	inv := dbtest.InsertEvent(t, pool, "ufk_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_ufk", "cus_dbtest_ufk", 2000))

	granted, release := make(chan struct{}), make(chan struct{})
	w.hooks = app.Hooks{AfterGrant: func(string) error { close(granted); <-release; return nil }}
	workerDone := make(chan error, 1)
	go func() {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		processed, err := w.processOne(pctx)
		if err == nil && !processed {
			err = errors.New("nothing processed")
		}
		workerDone <- err
	}()
	<-granted

	type outcome struct {
		res admin.UndoResult
		err error
	}
	undoDone := make(chan outcome, 1)
	go func() {
		res, err := admin.Undo(ctx, pool, undoReq(cus), admin.Options{})
		undoDone <- outcome{res, err}
	}()
	waiting := false
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%FROM customers t WHERE t.id = $1 FOR UPDATE%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			waiting = true
			break
		}
	}
	close(release)
	werr := <-workerDone
	u := <-undoDone

	var pgErr *pgconn.PgError
	for _, err := range []error{werr, u.err} {
		if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "23503") {
			t.Fatalf("lock risk materialised: %v", err)
		}
	}
	if !waiting {
		t.Fatal("the undo never waited for the worker's KEY SHARE on the customer")
	}
	if werr != nil {
		t.Fatalf("worker: %v", werr)
	}
	wantStatus(t, pool, inv, "done")
	if !errors.Is(u.err, admin.ErrConflict) || u.res.Outcome != admin.UndoRefused {
		t.Fatalf("undo: %+v, %v; want ErrConflict (the invoice committed while it waited)", u.res, u.err)
	}
	named := false
	for _, c := range u.res.Conflicts {
		named = named || (c.InboxID == inv && strings.Contains(c.Explanation, "first"))
	}
	if !named {
		t.Fatalf("conflicts %+v, want the invoice (inbox #%d) named", u.res.Conflicts, inv)
	}
	wantStatus(t, pool, cus, "done")
}

// The worker claims only pending events, so an undone event is never
// processed again, even when its next_attempt_at is due.
func TestDBWorkerIgnoresUndoneEvent(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := newTestWorker(pool)
	dbtest.InsertEvent(t, pool, "wiu_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_wiu"))
	inv := dbtest.InsertEvent(t, pool, "wiu_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_wiu", "cus_dbtest_wiu", 2000))
	processN(t, w, 2)
	if res, err := admin.Undo(ctx, pool, undoReq(inv), admin.Options{}); err != nil || res.Outcome != admin.UndoDone {
		t.Fatalf("undo: %+v, %v", res, err)
	}
	dbtest.ExpireLease(t, pool, inv) // due now: only the status keeps it out
	logged := dbtest.Count(t, pool, "change_log")

	processed, err := w.processOne(ctx)
	if err != nil || processed {
		t.Fatalf("processOne with only an undone event: processed=%v err=%v, want nothing claimed", processed, err)
	}
	wantStatus(t, pool, inv, "undone")
	if n := dbtest.Count(t, pool, "change_log"); n != logged {
		t.Fatalf("change_log %d rows, want %d", n, logged)
	}
	if n := dbtest.Count(t, pool, "credit_grants"); n != 0 {
		t.Fatalf("%d grants, want 0", n)
	}
}
