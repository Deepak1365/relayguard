package sim

// DB tests for the change_log checker, including negative controls: each
// one damages the log or the data in a specific way and asserts the checker
// reports exactly that. Skipped unless DB tests are enabled; see
// internal/dbtest.
//
// The tests use scopes built from dbtest's evt_dbtest_ / cus_dbtest_ /
// sub_dbtest_ prefixes, not real seed ids: dbtest.Truncate refuses to wipe
// inbox rows it did not create. CheckChangeLog(seed) is the same code with
// seedScope(seed), whose prefixes are unit tested in changelog_check_test.go.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

func testScope(tag string) changeLogScope {
	return changeLogScope{
		events:        dbtest.EventPrefix + tag + "_",
		customers:     "cus_dbtest_" + tag + "_",
		subscriptions: "sub_dbtest_" + tag + "_",
	}
}

// fixture is one small history in its own scope.
type fixture struct {
	customer, sub        string
	customerRow, subRow  int64
	invoice, stale, skip int64 // inbox ids
}

// history writes and applies, in this order: customer.created, invoice.paid,
// subscription created (T+1), updated (T+3), an older update (T+2, stale)
// and an unhandled type (skipped). Expected change_log: 1+2+1+1+0+0 rows.
func history(t *testing.T, pool *pgxpool.Pool, tag string) fixture {
	t.Helper()
	return buildHistory(t, pool, tag, false)
}

// buildHistory is history; with previewFirst it runs admin.Preview on each
// event just before applying it.
func buildHistory(t *testing.T, pool *pgxpool.Pool, tag string, previewFirst bool) fixture {
	t.Helper()
	f := fixture{customer: "cus_dbtest_" + tag + "_1", sub: "sub_dbtest_" + tag + "_1"}
	T := dbtest.DefaultCreated
	sub := func(status string) map[string]any {
		return map[string]any{"id": f.sub, "object": "subscription", "customer": f.customer, "status": status}
	}
	steps := []struct {
		suffix, typ string
		created     int64
		object      map[string]any
		want        string
		id          *int64
	}{
		{"cus", "customer.created", T, dbtest.CustomerObject(f.customer), "done", nil},
		{"inv", "invoice.paid", T, dbtest.InvoiceObject("in_dbtest_"+tag+"_1", f.customer, 2000), "done", &f.invoice},
		{"sub_c", "customer.subscription.created", T + 1, sub("active"), "done", nil},
		{"sub_u", "customer.subscription.updated", T + 3, sub("past_due"), "done", nil},
		{"sub_old", "customer.subscription.updated", T + 2, sub("active"), "stale", &f.stale},
		{"skip", "charge.refunded", T, map[string]any{"id": "ch_dbtest_" + tag}, "skipped", &f.skip},
	}
	for _, s := range steps {
		id := dbtest.InsertEventAt(t, pool, tag+"_"+s.suffix, s.typ, s.created, s.object)
		if previewFirst {
			if _, err := admin.Preview(context.Background(), pool, id, admin.Options{}); err != nil {
				t.Fatalf("preview %s: %v", s.suffix, err)
			}
		}
		if got := dbtest.ApplyInbox(t, pool, id); got != s.want {
			t.Fatalf("%s: status %s, want %s", s.suffix, got, s.want)
		}
		if s.id != nil {
			*s.id = id
		}
	}
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT id FROM customers WHERE stripe_customer_id = $1`, f.customer).Scan(&f.customerRow); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM subscriptions WHERE stripe_subscription_id = $1`, f.sub).Scan(&f.subRow); err != nil {
		t.Fatal(err)
	}
	return f
}

func check(t *testing.T, pool *pgxpool.Pool, tag string) []Violation {
	t.Helper()
	v, err := checkChangeLog(context.Background(), pool, testScope(tag))
	if err != nil {
		t.Fatalf("checkChangeLog(%s): %v", tag, err)
	}
	return v
}

// keys renders violations as sorted "kind subject" strings for exact asserts.
func keys(vs []Violation) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Kind + " " + v.Subject
	}
	sort.Strings(out)
	return out
}

func wantViolations(t *testing.T, got []Violation, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(keys(got), "\n") != strings.Join(want, "\n") {
		t.Fatalf("violations:\n%s\nwant:\n%s\ndetails: %+v", strings.Join(keys(got), "\n"), strings.Join(want, "\n"), got)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// Positive control: an untouched history (with stale and skipped events) is
// clean. Without this, the negative controls below could pass because the
// checker reports everything.
func TestDBChangeLogCheckCleanHistory(t *testing.T) {
	pool := dbtest.Pool(t)
	history(t, pool, "a")
	wantViolations(t, check(t, pool, "a"))
	if n := dbtest.Count(t, pool, "change_log"); n != 5 {
		t.Fatalf("change_log rows: %d, want 5", n)
	}
}

// The plan's negative control. A raw UPDATE bypasses app.Apply, so nothing
// logs it (the trigger still bumps row_version). How it surfaces depends on
// what follows:
//   - if a logged write follows (customer: another invoice), that write's
//     before image is not the last logged after image: chain break;
//   - if it is the row's last write (subscription), the log no longer folds
//     to the stored row: final mismatch.
func TestDBChangeLogCheckDetectsUnloggedWrite(t *testing.T) {
	pool := dbtest.Pool(t)
	f := history(t, pool, "a")

	mustExec(t, pool, `UPDATE customers SET credit_balance = credit_balance + 1 WHERE id = $1`, f.customerRow)
	inv2 := dbtest.InsertEvent(t, pool, "a_inv2", "invoice.paid", dbtest.InvoiceObject("in_dbtest_a_2", f.customer, 1000))
	if got := dbtest.ApplyInbox(t, pool, inv2); got != "done" {
		t.Fatalf("second invoice: %s", got)
	}
	mustExec(t, pool, `UPDATE subscriptions SET status = 'canceled' WHERE id = $1`, f.subRow)

	wantViolations(t, check(t, pool, "a"),
		fmt.Sprintf("%s customers/%d", KindChangeLogChainBreak, f.customerRow),
		fmt.Sprintf("%s subscriptions/%d", KindChangeLogFinalMismatch, f.subRow))
}

// The image link on its own. In the unlogged-write control the trigger also
// bumps row_version, so the "+1 per update" rule reports the break even if
// the before/after comparison were broken (a mutation check showed exactly
// that). Here row_version stays consistent and only the before image of the
// customer update is altered, so only the image comparison can see it.
func TestDBChangeLogCheckDetectsBrokenImageLink(t *testing.T) {
	pool := dbtest.Pool(t)
	f := history(t, pool, "a")
	mustExec(t, pool, `
		UPDATE change_log SET before_image = jsonb_set(before_image, '{email}', '"tampered@example.test"')
		WHERE inbox_id = $1 AND seq = 2`, f.invoice)
	v := check(t, pool, "a")
	wantViolations(t, v, fmt.Sprintf("%s customers/%d", KindChangeLogChainBreak, f.customerRow))
	if !strings.Contains(v[0].Detail, "before_image differs") {
		t.Fatalf("detail %q, want the image-link reason", v[0].Detail)
	}
}

// Deleting a logged row: the event is short one row, and the grant it
// created has no entry at all.
func TestDBChangeLogCheckDetectsMissing(t *testing.T) {
	pool := dbtest.Pool(t)
	f := history(t, pool, "a")
	var grantRow int64
	if err := pool.QueryRow(context.Background(), `
		DELETE FROM change_log WHERE inbox_id = $1 AND seq = 1 RETURNING row_id`, f.invoice).Scan(&grantRow); err != nil {
		t.Fatal(err)
	}
	wantViolations(t, check(t, pool, "a"),
		KindChangeLogMissing+" "+dbtest.EventPrefix+"a_inv",
		fmt.Sprintf("%s credit_grants/%d", KindChangeLogMissing, grantRow))
}

// A log row for an event that changed nothing (stale) is unexpected. The
// injected row is chained correctly (before = the last after image) and
// matches the stored row, so the final check is satisfied; but it claims an
// update without a row_version bump, so the chain check objects as well, and
// its image carries the newer event's last_event_id, so the attribution
// check objects too.
func TestDBChangeLogCheckDetectsUnexpected(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	f := history(t, pool, "a")
	var img []byte
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(s.*) FROM subscriptions s WHERE s.id = $1`, f.subRow).Scan(&img); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := changelog.NewApply(f.stale, 0).Record(ctx, tx, changelog.Change{
		Table: "subscriptions", RowID: f.subRow, Op: changelog.OpUpdate, Before: img, After: img}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantViolations(t, check(t, pool, "a"),
		KindChangeLogUnexpected+" "+dbtest.EventPrefix+"a_sub_old",
		KindChangeLogMisattributed+" "+dbtest.EventPrefix+"a_sub_old",
		fmt.Sprintf("%s subscriptions/%d", KindChangeLogChainBreak, f.subRow))
}

// Reviewer note from step 1: nothing stops an INSERT from setting
// row_version, so the chain must start at 1. Here a customer is inserted
// with row_version 5 and logged faithfully (the count and final checks are
// satisfied); only the first-version rule catches it.
func TestDBChangeLogCheckDetectsBadFirstVersion(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	inboxID := dbtest.InsertEvent(t, pool, "a_rv", "customer.created", dbtest.CustomerObject("cus_dbtest_a_rv"))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var rowID int64
	var img []byte
	if err := tx.QueryRow(ctx, `
		INSERT INTO customers AS c (stripe_customer_id, email, row_version) VALUES ('cus_dbtest_a_rv', 'x@example.test', 5)
		RETURNING c.id, to_jsonb(c.*)`).Scan(&rowID, &img); err != nil {
		t.Fatal(err)
	}
	if err := changelog.NewApply(inboxID, 0).Record(ctx, tx, changelog.Change{
		Table: "customers", RowID: rowID, Op: changelog.OpInsert, After: img}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE inbox SET status = 'done' WHERE id = $1`, inboxID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	v := check(t, pool, "a")
	wantViolations(t, v, fmt.Sprintf("%s customers/%d", KindChangeLogChainBreak, rowID))
	if !strings.Contains(v[0].Detail, "want 1") {
		t.Fatalf("detail %q, want the first-version reason", v[0].Detail)
	}
}

// Scoping: damage in one run's rows must not be reported for another run
// sharing the database, and vice versa.
func TestDBChangeLogCheckIgnoresOtherScopes(t *testing.T) {
	pool := dbtest.Pool(t)
	history(t, pool, "a")
	b := history(t, pool, "b")
	mustExec(t, pool, `UPDATE subscriptions SET status = 'canceled' WHERE id = $1`, b.subRow)
	var grantB int64
	if err := pool.QueryRow(context.Background(), `
		DELETE FROM change_log WHERE inbox_id = $1 AND seq = 1 RETURNING row_id`, b.invoice).Scan(&grantB); err != nil {
		t.Fatal(err)
	}

	wantViolations(t, check(t, pool, "a"))
	// Control: the damage in b is detectable, exactly where it was done.
	wantViolations(t, check(t, pool, "b"),
		fmt.Sprintf("%s subscriptions/%d", KindChangeLogFinalMismatch, b.subRow),
		KindChangeLogMissing+" "+dbtest.EventPrefix+"b_inv",
		fmt.Sprintf("%s credit_grants/%d", KindChangeLogMissing, grantB))
}

// Attribution: two invoice events for the same customer swap their
// change_log rows. Counts, seq numbering, chains and final state are all
// still fine (the rows themselves are untouched), so only the content
// check can tell that each event's grant is the other invoice's.
func TestDBChangeLogCheckDetectsSwappedEvents(t *testing.T) {
	pool := dbtest.Pool(t)
	f := history(t, pool, "a")
	inv2 := dbtest.InsertEvent(t, pool, "a_inv2", "invoice.paid", dbtest.InvoiceObject("in_dbtest_a_2", f.customer, 1000))
	if got := dbtest.ApplyInbox(t, pool, inv2); got != "done" {
		t.Fatalf("second invoice: %s", got)
	}
	wantViolations(t, check(t, pool, "a")) // clean before the swap

	// Swap through a third inbox row (the skipped event, which has no rows):
	// the unique index (inbox_id, generation, seq) is checked per row, so a
	// direct two-way swap in one statement could collide midway.
	mustExec(t, pool, `UPDATE change_log SET inbox_id = $2 WHERE inbox_id = $1`, f.invoice, f.skip)
	mustExec(t, pool, `UPDATE change_log SET inbox_id = $2 WHERE inbox_id = $1`, inv2, f.invoice)
	mustExec(t, pool, `UPDATE change_log SET inbox_id = $2 WHERE inbox_id = $1`, f.skip, inv2)

	v := check(t, pool, "a")
	wantViolations(t, v,
		KindChangeLogMisattributed+" "+dbtest.EventPrefix+"a_inv",
		KindChangeLogMisattributed+" "+dbtest.EventPrefix+"a_inv2")
	for _, x := range v {
		if !strings.Contains(x.Detail, "grant for invoice") {
			t.Fatalf("detail %q, want the invoice-id reason", x.Detail)
		}
	}
}

// Right tables, wrong numbering: the count check also requires seq 1..n.
func TestDBChangeLogCheckDetectsSeqGap(t *testing.T) {
	pool := dbtest.Pool(t)
	f := history(t, pool, "a")
	mustExec(t, pool, `UPDATE change_log SET seq = 3 WHERE inbox_id = $1 AND seq = 2`, f.invoice)
	v := check(t, pool, "a")
	wantViolations(t, v, KindChangeLogUnexpected+" "+dbtest.EventPrefix+"a_inv")
	if !strings.Contains(v[0].Detail, "no gaps") {
		t.Fatalf("detail %q, want the numbering reason", v[0].Detail)
	}
}

// inboxIDOf returns the inbox id of event dbtest.EventPrefix+suffix.
func inboxIDOf(t *testing.T, pool *pgxpool.Pool, suffix string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM inbox WHERE event_id = $1`, dbtest.EventPrefix+suffix).Scan(&id); err != nil {
		t.Fatalf("inbox id of %s: %v", suffix, err)
	}
	return id
}

// One negative control per attribution rule. Each case damages exactly the
// field its rule reads, on a clean history, and expects exactly one
// changelog_misattributed with that rule's reason. Rules are evaluated in
// order and the first failure is reported, so touching any earlier field
// would let an earlier rule mask the one under test. Image edits also break
// the chain or final state; those kinds are not what is tested here, so only
// changelog_misattributed violations are compared.
//
// The grant amount case edits the payload instead of the grant image: a
// changed grant amount would also (correctly) fail the customer update's
// "balance change = grant amount" rule, giving two violations.
// The grant invoice id rule is covered by TestDBChangeLogCheckDetectsSwappedEvents.
func TestDBChangeLogCheckAttributionRules(t *testing.T) {
	cases := []struct {
		name    string
		event   string // inbox suffix of the event expected to be reported
		damage  string // SQL; $1 = that event's inbox id
		wantSub string // substring of the violation detail
	}{
		{"row_id is not the image id", "a_cus",
			`UPDATE change_log SET row_id = row_id + 1000 WHERE inbox_id = $1 AND seq = 1`,
			"row_id 1001 but the image is of row 1"},
		{"customer.created customer", "a_cus",
			`UPDATE change_log SET after_image = jsonb_set(after_image, '{stripe_customer_id}', '"cus_dbtest_a_other"')
			 WHERE inbox_id = $1 AND seq = 1`,
			"customer cus_dbtest_a_other, event is for cus_dbtest_a_1"},
		{"grant amount vs amount_paid", "a_inv",
			`UPDATE inbox SET payload = jsonb_set(payload, '{data,object,amount_paid}', '2100') WHERE id = $1`,
			"grant amount 20, event pays 2100 cents"},
		{"invoice update stripe customer", "a_inv",
			`UPDATE change_log SET after_image = jsonb_set(after_image, '{stripe_customer_id}', '"cus_dbtest_a_other"')
			 WHERE inbox_id = $1 AND seq = 2`,
			"customer cus_dbtest_a_other, invoice is for cus_dbtest_a_1"},
		{"grant customer_id vs updated row", "a_inv",
			`UPDATE change_log SET after_image = jsonb_set(after_image, '{customer_id}', '999')
			 WHERE inbox_id = $1 AND seq = 1`,
			"customer row 1, the event's grant is for customer 999"},
		{"balance change vs grant amount", "a_inv",
			`UPDATE change_log SET before_image = jsonb_set(before_image, '{credit_balance}', '1')
			 WHERE inbox_id = $1 AND seq = 2`,
			"balance 1 -> 20, the event's grant is 20"},
		{"subscription last_event_id", "a_sub_u",
			`UPDATE change_log SET after_image = jsonb_set(after_image, '{last_event_id}', '"evt_other"')
			 WHERE inbox_id = $1 AND seq = 1`,
			"last_event_id evt_other"},
		{"subscription id", "a_sub_u",
			`UPDATE change_log SET after_image = jsonb_set(after_image, '{stripe_subscription_id}', '"sub_other"')
			 WHERE inbox_id = $1 AND seq = 1`,
			"subscription sub_other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := dbtest.Pool(t) // fresh tables per case
			history(t, pool, "a")
			wantViolations(t, check(t, pool, "a")) // clean before the damage
			mustExec(t, pool, c.damage, inboxIDOf(t, pool, c.event))

			var mis []Violation
			for _, v := range check(t, pool, "a") {
				if v.Kind == KindChangeLogMisattributed {
					mis = append(mis, v)
				}
			}
			if len(mis) != 1 || mis[0].Subject != dbtest.EventPrefix+c.event || !strings.Contains(mis[0].Detail, c.wantSub) {
				t.Fatalf("misattributed violations: %+v\nwant exactly one for %s containing %q",
					mis, dbtest.EventPrefix+c.event, c.wantSub)
			}
		})
	}
}

// Positive control for the amount rule on values chaos never generates
// (it only pays whole dollars): 50 cents grants 0 credits, 2050 grants 20,
// and an invoice without amount_paid grants 0 (Go decodes the missing field
// as 0). The checker's SQL arithmetic must agree with internal/app on all
// of them, so a clean history must stay clean.
func TestDBChangeLogCheckRounding(t *testing.T) {
	pool := dbtest.Pool(t)
	f := history(t, pool, "a")
	for _, inv := range []struct {
		suffix string
		object map[string]any
	}{
		{"a_inv_50", dbtest.InvoiceObject("in_dbtest_a_50", f.customer, 50)},
		{"a_inv_2050", dbtest.InvoiceObject("in_dbtest_a_2050", f.customer, 2050)},
		{"a_inv_none", map[string]any{"id": "in_dbtest_a_none", "object": "invoice", "customer": f.customer}},
	} {
		id := dbtest.InsertEvent(t, pool, inv.suffix, "invoice.paid", inv.object)
		if got := dbtest.ApplyInbox(t, pool, id); got != "done" {
			t.Fatalf("%s: status %s", inv.suffix, got)
		}
	}
	wantViolations(t, check(t, pool, "a"))
	var amounts []int64
	rows, err := pool.Query(context.Background(),
		`SELECT amount FROM credit_grants WHERE stripe_invoice_id IN ('in_dbtest_a_50', 'in_dbtest_a_2050', 'in_dbtest_a_none') ORDER BY stripe_invoice_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a int64
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		amounts = append(amounts, a)
	}
	// Sorted by invoice id: in_dbtest_a_2050, in_dbtest_a_50, in_dbtest_a_none.
	if fmt.Sprint(amounts) != "[20 0 0]" {
		t.Fatalf("grant amounts %v, want [20 0 0] (the app's truncation)", amounts)
	}
}

// Generations (step 5a). The count check compares only the rows of the
// event's current generation (change_log.generation = inbox.replay_count)
// with what its status implies, and reports rows of any other generation as
// changelog_unexpected. Synthetic: the invoice's rows and replay_count are
// edited directly. Moving rows between generations changes nothing the
// chain, final-state or attribution checks read, so every violation here
// comes from the count check.
//
// Each case asserts the exact set, so the two rules are told apart: "rows in
// the wrong generation" yields changelog_missing (current generation empty)
// AND changelog_unexpected (the rows elsewhere). Counting all generations
// together would lose the first; dropping the other-generation rule would
// lose the second.
func TestDBChangeLogCheckCountsCurrentGenerationOnly(t *testing.T) {
	inv := dbtest.EventPrefix + "a_inv"
	cases := []struct {
		name          string
		rowGeneration int // generation of the invoice's change_log rows
		replayCount   int // inbox.replay_count of the invoice
		want          []string
		wantDetail    string // substring of the changelog_unexpected detail
	}{
		{"rows in the current generation 1", 1, 1, nil, ""},
		{"rows left in generation 0 after replay", 0, 1,
			[]string{KindChangeLogMissing + " " + inv, KindChangeLogUnexpected + " " + inv}, "generation(s) [0], but the event is at generation 1"},
		{"rows in a generation above replay_count", 2, 1,
			[]string{KindChangeLogMissing + " " + inv, KindChangeLogUnexpected + " " + inv}, "generation(s) [2], but the event is at generation 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := dbtest.Pool(t)
			f := history(t, pool, "a")
			mustExec(t, pool, `UPDATE change_log SET generation = $2 WHERE inbox_id = $1`, f.invoice, c.rowGeneration)
			mustExec(t, pool, `UPDATE inbox SET replay_count = $2 WHERE id = $1`, f.invoice, c.replayCount)
			v := check(t, pool, "a")
			wantViolations(t, v, c.want...)
			for _, x := range v {
				if x.Kind == KindChangeLogUnexpected && !strings.Contains(x.Detail, c.wantDetail) {
					t.Fatalf("detail %q, want %q", x.Detail, c.wantDetail)
				}
			}
		})
	}
}

// The realistic path: an invoice that arrived before its customer is dead
// (no rows), and a stale subscription event (no rows); both are replayed
// (raw SQL until admin.Replay exists in step 5b) and applied again. The
// invoice now applies under generation 1, the stale event is stale again.
// The whole history must check clean, and the invoice's rows must really be
// in generation 1 (otherwise "clean" could just mean "not looked at").
func TestDBChangeLogCheckReplayedEventClean(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	f := history(t, pool, "a")

	// Dead: an invoice for a customer that did not exist when it was tried.
	early := dbtest.InsertEvent(t, pool, "a_inv_early", "invoice.paid", dbtest.InvoiceObject("in_dbtest_a_early", "cus_dbtest_a_2", 3000))
	mustExec(t, pool, `UPDATE inbox SET status = 'dead', attempts = 8, last_error = 'unknown customer cus_dbtest_a_2', processed_at = now() WHERE id = $1`, early)
	cus2 := dbtest.InsertEvent(t, pool, "a_cus2", "customer.created", dbtest.CustomerObject("cus_dbtest_a_2"))
	if got := dbtest.ApplyInbox(t, pool, cus2); got != "done" {
		t.Fatalf("second customer: %s", got)
	}
	wantViolations(t, check(t, pool, "a")) // dead with no rows is clean

	replay := `UPDATE inbox SET status = 'pending', attempts = 0, last_error = NULL, processed_at = NULL,
		next_attempt_at = now(), replay_count = replay_count + 1 WHERE id = $1`
	mustExec(t, pool, replay, early)
	mustExec(t, pool, replay, f.stale)
	if got := dbtest.ApplyInbox(t, pool, early); got != "done" {
		t.Fatalf("replayed invoice: %s, want done", got)
	}
	if got := dbtest.ApplyInbox(t, pool, f.stale); got != "stale" {
		t.Fatalf("replayed stale event: %s, want stale again", got)
	}

	wantViolations(t, check(t, pool, "a"))
	rows := dbtest.ChangeLog(t, pool, early)
	if len(rows) != 2 || rows[0].Generation != 1 || rows[1].Generation != 1 {
		t.Fatalf("replayed invoice change_log: %+v, want 2 rows in generation 1", rows)
	}
	if n := len(dbtest.ChangeLog(t, pool, f.stale)); n != 0 {
		t.Fatalf("replayed stale event: %d change_log rows, want 0", n)
	}
	var replays int
	if err := pool.QueryRow(ctx, `SELECT replay_count FROM inbox WHERE id = $1`, f.stale).Scan(&replays); err != nil {
		t.Fatal(err)
	}
	if replays != 1 {
		t.Fatalf("stale event replay_count %d, want 1", replays)
	}
}

// Previews write change_log rows and roll them back, which burns sequence
// ids. The checker must not care: previewing every event of a history right
// before applying it must still give 0 violations. The id gap proves the
// previews really ran their inserts (otherwise this would test nothing).
func TestDBChangeLogCheckAfterPreviews(t *testing.T) {
	pool := dbtest.Pool(t)
	buildHistory(t, pool, "a", true)
	wantViolations(t, check(t, pool, "a"))
	var maxID, count int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(id), 0), count(*) FROM change_log`).Scan(&maxID, &count); err != nil {
		t.Fatal(err)
	}
	if count != 5 || maxID <= count {
		t.Fatalf("change_log: %d rows, max id %d; want 5 rows and max id > 5 (ids burned by previews)", count, maxID)
	}
}
