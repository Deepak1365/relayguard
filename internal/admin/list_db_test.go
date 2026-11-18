package admin

// DB tests for the read API (list.go) and the Log snapshot. Skipped unless DB
// tests are enabled; see internal/dbtest (run through `dev.ps1 db-test`).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// With a nil pool any database access would panic, so these prove the input
// is validated first.
func TestListValidationBeforeDB(t *testing.T) {
	ctx := context.Background()
	if _, err := ListEvents(ctx, nil, "bogus", 0, 0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad status: %v", err)
	}
	if _, err := ListEvents(ctx, nil, "", -1, 0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative before: %v", err)
	}
	if _, err := ListEvents(ctx, nil, "", 0, -1); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative limit: %v", err)
	}
	if _, err := CustomerTimeline(ctx, nil, "", 0, 0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty customer: %v", err)
	}
	if _, err := ListUnattributed(ctx, nil, -5, 0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unattributed negative before: %v", err)
	}
	if _, err := ListActions(ctx, nil, 0, -2); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("actions negative limit: %v", err)
	}
}

func eventIDs(p EventPage) []int64 {
	var ids []int64
	for _, e := range p.Events {
		ids = append(ids, e.ID)
	}
	return ids
}

// insertRaw stores an inbox row with an arbitrary payload (poison and
// wrong-shape cases that dbtest.InsertEvent cannot build).
func insertRaw(t *testing.T, pool *pgxpool.Pool, suffix, typ, payload string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO inbox (provider, event_id, event_type, payload)
		VALUES ('stripe', $1, $2, $3::text::jsonb) RETURNING id`,
		dbtest.EventPrefix+suffix, typ, payload).Scan(&id); err != nil {
		t.Fatalf("insert raw: %v", err)
	}
	return id
}

func TestDBListEventsStatusFilters(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	ids := map[string]int64{}
	for _, s := range []string{"pending", "failed", "done", "skipped", "stale", "dead", "undone"} {
		ids[s] = dbtest.InsertEvent(t, pool, "lst_"+s, "invoice.paid", dbtest.InvoiceObject("in_lst_"+s, "cus_lst", 100))
	}
	for _, s := range []string{"done", "skipped", "stale", "dead", "undone"} {
		setStatus(t, pool, ids[s], s, "")
	}
	// failed: still pending, but has failed at least once.
	if _, err := pool.Exec(ctx, `UPDATE inbox SET last_error = 'boom', attempts = 2 WHERE id = $1`, ids["failed"]); err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{"done", "skipped", "stale", "dead", "undone", "failed"} {
		page, err := ListEvents(ctx, pool, s, 0, 0)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if got := eventIDs(page); !slices.Equal(got, []int64{ids[s]}) {
			t.Errorf("filter %s: got ids %v, want [%d]", s, got, ids[s])
		}
	}
	// "pending" is the column value: it includes the retrying event.
	page, err := ListEvents(ctx, pool, "pending", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := eventIDs(page); !slices.Equal(got, []int64{ids["failed"], ids["pending"]}) {
		t.Errorf("filter pending: got %v, want [%d %d]", got, ids["failed"], ids["pending"])
	}
	// No filter: all seven, newest first.
	page, err = ListEvents(ctx, pool, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 7 || page.NextBefore != 0 {
		t.Fatalf("no filter: %d events, next %d; want 7, 0", len(page.Events), page.NextBefore)
	}
	// Summary fields.
	f, _ := ListEvents(ctx, pool, "failed", 0, 0)
	e := f.Events[0]
	if e.LastError != "boom" || e.Attempts != 2 || e.Status != "pending" || e.EventType != "invoice.paid" ||
		e.EventCreated == nil || *e.EventCreated != dbtest.DefaultCreated || e.CustomerID != "cus_lst" {
		t.Errorf("summary: %+v", e)
	}
	if _, err := ListEvents(ctx, pool, "bogus", 0, 0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown status: %v", err)
	}
}

func TestDBListEventsKeysetPaging(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	var all []int64
	for i := 0; i < 25; i++ {
		all = append(all, dbtest.InsertEvent(t, pool, fmt.Sprintf("pg_%02d", i), "invoice.paid",
			dbtest.InvoiceObject(fmt.Sprintf("in_pg_%02d", i), "cus_pg", 100)))
	}
	slices.Reverse(all) // newest first
	// Every third event is dead, to page a filtered list too.
	var dead []int64
	for i, id := range all {
		if i%3 == 0 {
			setStatus(t, pool, id, "dead", "x")
			dead = append(dead, id)
		}
	}

	walk := func(status string, size int, want []int64, midWalk func()) {
		t.Helper()
		var got []int64
		var before int64
		for pages := 0; ; pages++ {
			if pages > 100 {
				t.Fatal("paging did not terminate")
			}
			p, err := ListEvents(ctx, pool, status, before, size)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, eventIDs(p)...)
			if pages == 0 && midWalk != nil {
				midWalk()
			}
			if p.NextBefore == 0 {
				break
			}
			if p.NextBefore != p.Events[len(p.Events)-1].ID {
				t.Fatalf("NextBefore %d is not the last id %d", p.NextBefore, p.Events[len(p.Events)-1].ID)
			}
			before = p.NextBefore
		}
		if !slices.Equal(got, want) {
			t.Fatalf("status %q size %d: paged ids %v, want %v (duplicate or gap)", status, size, got, want)
		}
	}
	walk("", 10, all, nil)
	walk("", 5, all, nil)  // exact multiple of the page size: last page must say "no more"
	walk("", 25, all, nil) // everything in one page, NextBefore 0
	walk("dead", 3, dead, nil)

	// A newer event arriving between pages must not shift older pages.
	walk("", 10, all, func() {
		dbtest.InsertEvent(t, pool, "pg_new", "invoice.paid", dbtest.InvoiceObject("in_pg_new", "cus_pg", 100))
	})
}

func TestDBListLimits(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	for i := 0; i < MaxLimit+5; i++ {
		dbtest.InsertEvent(t, pool, fmt.Sprintf("lim_%03d", i), "invoice.paid",
			dbtest.InvoiceObject(fmt.Sprintf("in_lim_%03d", i), "cus_lim", 100))
	}
	for _, c := range []struct{ limit, want int }{{0, DefaultLimit}, {7, 7}, {MaxLimit, MaxLimit}, {MaxLimit + 1, MaxLimit}, {100000, MaxLimit}} {
		p, err := ListEvents(ctx, pool, "", 0, c.limit)
		if err != nil || len(p.Events) != c.want || p.NextBefore == 0 {
			t.Errorf("limit %d: %d events, next %d, err %v; want %d with more", c.limit, len(p.Events), p.NextBefore, err, c.want)
		}
	}
	for _, f := range []func() error{
		func() error { _, err := ListEvents(ctx, pool, "", 0, -1); return err },
		func() error { _, err := ListEvents(ctx, pool, "", -1, 10); return err },
		func() error { _, err := CustomerTimeline(ctx, pool, "cus_lim", -1, 10); return err },
		func() error { _, err := ListUnattributed(ctx, pool, 0, -1); return err },
		func() error { _, err := ListActions(ctx, pool, -1, 10); return err },
	} {
		if err := f(); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("invalid limit/cursor: err %v, want ErrInvalidInput", err)
		}
	}
}

func TestDBCustomerTimeline(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	const a, b, a10 = "cus_tl_a", "cus_tl_b", "cus_tl_a10" // a10 shares a prefix with a

	// Customer a, in arrival order: invoice (created 300) before the customer
	// event (created 100), then a subscription (created 200): out of order.
	inv := dbtest.InsertEventAt(t, pool, "tl_inv", "invoice.paid", 300, dbtest.InvoiceObject("in_tl", a, 100))
	cus := dbtest.InsertEventAt(t, pool, "tl_cus", "customer.created", 100, dbtest.CustomerObject(a))
	sub := dbtest.InsertEventAt(t, pool, "tl_sub", "customer.subscription.updated", 200,
		dbtest.SubscriptionObject("sub_tl", a, "active"))
	// Negative controls: other customers' events of all three shapes.
	dbtest.InsertEventAt(t, pool, "tl_b_cus", "customer.created", 150, dbtest.CustomerObject(b))
	dbtest.InsertEventAt(t, pool, "tl_b_inv", "invoice.paid", 160, dbtest.InvoiceObject("in_tl_b", b, 100))
	dbtest.InsertEventAt(t, pool, "tl_a10_sub", "customer.subscription.deleted", 170,
		dbtest.SubscriptionObject("sub_tl_a10", a10, "canceled"))
	dbtest.InsertEventAt(t, pool, "tl_a10_cus", "customer.created", 180, dbtest.CustomerObject(a10))

	p, err := CustomerTimeline(ctx, pool, a, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := eventIDs(p), []int64{sub, cus, inv}; !slices.Equal(got, want) {
		t.Fatalf("timeline a: ids %v, want %v (arrival order, other customers excluded)", got, want)
	}
	for i, wantCreated := range []int64{200, 100, 300} {
		e := p.Events[i]
		if e.EventCreated == nil || *e.EventCreated != wantCreated || e.CustomerID != a {
			t.Errorf("entry %d: created %v customer %q; want %d, %s", i, e.EventCreated, e.CustomerID, wantCreated, a)
		}
	}

	// Paging inside a timeline.
	p1, _ := CustomerTimeline(ctx, pool, a, 0, 2)
	p2, _ := CustomerTimeline(ctx, pool, a, p1.NextBefore, 2)
	if got := append(eventIDs(p1), eventIDs(p2)...); !slices.Equal(got, []int64{sub, cus, inv}) || p2.NextBefore != 0 {
		t.Errorf("timeline paging: %v next %d", got, p2.NextBefore)
	}

	if p, err := CustomerTimeline(ctx, pool, "cus_nobody", 0, 0); err != nil || len(p.Events) != 0 {
		t.Errorf("unknown customer: %v, %v", eventIDs(p), err)
	}
}

func TestDBListUnattributed(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	const c = "cus_un"

	good := dbtest.InsertEvent(t, pool, "un_good", "customer.created", dbtest.CustomerObject(c))
	poison := dbtest.InsertEvent(t, pool, "un_poison", "test.poison", map[string]any{"id": "x"})
	scalar := insertRaw(t, pool, "un_scalar", "invoice.paid", `"not an object"`)
	numCustomer := insertRaw(t, pool, "un_num", "invoice.paid",
		`{"id":"e","type":"invoice.paid","created":1,"data":{"object":{"id":"in_1","customer":5}}}`)
	noObject := insertRaw(t, pool, "un_noobj", "customer.created", `{"id":"e","type":"customer.created","data":{}}`)
	dead := dbtest.InsertEvent(t, pool, "un_dead", "test.poison", map[string]any{"id": "y"})
	setStatus(t, pool, dead, "dead", "gave up")

	// One control per rule in customerKeySQL: each looks attributable to a
	// weaker implementation, so each must fail if its rule is removed.
	// Empty strings (NULLIF rule).
	emptyCustomer := insertRaw(t, pool, "un_emptycus", "invoice.paid",
		`{"id":"e","type":"invoice.paid","created":1,"data":{"object":{"id":"in_2","customer":""}}}`)
	emptyID := insertRaw(t, pool, "un_emptyid", "customer.created",
		`{"id":"e","type":"customer.created","created":1,"data":{"object":{"id":""}}}`)
	// Unhandled type (Apply returns ErrUnhandledType) with a customer field.
	unhandled := insertRaw(t, pool, "un_charge", "charge.succeeded",
		`{"id":"e","type":"charge.succeeded","created":1,"data":{"object":{"id":"ch_1","customer":"`+c+`"}}}`)
	// customer.created whose id is a number (Apply cannot decode it).
	numID := insertRaw(t, pool, "un_numid", "customer.created",
		`{"id":"e","type":"customer.created","created":1,"data":{"object":{"id":7}}}`)
	// The event_type column says customer.created, the payload says otherwise:
	// Apply dispatches on the payload's type.
	mismatch := insertRaw(t, pool, "un_mismatch", "customer.created",
		`{"id":"e","type":"test.poison","created":1,"data":{"object":{"id":"cus_un_mm"}}}`)

	p, err := ListUnattributed(ctx, pool, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []int64{dead, noObject, numCustomer, scalar, poison, emptyCustomer, emptyID, unhandled, numID, mismatch}
	slices.SortFunc(wantIDs, func(a, b int64) int { return int(b - a) }) // newest first
	if got := eventIDs(p); !slices.Equal(got, wantIDs) {
		t.Fatalf("unattributed: ids %v, want %v (good event %d must be absent, dead included)", got, wantIDs, good)
	}
	// Candidate customers must have no timeline entries for those events.
	for _, cand := range []string{"7", "cus_un_mm"} {
		if tl, err := CustomerTimeline(ctx, pool, cand, 0, 0); err != nil || len(tl.Events) != 0 {
			t.Errorf("timeline %q: %v, %v; want empty", cand, eventIDs(tl), err)
		}
	}
	for _, e := range p.Events {
		if e.CustomerID != "" {
			t.Errorf("event %d has customer %q", e.ID, e.CustomerID)
		}
	}
	// The scalar payload has no created time; the list must still work.
	for _, e := range p.Events {
		if e.ID == scalar && e.EventCreated != nil {
			t.Errorf("scalar payload: created %v, want nil", *e.EventCreated)
		}
	}
	// None of them leaks into a customer timeline.
	tl, err := CustomerTimeline(ctx, pool, c, 0, 0)
	if err != nil || !slices.Equal(eventIDs(tl), []int64{good}) {
		t.Errorf("timeline: %v, %v; want only [%d]", eventIDs(tl), err, good)
	}
}

func TestDBListActionsPaging(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	if p, err := ListActions(ctx, pool, 0, 0); err != nil || len(p.Actions) != 0 || p.NextBefore != 0 {
		t.Fatalf("empty log: %+v, %v", p, err)
	}
	ev := dbtest.InsertEvent(t, pool, "act_ev", "customer.created", dbtest.CustomerObject("cus_act"))
	var want []int64
	for i := 0; i < 5; i++ {
		outcome := "done"
		if i%2 == 1 {
			outcome = "refused"
		}
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
			VALUES ('replay', $1, $2, 'why', $3) RETURNING id`,
			ev, fmt.Sprintf("actor%d", i), outcome).Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append([]int64{id}, want...) // newest first
	}
	var got []int64
	var before int64
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		p, err := ListActions(ctx, pool, before, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range p.Actions {
			got = append(got, a.ID)
			if a.InboxID != ev || a.Action != "replay" || a.Reason != "why" || a.Detail == "" {
				t.Errorf("action fields: %+v", a)
			}
		}
		if p.NextBefore == 0 {
			break
		}
		before = p.NextBefore
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged action ids %v, want %v", got, want)
	}
}

// Log must show one snapshot. The hook commits a real Replay (status dead ->
// pending plus an admin_actions row) between Log's inbox read and its other
// reads. In one REPEATABLE READ transaction Log shows the event as it was
// (dead, no actions); with separate pool queries it would show dead AND the
// replay action, a state that never existed.
func TestDBLogIsOneSnapshot(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	id := deadInvoice(t, pool, "logsnap")

	hookRan := false
	res, err := logEvent(ctx, pool, id, Options{beforeReread: func() {
		hookRan = true
		if _, err := Replay(ctx, pool, ReplayRequest{InboxID: id, Actor: "alice", Reason: "snapshot test"}, Options{}); err != nil {
			t.Errorf("replay inside the hook: %v", err)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !hookRan {
		t.Fatal("the hook did not run")
	}
	if res.Status != "dead" || len(res.Actions) != 0 {
		t.Fatalf("log across a concurrent replay: status %s with %d actions; want the old snapshot (dead, 0)", res.Status, len(res.Actions))
	}

	// Control: a later Log sees the committed replay, consistently.
	res, err = Log(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "pending" || len(res.Actions) != 1 || res.Actions[0].InboxID != id {
		t.Fatalf("log after the replay: status %s, %d actions; want pending, 1", res.Status, len(res.Actions))
	}
}
