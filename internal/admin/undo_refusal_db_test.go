package admin

// DB tests for Undo refusals that are not conflicts, input handling, and the
// undone state seen by replay and preview.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// Only done events can be undone. Each refusal is audited, commits, and
// changes nothing else. "undone" is reached through a real undo.
func TestDBUndoRefusesStatus(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	appliedCustomer(t, pool)
	pending := dbtest.InsertEvent(t, pool, "us_pending", "invoice.paid", dbtest.InvoiceObject("in_dbtest_us1", testCustomer, 2000))
	dead := deadInvoice(t, pool, "us_dead")
	skipped := dbtest.InsertEvent(t, pool, "us_skipped", "charge.refunded", map[string]any{"id": "ch_dbtest_us"})
	if got := dbtest.ApplyInbox(t, pool, skipped); got != "skipped" {
		t.Fatalf("setup skipped: %s", got)
	}
	sub := dbtest.SubscriptionObject("sub_dbtest_us", testCustomer, "active")
	appliedEvent(t, pool, "us_new", "customer.subscription.updated", dbtest.DefaultCreated+10, sub)
	stale := dbtest.InsertEvent(t, pool, "us_stale", "customer.subscription.updated", sub)
	if got := dbtest.ApplyInbox(t, pool, stale); got != "stale" {
		t.Fatalf("setup stale: %s", got)
	}
	undone := appliedInvoice(t, pool, "us_undone", 1000)
	mustUndo(t, pool, undone)

	for _, c := range []struct {
		name   string
		id     int64
		reason string
	}{
		{"pending", pending, "status pending"}, {"dead", dead, "status dead"}, {"skipped", skipped, "status skipped"},
		{"stale", stale, "status stale"}, {"undone", undone, "already undone"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := tablesSnapshot(t, pool)
			nActs := len(actions(t, pool, c.id))
			res, err := Undo(ctx, pool, undoReq(c.id), Options{})
			if !errors.Is(err, ErrNotUndoable) || res.Outcome != UndoRefused || res.ActionID == 0 ||
				!strings.Contains(res.Reason, c.reason) {
				t.Fatalf("undo of a %s event: %+v, %v; want ErrNotUndoable (%s)", c.name, res, err, c.reason)
			}
			for tb, s := range tablesSnapshot(t, pool) {
				if s != before[tb] {
					t.Fatalf("refused undo changed %s", tb)
				}
			}
			acts := actions(t, pool, c.id)
			if len(acts) != nActs+1 || acts[nActs].ID != res.ActionID || acts[nActs].Action != "undo" ||
				acts[nActs].Outcome != "refused" {
				t.Fatalf("audit rows %+v, want one more refused undo", acts)
			}
		})
	}
}

// A done event without apply rows in its current generation (e.g. applied
// before change capture existed) has nothing recorded to revert.
func TestDBUndoNoChangeLogRows(t *testing.T) {
	pool := dbtest.Pool(t)
	id := dbtest.InsertEvent(t, pool, "un_nolog", "customer.created", dbtest.CustomerObject("cus_dbtest_nolog"))
	exec(t, pool, `UPDATE inbox SET status = 'done', attempts = 1, processed_at = now() WHERE id = $1`, id)
	res, err := Undo(context.Background(), pool, undoReq(id), Options{})
	if !errors.Is(err, ErrNotUndoable) || !strings.Contains(res.Reason, "no change_log apply rows") {
		t.Fatalf("undo without change_log rows: %+v, %v; want ErrNotUndoable", res, err)
	}
	if s := readInbox(t, pool, id); s.Status != "done" {
		t.Fatalf("status %s, want done", s.Status)
	}
}

func TestDBUndoValidatesInput(t *testing.T) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	id := appliedInvoice(t, pool, "un_val", 2000)
	before := snapshot(t, pool)
	for _, r := range []UndoRequest{
		{InboxID: id, Actor: "", Reason: "why"},
		{InboxID: id, Actor: "alice", Reason: " "},
		{InboxID: id, Actor: strings.Repeat("a", 201), Reason: "why"},
		{InboxID: id, Actor: "alice", Reason: strings.Repeat("r", 1001)},
		{InboxID: 0, Actor: "alice", Reason: "why"},
	} {
		if _, err := Undo(context.Background(), pool, r, Options{}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("err %v, want ErrInvalidInput", err)
		}
	}
	if after := snapshot(t, pool); after != before {
		t.Fatalf("invalid requests changed the database.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDBUndoNotFound(t *testing.T) {
	pool := dbtest.Pool(t)
	if _, err := Undo(context.Background(), pool, undoReq(999999), Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
	if n := dbtest.Count(t, pool, "admin_actions"); n != 0 {
		t.Fatalf("admin_actions: %d rows, want 0", n)
	}
}

// If the refusal's audit insert fails, no outcome may be reported (the CLI
// would print "the refusal is audited"). Same trigger technique as the
// replay test.
func TestDBUndoRefusalAuditFailure(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	dropTrigger := func() {
		pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS dbtest_fail_audit ON admin_actions`)
		pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS dbtest_fail_audit()`)
	}
	dropTrigger()
	t.Cleanup(dropTrigger)
	exec(t, pool, `CREATE FUNCTION dbtest_fail_audit() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'dbtest: audit insert refused'; END $$`)
	exec(t, pool, `CREATE TRIGGER dbtest_fail_audit BEFORE INSERT ON admin_actions
		FOR EACH ROW WHEN (NEW.actor = 'fail_audit') EXECUTE FUNCTION dbtest_fail_audit()`)
	id := deadInvoice(t, pool, "un_audit_fail") // dead: takes the refusal path
	res, err := Undo(ctx, pool, UndoRequest{InboxID: id, Actor: "fail_audit", Reason: "force failure"}, Options{})
	if err == nil || errors.Is(err, ErrNotUndoable) || errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want an audit failure that is not a refusal", err)
	}
	if res.Outcome != "" || res.ActionID != 0 {
		t.Fatalf("result %+v: no outcome may be reported when the audit insert failed", res)
	}
	if n := len(actions(t, pool, id)); n != 0 {
		t.Fatalf("%d admin_actions rows, want 0", n)
	}
}

// The 5b undone guard against a REAL undo (TestDBReplayRefusesUndone used
// synthetic audit rows): status 'undone' and the done undo row both refuse.
func TestDBReplayRefusesRealUndo(t *testing.T) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	inv := appliedInvoice(t, pool, "rru", 2000)
	u := mustUndo(t, pool, inv)
	res, err := Replay(context.Background(), pool, replayReq(inv), Options{})
	if !errors.Is(err, ErrNotReplayable) || !strings.Contains(res.Reason, "undone") {
		t.Fatalf("replay of an undone event: %+v, %v; want ErrNotReplayable (undone)", res, err)
	}
	if s := readInbox(t, pool, inv); s.Status != "undone" || s.ReplayCount != 0 {
		t.Fatalf("row %+v, want undone, replay_count 0", s)
	}
	if acts := actions(t, pool, inv); len(acts) != 2 || acts[0].ID != u.ActionID || acts[1].Outcome != "refused" {
		t.Fatalf("audit rows %+v, want the undo then a refused replay", acts)
	}
}

func TestDBPreviewRefusesUndone(t *testing.T) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	inv := appliedInvoice(t, pool, "pru", 2000)
	mustUndo(t, pool, inv)
	before := snapshot(t, pool)
	if res, err := Preview(context.Background(), pool, inv, Options{}); !errors.Is(err, ErrUndone) {
		t.Fatalf("preview of an undone event: %+v, %v; want ErrUndone", res, err)
	}
	if after := snapshot(t, pool); after != before {
		t.Fatal("preview of an undone event changed the database")
	}
}
