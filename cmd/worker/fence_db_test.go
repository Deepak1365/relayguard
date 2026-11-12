package main

// Fence, lock-mode and replay tests (phase 6 steps 5a and 5b). Same rules as
// main_db_test.go: skipped unless DB tests are enabled, run through
// `dev.ps1 db-test`.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/app"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// replay runs the real admin.Replay (step 5b), which replaced the raw-SQL
// stand-in 5a used. It can produce every state these tests need, so no raw
// replay helper is left.
func replay(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	res, err := admin.Replay(context.Background(), pool,
		admin.ReplayRequest{InboxID: id, Actor: "dbtest", Reason: "worker test"}, admin.Options{})
	if err != nil || res.Outcome != admin.ReplayDone {
		t.Fatalf("replay inbox row %d: %+v, %v", id, res, err)
	}
}

// markDead records an outcome some other worker reached while the claim
// under test was stalled (replay only starts from dead or stale). Raw SQL on
// purpose: the tests need exactly "dead" while a claim is outstanding, which
// the real worker only produces after maxAttempts.
func markDead(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE inbox SET status = 'dead', last_error = 'dbtest: dead before replay', processed_at = now()
		WHERE id = $1`, id); err != nil {
		t.Fatalf("mark inbox row %d dead: %v", id, err)
	}
}

// Step-2 lock hazard. Foreign keys to inbox (change_log, admin_actions) make
// any transaction that inserts such rows hold FOR KEY SHARE on the inbox row
// until it commits; an admin replay or undo (steps 5b, 6) is such a
// transaction. With FOR UPDATE, the claim would skip the row (SKIP LOCKED)
// and the fence would wait for the admin transaction. FOR NO KEY UPDATE does
// not conflict with KEY SHARE, so both must go ahead. The deadline turns a
// fence that waits into a failure instead of a hang.
//
// Two ways of holding the lock: an explicit FOR KEY SHARE, and the real
// source, an FK check from an admin_actions insert.
func TestDBKeyShareDoesNotBlockClaimOrFence(t *testing.T) {
	cases := []struct {
		name, sql string // $1 = the inbox id
	}{
		{"explicit FOR KEY SHARE", `SELECT 1 FROM inbox WHERE id = $1 FOR KEY SHARE`},
		{"admin_actions foreign key", `
			INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
			VALUES ('replay', $1, 'dbtest', 'key share test', 'refused')`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := dbtest.Pool(t)
			ctx := context.Background()
			rowID := dbtest.InsertEvent(t, pool, "kshare_inbox", "customer.created", dbtest.CustomerObject("cus_dbtest_ksi"))

			other, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Rollback(ctx) // before the cleanup Truncate, which would wait on it
			if _, err := other.Exec(ctx, c.sql, rowID); err != nil {
				t.Fatalf("take KEY SHARE: %v", err)
			}

			w := newTestWorker(pool)
			wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			id, tok, ok, err := w.claim(wctx)
			if err != nil || !ok || id != rowID {
				t.Fatalf("claim while another transaction holds KEY SHARE: id=%d ok=%v err=%v, want row %d claimed "+
					"(claim must not treat KEY SHARE as locked)", id, ok, err, rowID)
			}
			if err := w.process(wctx, id, tok); err != nil {
				t.Fatalf("process while another transaction holds KEY SHARE: %v (the fence must not wait for KEY SHARE)", err)
			}
			wantStatus(t, pool, rowID, "done")
			if n := dbtest.Count(t, pool, "change_log"); n != 1 {
				t.Fatalf("change_log: %d rows, want 1", n)
			}
			if err := other.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The fence's own lock, taken by the real process: the AfterGrant hook runs
// mid-apply, after the fence and before commit, and tries to re-claim the
// row from a second worker. The lease has already expired, so only the
// fence's row lock can stop the claim. Without it, a stalled worker and a
// re-claiming worker could both apply the event.
//
// The lease is expired before process starts: an UPDATE from another session
// during the hook would wait on the fence's lock (that is the point of it).
func TestDBFenceLockBlocksReclaimMidApply(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w, other := newTestWorker(pool), newTestWorker(pool)
	dbtest.InsertEvent(t, pool, "midapply_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_mid"))
	processN(t, w, 1)
	inv := dbtest.InsertEvent(t, pool, "midapply_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_mid", "cus_dbtest_mid", 2000))

	id, tok, ok, err := w.claim(ctx)
	if err != nil || !ok || id != inv {
		t.Fatalf("claim: id=%d ok=%v err=%v, want %d", id, ok, err, inv)
	}
	dbtest.ExpireLease(t, pool, inv) // w "stalled" past its lease

	var (
		hookRan   bool
		ready     int
		reclaimed bool
		claimErr  error
	)
	w.hooks = app.Hooks{AfterGrant: func(string) error {
		hookRan = true
		// Control: by every non-locking measure the row is claimable now.
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM inbox WHERE id = $1 AND status = 'pending' AND next_attempt_at <= now()`, inv).
			Scan(&ready); err != nil {
			t.Errorf("read inbox in hook: %v", err)
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, _, reclaimed, claimErr = other.claim(cctx)
		return nil
	}}
	if err := w.process(ctx, id, tok); err != nil {
		t.Fatalf("process: %v", err)
	}

	if !hookRan || ready != 1 {
		t.Fatalf("hook ran=%v, ready rows in hook=%d; want the hook to run while the row looked claimable", hookRan, ready)
	}
	if claimErr != nil || reclaimed {
		t.Fatalf("claim mid-apply: claimed=%v err=%v, want nothing claimed (the fence must hold the row lock)", reclaimed, claimErr)
	}
	if r := dbtest.ReadRow(t, pool, inv); r.Status != "done" || r.Attempts != 1 {
		t.Fatalf("row: %+v, want done with 1 attempt (no re-claim)", r)
	}
}

// ABA on attempts: a claim stalls, the event is dead-lettered meanwhile and
// replayed (attempts back to 0), and a new claim brings attempts back to the
// stalled claim's value. Only replay_count tells the two claims apart; the
// stale one must apply nothing, and the new one applies under generation 1.
func TestDBReplayFencesOldClaim(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := dbtest.InsertEvent(t, pool, "aba_1", "customer.created", dbtest.CustomerObject("cus_dbtest_aba"))
	a, c := newTestWorker(pool), newTestWorker(pool)

	_, tokA, ok, err := a.claim(ctx)
	if err != nil || !ok || tokA != (claimToken{attempts: 1, generation: 0}) {
		t.Fatalf("claim A: token=%+v ok=%v err=%v, want attempts 1 generation 0", tokA, ok, err)
	}
	markDead(t, pool, rowID)
	replay(t, pool, rowID)
	_, tokC, ok, err := c.claim(ctx)
	if err != nil || !ok || tokC != (claimToken{attempts: 1, generation: 1}) {
		t.Fatalf("claim C after replay: token=%+v ok=%v err=%v, want attempts 1 generation 1", tokC, ok, err)
	}

	// A wakes up with the same attempts value as C's claim.
	if err := a.process(ctx, rowID, tokA); err != nil {
		t.Fatalf("process A: %v", err)
	}
	if n := dbtest.Count(t, pool, "customers"); n != 0 {
		t.Fatalf("stale claim A (previous generation) wrote %d customers, want 0", n)
	}
	if n := dbtest.Count(t, pool, "change_log"); n != 0 {
		t.Fatalf("stale claim A wrote %d change_log rows, want 0", n)
	}
	if r := dbtest.ReadRow(t, pool, rowID); r.Status != "pending" || r.Attempts != 1 {
		t.Fatalf("row after stale A: %+v, want pending with 1 attempt", r)
	}

	if err := c.process(ctx, rowID, tokC); err != nil {
		t.Fatalf("process C: %v", err)
	}
	wantStatus(t, pool, rowID, "done")
	cl := dbtest.ChangeLog(t, pool, rowID)
	if len(cl) != 1 || cl[0].Generation != 1 || cl[0].Seq != 1 || cl[0].Source != "apply" {
		t.Fatalf("change_log after C: %+v, want one apply row in generation 1", cl)
	}
	if n := dbtest.Count(t, pool, "customers"); n != 1 {
		t.Fatalf("customers after C: %d, want 1", n)
	}
}

// The same ABA for dead-lettering: a replayed poison event crashes the worker
// another maxAttempts times, so the new generation's claim has the same
// attempts value (maxAttempts+1) as a claim that stalled in the previous
// generation. That stalled claim must not dead-letter the row.
func TestDBDeadLetterFencedByGeneration(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := dbtest.InsertEvent(t, pool, "aba_dead", "customer.created", dbtest.CustomerObject("cus_dbtest_abadead"))
	a, b := newTestWorker(pool), newTestWorker(pool)

	dbtest.SetAttempts(t, pool, rowID, maxAttempts)
	_, tokA, ok, err := a.claim(ctx)
	if err != nil || !ok || tokA != (claimToken{attempts: maxAttempts + 1, generation: 0}) {
		t.Fatalf("claim A: token=%+v ok=%v err=%v", tokA, ok, err)
	}
	markDead(t, pool, rowID)
	replay(t, pool, rowID)
	dbtest.SetAttempts(t, pool, rowID, maxAttempts)
	_, tokB, ok, err := b.claim(ctx)
	if err != nil || !ok || tokB != (claimToken{attempts: maxAttempts + 1, generation: 1}) {
		t.Fatalf("claim B: token=%+v ok=%v err=%v", tokB, ok, err)
	}

	updated, err := a.deadLetterCrashed(ctx, rowID, tokA)
	if err != nil {
		t.Fatalf("dead-letter A: %v", err)
	}
	if updated {
		t.Fatal("stale claim A (previous generation) dead-lettered the replayed row")
	}
	if r := dbtest.ReadRow(t, pool, rowID); r.Status != "pending" || r.Attempts != maxAttempts+1 {
		t.Fatalf("row after stale A: %+v, want untouched (pending, attempts %d)", r, maxAttempts+1)
	}

	updated, err = b.deadLetterCrashed(ctx, rowID, tokB)
	if err != nil || !updated {
		t.Fatalf("dead-letter B: updated=%v err=%v", updated, err)
	}
	if r := dbtest.ReadRow(t, pool, rowID); r.Status != "dead" || r.Attempts != maxAttempts {
		t.Fatalf("row after B: %+v, want dead with attempts %d", r, maxAttempts)
	}
}

// The point of replay: an invoice that arrived before its customer is dead
// (unknown customer, last attempt). The customer arrives, an admin replays
// the invoice, and the real worker applies it under generation 1: a new set
// of change_log rows (the unique index allows it), numbered from 1, and the
// effect applied exactly once.
func TestDBReplayedDeadEventApplies(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := newTestWorker(pool)
	inv := invoiceForMissingCustomer(t, pool, "rp_dead")
	dbtest.SetAttempts(t, pool, inv, maxAttempts-1)
	processN(t, w, 1)
	if r := dbtest.ReadRow(t, pool, inv); r.Status != "dead" || !strings.Contains(r.LastErr, "unknown customer") {
		t.Fatalf("setup: %+v, want dead (unknown customer)", r)
	}
	if n := dbtest.Count(t, pool, "change_log"); n != 0 {
		t.Fatalf("dead event logged %d rows", n)
	}

	dbtest.InsertEvent(t, pool, "rp_dead_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_missing"))
	processN(t, w, 1) // the customer (the dead invoice is not pending)
	replay(t, pool, inv)
	processN(t, w, 1)

	r := dbtest.ReadRow(t, pool, inv)
	if r.Status != "done" || r.Attempts != 1 || r.LastErr != "" {
		t.Fatalf("replayed event: %+v, want done after 1 attempt with no last_error", r)
	}
	cl := dbtest.ChangeLog(t, pool, inv)
	if len(cl) != 2 {
		t.Fatalf("replayed event change_log: %+v, want 2 rows", cl)
	}
	// (wantChange asserts generation 0, so the rows are checked by hand.)
	for i, want := range []struct{ table, op string }{{"credit_grants", "insert"}, {"customers", "update"}} {
		c := cl[i]
		if c.Source != "apply" || c.Generation != 1 || c.Seq != i+1 || c.Table != want.table || c.Op != want.op {
			t.Fatalf("change_log row %+v, want apply, generation 1, seq %d, %s %s", c, i+1, want.op, want.table)
		}
	}
	var balance int64
	if err := pool.QueryRow(ctx,
		`SELECT credit_balance FROM customers WHERE stripe_customer_id = 'cus_dbtest_missing'`).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if n := dbtest.Count(t, pool, "credit_grants"); n != 1 || balance != 20 {
		t.Fatalf("grants %d, balance %d, want exactly 1 grant and balance 20", n, balance)
	}
	// Chain and final-state checks of the whole log are the checker's job:
	// sim.TestDBChangeLogCheckReplayedEventClean runs it over a replayed history.
}

// A stale event replayed is judged again against the subscription's current
// state: still older than what applied, so stale again with nothing logged,
// and the replay is visible only in replay_count and the audit trail.
func TestDBReplayedStaleEventIsStaleAgain(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := newTestWorker(pool)
	dbtest.InsertEvent(t, pool, "rs_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_rs"))
	dbtest.InsertEventAt(t, pool, "rs_new", "customer.subscription.updated", dbtest.DefaultCreated+10,
		dbtest.SubscriptionObject("sub_dbtest_rs", "cus_dbtest_rs", "past_due"))
	old := dbtest.InsertEventAt(t, pool, "rs_old", "customer.subscription.updated", dbtest.DefaultCreated,
		dbtest.SubscriptionObject("sub_dbtest_rs", "cus_dbtest_rs", "active"))
	processN(t, w, 3)
	wantStatus(t, pool, old, "stale")
	logged := dbtest.Count(t, pool, "change_log")

	replay(t, pool, old)
	wantStatus(t, pool, old, "pending")
	processN(t, w, 1)

	wantStatus(t, pool, old, "stale")
	var replays int
	if err := pool.QueryRow(ctx, `SELECT replay_count FROM inbox WHERE id = $1`, old).Scan(&replays); err != nil {
		t.Fatal(err)
	}
	if replays != 1 {
		t.Fatalf("replay_count %d, want 1", replays)
	}
	if n := dbtest.Count(t, pool, "change_log"); n != logged {
		t.Fatalf("change_log: %d rows, want %d (a stale replay changes nothing)", n, logged)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM subscriptions WHERE stripe_subscription_id = 'sub_dbtest_rs'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "past_due" {
		t.Fatalf("subscription status %s, want past_due (the newer event still wins)", status)
	}
}
