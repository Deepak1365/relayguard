package sim

// DB tests for UndoAll (phase 6 step 7a, experiment E4): two positive runs
// with the real admin.Undo/Replay, and one negative control per assertion
// family, each asserting the exact set of violation kinds it causes. Every
// control's extra kinds (e.g. the change_log check also seeing the damage)
// are part of the expected set, so a kind that silently disappears or
// appears fails the test.

import (
	"context"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

func runUndoAll(t *testing.T, pool *pgxpool.Pool, tag, mode string, seed int64, hooks undoAllHooks) (UndoReport, []Violation) {
	t.Helper()
	rep, v, err := undoAll(context.Background(), pool, testScope(tag), mode, rand.New(rand.NewSource(seed)), hooks)
	if err != nil {
		t.Fatalf("undoAll: %v", err)
	}
	return rep, v
}

// wantKinds asserts the DISTINCT kinds of vs are exactly kinds.
func wantKinds(t *testing.T, vs []Violation, kinds ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, v := range vs {
		seen[v.Kind] = true
	}
	got := sortedKeys(seen)
	sort.Strings(kinds)
	if !reflect.DeepEqual(got, kinds) {
		t.Fatalf("kinds %v, want %v\nviolations: %+v", got, kinds, vs)
	}
}

// subjectsOf lists the subjects of kind's violations, sorted.
func subjectsOf(vs []Violation, kind string) []string {
	var out []string
	for _, v := range vs {
		if v.Kind == kind {
			out = append(out, v.Subject)
		}
	}
	sort.Strings(out)
	return out
}

func auditCount(t *testing.T, pool *pgxpool.Pool, action, outcome string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM admin_actions WHERE actor = $1 AND action = $2 AND outcome = $3`,
		undoAllActor, action, outcome).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func applied(t *testing.T, pool *pgxpool.Pool, suffix, typ string, created int64, object map[string]any) int64 {
	t.Helper()
	id := dbtest.InsertEventAt(t, pool, suffix, typ, created, object)
	if got := dbtest.ApplyInbox(t, pool, id); got != "done" {
		t.Fatalf("%s: %s, want done", suffix, got)
	}
	return id
}

// Newest first over a history that already has undone events (undoneHistory:
// sub_u and the first invoice), two more invoices applied in the REVERSE of
// their created order, a stale and a skipped event, and a dead one. Every
// undo must succeed first time, the tables end empty, every probe is
// refused, and the order used is apply order (inv_y, applied last, is undone
// before inv_x although inv_x was created later).
func TestDBUndoAllNewestClean(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	u := undoneHistory(t, pool)
	T := dbtest.DefaultCreated
	invX := applied(t, pool, "a_inv_x", "invoice.paid", T+10, dbtest.InvoiceObject("in_dbtest_a_x", u.customer, 1000))
	invY := applied(t, pool, "a_inv_y", "invoice.paid", T+5, dbtest.InvoiceObject("in_dbtest_a_y", u.customer, 500))
	dead := dbtest.InsertEvent(t, pool, "a_inv_dead", "invoice.paid", dbtest.InvoiceObject("in_dbtest_a_dead", "cus_dbtest_a_9", 700))
	mustExec(t, pool, `UPDATE inbox SET status = 'dead', attempts = 8, last_error = 'synthetic', processed_at = now() WHERE id = $1`, dead)
	wantViolations(t, check(t, pool, "a"))

	rep, v := runUndoAll(t, pool, "a", UndoAllNewest, 1, undoAllHooks{})
	wantViolations(t, v)
	// Candidates: cus, sub_c, inv_x, inv_y. Probes: 6 undone x (undo + replay)
	// + stale, skipped, dead x undo = 15.
	if rep.Candidates != 4 || rep.Undone != 4 || rep.Refusals != 0 || rep.Passes != 1 || rep.Probes != 15 {
		t.Fatalf("report %+v; want candidates 4, undone 4, refusals 0, passes 1, probes 15", rep)
	}
	for _, table := range []string{"customers", "credit_grants", "subscriptions"} {
		if n := dbtest.Count(t, pool, table); n != 0 {
			t.Fatalf("%s: %d rows left", table, n)
		}
	}
	if got := []int{auditCount(t, pool, "undo", "done"), auditCount(t, pool, "undo", "refused"),
		auditCount(t, pool, "replay", "refused"), auditCount(t, pool, "replay", "done")}; !reflect.DeepEqual(got, []int{4, 9, 6, 0}) {
		t.Fatalf("harness audit rows undo done/refused, replay refused/done = %v, want [4 9 6 0]", got)
	}
	var yFirst bool
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT id FROM admin_actions WHERE inbox_id = $1 AND action = 'undo' AND outcome = 'done')
		     < (SELECT id FROM admin_actions WHERE inbox_id = $2 AND action = 'undo' AND outcome = 'done')`,
		invY, invX).Scan(&yFirst); err != nil {
		t.Fatal(err)
	}
	if !yFirst {
		t.Fatal("inv_x was undone before inv_y: order was not apply order")
	}
}

// Random order with a seed whose shuffle tries the customer first: it must
// be refused (still referenced, later changes not undone), retried in a
// later pass and finally undone, with only the allowed conflict kinds, every
// refusal counted and audited, and no violation.
func TestDBUndoAllRandomRetries(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	history(t, pool, "a")

	pre, err := readUndoPhase(ctx, pool, testScope("a"))
	if err != nil {
		t.Fatal(err)
	}
	seed := int64(0)
	for s := int64(1); s <= 200 && seed == 0; s++ {
		cs := append([]undoCandidate(nil), pre.done...)
		shuffleRandom(cs, rand.New(rand.NewSource(s)))
		if cs[0].EventID == dbtest.EventPrefix+"a_cus" {
			seed = s
		}
	}
	if seed == 0 {
		t.Fatal("no seed in 1..200 shuffles the customer first")
	}

	rep, v := runUndoAll(t, pool, "a", UndoAllRandom, seed, undoAllHooks{})
	wantViolations(t, v)
	if rep.Undone != 4 || rep.Refusals == 0 || rep.Passes < 2 || rep.RefusalKinds[admin.ConflictReferenced] == 0 {
		t.Fatalf("seed %d report %+v; want undone 4, refusals > 0, passes >= 2, referenced > 0", seed, rep)
	}
	for k := range rep.RefusalKinds {
		if !allowedRandomConflicts[k] {
			t.Fatalf("refusal kind %s in a clean history; kinds %v", k, rep.RefusalKinds)
		}
	}
	// Independently of the loop's own counter: the audited loop refusals are
	// the refused undo rows that are not probes.
	var loopRefused int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM admin_actions
		WHERE actor = $1 AND action = 'undo' AND outcome = 'refused' AND reason <> $2`,
		undoAllActor, undoAllProbeReason).Scan(&loopRefused); err != nil {
		t.Fatal(err)
	}
	if loopRefused != rep.Refusals {
		t.Fatalf("audited loop refusals %d, report says %d", loopRefused, rep.Refusals)
	}
}

// Control: an unlogged write to the customer between two invoices. The
// newer invoice still undoes; the older one and the customer meet
// chain_break (not a kind order can fix), so random mode gets stuck. The change_log check after the phase sees the break too.
func TestDBUndoAllControlChainBreak(t *testing.T) {
	pool := dbtest.Pool(t)
	cus := "cus_dbtest_a_1"
	T := dbtest.DefaultCreated
	applied(t, pool, "a_cus", "customer.created", T, dbtest.CustomerObject(cus))
	applied(t, pool, "a_inv1", "invoice.paid", T+1, dbtest.InvoiceObject("in_dbtest_a_1", cus, 1000))
	mustExec(t, pool, `UPDATE customers SET email = 'outside@example.test' WHERE stripe_customer_id = $1`, cus)
	applied(t, pool, "a_inv2", "invoice.paid", T+2, dbtest.InvoiceObject("in_dbtest_a_2", cus, 2000))

	rep, v := runUndoAll(t, pool, "a", UndoAllRandom, 1, undoAllHooks{})
	wantKinds(t, v, KindUndoUnexpectedConflict, KindUndoStuck, KindUndoRowsLeft, KindUndoInboxCounts,
		"undo_"+KindChangeLogChainBreak)
	// Both events whose undo reads the customer row's chain across the
	// unlogged write see it: inv1 and the customer itself.
	want := []string{dbtest.EventPrefix + "a_cus", dbtest.EventPrefix + "a_inv1"}
	if got := subjectsOf(v, KindUndoUnexpectedConflict); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected conflict subjects %v, want %v\nviolations: %+v", got, want, v)
	}
	for _, x := range v {
		if x.Kind == KindUndoUnexpectedConflict && !strings.Contains(x.Detail, admin.ConflictChainBreak) {
			t.Fatalf("detail %q does not name chain_break", x.Detail)
		}
		if x.Kind == KindUndoStuck && (!strings.Contains(x.Detail, "a_cus") || !strings.Contains(x.Detail, "a_inv1")) {
			t.Fatalf("stuck detail %q does not list a_cus and a_inv1", x.Detail)
		}
	}
	if rep.Undone != 1 {
		t.Fatalf("undone %d, want 1 (inv2)", rep.Undone)
	}
}

// Control: a customer row outside the seed's scope. CheckState skips it as
// foreign and the change_log check does not see it, so only the whole-table
// count can report it.
func TestDBUndoAllControlForeignRow(t *testing.T) {
	pool := dbtest.Pool(t)
	history(t, pool, "a")
	mustExec(t, pool, `INSERT INTO customers (stripe_customer_id, email) VALUES ('cus_foreign_1', 'f@example.test')`)

	_, v := runUndoAll(t, pool, "a", UndoAllNewest, 1, undoAllHooks{})
	wantKinds(t, v, KindUndoRowsLeft)
	if got := subjectsOf(v, KindUndoRowsLeft); !reflect.DeepEqual(got, []string{"customers"}) {
		t.Fatalf("rows-left subjects %v, want [customers] (the count, not CheckState)", got)
	}
}

// Control: newest mode in the wrong order (oldest first, via the order
// hook). The customer and the first subscription event are refused; the
// invoice and the newest subscription update still undo.
func TestDBUndoAllControlNewestWrongOrder(t *testing.T) {
	pool := dbtest.Pool(t)
	history(t, pool, "a")
	reverse := func(cs []undoCandidate) {
		for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
			cs[i], cs[j] = cs[j], cs[i]
		}
	}
	rep, v := runUndoAll(t, pool, "a", UndoAllNewest, 1, undoAllHooks{order: reverse})
	wantKinds(t, v, KindUndoRefusedNewestFirst, KindUndoRowsLeft, KindUndoInboxCounts)
	want := []string{dbtest.EventPrefix + "a_cus", dbtest.EventPrefix + "a_sub_c"}
	if got := subjectsOf(v, KindUndoRefusedNewestFirst); !reflect.DeepEqual(got, want) {
		t.Fatalf("refused subjects %v, want %v", got, want)
	}
	if rep.Refusals != 2 || rep.Undone != 2 {
		t.Fatalf("refusals %d undone %d, want 2 and 2", rep.Refusals, rep.Undone)
	}
}

// Control: an undone event's status is forced back to done after the loop.
// The probes still get refusals (the done undo audit row guards it), but
// from the wrong status, which they must report; the inbox counts and the
// change_log check (undo rows on a done event) see it too.
func TestDBUndoAllControlForcedDone(t *testing.T) {
	pool := dbtest.Pool(t)
	history(t, pool, "a")
	subU := inboxIDOf(t, pool, "a_sub_u")
	force := func() { mustExec(t, pool, `UPDATE inbox SET status = 'done' WHERE id = $1`, subU) }

	_, v := runUndoAll(t, pool, "a", UndoAllNewest, 1, undoAllHooks{afterLoop: force})
	wantKinds(t, v, KindUndoProbe, KindUndoInboxCounts, "undo_"+KindChangeLogUnexpected)
	want := []string{dbtest.EventPrefix + "a_sub_u", dbtest.EventPrefix + "a_sub_u"} // undo and replay probe
	if got := subjectsOf(v, KindUndoProbe); !reflect.DeepEqual(got, want) {
		t.Fatalf("probe subjects %v, want %v", got, want)
	}
}

// Control (audit, aggregate): one extra refused undo audit row for an
// in-scope event, written after the loop as if by someone else. Nothing
// else differs, so the only kind is undo_audit, on admin_actions (the
// per-event check counts done undo rows only).
func TestDBUndoAllControlAuditExtraRefusal(t *testing.T) {
	pool := dbtest.Pool(t)
	f := history(t, pool, "a")
	extra := func() {
		mustExec(t, pool, `INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
			VALUES ('undo', $1, 'outsider', 'control', 'refused')`, f.invoice)
	}
	_, v := runUndoAll(t, pool, "a", UndoAllNewest, 1, undoAllHooks{afterLoop: extra})
	wantKinds(t, v, KindUndoAudit)
	if got := subjectsOf(v, KindUndoAudit); !reflect.DeepEqual(got, []string{"admin_actions"}) {
		t.Fatalf("undo_audit subjects %v, want [admin_actions]\nviolations: %+v", got, v)
	}
	if !strings.Contains(v[0].Detail, "'undo refused'") {
		t.Fatalf("detail %q does not name 'undo refused'", v[0].Detail)
	}
}

// Control (audit, per event): a done undo audit row for an event the phase
// did NOT undo (the stale one), written after the loop. A second done undo
// row for an undone event, the literal double undo, cannot be written at
// all: admin_actions_undo_once refuses it, which the hook asserts first.
// Kinds: undo_audit per event (the stale event) and in total (one more
// 'undo done' than undos carried out), plus the change_log undo check
// seeing a done undo audit row that reverted nothing (changelog_missing:
// "done undo action but no undo rows").
func TestDBUndoAllControlAuditStrayDoneUndo(t *testing.T) {
	pool := dbtest.Pool(t)
	f := history(t, pool, "a")
	subU := inboxIDOf(t, pool, "a_sub_u")
	stray := func() {
		_, err := pool.Exec(context.Background(), `INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
			VALUES ('undo', $1, 'outsider', 'control', 'done')`, subU)
		if err == nil || !strings.Contains(err.Error(), "admin_actions_undo_once") {
			t.Fatalf("second done undo row for an undone event: err %v, want admin_actions_undo_once", err)
		}
		mustExec(t, pool, `INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
			VALUES ('undo', $1, 'outsider', 'control', 'done')`, f.stale)
	}
	_, v := runUndoAll(t, pool, "a", UndoAllNewest, 1, undoAllHooks{afterLoop: stray})
	wantKinds(t, v, KindUndoAudit, "undo_"+KindChangeLogMissing)
	want := []string{"admin_actions", dbtest.EventPrefix + "a_sub_old"}
	if got := subjectsOf(v, KindUndoAudit); !reflect.DeepEqual(got, want) {
		t.Fatalf("undo_audit subjects %v, want %v\nviolations: %+v", got, want, v)
	}
	if got := subjectsOf(v, "undo_"+KindChangeLogMissing); !reflect.DeepEqual(got, []string{dbtest.EventPrefix + "a_sub_old"}) {
		t.Fatalf("changelog subjects %v\nviolations: %+v", got, v)
	}
}

// Control (audit, per event, "too few" direction): an undone event's done
// undo audit row is turned into a refused one after the loop, so the event
// is undone with NO done undo row. Kinds: undo_audit per event (sub_u: 0 done
// rows, want 1), undo_audit in total (one fewer 'undo done', one more 'undo
// refused' than the phase saw), and the change_log undo check seeing undo rows
// with no done undo action (changelog_unexpected).
func TestDBUndoAllControlAuditMissingDoneUndo(t *testing.T) {
	pool := dbtest.Pool(t)
	history(t, pool, "a")
	subU := inboxIDOf(t, pool, "a_sub_u")
	demote := func() {
		mustExec(t, pool, `UPDATE admin_actions SET outcome = 'refused'
			WHERE inbox_id = $1 AND action = 'undo' AND outcome = 'done'`, subU)
	}
	_, v := runUndoAll(t, pool, "a", UndoAllNewest, 1, undoAllHooks{afterLoop: demote})
	wantKinds(t, v, KindUndoAudit, "undo_"+KindChangeLogUnexpected)
	want := []string{"admin_actions", "admin_actions", dbtest.EventPrefix + "a_sub_u"}
	if got := subjectsOf(v, KindUndoAudit); !reflect.DeepEqual(got, want) {
		t.Fatalf("undo_audit subjects %v, want %v\nviolations: %+v", got, want, v)
	}
	if got := subjectsOf(v, "undo_"+KindChangeLogUnexpected); !reflect.DeepEqual(got, []string{dbtest.EventPrefix + "a_sub_u"}) {
		t.Fatalf("changelog subjects %v\nviolations: %+v", got, v)
	}
}

// An event replayed to generation 1 (dead -> admin.Replay -> applied again)
// is a candidate like any other: its apply id is its generation-1 max, it is
// applied last so newest mode undoes it first, and the run is clean. Chaos
// never replays, so this is the only test of the generation filter in
// readUndoPhase; for a replayed dead event it cannot change the result
// (dead left no generation-0 apply rows), so the filter is defensive.
func TestDBUndoAllReplayedGeneration1(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	f := history(t, pool, "a")
	late := dbtest.InsertEvent(t, pool, "a_inv_late", "invoice.paid", dbtest.InvoiceObject("in_dbtest_a_late", f.customer, 3000))
	mustExec(t, pool, `UPDATE inbox SET status = 'dead', attempts = 8, last_error = 'synthetic', processed_at = now() WHERE id = $1`, late)
	if res, err := admin.Replay(ctx, pool, admin.ReplayRequest{InboxID: late, Actor: "dbtest", Reason: "undo-all test"}, admin.Options{}); err != nil || res.Outcome != admin.ReplayDone {
		t.Fatalf("replay: %+v, %v", res, err)
	}
	if got := dbtest.ApplyInbox(t, pool, late); got != "done" {
		t.Fatalf("replayed invoice: %s, want done", got)
	}
	wantViolations(t, check(t, pool, "a"))

	var gen1Max int64
	if err := pool.QueryRow(ctx, `
		SELECT max(cl.id) FROM change_log cl JOIN inbox i ON i.id = cl.inbox_id
		WHERE cl.inbox_id = $1 AND cl.source = 'apply' AND cl.generation = 1 AND i.replay_count = 1`, late).Scan(&gen1Max); err != nil {
		t.Fatal(err)
	}
	pre, err := readUndoPhase(ctx, pool, testScope("a"))
	if err != nil {
		t.Fatal(err)
	}
	cs := append([]undoCandidate(nil), pre.done...)
	sortNewest(cs)
	if cs[0].InboxID != late || cs[0].ApplyID != gen1Max || gen1Max == 0 {
		t.Fatalf("first candidate %+v, want inbox %d with apply id %d (generation 1 max)", cs[0], late, gen1Max)
	}

	rep, v := runUndoAll(t, pool, "a", UndoAllNewest, 1, undoAllHooks{})
	wantViolations(t, v)
	if rep.Candidates != 5 || rep.Undone != 5 || rep.Refusals != 0 {
		t.Fatalf("report %+v; want candidates 5, undone 5, refusals 0", rep)
	}
	var undoGen int
	if err := pool.QueryRow(ctx, `
		SELECT DISTINCT cl.generation FROM change_log cl JOIN admin_actions a ON a.id = cl.admin_action_id
		WHERE a.inbox_id = $1 AND a.action = 'undo' AND a.outcome = 'done'`, late).Scan(&undoGen); err != nil {
		t.Fatal(err)
	}
	if undoGen != 1 {
		t.Fatalf("undo rows of the replayed event at generation %d, want 1", undoGen)
	}
}
