package admin

// DB tests for preview and log. Skipped unless DB tests are enabled; see
// internal/dbtest (run through `dev.ps1 db-test`).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/app"
	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/dbtest"
	"github.com/Deepak1365/relayguard/internal/outbox"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
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

// Before step 5a the worker's fence held FOR UPDATE on the inbox row while
// it applied (since 5a it is FOR NO KEY UPDATE, which the preview's KEY SHARE
// does not wait for; the test keeps FOR UPDATE because it is the
// conflicting case the inbox-first lock guards against, for any FOR UPDATE
// holder). Here a transaction plays that worker: it holds the fence on a
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

// Step 5a: the preview applies under the event's current replay_count, the
// generation the worker would use. Its changes are read back from that
// generation, and an apply row already committed in that generation (a
// worker applied the replayed event meanwhile) is detected as ErrChanged,
// while one from an older generation is not this apply's business.
func TestDBPreviewUsesReplayCount(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	inv := dbtest.InsertEvent(t, pool, "adm_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_adm", testCustomer, 2000))
	if _, err := pool.Exec(ctx, `UPDATE inbox SET status = 'dead', replay_count = 2 WHERE id = $1`, inv); err != nil {
		t.Fatal(err)
	}

	res, err := Preview(ctx, pool, inv, Options{})
	if err != nil || res.Outcome != OutcomeApply || res.Generation != 2 || len(res.Changes) != 2 {
		t.Fatalf("preview at replay_count 2: %+v, %v; want apply in generation 2 with 2 changes", res, err)
	}

	committed := func(generation int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO change_log (inbox_id, generation, source, seq, table_name, row_id, op, after_image)
			VALUES ($1, $2, 'apply', 1, 'customers', 1, 'insert', '{"id": 1}')`, inv, generation); err != nil {
			t.Fatal(err)
		}
	}
	committed(1) // an older generation: no conflict
	if res, err := Preview(ctx, pool, inv, Options{}); err != nil || res.Outcome != OutcomeApply {
		t.Fatalf("preview with an older generation's row: %+v, %v; want apply", res, err)
	}
	committed(2) // the current generation: a concurrent apply
	if res, err := Preview(ctx, pool, inv, Options{}); !errors.Is(err, ErrChanged) {
		t.Fatalf("preview with a committed row in the current generation: %+v, %v; want ErrChanged", res, err)
	}
}

// Reviewer follow-up (5a): the final re-read must compare replay_count too.
// A replay that fails again while the preview runs leaves the status as it
// was (dead -> pending -> dead) in a newer generation; the preview's own
// change_log rows are in the old generation, so change_log_apply_once does
// not collide either. The hook commits exactly that net change from another
// session just before the re-read. Its UPDATE does not wait: it touches no
// key column, so the preview's KEY SHARE on the row does not block it.
func TestDBPreviewDetectsReplayDuringPreview(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	inv := dbtest.InsertEvent(t, pool, "adm_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_adm", testCustomer, 2000))
	if _, err := pool.Exec(ctx, `UPDATE inbox SET status = 'dead', attempts = 8, last_error = 'first failure' WHERE id = $1`, inv); err != nil {
		t.Fatal(err)
	}

	// Control: without a concurrent change the same preview succeeds.
	if res, err := Preview(ctx, pool, inv, Options{}); err != nil || res.Outcome != OutcomeApply {
		t.Fatalf("preview without a concurrent replay: %+v, %v; want apply", res, err)
	}

	hookRan := false
	opts := Options{LockTimeout: 2 * time.Second, beforeReread: func() {
		hookRan = true
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if _, err := pool.Exec(hctx, `
			UPDATE inbox SET status = 'dead', attempts = 8, last_error = 'failed again after replay',
			       replay_count = replay_count + 1
			WHERE id = $1`, inv); err != nil {
			t.Errorf("replay during the preview: %v", err)
		}
	}}
	res, err := Preview(ctx, pool, inv, opts)
	if !hookRan {
		t.Fatal("the beforeReread hook did not run")
	}
	if !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), "generation 0 -> 1") {
		t.Fatalf("preview across a replay that re-failed (dead -> dead): %+v, %v; want ErrChanged naming generation 0 -> 1", res, err)
	}
}

// workerMidApply plays a worker inside process for pending event id: it
// takes the real fence lock (FOR NO KEY UPDATE) and runs the real app.Apply
// at generation 0, leaving the writes uncommitted. The caller must defer
// Rollback itself (a t.Cleanup here would run after dbtest's cleanup
// Truncate, which would then wait on this open transaction) and finishes the
// apply with workerFinish.
func workerMidApply(t *testing.T, pool *pgxpool.Pool, id int64) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	worker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := worker.QueryRow(ctx,
		`SELECT payload::text FROM inbox WHERE id = $1 AND status = 'pending' FOR NO KEY UPDATE`, id).Scan(&payload); err != nil {
		worker.Rollback(ctx)
		t.Fatalf("worker fence: %v", err)
	}
	var evt stripeevent.Event
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		worker.Rollback(ctx)
		t.Fatal(err)
	}
	if err := app.Apply(ctx, worker, changelog.NewApply(id, 0), outbox.NewWriter(id, 0), evt, app.Hooks{}); err != nil {
		worker.Rollback(ctx)
		t.Fatalf("worker apply: %v", err)
	}
	return worker
}

// workerFinish completes workerMidApply as process does (mark done, commit)
// and checks the event is done with wantChanges apply rows in generation 0,
// i.e. exactly the worker's rows: the preview left nothing behind.
func workerFinish(t *testing.T, pool *pgxpool.Pool, worker pgx.Tx, id int64, wantChanges int) {
	t.Helper()
	ctx := context.Background()
	if _, err := worker.Exec(ctx, `UPDATE inbox SET status = 'done', processed_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("worker mark done: %v", err)
	}
	if err := worker.Commit(ctx); err != nil {
		t.Fatalf("worker commit: %v", err)
	}
	if r := dbtest.ReadRow(t, pool, id); r.Status != "done" {
		t.Fatalf("event after the worker committed: %+v, want done", r)
	}
	cl := dbtest.ChangeLog(t, pool, id)
	if len(cl) != wantChanges {
		t.Fatalf("change_log after the worker committed: %+v, want %d rows", cl, wantChanges)
	}
	for i, r := range cl {
		if r.Source != "apply" || r.Generation != 0 || r.Seq != i+1 {
			t.Fatalf("change_log row %+v, want apply, generation 0, seq %d", r, i+1)
		}
	}
}

// The real 5a interleaving, worker still running (TestDBPreviewWaitsForFenceWithoutDeadlock
// above covers a FOR UPDATE holder, which production code no longer is).
// The "worker" holds the real fence lock and uncommitted writes from the real
// app.Apply (workerMidApply). What this proves:
//   - the preview no longer blocks on the inbox lock: its KEY SHARE does not
//     conflict with the fence's FOR NO KEY UPDATE, so it overlaps the apply;
//   - it gives up with ErrBusy within lock_timeout on the first key the
//     worker holds uncommitted. For customer.created that is the customer's
//     unique key (the insert comes first); for invoice.paid it is the
//     worker's change_log_apply_once key (same event, generation, seq 1),
//     hit when the preview logs its grant, before it reaches the customer
//     row (the mutation without lock_timeout showed both);
//   - it writes nothing, and the worker then commits the event done with
//     exactly its own change_log rows.
//
// What it does NOT prove: freedom from deadlock. The 200ms lock_timeout is
// below deadlock_timeout (1s) and the worker never waits on the preview, so
// no 40P01 can arise here. That rests on reasoning: the preview runs the
// same app.Apply as the worker and so takes locks in the same order, and its
// only extra lock (inbox KEY SHARE, taken first) conflicts with neither the
// fence nor anything the apply takes.
//
// ErrBusy, not ErrChanged, is deterministic here: the worker commits only
// after the preview has returned. A worker committing mid-preview:
// TestDBPreviewWorkerCommitsDuringPreview.
func TestDBPreviewOverlapsRealFence(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, pool *pgxpool.Pool) int64 // returns the pending event
		wantChanges int
	}{
		{"customer.created: worker inserted the customer", func(t *testing.T, pool *pgxpool.Pool) int64 {
			return dbtest.InsertEvent(t, pool, "adm_cus", "customer.created", dbtest.CustomerObject(testCustomer))
		}, 1},
		{"invoice.paid: worker logged the grant", func(t *testing.T, pool *pgxpool.Pool) int64 {
			appliedCustomer(t, pool)
			return dbtest.InsertEvent(t, pool, "adm_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_adm", testCustomer, 2000))
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := dbtest.Pool(t)
			ctx := context.Background()
			id := c.setup(t, pool)
			worker := workerMidApply(t, pool, id)
			defer worker.Rollback(ctx) // before the cleanup Truncate; no-op after commit

			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			start := time.Now()
			res, err := Preview(pctx, pool, id, Options{LockTimeout: 200 * time.Millisecond, StatementTimeout: 3 * time.Second})
			elapsed := time.Since(start)
			if !errors.Is(err, ErrBusy) {
				t.Fatalf("preview overlapping the worker: %+v, %v after %s; want ErrBusy", res, err, elapsed)
			}
			if elapsed > 2*time.Second {
				t.Fatalf("ErrBusy after %s, want about the 200ms lock_timeout", elapsed)
			}
			workerFinish(t, pool, worker, id, c.wantChanges)
		})
	}
}

// Reviewer round 2: the NORMAL customer.created race since 5a. A worker
// apply takes milliseconds, far below the default 2s lock_timeout, so a
// preview that overlaps it waits on the worker's uncommitted customer
// insert; when the worker commits, that insert fails with a unique
// violation (23505). Without the re-read in the error branch the preview
// returned that raw error; it must be ErrChanged (with the 23505 still
// wrapped), because the event was simply processed meanwhile.
//
// Instead of sleeping a fixed time, the test commits once the preview is
// actually blocked by the worker (pg_blocking_pids), so the race always
// takes the path under test.
func TestDBPreviewWorkerCommitsDuringPreview(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	id := dbtest.InsertEvent(t, pool, "adm_cus", "customer.created", dbtest.CustomerObject(testCustomer))
	worker := workerMidApply(t, pool, id)
	defer worker.Rollback(ctx) // before the cleanup Truncate; no-op after commit
	var workerPID int32
	if err := worker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&workerPID); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		res PreviewResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := Preview(ctx, pool, id, Options{LockTimeout: 2 * time.Second, StatementTimeout: 5 * time.Second})
		done <- outcome{res, err}
	}()

	blocked := false
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, workerPID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("the preview never blocked on the worker's uncommitted insert")
	}
	workerFinish(t, pool, worker, id, 1)

	select {
	case o := <-done:
		var pgErr *pgconn.PgError
		if !errors.Is(o.err, ErrChanged) || !strings.Contains(o.err.Error(), "status pending -> done") {
			t.Fatalf("preview when the worker committed meanwhile: %+v, %v; want ErrChanged (status pending -> done)", o.res, o.err)
		}
		if !errors.As(o.err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("ErrChanged %v does not wrap the original unique violation", o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("preview did not finish after the worker committed")
	}
	// Exactly the worker's customer: the preview wrote nothing.
	if n := dbtest.Count(t, pool, "customers"); n != 1 {
		t.Fatalf("customers: %d, want 1", n)
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

// Preview runs the real Apply, which enqueues; the rollback must take the
// outbox rows with everything else. The outbox is also checked directly
// because snapshot (above) predates the table.
func TestDBPreviewLeavesNoOutbox(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	// A pending customer.created: previewing it would enqueue a welcome email.
	cus := dbtest.InsertEvent(t, pool, "adm_obcus", "customer.created", dbtest.CustomerObject(testCustomer))
	res, err := Preview(ctx, pool, cus, Options{})
	if err != nil || res.Outcome != OutcomeApply {
		t.Fatalf("preview: %+v, %v, want apply", res, err)
	}
	// Sequences are not transactional: last_value 1 proves the preview
	// really inserted the row, so an empty table means the rollback removed it.
	var seq int64
	var called bool
	if err := pool.QueryRow(ctx, `SELECT last_value, is_called FROM outbox_id_seq`).Scan(&seq, &called); err != nil {
		t.Fatal(err)
	}
	if !called || seq != 1 {
		t.Fatalf("outbox_id_seq = %d (called %v), want 1: the preview must have enqueued before rolling back", seq, called)
	}
	if rows := dbtest.Outbox(t, pool); len(rows) != 0 {
		t.Fatalf("preview left outbox rows: %+v", rows)
	}
}
