package admin

// DB tests for preview and log. Skipped unless DB tests are enabled; see
// internal/dbtest (run through `dev.ps1 db-test`).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

const testCustomer = "cus_dbtest_adm"

// snapshot renders every table a preview could touch as JSON, in id order.
// Sequences are deliberately not included: they are not transactional, so a
// rolled-back insert still consumes ids (the "provisional ids" note).
func snapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var b strings.Builder
	// Literal table names from this test, never input.
	for _, table := range []string{"customers", "credit_grants", "subscriptions", "change_log", "admin_actions", "inbox"} {
		var s string
		if err := pool.QueryRow(context.Background(),
			`SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY t.id), '[]')::text FROM `+table+` t`).Scan(&s); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		b.WriteString(table + ": " + s + "\n")
	}
	return b.String()
}

// appliedCustomer stores and applies customer.created for testCustomer.
func appliedCustomer(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	id := dbtest.InsertEvent(t, pool, "adm_cus", "customer.created", dbtest.CustomerObject(testCustomer))
	if got := dbtest.ApplyInbox(t, pool, id); got != "done" {
		t.Fatalf("customer.created: %s", got)
	}
	return id
}

func TestDBPreviewWritesNothing(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	inv := dbtest.InsertEvent(t, pool, "adm_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_adm", testCustomer, 2000))
	sub := dbtest.InsertEvent(t, pool, "adm_sub", "customer.subscription.created",
		dbtest.SubscriptionObject("sub_dbtest_adm", testCustomer, "active"))

	before := snapshot(t, pool)
	for _, c := range []struct {
		id      int64
		changes int
	}{{inv, 2}, {sub, 1}} {
		res, err := Preview(ctx, pool, c.id, Options{})
		if err != nil {
			t.Fatalf("preview %d: %v", c.id, err)
		}
		if res.Outcome != OutcomeApply || len(res.Changes) != c.changes {
			t.Fatalf("preview %d: outcome %s with %d changes, want apply with %d", c.id, res.Outcome, len(res.Changes), c.changes)
		}
	}
	if after := snapshot(t, pool); after != before {
		t.Fatalf("preview wrote to the database.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDBPreviewOutcomes(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	// Not found.
	if _, err := Preview(ctx, pool, 999999, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: err %v, want ErrNotFound", err)
	}

	// Dead invoice for a customer that does not exist yet: the apply would
	// fail now. A real outcome, not a refusal.
	inv := dbtest.InsertEvent(t, pool, "adm_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_adm", testCustomer, 2000))
	if _, err := pool.Exec(ctx, `UPDATE inbox SET status = 'dead', attempts = 8 WHERE id = $1`, inv); err != nil {
		t.Fatal(err)
	}
	res, err := Preview(ctx, pool, inv, Options{})
	if err != nil || res.Outcome != OutcomeError || !strings.Contains(res.Reason, "unknown customer "+testCustomer) || len(res.Changes) != 0 {
		t.Fatalf("dead invoice, no customer: %+v, %v; want outcome error (unknown customer) with no changes", res, err)
	}

	// Once the customer exists, the same dead event would apply.
	appliedCustomer(t, pool)
	res, err = Preview(ctx, pool, inv, Options{})
	if err != nil || res.Outcome != OutcomeApply || len(res.Changes) != 2 || res.Status != "dead" {
		t.Fatalf("dead invoice, customer exists: %+v, %v; want apply with 2 changes", res, err)
	}
	if c := res.Changes[1]; c.Table != "customers" || c.Op != changelog.OpUpdate {
		t.Fatalf("second change %+v, want the customer update", c)
	}

	// Stale: a newer subscription event already applied.
	newer := dbtest.InsertEventAt(t, pool, "adm_sub_new", "customer.subscription.updated", dbtest.DefaultCreated+10,
		dbtest.SubscriptionObject("sub_dbtest_adm", testCustomer, "past_due"))
	if got := dbtest.ApplyInbox(t, pool, newer); got != "done" {
		t.Fatalf("newer subscription event: %s", got)
	}
	older := dbtest.InsertEventAt(t, pool, "adm_sub_old", "customer.subscription.updated", dbtest.DefaultCreated,
		dbtest.SubscriptionObject("sub_dbtest_adm", testCustomer, "active"))
	res, err = Preview(ctx, pool, older, Options{})
	if err != nil || res.Outcome != OutcomeStale || len(res.Changes) != 0 {
		t.Fatalf("older subscription event: %+v, %v; want stale", res, err)
	}

	// Skip: unhandled type.
	skip := dbtest.InsertEvent(t, pool, "adm_skip", "charge.refunded", map[string]any{"id": "ch_dbtest_adm"})
	res, err = Preview(ctx, pool, skip, Options{})
	if err != nil || res.Outcome != OutcomeSkip {
		t.Fatalf("unhandled type: %+v, %v; want skip", res, err)
	}
}

func TestDBPreviewRefusesDone(t *testing.T) {
	pool := dbtest.Pool(t)
	cus := appliedCustomer(t, pool)
	before := snapshot(t, pool)
	_, err := Preview(context.Background(), pool, cus, Options{})
	if !errors.Is(err, ErrNotPreviewable) || !strings.Contains(err.Error(), "use log") {
		t.Fatalf("done event: err %v, want ErrNotPreviewable pointing at log", err)
	}
	if after := snapshot(t, pool); after != before {
		t.Fatal("refused preview changed the database")
	}
}

// A worker (here: another transaction) holds the customer row. The preview
// must give up after its lock_timeout with ErrBusy instead of queueing
// behind the worker (and making the worker's next statements queue behind it).
func TestDBPreviewLockTimeout(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	inv := dbtest.InsertEvent(t, pool, "adm_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_adm", testCustomer, 2000))

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) // before the cleanup Truncate
	if _, err := other.Exec(ctx,
		`SELECT 1 FROM customers WHERE stripe_customer_id = $1 FOR NO KEY UPDATE`, testCustomer); err != nil {
		t.Fatal(err)
	}

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = Preview(pctx, pool, inv, Options{LockTimeout: 200 * time.Millisecond, StatementTimeout: 5 * time.Second})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("preview against a locked customer: err %v after %s, want ErrBusy", err, elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ErrBusy after %s, want about the 200ms lock_timeout", elapsed)
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// With the lock gone the same preview succeeds.
	if res, err := Preview(ctx, pool, inv, Options{}); err != nil || res.Outcome != OutcomeApply {
		t.Fatalf("preview after unlock: %+v, %v", res, err)
	}
}

func TestDBLog(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	inv := dbtest.InsertEvent(t, pool, "adm_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_adm", testCustomer, 2000))
	if got := dbtest.ApplyInbox(t, pool, inv); got != "done" {
		t.Fatalf("invoice: %s", got)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome) VALUES ('replay', $1, 'dbtest', 'log test', 'refused')`,
		inv); err != nil {
		t.Fatal(err)
	}
	res, err := Log(ctx, pool, inv)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" || len(res.Changes) != 2 || len(res.Actions) != 1 {
		t.Fatalf("log: %+v, want done with 2 changes and 1 action", res)
	}
	if c := res.Changes[0]; c.Seq != 1 || c.Source != "apply" || c.Generation != 0 || c.Change.Table != "credit_grants" {
		t.Fatalf("first change %+v", c)
	}
	if a := res.Actions[0]; a.Action != "replay" || a.Outcome != "refused" || a.Actor != "dbtest" || a.Detail != "{}" {
		t.Fatalf("action %+v", a)
	}
	if _, err := Log(ctx, pool, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("log of unknown id: %v, want ErrNotFound", err)
	}
}

// Until step 5a the worker's fence holds FOR UPDATE on the inbox row while
// it applies. Here a transaction plays that worker: it holds the fence on a
// pending customer.created, the preview starts and must wait for it holding
// nothing, and the "worker" then inserts the same customer. Without the
// preview's inbox-first lock, the preview would already hold an
// uncommitted customer row and wait on the fence for its change_log FK,
// while the worker waits on that customer row: a deadlock (40P01), which
// Postgres may resolve by aborting the worker.
func TestDBPreviewWaitsForFenceWithoutDeadlock(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	cus := dbtest.InsertEvent(t, pool, "adm_cus", "customer.created", dbtest.CustomerObject(testCustomer))

	fence, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Rollback(ctx)
	var fencePID int32
	if err := fence.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&fencePID); err != nil {
		t.Fatal(err)
	}
	if _, err := fence.Exec(ctx, `SELECT 1 FROM inbox WHERE id = $1 FOR UPDATE`, cus); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		res PreviewResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		// Long lock_timeout: the preview must still be waiting when the
		// "worker" acts.
		res, err := Preview(ctx, pool, cus, Options{LockTimeout: 10 * time.Second, StatementTimeout: 15 * time.Second})
		done <- outcome{res, err}
	}()

	// Wait until the preview is blocked by the fence (whatever it waits on).
	blocked := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, fencePID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("the preview never blocked on the fence")
	}

	// The worker's apply: insert the customer, mark the event done, commit.
	if _, err := fence.Exec(ctx,
		`INSERT INTO customers (stripe_customer_id, email) VALUES ($1, 'fence@example.test')`, testCustomer); err != nil {
		t.Fatalf("worker-side insert while a preview waits: %v (40P01 = deadlock with the preview)", err)
	}
	if _, err := fence.Exec(ctx, `UPDATE inbox SET status = 'done' WHERE id = $1`, cus); err != nil {
		t.Fatal(err)
	}
	if err := fence.Commit(ctx); err != nil {
		t.Fatalf("worker-side commit: %v", err)
	}

	// The preview then gets the lock and sees an event that is now done.
	select {
	case o := <-done:
		if !errors.Is(o.err, ErrNotPreviewable) {
			t.Fatalf("preview after the worker committed: %+v, %v; want ErrNotPreviewable (the event is done now)", o.res, o.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("preview did not finish after the fence was released")
	}
}

// A committed apply row for the same event and generation means a worker
// applied it while the preview ran: the unique index stops the preview's own
// change_log insert, and the preview reports ErrChanged, writing nothing.
func TestDBPreviewDetectsConcurrentApply(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	cus := dbtest.InsertEvent(t, pool, "adm_cus", "customer.created", dbtest.CustomerObject(testCustomer))
	if _, err := pool.Exec(ctx, `
		INSERT INTO change_log (inbox_id, generation, source, seq, table_name, row_id, op, after_image)
		VALUES ($1, 0, 'apply', 1, 'customers', 1, 'insert', '{"id": 1}')`, cus); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, pool)
	res, err := Preview(ctx, pool, cus, Options{})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("preview with a committed apply row: %+v, %v; want ErrChanged", res, err)
	}
	if after := snapshot(t, pool); after != before {
		t.Fatalf("preview wrote to the database.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A failure outside the business allow-list (here: the customer already
// exists, a unique violation) is not an "apply would fail" outcome: the
// preview returns an error, so the CLI exits non-zero.
func TestDBPreviewUnlistedFailureIsAnError(t *testing.T) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	dup := dbtest.InsertEvent(t, pool, "adm_cus_dup", "customer.created", dbtest.CustomerObject(testCustomer))
	res, err := Preview(context.Background(), pool, dup, Options{})
	if err == nil || errors.Is(err, ErrBusy) || errors.Is(err, ErrChanged) || res.Outcome != "" {
		t.Fatalf("duplicate customer.created: %+v, %v; want a plain error and no outcome", res, err)
	}
}
