package main

// Integration tests against a real Postgres. Skipping, truncation and the
// safety rules before running them are documented in internal/dbtest, which
// holds the shared helpers.
//
// Not directly tested: the fence's own row lock (SELECT ... FOR UPDATE in
// process). TestDBHeldFenceLockBlocksReclaim takes an equivalent lock from
// the test itself, so it proves claim skips a locked row, but not that
// process actually takes that lock. That lock is what stops a second worker
// from re-claiming and applying the same event while the first is mid-apply
// after its lease expired; dropping FOR UPDATE would pass these tests.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// newTestWorker has no hooks and a nil fault injector (faults off), so the
// FAULT_* environment of whoever runs the tests cannot crash them.
func newTestWorker(pool *pgxpool.Pool) *worker {
	return &worker{pool: pool, lease: defaultLease}
}

func TestDBClaimHoldsLease(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := dbtest.InsertEvent(t, pool, "lease_1", "customer.created", dbtest.CustomerObject("cus_dbtest_1"))
	w := newTestWorker(pool)

	id, attempts, ok, err := w.claim(ctx)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if id != rowID || attempts != 1 {
		t.Fatalf("first claim: id=%d attempts=%d, want id=%d attempts=1", id, attempts, rowID)
	}

	if _, _, ok, err := w.claim(ctx); err != nil || ok {
		t.Fatalf("second claim during the lease: ok=%v err=%v, want nothing claimed", ok, err)
	}
	if r := dbtest.ReadRow(t, pool, rowID); r.Status != "pending" || r.Attempts != 1 {
		t.Fatalf("row after claims: %+v, want pending with 1 attempt", r)
	}
}

func TestDBLeaseExpiryAndFencing(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := dbtest.InsertEvent(t, pool, "fence_1", "customer.created", dbtest.CustomerObject("cus_dbtest_2"))
	a, b := newTestWorker(pool), newTestWorker(pool)

	_, attA, ok, err := a.claim(ctx)
	if err != nil || !ok || attA != 1 {
		t.Fatalf("claim A: attempts=%d ok=%v err=%v", attA, ok, err)
	}
	dbtest.ExpireLease(t, pool, rowID) // A stalls past its lease
	idB, attB, ok, err := b.claim(ctx)
	if err != nil || !ok || idB != rowID || attB != 2 {
		t.Fatalf("claim B: id=%d attempts=%d ok=%v err=%v, want id=%d attempts=2", idB, attB, ok, err, rowID)
	}

	// A wakes up and tries to process with its old claim: the fence rejects it.
	if err := a.process(ctx, rowID, attA); err != nil {
		t.Fatalf("process A: %v", err)
	}
	if r := dbtest.ReadRow(t, pool, rowID); r.Status != "pending" || r.Attempts != 2 {
		t.Fatalf("row after stale A: %+v, want pending with 2 attempts", r)
	}
	if n := dbtest.Count(t, pool, "customers"); n != 0 {
		t.Fatalf("stale worker A wrote %d customers, want 0", n)
	}

	if err := b.process(ctx, rowID, attB); err != nil {
		t.Fatalf("process B: %v", err)
	}
	if r := dbtest.ReadRow(t, pool, rowID); r.Status != "done" || r.Attempts != 2 {
		t.Fatalf("row after B: %+v, want done with 2 attempts", r)
	}
	if n := dbtest.Count(t, pool, "customers"); n != 1 {
		t.Fatalf("customers after B: %d, want exactly 1", n)
	}
}

// While a worker is inside process, it holds the row lock (SELECT ... FOR
// UPDATE in the fencing check). Even if its lease has expired by then, claim's
// FOR UPDATE SKIP LOCKED must skip the row instead of re-claiming it.
//
// Ordering: claim, then expire the lease with a plain UPDATE, and only then
// take the lock in a separate transaction. Doing the UPDATE after taking the
// lock would block on that same lock (and deadlock the test).
func TestDBHeldFenceLockBlocksReclaim(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := dbtest.InsertEvent(t, pool, "lock_1", "customer.created", dbtest.CustomerObject("cus_dbtest_5"))
	w := newTestWorker(pool)

	if _, att, ok, err := w.claim(ctx); err != nil || !ok || att != 1 {
		t.Fatalf("claim: attempts=%d ok=%v err=%v", att, ok, err)
	}
	dbtest.ExpireLease(t, pool, rowID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM inbox WHERE id = $1 FOR UPDATE`, rowID); err != nil {
		t.Fatalf("take fence lock: %v", err)
	}

	// If claim ever lost SKIP LOCKED it would wait on our lock forever; the
	// timeout turns that regression into a failure instead of a hang.
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, ok, err := w.claim(claimCtx); err != nil || ok {
		t.Fatalf("claim while the row is locked: ok=%v err=%v, want nothing claimed", ok, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Control: with the lock released, the expired row is claimable, so the
	// empty claim above was caused by the lock, not by the lease.
	id, att, ok, err := w.claim(ctx)
	if err != nil || !ok || id != rowID || att != 2 {
		t.Fatalf("claim after unlock: id=%d attempts=%d ok=%v err=%v, want id=%d attempts=2", id, att, ok, err, rowID)
	}
}

func TestDBCrashedEventIsDeadLettered(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	// An event whose 8 attempts all crashed the worker: attempts were counted
	// by claims but no result was ever recorded.
	rowID := dbtest.InsertEvent(t, pool, "crashed_1", "customer.created", dbtest.CustomerObject("cus_dbtest_3"))
	dbtest.SetAttempts(t, pool, rowID, maxAttempts)
	processed, err := newTestWorker(pool).processOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processOne: processed=%v err=%v", processed, err)
	}
	r := dbtest.ReadRow(t, pool, rowID)
	wantErr := "gave up after 8 attempts; last error: none recorded (the worker crashed or was killed while processing)"
	if r.Status != "dead" || r.Attempts != maxAttempts || r.LastErr != wantErr {
		t.Fatalf("row: %+v, want dead, attempts %d, last_error %q", r, maxAttempts, wantErr)
	}
	if n := dbtest.Count(t, pool, "customers"); n != 0 {
		t.Fatalf("dead-lettered event was applied: %d customers", n)
	}
}

func TestDBDeadLetterRespectsReclaim(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := dbtest.InsertEvent(t, pool, "crashed_2", "customer.created", dbtest.CustomerObject("cus_dbtest_4"))
	dbtest.SetAttempts(t, pool, rowID, maxAttempts)

	// A claims (attempts 9) and stalls before dead-lettering; its lease
	// expires and B re-claims (attempts 10).
	a, b := newTestWorker(pool), newTestWorker(pool)
	_, attA, ok, err := a.claim(ctx)
	if err != nil || !ok || attA != maxAttempts+1 {
		t.Fatalf("claim A: attempts=%d ok=%v err=%v", attA, ok, err)
	}
	dbtest.ExpireLease(t, pool, rowID)
	_, attB, ok, err := b.claim(ctx)
	if err != nil || !ok || attB != maxAttempts+2 {
		t.Fatalf("claim B: attempts=%d ok=%v err=%v", attB, ok, err)
	}

	updated, err := a.deadLetterCrashed(ctx, rowID, attA)
	if err != nil {
		t.Fatalf("dead-letter A: %v", err)
	}
	if updated {
		t.Fatal("stale worker A dead-lettered a row that B had re-claimed")
	}
	if r := dbtest.ReadRow(t, pool, rowID); r.Status != "pending" || r.Attempts != maxAttempts+2 {
		t.Fatalf("row after stale A: %+v, want untouched (pending, attempts %d)", r, maxAttempts+2)
	}

	// B, the current claim holder, dead-letters it. It records maxAttempts,
	// not claimedAttempts-1 (which would be 9): A's lost claim never ran
	// the event.
	updated, err = b.deadLetterCrashed(ctx, rowID, attB)
	if err != nil || !updated {
		t.Fatalf("dead-letter B: updated=%v err=%v", updated, err)
	}
	r := dbtest.ReadRow(t, pool, rowID)
	if r.Status != "dead" || r.Attempts != maxAttempts || !strings.HasPrefix(r.LastErr, "gave up after 8 attempts") {
		t.Fatalf("row after B: %+v, want dead with attempts %d and the reason", r, maxAttempts)
	}
}

// invoiceForMissingCustomer is an event app.Apply rejects with
// ErrUnknownCustomer: a real error (not a skip), so the worker retries it with
// backoff and dead-letters it on the last attempt.
func invoiceForMissingCustomer(t *testing.T, pool *pgxpool.Pool, suffix string) int64 {
	t.Helper()
	return dbtest.InsertEvent(t, pool, suffix, "invoice.paid", map[string]any{
		"id": "in_dbtest_" + suffix, "object": "invoice", "customer": "cus_dbtest_missing", "amount_paid": 2000,
	})
}

func TestDBApplyErrorIsRetriedWithBackoff(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := invoiceForMissingCustomer(t, pool, "retry_1")

	processed, err := newTestWorker(pool).processOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processOne: processed=%v err=%v", processed, err)
	}
	r := dbtest.ReadRow(t, pool, rowID)
	if r.Status != "pending" || r.Attempts != 1 || !strings.Contains(r.LastErr, "unknown customer") {
		t.Fatalf("row: %+v, want pending, attempts 1, last_error with the app error", r)
	}
	// Only checks that no grant row was written. The customer lookup fails
	// before any write, so this does not exercise the savepoint rollback.
	if n := dbtest.Count(t, pool, "credit_grants"); n != 0 {
		t.Fatalf("credit_grants: %d, want 0 (no grant may be written)", n)
	}
	// The retry replaces the claim's 10s lease with backoff(1) = 1s.
	var inWindow bool
	if err := pool.QueryRow(ctx, `
		SELECT next_attempt_at BETWEEN now() + interval '0.5 second' AND now() + interval '1.5 second'
		FROM inbox WHERE id = $1`, rowID).Scan(&inWindow); err != nil {
		t.Fatal(err)
	}
	if !inWindow {
		t.Fatal("next_attempt_at is not about now()+1s: the retry did not use backoff(1)")
	}
}

func TestDBApplyErrorOnLastAttemptIsDead(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := invoiceForMissingCustomer(t, pool, "unknown_cus_1")
	dbtest.SetAttempts(t, pool, rowID, maxAttempts-1)

	processed, err := newTestWorker(pool).processOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processOne: processed=%v err=%v", processed, err)
	}
	r := dbtest.ReadRow(t, pool, rowID)
	if r.Status != "dead" || r.Attempts != maxAttempts || !strings.Contains(r.LastErr, "unknown customer cus_dbtest_missing") {
		t.Fatalf("row: %+v, want dead, attempts %d, last_error with the app error", r, maxAttempts)
	}
	// Only checks that no grant row was written (see the retry test above).
	if n := dbtest.Count(t, pool, "credit_grants"); n != 0 {
		t.Fatalf("credit_grants: %d, want 0 (no grant may be written)", n)
	}
}
