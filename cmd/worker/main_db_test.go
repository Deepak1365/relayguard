package main

// Integration tests against a real Postgres. They are skipped unless
// RELAYGUARD_DB_TESTS=1 and DATABASE_URL are set (`dev.ps1 db-test` does
// both), because they TRUNCATE the inbox AND the app tables (customers,
// credit_grants, subscriptions) before and after each test. They refuse to do
// that if the inbox holds events they did not create. They never create, drop
// or migrate schema: run `dev.ps1 migrate` first.
//
// Before running:
//   - Stop any worker. It polls the inbox and could claim evt_dbtest_ rows,
//     racing the tests and making them fail or flake.
//   - Never run during an experiment (chaos or manual): the tests wipe the
//     tables the experiment is measuring.
//   - The inbox must contain only evt_dbtest_ rows (or be empty); otherwise
//     the tests refuse to truncate and fail.
//
// Not directly tested: the fence's own row lock (SELECT ... FOR UPDATE in
// process). TestDBHeldFenceLockBlocksReclaim takes an equivalent lock from
// the test itself, so it proves claim skips a locked row, but not that
// process actually takes that lock. That lock is what stops a second worker
// from re-claiming and applying the same event while the first is mid-apply
// after its lease expired; dropping FOR UPDATE would pass these tests.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/db"
)

// Every inbox row these tests create has this prefix, so the safety check
// can tell them apart from rows a real receiver stored.
const testEventPrefix = "evt_dbtest_"

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if os.Getenv("RELAYGUARD_DB_TESTS") != "1" || url == "" {
		t.Skip("set RELAYGUARD_DB_TESTS=1 and DATABASE_URL to run database tests (they truncate tables)")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	truncate(t, pool)
	t.Cleanup(func() { truncate(t, pool) })
	return pool
}

// truncate empties the inbox and the app tables, but refuses if the inbox
// holds events these tests did not create: those belong to someone (a
// receiver, a chaos run) and must not be silently destroyed. The check and the
// TRUNCATE run in one transaction under an exclusive lock on inbox, so a
// receiver cannot insert a row between the check and the wipe (its INSERT
// waits for the lock, then lands in the emptied table).
func truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin truncate: %v", err)
	}
	defer tx.Rollback(ctx) // no-op after commit
	// Fail instead of hanging if something (a running worker, an open psql
	// transaction) holds a lock on inbox.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE inbox IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock inbox: %v", err)
	}
	var foreign int
	if err := tx.QueryRow(ctx,
		// Exact prefix match: in LIKE, '_' in the prefix is a wildcard.
		`SELECT count(*) FROM inbox WHERE left(event_id, length($1)) <> $1`, testEventPrefix).Scan(&foreign); err != nil {
		t.Fatalf("count foreign inbox rows: %v", err)
	}
	if foreign > 0 {
		t.Fatalf("inbox has %d rows not created by these tests; refusing to truncate (run dev.ps1 reset-data yourself if they are disposable)", foreign)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE inbox, subscriptions, credit_grants, customers RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit truncate: %v", err)
	}
}

func insertEvent(t *testing.T, pool *pgxpool.Pool, suffix, typ string, object map[string]any) int64 {
	t.Helper()
	eventID := testEventPrefix + suffix
	payload, err := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "type": typ, "created": 1793800000,
		"data": map[string]any{"object": object},
	})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO inbox (provider, event_id, event_type, payload)
		VALUES ('stripe', $1, $2, $3::text::jsonb)
		RETURNING id`, eventID, typ, string(payload)).Scan(&id); err != nil {
		t.Fatalf("insert inbox row: %v", err)
	}
	return id
}

// setAttempts makes a row look like it has already used n attempts and is
// ready to be claimed again.
func setAttempts(t *testing.T, pool *pgxpool.Pool, id int64, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE inbox SET attempts = $2, next_attempt_at = now() - interval '1 second' WHERE id = $1`,
		id, n); err != nil {
		t.Fatalf("set attempts: %v", err)
	}
}

// expireLease simulates a claim's lease running out (the worker holding it
// stalled or died) without sleeping, so the tests do not depend on timing.
func expireLease(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE inbox SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

type inboxRow struct {
	status   string
	attempts int
	lastErr  string
}

func readRow(t *testing.T, pool *pgxpool.Pool, id int64) inboxRow {
	t.Helper()
	var r inboxRow
	if err := pool.QueryRow(context.Background(),
		`SELECT status, attempts, COALESCE(last_error, '') FROM inbox WHERE id = $1`, id).
		Scan(&r.status, &r.attempts, &r.lastErr); err != nil {
		t.Fatalf("read inbox row %d: %v", id, err)
	}
	return r
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	// table is always a literal from these tests, never input.
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// newTestWorker has no hooks and a nil fault injector (faults off), so the
// FAULT_* environment of whoever runs the tests cannot crash them.
func newTestWorker(pool *pgxpool.Pool) *worker {
	return &worker{pool: pool, lease: defaultLease}
}

func customerObject(id string) map[string]any {
	return map[string]any{"id": id, "object": "customer", "email": id + "@example.test"}
}

func TestDBClaimHoldsLease(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	rowID := insertEvent(t, pool, "lease_1", "customer.created", customerObject("cus_dbtest_1"))
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
	if r := readRow(t, pool, rowID); r.status != "pending" || r.attempts != 1 {
		t.Fatalf("row after claims: %+v, want pending with 1 attempt", r)
	}
}

func TestDBLeaseExpiryAndFencing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	rowID := insertEvent(t, pool, "fence_1", "customer.created", customerObject("cus_dbtest_2"))
	a, b := newTestWorker(pool), newTestWorker(pool)

	_, attA, ok, err := a.claim(ctx)
	if err != nil || !ok || attA != 1 {
		t.Fatalf("claim A: attempts=%d ok=%v err=%v", attA, ok, err)
	}
	expireLease(t, pool, rowID) // A stalls past its lease
	idB, attB, ok, err := b.claim(ctx)
	if err != nil || !ok || idB != rowID || attB != 2 {
		t.Fatalf("claim B: id=%d attempts=%d ok=%v err=%v, want id=%d attempts=2", idB, attB, ok, err, rowID)
	}

	// A wakes up and tries to process with its old claim: the fence rejects it.
	if err := a.process(ctx, rowID, attA); err != nil {
		t.Fatalf("process A: %v", err)
	}
	if r := readRow(t, pool, rowID); r.status != "pending" || r.attempts != 2 {
		t.Fatalf("row after stale A: %+v, want pending with 2 attempts", r)
	}
	if n := count(t, pool, "customers"); n != 0 {
		t.Fatalf("stale worker A wrote %d customers, want 0", n)
	}

	if err := b.process(ctx, rowID, attB); err != nil {
		t.Fatalf("process B: %v", err)
	}
	if r := readRow(t, pool, rowID); r.status != "done" || r.attempts != 2 {
		t.Fatalf("row after B: %+v, want done with 2 attempts", r)
	}
	if n := count(t, pool, "customers"); n != 1 {
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
	pool := testPool(t)
	ctx := context.Background()
	rowID := insertEvent(t, pool, "lock_1", "customer.created", customerObject("cus_dbtest_5"))
	w := newTestWorker(pool)

	if _, att, ok, err := w.claim(ctx); err != nil || !ok || att != 1 {
		t.Fatalf("claim: attempts=%d ok=%v err=%v", att, ok, err)
	}
	expireLease(t, pool, rowID)

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
	pool := testPool(t)
	ctx := context.Background()

	// An event whose 8 attempts all crashed the worker: attempts were counted
	// by claims but no result was ever recorded.
	rowID := insertEvent(t, pool, "crashed_1", "customer.created", customerObject("cus_dbtest_3"))
	setAttempts(t, pool, rowID, maxAttempts)
	processed, err := newTestWorker(pool).processOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processOne: processed=%v err=%v", processed, err)
	}
	r := readRow(t, pool, rowID)
	wantErr := "gave up after 8 attempts; last error: none recorded (the worker crashed or was killed while processing)"
	if r.status != "dead" || r.attempts != maxAttempts || r.lastErr != wantErr {
		t.Fatalf("row: %+v, want dead, attempts %d, last_error %q", r, maxAttempts, wantErr)
	}
	if n := count(t, pool, "customers"); n != 0 {
		t.Fatalf("dead-lettered event was applied: %d customers", n)
	}
}

func TestDBDeadLetterRespectsReclaim(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	rowID := insertEvent(t, pool, "crashed_2", "customer.created", customerObject("cus_dbtest_4"))
	setAttempts(t, pool, rowID, maxAttempts)

	// A claims (attempts 9) and stalls before dead-lettering; its lease
	// expires and B re-claims (attempts 10).
	a, b := newTestWorker(pool), newTestWorker(pool)
	_, attA, ok, err := a.claim(ctx)
	if err != nil || !ok || attA != maxAttempts+1 {
		t.Fatalf("claim A: attempts=%d ok=%v err=%v", attA, ok, err)
	}
	expireLease(t, pool, rowID)
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
	if r := readRow(t, pool, rowID); r.status != "pending" || r.attempts != maxAttempts+2 {
		t.Fatalf("row after stale A: %+v, want untouched (pending, attempts %d)", r, maxAttempts+2)
	}

	// B, the current claim holder, dead-letters it. It records maxAttempts,
	// not claimedAttempts-1 (which would be 9): A's lost claim never ran
	// the event.
	updated, err = b.deadLetterCrashed(ctx, rowID, attB)
	if err != nil || !updated {
		t.Fatalf("dead-letter B: updated=%v err=%v", updated, err)
	}
	r := readRow(t, pool, rowID)
	if r.status != "dead" || r.attempts != maxAttempts || !strings.HasPrefix(r.lastErr, "gave up after 8 attempts") {
		t.Fatalf("row after B: %+v, want dead with attempts %d and the reason", r, maxAttempts)
	}
}

// invoiceForMissingCustomer is an event app.Apply rejects with
// ErrUnknownCustomer: a real error (not a skip), so the worker retries it with
// backoff and dead-letters it on the last attempt.
func invoiceForMissingCustomer(t *testing.T, pool *pgxpool.Pool, suffix string) int64 {
	t.Helper()
	return insertEvent(t, pool, suffix, "invoice.paid", map[string]any{
		"id": "in_dbtest_" + suffix, "object": "invoice", "customer": "cus_dbtest_missing", "amount_paid": 2000,
	})
}

func TestDBApplyErrorIsRetriedWithBackoff(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	rowID := invoiceForMissingCustomer(t, pool, "retry_1")

	processed, err := newTestWorker(pool).processOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processOne: processed=%v err=%v", processed, err)
	}
	r := readRow(t, pool, rowID)
	if r.status != "pending" || r.attempts != 1 || !strings.Contains(r.lastErr, "unknown customer") {
		t.Fatalf("row: %+v, want pending, attempts 1, last_error with the app error", r)
	}
	// Only checks that no grant row was written. The customer lookup fails
	// before any write, so this does not exercise the savepoint rollback.
	if n := count(t, pool, "credit_grants"); n != 0 {
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
	pool := testPool(t)
	ctx := context.Background()
	rowID := invoiceForMissingCustomer(t, pool, "unknown_cus_1")
	setAttempts(t, pool, rowID, maxAttempts-1)

	processed, err := newTestWorker(pool).processOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processOne: processed=%v err=%v", processed, err)
	}
	r := readRow(t, pool, rowID)
	if r.status != "dead" || r.attempts != maxAttempts || !strings.Contains(r.lastErr, "unknown customer cus_dbtest_missing") {
		t.Fatalf("row: %+v, want dead, attempts %d, last_error with the app error", r, maxAttempts)
	}
	// Only checks that no grant row was written (see the retry test above).
	if n := count(t, pool, "credit_grants"); n != 0 {
		t.Fatalf("credit_grants: %d, want 0 (no grant may be written)", n)
	}
}
