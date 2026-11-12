package main

// Change-capture tests (phase 6 step 2). Same rules as main_db_test.go:
// skipped unless DB tests are enabled, run through `dev.ps1 db-test`.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/app"
	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/dbtest"
	"github.com/Deepak1365/relayguard/internal/fault"
)

// processN runs processOne n times and fails if any call found nothing or
// returned an error.
func processN(t *testing.T, w *worker, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		processed, err := w.processOne(context.Background())
		if err != nil || !processed {
			t.Fatalf("processOne %d/%d: processed=%v err=%v", i+1, n, processed, err)
		}
	}
}

func wantStatus(t *testing.T, pool *pgxpool.Pool, id int64, status string) {
	t.Helper()
	if r := dbtest.ReadRow(t, pool, id); r.Status != status {
		t.Fatalf("inbox row %d: %+v, want status %s", id, r, status)
	}
}

// wantChange checks the fixed parts of an apply row: source 'apply',
// generation 0 (no replays yet), no admin action, and seq/table/op.
func wantChange(t *testing.T, r dbtest.ChangeRow, seq int, table, op string) {
	t.Helper()
	if r.Source != "apply" || r.Generation != 0 || r.AdminActionID != nil ||
		r.Seq != seq || r.Table != table || r.Op != op {
		t.Fatalf("change_log row %+v, want source apply, generation 0, no admin action, seq %d, %s %s",
			r, seq, op, table)
	}
	if (op == changelog.OpInsert) != (r.Before == nil) || r.After == nil {
		t.Fatalf("change_log row %+v: wrong images for %s", r, op)
	}
}

// field returns an image column as text: json.Number for numbers, the
// string itself for strings.
func field(t *testing.T, img map[string]any, col string) string {
	t.Helper()
	switch v := img[col].(type) {
	case json.Number:
		return v.String()
	case string:
		return v
	default:
		t.Fatalf("image column %s = %#v (%T), want a number or string", col, img[col], img[col])
		return ""
	}
}

// sqlTrue runs a boolean query; used for jsonb comparisons done by Postgres
// itself, so the test compares exactly what is stored.
func sqlTrue(t *testing.T, pool *pgxpool.Pool, what, query string, args ...any) {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&ok); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if !ok {
		t.Fatalf("%s: false", what)
	}
}

const (
	// The next row's before image must equal the previous row's after image.
	chainSQL = `SELECT (SELECT before_image FROM change_log WHERE id = $1) =
	                   (SELECT after_image  FROM change_log WHERE id = $2)`
	// The last after image must equal the row as stored now. A plain jsonb
	// comparison is enough because every session is pinned to UTC (db.Connect).
	customerFinalSQL = `SELECT to_jsonb(c.*) = (SELECT after_image FROM change_log WHERE id = $2) FROM customers c WHERE c.id = $1`
	grantFinalSQL    = `SELECT to_jsonb(g.*) = (SELECT after_image FROM change_log WHERE id = $2) FROM credit_grants g WHERE g.id = $1`
	subFinalSQL      = `SELECT to_jsonb(s.*) = (SELECT after_image FROM change_log WHERE id = $2) FROM subscriptions s WHERE s.id = $1`
)

func TestDBApplyWritesChangeLog(t *testing.T) {
	pool := dbtest.Pool(t)
	cus := dbtest.InsertEvent(t, pool, "cl_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_cl"))
	inv := dbtest.InsertEvent(t, pool, "cl_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_cl", "cus_dbtest_cl", 2000))
	subC := dbtest.InsertEventAt(t, pool, "cl_sub_c", "customer.subscription.created", dbtest.DefaultCreated,
		dbtest.SubscriptionObject("sub_dbtest_cl", "cus_dbtest_cl", "active"))
	subU := dbtest.InsertEventAt(t, pool, "cl_sub_u", "customer.subscription.updated", dbtest.DefaultCreated+10,
		dbtest.SubscriptionObject("sub_dbtest_cl", "cus_dbtest_cl", "past_due"))
	processN(t, newTestWorker(pool), 4)
	for _, id := range []int64{cus, inv, subC, subU} {
		wantStatus(t, pool, id, "done")
	}

	// customer.created: one insert.
	cl := dbtest.ChangeLog(t, pool, cus)
	if len(cl) != 1 {
		t.Fatalf("customer.created: %d change_log rows, want 1: %+v", len(cl), cl)
	}
	wantChange(t, cl[0], 1, "customers", changelog.OpInsert)
	customerID := cl[0].RowID
	if field(t, cl[0].After, "stripe_customer_id") != "cus_dbtest_cl" ||
		field(t, cl[0].After, "credit_balance") != "0" || field(t, cl[0].After, "row_version") != "1" {
		t.Fatalf("customer insert image: %v", cl[0].After)
	}

	// invoice.paid: grant insert (seq 1), then customer update (seq 2).
	il := dbtest.ChangeLog(t, pool, inv)
	if len(il) != 2 {
		t.Fatalf("invoice.paid: %d change_log rows, want 2: %+v", len(il), il)
	}
	wantChange(t, il[0], 1, "credit_grants", changelog.OpInsert)
	if field(t, il[0].After, "amount") != "20" || field(t, il[0].After, "row_version") != "1" ||
		field(t, il[0].After, "customer_id") != strconv.FormatInt(customerID, 10) {
		t.Fatalf("grant insert image: %v", il[0].After)
	}
	wantChange(t, il[1], 2, "customers", changelog.OpUpdate)
	if il[1].RowID != customerID ||
		field(t, il[1].Before, "credit_balance") != "0" || field(t, il[1].After, "credit_balance") != "20" ||
		field(t, il[1].Before, "row_version") != "1" || field(t, il[1].After, "row_version") != "2" {
		t.Fatalf("customer update: row %d before %v after %v, want row %d balance 0->20, row_version 1->2",
			il[1].RowID, il[1].Before, il[1].After, customerID)
	}

	// Subscription: insert, then update.
	sc := dbtest.ChangeLog(t, pool, subC)
	su := dbtest.ChangeLog(t, pool, subU)
	if len(sc) != 1 || len(su) != 1 {
		t.Fatalf("subscription rows: created %d, updated %d, want 1 each", len(sc), len(su))
	}
	wantChange(t, sc[0], 1, "subscriptions", changelog.OpInsert)
	wantChange(t, su[0], 1, "subscriptions", changelog.OpUpdate)
	if su[0].RowID != sc[0].RowID ||
		field(t, su[0].Before, "status") != "active" || field(t, su[0].After, "status") != "past_due" ||
		field(t, su[0].Before, "row_version") != "1" || field(t, su[0].After, "row_version") != "2" ||
		field(t, su[0].After, "last_event_id") != dbtest.EventPrefix+"cl_sub_u" {
		t.Fatalf("subscription update: before %v after %v", su[0].Before, su[0].After)
	}

	// Chains: each update's before image is the previous after image, byte
	// for byte as jsonb (this would break if images were not canonical).
	sqlTrue(t, pool, "customer chain", chainSQL, il[1].ID, cl[0].ID)
	sqlTrue(t, pool, "subscription chain", chainSQL, su[0].ID, sc[0].ID)
	// Fold == final state: the last after image is the row as stored.
	sqlTrue(t, pool, "customer final", customerFinalSQL, customerID, il[1].ID)
	sqlTrue(t, pool, "grant final", grantFinalSQL, il[0].RowID, il[0].ID)
	sqlTrue(t, pool, "subscription final", subFinalSQL, su[0].RowID, su[0].ID)

	if n := dbtest.Count(t, pool, "change_log"); n != 5 {
		t.Fatalf("change_log total: %d, want 5", n)
	}
}

// invoice.paid with amount_paid < 100 grants 0 credits: credit_balance + 0
// changes nothing, but the trigger still bumps row_version, so the update
// must still be logged or the row_version chain has a gap.
func TestDBZeroCreditInvoiceLogsUpdate(t *testing.T) {
	pool := dbtest.Pool(t)
	dbtest.InsertEvent(t, pool, "zero_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_zero"))
	inv := dbtest.InsertEvent(t, pool, "zero_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_zero", "cus_dbtest_zero", 50))
	processN(t, newTestWorker(pool), 2)
	wantStatus(t, pool, inv, "done")

	il := dbtest.ChangeLog(t, pool, inv)
	if len(il) != 2 {
		t.Fatalf("0-credit invoice: %d change_log rows, want 2: %+v", len(il), il)
	}
	wantChange(t, il[0], 1, "credit_grants", changelog.OpInsert)
	if field(t, il[0].After, "amount") != "0" {
		t.Fatalf("grant image: %v, want amount 0", il[0].After)
	}
	wantChange(t, il[1], 2, "customers", changelog.OpUpdate)
	if field(t, il[1].Before, "credit_balance") != "0" || field(t, il[1].After, "credit_balance") != "0" ||
		field(t, il[1].Before, "row_version") != "1" || field(t, il[1].After, "row_version") != "2" {
		t.Fatalf("customer update: before %v after %v, want balance 0->0 and row_version 1->2", il[1].Before, il[1].After)
	}
	var stored int64
	if err := pool.QueryRow(context.Background(),
		`SELECT row_version FROM customers WHERE id = $1`, il[1].RowID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Fatalf("customers.row_version = %d, want 2 (matches the logged after image)", stored)
	}
	sqlTrue(t, pool, "customer final", customerFinalSQL, il[1].RowID, il[1].ID)
}

// Stale and skipped events are recorded in the inbox but change nothing, so
// they must leave no change_log rows. (Lost claims: TestDBLeaseExpiryAndFencing.)
func TestDBStaleAndSkippedWriteNoChangeLog(t *testing.T) {
	pool := dbtest.Pool(t)
	dbtest.InsertEvent(t, pool, "st_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_st"))
	newer := dbtest.InsertEventAt(t, pool, "st_sub_new", "customer.subscription.updated", dbtest.DefaultCreated+10,
		dbtest.SubscriptionObject("sub_dbtest_st", "cus_dbtest_st", "past_due"))
	older := dbtest.InsertEventAt(t, pool, "st_sub_old", "customer.subscription.created", dbtest.DefaultCreated,
		dbtest.SubscriptionObject("sub_dbtest_st", "cus_dbtest_st", "active"))
	skipped := dbtest.InsertEvent(t, pool, "st_skip", "charge.refunded", map[string]any{"id": "ch_dbtest_st"})
	processN(t, newTestWorker(pool), 4)

	wantStatus(t, pool, newer, "done")
	wantStatus(t, pool, older, "stale")
	wantStatus(t, pool, skipped, "skipped")
	if cl := dbtest.ChangeLog(t, pool, older); len(cl) != 0 {
		t.Fatalf("stale event logged %d rows: %+v", len(cl), cl)
	}
	if cl := dbtest.ChangeLog(t, pool, skipped); len(cl) != 0 {
		t.Fatalf("skipped event logged %d rows: %+v", len(cl), cl)
	}
	// customer insert + subscription insert, nothing else.
	if n := dbtest.Count(t, pool, "change_log"); n != 2 {
		t.Fatalf("change_log total: %d, want 2", n)
	}
	// The stale upsert's WHERE false fires no BEFORE UPDATE trigger, which is
	// why there is nothing to log for it.
	var version int64
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT row_version, status FROM subscriptions WHERE stripe_subscription_id = 'sub_dbtest_st'`).
		Scan(&version, &status); err != nil {
		t.Fatal(err)
	}
	if version != 1 || status != "past_due" {
		t.Fatalf("subscription after stale event: row_version %d status %s, want 1 and past_due", version, status)
	}
}

// AfterGrant fails after the grant and its log row are written. The savepoint
// rollback must remove both, and the failure is recorded for a retry.
func TestDBApplyErrorAfterWriteRollsBackChangeLog(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := newTestWorker(pool)
	dbtest.InsertEvent(t, pool, "err_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_err"))
	processN(t, w, 1)
	inv := dbtest.InsertEvent(t, pool, "err_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_err", "cus_dbtest_err", 2000))

	// Sequences are not transactional: another session sees nextval at once.
	// change_log_id_seq at 2 inside the hook proves the grant's log row was
	// already inserted (row 1 is the customer insert) before the hook ran.
	var seqAtHook int64 = -1
	w.hooks = app.Hooks{AfterGrant: func(string) error {
		if err := pool.QueryRow(ctx, `SELECT last_value FROM change_log_id_seq`).Scan(&seqAtHook); err != nil {
			t.Errorf("read change_log_id_seq in hook: %v", err)
		}
		return errors.New("injected after-grant failure")
	}}
	processN(t, w, 1)

	if seqAtHook != 2 {
		t.Fatalf("change_log_id_seq in the hook = %d, want 2 (the grant must be logged before AfterGrant)", seqAtHook)
	}
	r := dbtest.ReadRow(t, pool, inv)
	if r.Status != "pending" || r.Attempts != 1 || !strings.Contains(r.LastErr, "injected after-grant failure") {
		t.Fatalf("inbox row: %+v, want pending, attempts 1, last_error with the hook error", r)
	}
	if cl := dbtest.ChangeLog(t, pool, inv); len(cl) != 0 {
		t.Fatalf("failed apply left %d change_log rows: %+v", len(cl), cl)
	}
	if n := dbtest.Count(t, pool, "credit_grants"); n != 0 {
		t.Fatalf("credit_grants: %d, want 0 (rolled back)", n)
	}
	var balance, version int64
	if err := pool.QueryRow(ctx,
		`SELECT credit_balance, row_version FROM customers WHERE stripe_customer_id = 'cus_dbtest_err'`).
		Scan(&balance, &version); err != nil {
		t.Fatal(err)
	}
	if balance != 0 || version != 1 {
		t.Fatalf("customer: balance %d row_version %d, want 0 and 1", balance, version)
	}

	// The retry gets a fresh recorder: seq starts at 1 again and does not
	// collide with anything, because the failed rows were never committed.
	w.hooks = app.Hooks{}
	dbtest.ExpireLease(t, pool, inv)
	processN(t, w, 1)
	wantStatus(t, pool, inv, "done")
	il := dbtest.ChangeLog(t, pool, inv)
	if len(il) != 2 {
		t.Fatalf("after retry: %d change_log rows, want 2", len(il))
	}
	wantChange(t, il[0], 1, "credit_grants", changelog.OpInsert)
	wantChange(t, il[1], 2, "customers", changelog.OpUpdate)
}

// crashChildEnv makes TestDBCrashMidApplyLeavesNoChangeLog act as the child
// worker process instead of the test.
const crashChildEnv = "RELAYGUARD_WORKER_CRASH_CHILD"

// A real crash, not a returned error: a child process (this test binary,
// re-executed) runs the worker with fault.Always("after_grant") and exits
// with os.Exit(1) in the middle of the apply, with the grant and its log row
// written but uncommitted. Postgres must discard both with the connection.
func TestDBCrashMidApplyLeavesNoChangeLog(t *testing.T) {
	if os.Getenv(crashChildEnv) == "1" {
		runCrashChild(t)
		return
	}
	pool := dbtest.Pool(t)
	ctx := context.Background()
	dbtest.InsertEvent(t, pool, "crash_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_crash"))
	processN(t, newTestWorker(pool), 1)
	inv := dbtest.InsertEvent(t, pool, "crash_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_crash", "cus_dbtest_crash", 2000))

	cmd := exec.Command(os.Args[0], "-test.run=^TestDBCrashMidApplyLeavesNoChangeLog$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), crashChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("child: err=%v, want a non-zero exit; output:\n%s", err, out)
	}
	// A failing child test also exits non-zero; the fault's own log line
	// shows the exit really was the injected crash.
	if !strings.Contains(string(out), "FAULT: crashing at after_grant") {
		t.Fatalf("child exited (%v) without the injected crash; output:\n%s", err, out)
	}

	if n := dbtest.Count(t, pool, "change_log"); n != 1 {
		t.Fatalf("change_log after the crash: %d rows, want 1 (only the customer insert)", n)
	}
	// Sequences are not rolled back: id 1 is the customer insert, id 2 the
	// grant's log row the child wrote before it crashed. So the row really was
	// written, and the crash (not a missing write) is why it is gone.
	var seq int64
	if err := pool.QueryRow(ctx, `SELECT last_value FROM change_log_id_seq`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("change_log_id_seq after the crash = %d, want 2 (the child logged the grant, then crashed)", seq)
	}
	if n := dbtest.Count(t, pool, "credit_grants"); n != 0 {
		t.Fatalf("credit_grants after the crash: %d, want 0", n)
	}
	var balance int64
	if err := pool.QueryRow(ctx,
		`SELECT credit_balance FROM customers WHERE stripe_customer_id = 'cus_dbtest_crash'`).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		t.Fatalf("balance after the crash: %d, want 0", balance)
	}
	if r := dbtest.ReadRow(t, pool, inv); r.Status != "pending" || r.Attempts != 1 {
		t.Fatalf("inbox row after the crash: %+v, want pending with 1 attempt (claim committed, apply lost)", r)
	}

	// Recovery: once the lease expires a normal worker applies it once.
	dbtest.ExpireLease(t, pool, inv)
	processN(t, newTestWorker(pool), 1)
	wantStatus(t, pool, inv, "done")
	il := dbtest.ChangeLog(t, pool, inv)
	if len(il) != 2 {
		t.Fatalf("after recovery: %d change_log rows for the invoice, want exactly 2: %+v", len(il), il)
	}
	wantChange(t, il[0], 1, "credit_grants", changelog.OpInsert)
	wantChange(t, il[1], 2, "customers", changelog.OpUpdate)
}

// B1 regression. Inserting a credit_grants row makes its foreign key check
// take FOR KEY SHARE on the customer, held until commit. If addCredits read
// its before image with FOR UPDATE (which conflicts with KEY SHARE), two
// workers paying invoices for the same customer would each wait for the
// other's KEY SHARE: a deadlock. Here the "other worker" is an open
// transaction that inserted a grant for the same customer; the apply must
// not wait for it. One-sided, it shows up as blocking (until the deadline)
// rather than as 40P01, but the conflicting lock pair is the same.
func TestDBCustomerLockAllowsConcurrentGrant(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := newTestWorker(pool)
	dbtest.InsertEvent(t, pool, "kshare_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_kshare"))
	processN(t, w, 1)
	inv := dbtest.InsertEvent(t, pool, "kshare_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_kshare", "cus_dbtest_kshare", 2000))

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) // before the cleanup Truncate, which would wait on it
	tag, err := other.Exec(ctx, `
		INSERT INTO credit_grants (customer_id, stripe_invoice_id, amount)
		SELECT id, 'in_dbtest_kshare_other', 1 FROM customers WHERE stripe_customer_id = 'cus_dbtest_kshare'`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("other transaction's grant: rows=%d err=%v", tag.RowsAffected(), err)
	}

	// A healthy apply takes milliseconds; the deadline turns a lock wait into
	// a failure instead of a hang.
	procCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	processed, err := w.processOne(procCtx)
	if err != nil || !processed {
		t.Fatalf("processOne while another transaction holds KEY SHARE on the customer: processed=%v err=%v "+
			"(the customer pre-read must not conflict with FOR KEY SHARE)", processed, err)
	}
	wantStatus(t, pool, inv, "done")
	if il := dbtest.ChangeLog(t, pool, inv); len(il) != 2 {
		t.Fatalf("invoice change_log rows: %d, want 2", len(il))
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

// runCrashChild is the child side. It does not use dbtest.Pool: the parent
// holds the DB test lock and owns the tables, so the child must neither wait
// for that lock nor truncate anything.
func runCrashChild(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("child connect: %v", err)
	}
	w := newWorker(pool, fault.Always("after_grant"), defaultLease)
	processed, err := w.processOne(ctx)
	// Reaching this line means the fault did not fire.
	t.Fatalf("child survived the after_grant fault: processed=%v err=%v", processed, err)
}

// The partial unique index (inbox_id, generation, seq) WHERE source='apply'
// is the database backstop for "one generation of an event commits one set
// of changes": a second apply of the same generation cannot commit its log.
// A new generation (replay, step 5) can.
func TestDBChangeLogApplyOnceIndex(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	inboxID := dbtest.InsertEvent(t, pool, "once_1", "customer.created", dbtest.CustomerObject("cus_dbtest_once"))

	record := func(generation int) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		rec := changelog.NewApply(inboxID, generation)
		if err := rec.Record(ctx, tx, changelog.Change{
			Table: "customers", RowID: 1, Op: changelog.OpInsert, After: []byte(`{"id": 1}`)}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	if err := record(0); err != nil {
		t.Fatalf("first apply, generation 0: %v", err)
	}
	err := record(0)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "change_log_apply_once" {
		t.Fatalf("second apply, generation 0: err=%v, want a unique violation on change_log_apply_once", err)
	}
	if err := record(1); err != nil {
		t.Fatalf("apply, generation 1: %v", err)
	}
	if n := dbtest.Count(t, pool, "change_log"); n != 2 {
		t.Fatalf("change_log: %d rows, want 2", n)
	}
}

// R3: images must be written in UTC whatever the server default is, so
// jsonb comparisons between images (and with to_jsonb of the live row) work.
func TestDBImagesAreUTC(t *testing.T) {
	pool := dbtest.Pool(t)
	var tz string
	if err := pool.QueryRow(context.Background(), `SHOW TimeZone`).Scan(&tz); err != nil {
		t.Fatal(err)
	}
	if tz != db.SessionTimeZone {
		t.Fatalf("session TimeZone %q, want %q (set by db.Connect)", tz, db.SessionTimeZone)
	}
	cus := dbtest.InsertEvent(t, pool, "utc_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_utc"))
	processN(t, newTestWorker(pool), 1)
	cl := dbtest.ChangeLog(t, pool, cus)
	if len(cl) != 1 {
		t.Fatalf("%d change_log rows, want 1", len(cl))
	}
	if created := field(t, cl[0].After, "created_at"); !strings.HasSuffix(created, "+00:00") {
		t.Fatalf("image created_at %q, want a +00:00 offset", created)
	}
}
