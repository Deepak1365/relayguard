package main

// Outbox enqueue tests (phase 9 step 9a). The outbox row is written by
// app.Apply inside the worker's savepoint, so it must share the effect's fate
// in every path. Same rules as main_db_test.go: skipped unless DB tests are
// enabled, run through `dev.ps1 db-test`.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Deepak1365/relayguard/internal/app"
	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/dbtest"
	"github.com/Deepak1365/relayguard/internal/fault"
	"github.com/Deepak1365/relayguard/internal/outbox"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

func wantOutboxCount(t *testing.T, rows []dbtest.OutboxRow, want int, what string) {
	t.Helper()
	if len(rows) != want {
		t.Fatalf("%s: %d outbox rows, want %d: %+v", what, len(rows), want, rows)
	}
}

// One message per effect kind, with the stable key, generation 0 and a
// payload built from the event and the customer's stored email.
func TestDBOutboxEnqueuedWithApply(t *testing.T) {
	pool := dbtest.Pool(t)
	w := newTestWorker(pool)
	cus := dbtest.InsertEvent(t, pool, "ob_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_ob"))
	inv := dbtest.InsertEvent(t, pool, "ob_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_ob", "cus_dbtest_ob", 2500))
	sub := dbtest.InsertEvent(t, pool, "ob_sub", "customer.subscription.created",
		dbtest.SubscriptionObject("sub_dbtest_ob", "cus_dbtest_ob", "active"))
	processN(t, w, 3)

	rows := dbtest.Outbox(t, pool)
	wantOutboxCount(t, rows, 3, "after three applies")
	want := []struct {
		inbox            int64
		kind, key, bodyQ string
	}{
		{cus, "welcome_email", "rg:stripe:evt_dbtest_ob_cus:welcome_email",
			`SELECT payload = '{"to": "cus_dbtest_ob@example.test", "customer_id": "cus_dbtest_ob"}'::jsonb FROM outbox WHERE inbox_id = $1`},
		{inv, "receipt_email", "rg:stripe:evt_dbtest_ob_inv:receipt_email",
			`SELECT payload = '{"to": "cus_dbtest_ob@example.test", "invoice_id": "in_dbtest_ob", "credits": 25}'::jsonb FROM outbox WHERE inbox_id = $1`},
		{sub, "subscription_notice", "rg:stripe:evt_dbtest_ob_sub:subscription_notice",
			`SELECT payload = '{"to": "cus_dbtest_ob@example.test", "subscription_id": "sub_dbtest_ob", "status": "active"}'::jsonb FROM outbox WHERE inbox_id = $1`},
	}
	for i, c := range want {
		r := rows[i]
		if r.InboxID != c.inbox || r.Kind != c.kind || r.Key != c.key || r.Generation != 0 ||
			r.Status != "pending" || r.Attempts != 0 {
			t.Fatalf("outbox row %d: %+v, want inbox %d kind %s key %s generation 0 pending attempts 0",
				i, r, c.inbox, c.kind, c.key)
		}
		sqlTrue(t, pool, "payload of "+c.kind, c.bodyQ, c.inbox)
	}

	// A subscription update that applies enqueues a second notice (a new
	// event, so a new key).
	upd := dbtest.InsertEventAt(t, pool, "ob_upd", "customer.subscription.updated", dbtest.DefaultCreated+5,
		dbtest.SubscriptionObject("sub_dbtest_ob", "cus_dbtest_ob", "past_due"))
	processN(t, w, 1)
	wantStatus(t, pool, upd, "done")
	rows = dbtest.Outbox(t, pool)
	wantOutboxCount(t, rows, 4, "after the update")
	if rows[3].Kind != "subscription_notice" || rows[3].Key != "rg:stripe:evt_dbtest_ob_upd:subscription_notice" {
		t.Fatalf("update notice: %+v", rows[3])
	}
}

// AfterGrant fails after the grant is written, before the receipt is
// enqueued (and before addCredits). A failed apply leaves no outbox row for
// the invoice, and the retry enqueues exactly one. This does not prove that
// an already-written outbox row is rolled back (nothing is written before
// the failure); TestDBOutboxCrashBeforeCommit and TestDBPreviewLeavesNoOutbox
// cover that.
func TestDBOutboxNoneOnFailedApply(t *testing.T) {
	pool := dbtest.Pool(t)
	w := newTestWorker(pool)
	dbtest.InsertEvent(t, pool, "obf_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_obf"))
	processN(t, w, 1)
	inv := dbtest.InsertEvent(t, pool, "obf_inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_obf", "cus_dbtest_obf", 2000))

	w.hooks = app.Hooks{AfterGrant: func(string) error { return errors.New("injected after-grant failure") }}
	processN(t, w, 1)
	if r := dbtest.ReadRow(t, pool, inv); r.Status != "pending" || r.Attempts != 1 {
		t.Fatalf("inbox row: %+v, want pending with 1 attempt", r)
	}
	// Only the customer's welcome email exists; the failed invoice left none.
	rows := dbtest.Outbox(t, pool)
	wantOutboxCount(t, rows, 1, "after the failed apply")
	if rows[0].Kind != "welcome_email" {
		t.Fatalf("outbox after the failed apply: %+v, want only the welcome email", rows)
	}

	w.hooks = app.Hooks{}
	dbtest.ExpireLease(t, pool, inv)
	processN(t, w, 1)
	wantStatus(t, pool, inv, "done")
	rows = dbtest.Outbox(t, pool)
	wantOutboxCount(t, rows, 2, "after the retry")
	if rows[1].InboxID != inv || rows[1].Kind != "receipt_email" {
		t.Fatalf("outbox after the retry: %+v", rows)
	}
}

// A stale subscription event is judged by the upsert; its notice must not
// exist. Only the applied event's notice does.
func TestDBOutboxNoneForStale(t *testing.T) {
	pool := dbtest.Pool(t)
	w := newTestWorker(pool)
	dbtest.InsertEvent(t, pool, "obs_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_obs"))
	newer := dbtest.InsertEventAt(t, pool, "obs_new", "customer.subscription.updated", dbtest.DefaultCreated+10,
		dbtest.SubscriptionObject("sub_dbtest_obs", "cus_dbtest_obs", "past_due"))
	old := dbtest.InsertEventAt(t, pool, "obs_old", "customer.subscription.updated", dbtest.DefaultCreated,
		dbtest.SubscriptionObject("sub_dbtest_obs", "cus_dbtest_obs", "active"))
	processN(t, w, 3)
	wantStatus(t, pool, old, "stale")

	rows := dbtest.Outbox(t, pool)
	wantOutboxCount(t, rows, 2, "welcome + the applied notice")
	if rows[1].InboxID != newer || rows[1].Kind != "subscription_notice" {
		t.Fatalf("outbox: %+v, want the newer event's notice and nothing for the stale one", rows)
	}
	for _, r := range rows {
		if r.InboxID == old {
			t.Fatalf("stale event %d has an outbox row: %+v", old, r)
		}
	}

	// The worker's savepoint rollback would hide a stale branch that enqueued
	// too early, so also run Apply directly on the stale event and look
	// inside its own transaction, before any rollback.
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	evt := stripeevent.Event{ID: dbtest.EventPrefix + "obs_old", Type: "customer.subscription.updated", Created: dbtest.DefaultCreated}
	evt.Data.Object = json.RawMessage(`{"id":"sub_dbtest_obs","customer":"cus_dbtest_obs","status":"active"}`)
	out := outbox.NewWriter(old, 0)
	if err := app.Apply(ctx, tx, changelog.NewApply(old, 0), out, evt, app.Hooks{}); !errors.Is(err, app.ErrStale) {
		t.Fatalf("direct Apply of the stale event: %v, want ErrStale", err)
	}
	var inTx int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE inbox_id = $1`, old).Scan(&inTx); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || inTx != 0 {
		t.Fatalf("stale Apply enqueued %d messages (%d rows visible in its tx), want 0", out.Len(), inTx)
	}
}

func TestDBOutboxNoneForSkipped(t *testing.T) {
	pool := dbtest.Pool(t)
	id := dbtest.InsertEvent(t, pool, "obk_1", "charge.refunded", map[string]any{"id": "ch_dbtest_obk"})
	processN(t, newTestWorker(pool), 1)
	wantStatus(t, pool, id, "skipped")
	wantOutboxCount(t, dbtest.Outbox(t, pool), 0, "skipped event")
}

// A payload that does not decode is dead-lettered before Apply runs.
func TestDBOutboxNoneForDecodeDead(t *testing.T) {
	pool := dbtest.Pool(t)
	id := dbtest.InsertEvent(t, pool, "obd_1", "customer.created", dbtest.CustomerObject("cus_dbtest_obd"))
	// "created" must be a number: json.Unmarshal into stripeevent.Event fails.
	if _, err := pool.Exec(context.Background(),
		`UPDATE inbox SET payload = jsonb_set(payload, '{created}', '"not a number"') WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	processN(t, newTestWorker(pool), 1)
	r := dbtest.ReadRow(t, pool, id)
	if r.Status != "dead" || !strings.HasPrefix(r.LastErr, app.DecodePayloadPrefix) {
		t.Fatalf("inbox row: %+v, want dead with a decode error", r)
	}
	wantOutboxCount(t, dbtest.Outbox(t, pool), 0, "decode-dead event")
}

// A worker that lost its claim (the generation moved on) applies nothing and
// so enqueues nothing; the claim that holds the fence enqueues once, at the
// new generation.
func TestDBOutboxNoneOnLostFence(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	rowID := dbtest.InsertEvent(t, pool, "obl_1", "customer.created", dbtest.CustomerObject("cus_dbtest_obl"))
	a, c := newTestWorker(pool), newTestWorker(pool)
	_, tokA, ok, err := a.claim(ctx)
	if err != nil || !ok {
		t.Fatalf("claim A: ok=%v err=%v", ok, err)
	}
	markDead(t, pool, rowID)
	replay(t, pool, rowID)
	_, tokC, ok, err := c.claim(ctx)
	if err != nil || !ok {
		t.Fatalf("claim C: ok=%v err=%v", ok, err)
	}

	if err := a.process(ctx, rowID, tokA); err != nil {
		t.Fatalf("process A: %v", err)
	}
	wantOutboxCount(t, dbtest.Outbox(t, pool), 0, "after the lost-fence worker")

	if err := c.process(ctx, rowID, tokC); err != nil {
		t.Fatalf("process C: %v", err)
	}
	rows := dbtest.Outbox(t, pool)
	wantOutboxCount(t, rows, 1, "after the fence holder")
	if rows[0].Generation != 1 || rows[0].InboxID != rowID {
		t.Fatalf("outbox row %+v, want inbox %d generation 1", rows[0], rowID)
	}
}

// outboxCrashChildVar makes TestDBOutboxCrashBeforeCommit act as the child
// worker process instead of the test.
const outboxCrashChildVar = "RELAYGUARD_WORKER_OUTBOX_CRASH_CHILD"

// A real crash at before_commit: the apply succeeded (state, change_log and
// outbox row all written in the open transaction) and the process exits
// before COMMIT. Postgres must discard all of it, and a later worker's retry
// must leave exactly one outbox row.
func TestDBOutboxCrashBeforeCommit(t *testing.T) {
	if os.Getenv(outboxCrashChildVar) == "1" {
		pool, err := db.Connect(context.Background(), os.Getenv("DATABASE_URL"))
		if err != nil {
			t.Fatalf("child connect: %v", err)
		}
		w := newWorker(pool, fault.Always("before_commit"), defaultLease)
		processed, err := w.processOne(context.Background())
		t.Fatalf("child survived the before_commit fault: processed=%v err=%v", processed, err)
	}
	pool := dbtest.Pool(t)
	id := dbtest.InsertEvent(t, pool, "obc_1", "customer.created", dbtest.CustomerObject("cus_dbtest_obc"))

	cmd := exec.Command(os.Args[0], "-test.run=^TestDBOutboxCrashBeforeCommit$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), outboxCrashChildVar+"=1")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("child: err=%v, want a non-zero exit; output:\n%s", err, out)
	}
	// A failing child test also exits non-zero; the fault's own log line
	// shows the exit really was the injected crash.
	if !strings.Contains(string(out), "FAULT: crashing at before_commit") {
		t.Fatalf("child exited (%v) without the injected crash; output:\n%s", err, out)
	}

	wantOutboxCount(t, dbtest.Outbox(t, pool), 0, "after the crash")
	// Sequences are not rolled back: last_value 1 proves the child really
	// inserted the row, so the crash (not a missing write) is why it is gone.
	var seq int64
	var called bool
	if err := pool.QueryRow(context.Background(), `SELECT last_value, is_called FROM outbox_id_seq`).Scan(&seq, &called); err != nil {
		t.Fatal(err)
	}
	if !called || seq != 1 {
		t.Fatalf("outbox_id_seq = %d (called %v), want 1: the child must have written the row before crashing", seq, called)
	}
	if n := dbtest.Count(t, pool, "customers"); n != 0 {
		t.Fatalf("customers after the crash: %d, want 0", n)
	}
	if r := dbtest.ReadRow(t, pool, id); r.Status != "pending" || r.Attempts != 1 {
		t.Fatalf("inbox row after the crash: %+v, want pending with 1 attempt", r)
	}

	dbtest.ExpireLease(t, pool, id)
	processN(t, newTestWorker(pool), 1)
	wantStatus(t, pool, id, "done")
	rows := dbtest.Outbox(t, pool)
	wantOutboxCount(t, rows, 1, "after the retry")
	if rows[0].InboxID != id || rows[0].Kind != "welcome_email" {
		t.Fatalf("outbox after the retry: %+v", rows[0])
	}
}

// outbox_once is the database backstop for "one message per event and kind":
// a second row for the same (inbox, kind) fails on outbox_once even with a
// different key, and a reused key fails on the key's own unique constraint.
func TestDBOutboxOncePerEventKind(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	id := dbtest.InsertEvent(t, pool, "obo_1", "customer.created", dbtest.CustomerObject("cus_dbtest_obo"))
	other := dbtest.InsertEvent(t, pool, "obo_2", "customer.created", dbtest.CustomerObject("cus_dbtest_obo2"))
	insert := func(inbox int64, kind, key string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO outbox (inbox_id, generation, kind, idempotency_key, payload)
			VALUES ($1, 0, $2, $3, '{}')`, inbox, kind, key)
		return err
	}
	if err := insert(id, "welcome_email", "k1"); err != nil {
		t.Fatal(err)
	}
	wantConstraint := func(err error, name string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != name {
			t.Fatalf("err = %v, want unique violation of %s", err, name)
		}
	}
	wantConstraint(insert(id, "welcome_email", "k2"), "outbox_once")
	wantConstraint(insert(other, "welcome_email", "k1"), "outbox_idempotency_key_key")
	// A different kind for the same event is allowed.
	if err := insert(id, "receipt_email", "k3"); err != nil {
		t.Fatalf("different kind for the same event: %v", err)
	}
}

// A stale event that is replayed and then applies enqueues at generation 1.
// The subscription row is aged by a direct UPDATE so the replay applies
// instead of being stale again (the same state an out-of-order history could
// have left).
func TestDBReplayStaleEnqueuesAtGeneration1(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := newTestWorker(pool)
	dbtest.InsertEvent(t, pool, "obr_cus", "customer.created", dbtest.CustomerObject("cus_dbtest_obr"))
	dbtest.InsertEventAt(t, pool, "obr_new", "customer.subscription.updated", dbtest.DefaultCreated+10,
		dbtest.SubscriptionObject("sub_dbtest_obr", "cus_dbtest_obr", "past_due"))
	old := dbtest.InsertEventAt(t, pool, "obr_old", "customer.subscription.updated", dbtest.DefaultCreated+5,
		dbtest.SubscriptionObject("sub_dbtest_obr", "cus_dbtest_obr", "active"))
	processN(t, w, 3)
	wantStatus(t, pool, old, "stale")
	wantOutboxCount(t, dbtest.Outbox(t, pool), 2, "before the replay (welcome + newer notice)")

	if _, err := pool.Exec(ctx,
		`UPDATE subscriptions SET last_event_created = $1 WHERE stripe_subscription_id = 'sub_dbtest_obr'`,
		dbtest.DefaultCreated); err != nil {
		t.Fatal(err)
	}
	replay(t, pool, old)
	processN(t, w, 1)
	wantStatus(t, pool, old, "done")

	rows := dbtest.Outbox(t, pool)
	wantOutboxCount(t, rows, 3, "after the replay applied")
	last := rows[2]
	if last.InboxID != old || last.Generation != 1 || last.Kind != "subscription_notice" ||
		last.Key != "rg:stripe:evt_dbtest_obr_old:subscription_notice" {
		t.Fatalf("replayed event's outbox row: %+v, want inbox %d generation 1 subscription_notice", last, old)
	}
}
