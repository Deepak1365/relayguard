package admin

// DB tests for Undo, success paths. Skipped unless DB tests are enabled; see
// internal/dbtest (run through `dev.ps1 db-test`).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

func undoReq(id int64) UndoRequest {
	return UndoRequest{InboxID: id, Actor: "alice", Reason: "granted by mistake"}
}

// appliedEvent stores an event and applies it; it must end done.
func appliedEvent(t *testing.T, pool *pgxpool.Pool, suffix, typ string, created int64, object map[string]any) int64 {
	t.Helper()
	id := dbtest.InsertEventAt(t, pool, suffix, typ, created, object)
	if got := dbtest.ApplyInbox(t, pool, id); got != "done" {
		t.Fatalf("%s %s: %s, want done", typ, suffix, got)
	}
	return id
}

func appliedInvoice(t *testing.T, pool *pgxpool.Pool, suffix string, amountPaid int64) int64 {
	t.Helper()
	return appliedEvent(t, pool, suffix, "invoice.paid", dbtest.DefaultCreated,
		dbtest.InvoiceObject("in_dbtest_"+suffix, testCustomer, amountPaid))
}

func mustUndo(t *testing.T, pool *pgxpool.Pool, id int64) UndoResult {
	t.Helper()
	res, err := Undo(context.Background(), pool, undoReq(id), Options{})
	if err != nil || res.Outcome != UndoDone || res.ActionID == 0 {
		t.Fatalf("undo inbox %d: %+v, %v; want done", id, res, err)
	}
	return res
}

// rowJSON returns one app row as decoded JSON (UseNumber), or nil if gone.
func rowJSON(t *testing.T, pool *pgxpool.Pool, table string, id int64) map[string]any {
	t.Helper()
	var s *string
	// table is a literal from the tests.
	if err := pool.QueryRow(context.Background(),
		`SELECT (SELECT to_jsonb(t)::text FROM `+table+` t WHERE t.id = $1)`, id).Scan(&s); err != nil {
		t.Fatalf("read %s %d: %v", table, id, err)
	}
	if s == nil {
		return nil
	}
	m, err := decodeImage([]byte(*s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// wantUndoRows checks an undone event's change_log: its apply rows, then one
// undo row per apply row in reverse order, in the SAME generation, tied to
// the undo's audit row, numbered from 1, each undo's before image equal to
// the row's last logged image before it (the log chain is unbroken).
func wantUndoRows(t *testing.T, pool *pgxpool.Pool, id int64, res UndoResult, wantOps ...string) []dbtest.ChangeRow {
	t.Helper()
	cl := dbtest.ChangeLog(t, pool, id)
	n := len(wantOps)
	if len(cl) != 2*n {
		t.Fatalf("change_log %+v, want %d apply + %d undo rows", cl, n, n)
	}
	for i := 0; i < n; i++ {
		a, u := cl[n-1-i], cl[n+i]
		if a.Source != "apply" || u.Source != "undo" || u.AdminActionID == nil || *u.AdminActionID != res.ActionID ||
			u.Generation != a.Generation || u.Generation != res.Generation || u.Seq != i+1 ||
			u.Table != a.Table || u.RowID != a.RowID || u.Op != wantOps[i] {
			t.Fatalf("undo row %d %+v does not revert apply row %+v (want op %s)", i+1, u, a, wantOps[i])
		}
		if prev := lastImageBefore(t, pool, u); !jsonEqual(u.Before, prev) {
			t.Fatalf("undo row %d before image %v, want the row's last logged image %v", i+1, u.Before, prev)
		}
	}
	return cl
}

// lastImageBefore is the after image of the last change_log row on u's row
// written before u (by any event or source).
func lastImageBefore(t *testing.T, pool *pgxpool.Pool, u dbtest.ChangeRow) map[string]any {
	t.Helper()
	var s *string
	if err := pool.QueryRow(context.Background(), `
		SELECT after_image::text FROM change_log
		WHERE table_name = $1 AND row_id = $2 AND id < $3 ORDER BY id DESC LIMIT 1`, u.Table, u.RowID, u.ID).Scan(&s); err != nil {
		t.Fatalf("previous image of %s %d: %v", u.Table, u.RowID, err)
	}
	if s == nil {
		return nil
	}
	m, err := decodeImage([]byte(*s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func wantUndone(t *testing.T, pool *pgxpool.Pool, id int64, res UndoResult) {
	t.Helper()
	if s := readInbox(t, pool, id); s.Status != "undone" {
		t.Fatalf("inbox %d status %s, want undone", id, s.Status)
	}
	acts := actions(t, pool, id)
	if len(acts) != 1 || acts[0].ID != res.ActionID || acts[0].Action != "undo" || acts[0].Outcome != "done" ||
		acts[0].Actor != "alice" || acts[0].Reason != "granted by mistake" {
		t.Fatalf("audit rows %+v, want one done undo", acts)
	}
}

func TestDBUndoCustomerCreated(t *testing.T) {
	pool := dbtest.Pool(t)
	id := appliedCustomer(t, pool)
	cl := dbtest.ChangeLog(t, pool, id)
	customerID := cl[0].RowID

	res := mustUndo(t, pool, id)
	wantUndone(t, pool, id, res)
	wantUndoRows(t, pool, id, res, "delete")
	if rowJSON(t, pool, "customers", customerID) != nil {
		t.Fatal("customer still exists after undoing customer.created")
	}
	// Q4: allowed (nothing references it), with a warning.
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], testCustomer) || !strings.Contains(res.Warnings[0], "dead-letter") {
		t.Fatalf("warnings %q, want the deleted-customer warning", res.Warnings)
	}
}

// invoice.paid: the balance is restored from the before image (by UPDATE, so
// row_version goes on to 3), then the grant is deleted.
func TestDBUndoInvoicePaid(t *testing.T) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	inv := appliedInvoice(t, pool, "un_inv", 2000)
	cl := dbtest.ChangeLog(t, pool, inv)
	grantID, customerID := cl[0].RowID, cl[1].RowID
	original := cl[1].Before

	res := mustUndo(t, pool, inv)
	wantUndone(t, pool, inv, res)
	rows := wantUndoRows(t, pool, inv, res, "update", "delete")
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings %q, want none", res.Warnings)
	}
	if rowJSON(t, pool, "credit_grants", grantID) != nil {
		t.Fatal("grant still exists")
	}
	cus := rowJSON(t, pool, "customers", customerID)
	if cus["row_version"] != json.Number("3") {
		t.Fatalf("customer row_version %v, want 3 (insert 1, invoice 2, undo 3)", cus["row_version"])
	}
	if !jsonEqual(rows[2].After, cus) {
		t.Fatalf("undo after image %v differs from the row %v", rows[2].After, cus)
	}
	delete(cus, "row_version")
	delete(original, "row_version")
	if !jsonEqual(cus, original) {
		t.Fatalf("customer after undo %v, want the before image %v (apart from row_version)", cus, original)
	}
}

// Newest first on one subscription: undo the update (restores status and the
// ordering columns), then the insert (deletes the row).
func TestDBUndoSubscriptionUpdateThenInsert(t *testing.T) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	sub := dbtest.SubscriptionObject("sub_dbtest_un", testCustomer, "active")
	created := appliedEvent(t, pool, "un_sub_c", "customer.subscription.created", dbtest.DefaultCreated, sub)
	sub["status"] = "past_due"
	updated := appliedEvent(t, pool, "un_sub_u", "customer.subscription.updated", dbtest.DefaultCreated+10, sub)
	subID := dbtest.ChangeLog(t, pool, created)[0].RowID
	beforeUpdate := dbtest.ChangeLog(t, pool, updated)[0].Before

	res := mustUndo(t, pool, updated)
	wantUndone(t, pool, updated, res)
	wantUndoRows(t, pool, updated, res, "update")
	row := rowJSON(t, pool, "subscriptions", subID)
	if row["status"] != "active" || row["last_event_created"] != json.Number("1793800000") ||
		row["last_event_id"] != "evt_dbtest_un_sub_c" || row["updated_at"] != beforeUpdate["updated_at"] {
		t.Fatalf("subscription after undoing the update: %v, want the before image %v", row, beforeUpdate)
	}

	res = mustUndo(t, pool, created)
	wantUndone(t, pool, created, res)
	wantUndoRows(t, pool, created, res, "delete")
	if rowJSON(t, pool, "subscriptions", subID) != nil {
		t.Fatal("subscription still exists after undoing its creation")
	}
}

// A later invoice applies to the restored balance, and its before image shows
// the restored row (version 3), so the log chain continues.
func TestDBUndoLaterInvoiceAfterUndo(t *testing.T) {
	pool := dbtest.Pool(t)
	cus := appliedCustomer(t, pool)
	inv1 := appliedInvoice(t, pool, "un_li1", 2000)
	mustUndo(t, pool, inv1)
	inv2 := appliedInvoice(t, pool, "un_li2", 3000)
	upd := dbtest.ChangeLog(t, pool, inv2)[1]
	if upd.Before["credit_balance"] != json.Number("0") || upd.Before["row_version"] != json.Number("3") ||
		upd.After["credit_balance"] != json.Number("30") || upd.After["row_version"] != json.Number("4") {
		t.Fatalf("later invoice update %v -> %v, want balance 0 -> 30, row_version 3 -> 4", upd.Before, upd.After)
	}
	if n := dbtest.Count(t, pool, "credit_grants"); n != 1 {
		t.Fatalf("%d grants, want 1 (the undone one is gone)", n)
	}

	// The undo row -> later apply link on the same customer row must not read
	// as "changed outside": inv2 (applied on the restored row) is undoable,
	// and so is everything below it, down to the customer.
	mustUndo(t, pool, inv2)
	mustUndo(t, pool, cus)
	for _, tb := range []string{"customers", "credit_grants"} {
		if n := dbtest.Count(t, pool, tb); n != 0 {
			t.Fatalf("%s: %d rows after undoing everything, want 0", tb, n)
		}
	}
	for _, id := range []int64{cus, inv1, inv2} {
		if s := readInbox(t, pool, id); s.Status != "undone" {
			t.Fatalf("inbox %d status %s, want undone", id, s.Status)
		}
	}
}

// Subscription variant of the link above: an admin-replayed apply (generation
// 1) lands on a row last written by an undo, and everything is still undoable
// newest first. The stale event is older than the update but newer than the
// create, so after undoing the update it applies on replay.
func TestDBUndoSubscriptionChainAcrossUndoAndReplay(t *testing.T) {
	pool := dbtest.Pool(t)
	appliedCustomer(t, pool)
	sub := dbtest.SubscriptionObject("sub_dbtest_ch", testCustomer, "active")
	created := appliedEvent(t, pool, "ch_c", "customer.subscription.created", dbtest.DefaultCreated, sub)
	sub["status"] = "past_due"
	updated := appliedEvent(t, pool, "ch_u", "customer.subscription.updated", dbtest.DefaultCreated+10, sub)
	late := map[string]any{"id": "sub_dbtest_ch", "object": "subscription", "customer": testCustomer, "status": "canceled"}
	stale := dbtest.InsertEventAt(t, pool, "ch_s", "customer.subscription.updated", dbtest.DefaultCreated+5, late)
	if got := dbtest.ApplyInbox(t, pool, stale); got != "stale" {
		t.Fatalf("older event: %s, want stale", got)
	}

	mustUndo(t, pool, updated)
	if _, err := Replay(context.Background(), pool, ReplayRequest{InboxID: stale, Actor: "alice", Reason: "late event"}, Options{}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := dbtest.ApplyInbox(t, pool, stale); got != "done" {
		t.Fatalf("replayed event: %s, want done", got)
	}

	// Newest first: the replayed apply sits on the undo row, above the create.
	if cs := wantConflict(t, pool, created); !hasConflict(cs, ConflictLaterEvent, "subscriptions", stale, "") {
		t.Fatalf("conflicts %+v, want the replayed event named as later event", cs)
	}
	res := mustUndo(t, pool, stale)
	if res.Generation != 1 {
		t.Fatalf("undo generation %d, want 1", res.Generation)
	}
	mustUndo(t, pool, created)
	if n := dbtest.Count(t, pool, "subscriptions"); n != 0 {
		t.Fatalf("subscriptions: %d rows after undoing everything, want 0", n)
	}
}

// Undo reverts change_log images only. An outbox row is an append-only fact (an
// email cannot be unsent), so undo must leave it exactly as it was, with no
// compensating row.
func TestDBUndoLeavesOutboxUntouched(t *testing.T) {
	pool := dbtest.Pool(t)
	id := appliedCustomer(t, pool)
	before := dbtest.Outbox(t, pool)
	if len(before) != 1 || before[0].InboxID != id || before[0].Kind != "welcome_email" || before[0].Status != "pending" {
		t.Fatalf("outbox before undo: %+v, want the one pending welcome email", before)
	}

	res := mustUndo(t, pool, id)
	wantUndone(t, pool, id, res)
	after := dbtest.Outbox(t, pool)
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("outbox after undo: %+v, want it unchanged: %+v", after, before)
	}
}
