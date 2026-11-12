package admin

// DB tests for Replay. Skipped unless DB tests are enabled; see
// internal/dbtest (run through `dev.ps1 db-test`).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// deadInvoice stores an invoice.paid event and puts it in the state the
// worker leaves an event in after giving up: dead, 8 attempts, a recorded
// error and a processed_at (so the audit detail has real values to match).
func deadInvoice(t *testing.T, pool *pgxpool.Pool, suffix string) int64 {
	t.Helper()
	id := dbtest.InsertEvent(t, pool, suffix, "invoice.paid", dbtest.InvoiceObject("in_dbtest_"+suffix, testCustomer, 2000))
	setStatus(t, pool, id, "dead", "unknown customer "+testCustomer)
	return id
}

func setStatus(t *testing.T, pool *pgxpool.Pool, id int64, status, lastErr string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE inbox SET status = $2, attempts = 8, last_error = $3, processed_at = now(),
			next_attempt_at = now() + interval '1 hour' WHERE id = $1`,
		id, status, lastErr); err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}

// inboxState is the replay-relevant part of an inbox row.
type inboxState struct {
	Status      string
	Attempts    int
	LastError   *string
	ProcessedAt *time.Time
	ReplayCount int
	NextAttempt time.Time
}

func readInbox(t *testing.T, pool *pgxpool.Pool, id int64) inboxState {
	t.Helper()
	var s inboxState
	if err := pool.QueryRow(context.Background(), `
		SELECT status, attempts, last_error, processed_at, replay_count, next_attempt_at FROM inbox WHERE id = $1`, id).
		Scan(&s.Status, &s.Attempts, &s.LastError, &s.ProcessedAt, &s.ReplayCount, &s.NextAttempt); err != nil {
		t.Fatalf("read inbox %d: %v", id, err)
	}
	return s
}

func tableJSON(t *testing.T, pool *pgxpool.Pool, table string) string {
	t.Helper()
	var s string
	// table is a literal from the tests, never input.
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY t.id), '[]')::text FROM `+table+` t`).Scan(&s); err != nil {
		t.Fatalf("snapshot %s: %v", table, err)
	}
	return s
}

// actions returns the admin_actions rows of an event.
func actions(t *testing.T, pool *pgxpool.Pool, id int64) []AdminAction {
	t.Helper()
	res, err := Log(context.Background(), pool, id)
	if err != nil {
		t.Fatalf("log %d: %v", id, err)
	}
	return res.Actions
}

func replayReq(id int64) ReplayRequest {
	return ReplayRequest{InboxID: id, Actor: "alice", Reason: "customer arrived late"}
}

// A dead (or stale) event goes back to a clean pending row in a new
// generation, and the audit row records what it was and what it became.
func TestDBReplayResetsDeadRow(t *testing.T) {
	for _, status := range []string{"dead", "stale"} {
		t.Run(status, func(t *testing.T) {
			pool := dbtest.Pool(t)
			ctx := context.Background()
			id := deadInvoice(t, pool, "rp_"+status)
			setStatus(t, pool, id, status, "recorded problem")
			before := readInbox(t, pool, id)
			if before.ProcessedAt == nil || before.LastError == nil {
				t.Fatalf("setup: %+v", before)
			}

			res, err := Replay(ctx, pool, ReplayRequest{InboxID: id, Actor: "  alice ", Reason: " customer arrived late\n"}, Options{})
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if res.Outcome != ReplayDone || res.Generation != 1 || res.FromStatus != status || res.FromAttempts != 8 ||
				res.FromLastError != "recorded problem" || res.ActionID == 0 {
				t.Fatalf("result %+v", res)
			}

			after := readInbox(t, pool, id)
			if after.Status != "pending" || after.Attempts != 0 || after.LastError != nil || after.ProcessedAt != nil ||
				after.ReplayCount != 1 {
				t.Fatalf("row after replay: %+v, want pending, attempts 0, last_error NULL, processed_at NULL, replay_count 1", after)
			}
			if after.NextAttempt.After(time.Now()) {
				t.Fatalf("next_attempt_at %s is in the future: the event must be claimable at once", after.NextAttempt)
			}

			acts := actions(t, pool, id)
			if len(acts) != 1 {
				t.Fatalf("admin_actions: %+v, want 1 row", acts)
			}
			a := acts[0]
			if a.ID != res.ActionID || a.Action != "replay" || a.Outcome != "done" ||
				a.Actor != "alice" || a.Reason != "customer arrived late" { // trimmed
				t.Fatalf("audit row %+v", a)
			}
			var d struct {
				FromStatus      string    `json:"from_status"`
				FromAttempts    int       `json:"from_attempts"`
				FromLastError   string    `json:"from_last_error"`
				FromProcessedAt time.Time `json:"from_processed_at"`
				Generation      int       `json:"generation"`
			}
			if err := json.Unmarshal([]byte(a.Detail), &d); err != nil {
				t.Fatalf("detail %s: %v", a.Detail, err)
			}
			if d.FromStatus != status || d.FromAttempts != 8 || d.FromLastError != "recorded problem" ||
				!d.FromProcessedAt.Equal(*before.ProcessedAt) || d.Generation != after.ReplayCount {
				t.Fatalf("detail %s does not match the row before (%+v) and the new replay_count %d", a.Detail, before, after.ReplayCount)
			}
		})
	}
}

// Only dead and stale are replayable. Each refusal is audited, commits, and
// changes nothing else.
func TestDBReplayRefuses(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)

	pending := dbtest.InsertEvent(t, pool, "rf_pending", "invoice.paid", dbtest.InvoiceObject("in_dbtest_rf1", testCustomer, 2000))
	done := dbtest.InsertEvent(t, pool, "rf_done", "invoice.paid", dbtest.InvoiceObject("in_dbtest_rf2", testCustomer, 2000))
	if got := dbtest.ApplyInbox(t, pool, done); got != "done" {
		t.Fatalf("setup done: %s", got)
	}
	skipped := dbtest.InsertEvent(t, pool, "rf_skipped", "charge.refunded", map[string]any{"id": "ch_dbtest_rf"})
	if got := dbtest.ApplyInbox(t, pool, skipped); got != "skipped" {
		t.Fatalf("setup skipped: %s", got)
	}

	for _, c := range []struct {
		name   string
		id     int64
		status string
	}{{"pending", pending, "pending"}, {"done", done, "done"}, {"skipped", skipped, "skipped"}} {
		t.Run(c.name, func(t *testing.T) {
			tables := []string{"inbox", "customers", "credit_grants", "subscriptions", "change_log"}
			before := map[string]string{}
			for _, tb := range tables {
				before[tb] = tableJSON(t, pool, tb)
			}

			res, err := Replay(ctx, pool, replayReq(c.id), Options{})
			if !errors.Is(err, ErrNotReplayable) {
				t.Fatalf("replay of a %s event: %+v, %v; want ErrNotReplayable", c.name, res, err)
			}
			if res.Outcome != ReplayRefused || !strings.Contains(res.Reason, "status "+c.status) || res.ActionID == 0 {
				t.Fatalf("result %+v", res)
			}
			for _, tb := range tables {
				if after := tableJSON(t, pool, tb); after != before[tb] {
					t.Fatalf("refused replay changed %s:\nbefore %s\nafter  %s", tb, before[tb], after)
				}
			}
			acts := actions(t, pool, c.id)
			if len(acts) != 1 || acts[0].ID != res.ActionID || acts[0].Action != "replay" || acts[0].Outcome != "refused" ||
				acts[0].Actor != "alice" || !strings.Contains(acts[0].Detail, "not replayable") {
				t.Fatalf("audit rows %+v, want exactly one refused replay", acts)
			}
		})
	}
}

// Undone guard (user decision Q5; the undo command itself arrives in step 6,
// so the undo audit rows here are synthetic). An event with a DONE undo is
// refused even though its status says dead; a refused undo changed nothing
// and does not block.
func TestDBReplayRefusesUndone(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	undone := deadInvoice(t, pool, "un_done")
	refusedUndo := deadInvoice(t, pool, "un_refused")
	for id, outcome := range map[int64]string{undone: "done", refusedUndo: "refused"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
			VALUES ('undo', $1, 'dbtest', 'synthetic undo', $2)`, id, outcome); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Replay(ctx, pool, replayReq(undone), Options{})
	if !errors.Is(err, ErrNotReplayable) || !strings.Contains(res.Reason, "undone") {
		t.Fatalf("replay of an undone event: %+v, %v; want ErrNotReplayable (undone)", res, err)
	}
	if s := readInbox(t, pool, undone); s.Status != "dead" || s.ReplayCount != 0 || s.Attempts != 8 {
		t.Fatalf("undone event changed: %+v", s)
	}
	acts := actions(t, pool, undone)
	if len(acts) != 2 || acts[1].Action != "replay" || acts[1].Outcome != "refused" {
		t.Fatalf("audit rows %+v, want the synthetic undo then a refused replay", acts)
	}

	// Control: with only a refused undo the same event is replayable, so the
	// refusal above came from the 'done' undo, not from any undo row.
	if res, err := Replay(ctx, pool, replayReq(refusedUndo), Options{}); err != nil || res.Outcome != ReplayDone {
		t.Fatalf("replay with only a refused undo: %+v, %v; want done", res, err)
	}
}

// If the refusal's audit insert fails, nothing is audited, so Replay must not
// report a "refused" outcome (the CLI would print "the refusal is audited").
// A trigger that rejects inserts for one actor forces the failure exactly at
// the insert, after the refusal decision.
func TestDBReplayRefusalAuditFailureReportsNoOutcome(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	// Register cleanup and drop leftovers first, so a failed setup or a killed
	// earlier run cannot leave the trigger behind and break later runs.
	dropTrigger := func() {
		pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS dbtest_fail_audit ON admin_actions`)
		pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS dbtest_fail_audit()`)
	}
	dropTrigger()
	t.Cleanup(dropTrigger)
	for _, stmt := range []string{
		`CREATE FUNCTION dbtest_fail_audit() RETURNS trigger LANGUAGE plpgsql AS
		 $$ BEGIN RAISE EXCEPTION 'dbtest: audit insert refused'; END $$`,
		`CREATE TRIGGER dbtest_fail_audit BEFORE INSERT ON admin_actions
		 FOR EACH ROW WHEN (NEW.actor = 'fail_audit') EXECUTE FUNCTION dbtest_fail_audit()`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup trigger: %v", err)
		}
	}
	id := deadInvoice(t, pool, "audit_fail")
	setStatus(t, pool, id, "done", "") // not replayable: takes the refusal path
	res, err := Replay(ctx, pool, ReplayRequest{InboxID: id, Actor: "fail_audit", Reason: "force failure"}, Options{})
	if err == nil || errors.Is(err, ErrNotReplayable) {
		t.Fatalf("err = %v, want a non-ErrNotReplayable audit failure", err)
	}
	if res.Outcome != "" || res.ActionID != 0 {
		t.Fatalf("result %+v: no outcome may be reported when the audit insert failed", res)
	}
	if n := len(actions(t, pool, id)); n != 0 {
		t.Fatalf("%d admin_actions rows, want 0", n)
	}
}

// Bad input is rejected before the database is touched: nothing is written
// and nothing is audited.
func TestDBReplayValidatesInput(t *testing.T) {
	pool := dbtest.Pool(t)
	id := deadInvoice(t, pool, "val")
	before := snapshot(t, pool)
	for _, r := range []ReplayRequest{
		{InboxID: id, Actor: "", Reason: "why"},
		{InboxID: id, Actor: "alice", Reason: "   "},
		{InboxID: id, Actor: strings.Repeat("a", 201), Reason: "why"},
		{InboxID: id, Actor: "alice", Reason: strings.Repeat("r", 1001)},
		{InboxID: 0, Actor: "alice", Reason: "why"},
	} {
		if _, err := Replay(context.Background(), pool, r, Options{}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%.30q: err %v, want ErrInvalidInput", r.Actor+"/"+r.Reason, err)
		}
	}
	if after := snapshot(t, pool); after != before {
		t.Fatalf("invalid requests changed the database.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// An unknown id has no row to attach an audit entry to: ErrNotFound, nothing
// written.
func TestDBReplayNotFound(t *testing.T) {
	pool := dbtest.Pool(t)
	if _, err := Replay(context.Background(), pool, replayReq(999999), Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
	if n := dbtest.Count(t, pool, "admin_actions"); n != 0 {
		t.Fatalf("admin_actions: %d rows, want 0", n)
	}
}

// Another transaction holds the row (a worker fence). Replay gives up after
// lock_timeout with ErrBusy, writes nothing, and works once the lock is gone.
func TestDBReplayBusy(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	id := deadInvoice(t, pool, "busy")

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) // before the cleanup Truncate
	if _, err := other.Exec(ctx, `SELECT 1 FROM inbox WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, pool)

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = Replay(rctx, pool, replayReq(id), Options{LockTimeout: 200 * time.Millisecond, StatementTimeout: 3 * time.Second})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("replay against a locked row: err %v after %s, want ErrBusy", err, elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ErrBusy after %s, want about the 200ms lock_timeout", elapsed)
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, pool); after != before {
		t.Fatalf("a busy replay wrote something.\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if res, err := Replay(ctx, pool, replayReq(id), Options{}); err != nil || res.Outcome != ReplayDone {
		t.Fatalf("replay after unlock: %+v, %v", res, err)
	}
}

// dead -> replay -> dead again -> replay: replay_count 2, and one audit row
// per replay whose generation and from_* values describe that replay.
func TestDBReplayTwice(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	id := deadInvoice(t, pool, "twice")

	if res, err := Replay(ctx, pool, replayReq(id), Options{}); err != nil || res.Generation != 1 {
		t.Fatalf("first replay: %+v, %v", res, err)
	}
	setStatus(t, pool, id, "dead", "failed again")
	if res, err := Replay(ctx, pool, replayReq(id), Options{}); err != nil || res.Generation != 2 || res.FromLastError != "failed again" {
		t.Fatalf("second replay: %+v, %v", res, err)
	}
	if s := readInbox(t, pool, id); s.ReplayCount != 2 || s.Status != "pending" {
		t.Fatalf("row: %+v, want replay_count 2, pending", s)
	}
	acts := actions(t, pool, id)
	if len(acts) != 2 {
		t.Fatalf("audit rows %+v, want 2", acts)
	}
	for i, wantGen := range []int{1, 2} {
		var d map[string]any
		if err := json.Unmarshal([]byte(acts[i].Detail), &d); err != nil {
			t.Fatal(err)
		}
		if acts[i].Outcome != "done" || d["generation"] != float64(wantGen) {
			t.Fatalf("audit row %d: %+v, want done in generation %d", i, acts[i], wantGen)
		}
	}
	if d := acts[1].Detail; !strings.Contains(d, `"from_last_error": "failed again"`) {
		t.Fatalf("second audit detail %s does not record the second failure", d)
	}
}

// Two replays of the same event at once: the row lock makes the second one
// wait, then see a pending event and refuse. This proves the lock is
// exclusive between replays (FOR KEY SHARE would let both read "dead" and
// bump replay_count twice). Deterministic: the first replay is held inside
// its transaction by the afterLock test hook until the second is blocked.
func TestDBReplayConcurrentSecondIsRefused(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	id := deadInvoice(t, pool, "conc")

	locked, release := make(chan struct{}), make(chan struct{})
	type outcome struct {
		res ReplayResult
		err error
	}
	first, second := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		res, err := Replay(ctx, pool, replayReq(id), Options{afterLock: func() { close(locked); <-release }})
		first <- outcome{res, err}
	}()
	<-locked
	go func() {
		res, err := Replay(ctx, pool, ReplayRequest{InboxID: id, Actor: "bob", Reason: "me too"}, Options{})
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
		t.Error("the second replay never waited for the first one's row lock")
	}
	if o1.err != nil || o1.res.Generation != 1 {
		t.Fatalf("first replay: %+v, %v", o1.res, o1.err)
	}
	if !errors.Is(o2.err, ErrNotReplayable) || !strings.Contains(o2.res.Reason, "status pending") {
		t.Fatalf("second replay: %+v, %v; want ErrNotReplayable (the event is pending now)", o2.res, o2.err)
	}
	if s := readInbox(t, pool, id); s.ReplayCount != 1 {
		t.Fatalf("replay_count %d, want 1", s.ReplayCount)
	}
	acts := actions(t, pool, id)
	if len(acts) != 2 || acts[0].Outcome != "done" || acts[0].Actor != "alice" || acts[1].Outcome != "refused" || acts[1].Actor != "bob" {
		t.Fatalf("audit rows %+v, want alice done then bob refused", acts)
	}
}

// Preview of a replayed event runs in the new generation: its changes are
// read back from that generation.
func TestDBPreviewAfterReplayUsesNewGeneration(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	id := deadInvoice(t, pool, "pv_gen")
	if _, err := Replay(ctx, pool, replayReq(id), Options{}); err != nil {
		t.Fatal(err)
	}
	res, err := Preview(ctx, pool, id, Options{})
	if err != nil || res.Status != "pending" || res.Generation != 1 || res.Outcome != OutcomeApply || len(res.Changes) != 2 {
		t.Fatalf("preview after replay: %+v, %v; want pending, generation 1, apply with 2 changes", res, err)
	}
}

// The real Replay, committed while a preview runs, makes the preview report
// ErrChanged (status and replay_count both moved). Replay's UPDATE does not
// wait for the preview: its KEY SHARE does not conflict with NO KEY UPDATE.
func TestDBPreviewDetectsRealReplay(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	id := deadInvoice(t, pool, "pv_rp")

	var replayErr error
	opts := Options{beforeReread: func() {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, replayErr = Replay(hctx, pool, replayReq(id), Options{})
	}}
	res, err := Preview(ctx, pool, id, opts)
	if replayErr != nil {
		t.Fatalf("replay during the preview: %v", replayErr)
	}
	if !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), "generation 0 -> 1") {
		t.Fatalf("preview across a real replay: %+v, %v; want ErrChanged naming generation 0 -> 1", res, err)
	}
}
