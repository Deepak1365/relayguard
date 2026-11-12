package sim

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/changelog"
)

// Violation kinds reported by CheckChangeLog.
const (
	KindChangeLogMissing       = "changelog_missing"
	KindChangeLogUnexpected    = "changelog_unexpected"
	KindChangeLogChainBreak    = "changelog_chain_break"
	KindChangeLogFinalMismatch = "changelog_final_mismatch"
	// KindChangeLogMisattributed: an event has the right number and kind of
	// rows, but their content belongs to another event (e.g. a grant for a
	// different invoice). Separate from changelog_unexpected because the
	// rows themselves are fine and nothing is extra; what is wrong is which
	// event they are filed under, which is exactly what undo (step 6)
	// trusts when it reverts "this event's" changes.
	KindChangeLogMisattributed = "changelog_misattributed"
)

// changeLogScope says which inbox events and app rows belong to one run.
// Every value is an exact prefix ending in "_", so seed 1 never matches
// seed 12's ids ("evt_s1_" is not a prefix of "evt_s12_...").
type changeLogScope struct {
	events        string // inbox.event_id
	customers     string // customers.stripe_customer_id
	subscriptions string // subscriptions.stripe_subscription_id
}

// seedScope is the scope of one generated history (see Generate): customer
// and subscription ids are per seed, and every grant belongs to a customer.
func seedScope(seed int64) changeLogScope {
	return changeLogScope{
		events:        fmt.Sprintf("evt_s%d_", seed),
		customers:     fmt.Sprintf("cus_s%d_", seed),
		subscriptions: fmt.Sprintf("sub_s%d_", seed),
	}
}

// CheckChangeLog verifies that change_log is a complete, consistent record
// of what this seed's events did (RelayGuard target only; the naive receiver
// writes no change_log by design):
//
//  1. Per inbox event: a done event has exactly the apply rows its type
//     implies (tables in seq order); stale, skipped, dead and pending events
//     have none. The mapping is written here, independent of internal/app,
//     so a change in app cannot silently redefine "correct".
//  2. Per app row: its entries, ordered by id, start with an insert at
//     row_version 1; each before image equals the previous after image; each
//     update raises row_version by exactly 1.
//  3. Per app row: the last after image equals the row as stored, and every
//     row in scope has at least one entry.
//  4. Per apply row: its content belongs to its event (see checkAttribution).
//
// Rows in scope: this seed's customers, their grants, this seed's
// subscriptions (or ones owned by this seed's customers), plus any row this
// seed's events logged. A row's chain uses ALL its entries, whoever wrote
// them, because the chain is a property of the row, not of one event.
func CheckChangeLog(ctx context.Context, pool *pgxpool.Pool, seed int64) ([]Violation, error) {
	return checkChangeLog(ctx, pool, seedScope(seed))
}

func checkChangeLog(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope) ([]Violation, error) {
	v, err := checkApplyCounts(ctx, pool, sc)
	if err != nil {
		return nil, fmt.Errorf("change_log counts: %w", err)
	}
	chains, err := checkChains(ctx, pool, sc)
	if err != nil {
		return nil, fmt.Errorf("change_log chains: %w", err)
	}
	final, err := checkFinalState(ctx, pool, sc)
	if err != nil {
		return nil, fmt.Errorf("change_log final state: %w", err)
	}
	attr, err := checkAttribution(ctx, pool, sc)
	if err != nil {
		return nil, fmt.Errorf("change_log attribution: %w", err)
	}
	v = append(v, chains...)
	v = append(v, final...)
	return append(v, attr...), nil
}

// checkAttribution checks that each apply row's content belongs to the event
// it is filed under, using the event's own payload (independent of
// internal/app). The count check only sees table sequences, so two invoice
// events with swapped rows would pass it. Per row, the first failing rule
// is reported:
//   - every row: row_id is the id inside its image;
//   - customer.created: the customer is the payload's customer;
//   - invoice.paid grant: the payload's invoice id, and amount_paid/100 credits;
//   - invoice.paid customer update: the payload's customer, the grant's
//     customer_id, and a balance change equal to the grant amount;
//   - subscription events: the payload's subscription, last_event_id = event id.
//
// Rows whose shape is wrong (missing grant etc.) are the count check's job:
// the grant-based rules are skipped when the event has no grant row, so one
// deleted row is not reported twice.
func checkAttribution(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope) ([]Violation, error) {
	rows, err := pool.Query(ctx, `
		WITH r AS (
			SELECT i.event_id, i.event_type, i.payload->'data'->'object' AS obj,
			       cl.seq, cl.table_name, cl.row_id, cl.before_image, cl.after_image,
			       COALESCE(cl.after_image, cl.before_image) AS img,
			       g.customer_id AS grant_customer, g.amount AS grant_amount
			FROM inbox i
			JOIN change_log cl ON cl.inbox_id = i.id AND cl.source = 'apply'
			LEFT JOIN LATERAL (
				SELECT (x.after_image->>'customer_id')::bigint AS customer_id,
				       (x.after_image->>'amount')::bigint      AS amount
				FROM change_log x
				WHERE x.inbox_id = cl.inbox_id AND x.generation = cl.generation
				  AND x.source = 'apply' AND x.table_name = 'credit_grants'
				ORDER BY x.seq LIMIT 1
			) g ON true
			WHERE left(i.event_id, length($1)) = $1
		)
		SELECT event_id, seq, table_name, row_id, reason FROM (
			SELECT r.*, CASE
				WHEN (img->>'id')::bigint IS DISTINCT FROM row_id
					THEN format('row_id %s but the image is of row %s', row_id, img->>'id')
				WHEN event_type = 'customer.created' AND table_name = 'customers'
				     AND img->>'stripe_customer_id' IS DISTINCT FROM obj->>'id'
					THEN format('customer %s, event is for %s', img->>'stripe_customer_id', obj->>'id')
				WHEN event_type = 'invoice.paid' AND table_name = 'credit_grants'
				     AND img->>'stripe_invoice_id' IS DISTINCT FROM obj->>'id'
					THEN format('grant for invoice %s, event is for %s', img->>'stripe_invoice_id', obj->>'id')
				-- COALESCE(..., 0): a payload without amount_paid decodes to
				-- 0 in Go (zero value), so the app grants 0 credits; SQL would
				-- give NULL and report a false mismatch. bigint / 100
				-- truncates like int64 division in Go (TestDBChangeLogCheckRounding).
				WHEN event_type = 'invoice.paid' AND table_name = 'credit_grants'
				     AND (img->>'amount')::bigint IS DISTINCT FROM COALESCE((obj->>'amount_paid')::bigint, 0) / 100
					THEN format('grant amount %s, event pays %s cents', img->>'amount', COALESCE((obj->>'amount_paid')::bigint, 0))
				WHEN event_type = 'invoice.paid' AND table_name = 'customers'
				     AND img->>'stripe_customer_id' IS DISTINCT FROM obj->>'customer'
					THEN format('customer %s, invoice is for %s', img->>'stripe_customer_id', obj->>'customer')
				WHEN event_type = 'invoice.paid' AND table_name = 'customers'
				     AND grant_customer IS NOT NULL AND row_id IS DISTINCT FROM grant_customer
					THEN format('customer row %s, the event''s grant is for customer %s', row_id, grant_customer)
				WHEN event_type = 'invoice.paid' AND table_name = 'customers' AND grant_amount IS NOT NULL
				     AND (after_image->>'credit_balance')::bigint - (before_image->>'credit_balance')::bigint
				         IS DISTINCT FROM grant_amount
					THEN format('balance %s -> %s, the event''s grant is %s',
					            before_image->>'credit_balance', after_image->>'credit_balance', grant_amount)
				WHEN event_type LIKE 'customer.subscription.%' AND table_name = 'subscriptions'
				     AND (img->>'stripe_subscription_id' IS DISTINCT FROM obj->>'id'
				          OR img->>'last_event_id' IS DISTINCT FROM event_id)
					THEN format('subscription %s last_event_id %s, event is %s for %s',
					            img->>'stripe_subscription_id', img->>'last_event_id', event_id, obj->>'id')
			END AS reason
			FROM r
		) x
		WHERE reason IS NOT NULL
		ORDER BY event_id, seq`, sc.events)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var v []Violation
	for rows.Next() {
		var eventID, table, reason string
		var seq int32
		var rowID int64
		if err := rows.Scan(&eventID, &seq, &table, &rowID, &reason); err != nil {
			return nil, err
		}
		v = append(v, Violation{KindChangeLogMisattributed, eventID,
			fmt.Sprintf("seq %d %s/%d: %s", seq, table, rowID, reason)})
	}
	return v, rows.Err()
}

// expectedApplyTables is what one successful apply of eventType writes, in
// order. Deliberately a copy of the business rules, not a call into
// internal/app: the checker is the independent judge.
func expectedApplyTables(eventType string) []string {
	switch eventType {
	case "customer.created":
		return []string{"customers"}
	case "invoice.paid":
		return []string{"credit_grants", "customers"} // grant insert, then balance update
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		return []string{"subscriptions"}
	}
	return nil
}

// compareApplyRows judges one inbox event's apply rows: their tables and seq
// numbers, both in seq order. Only done events have an effect; any other
// status must have none. With the right tables, seq must still run 1..n:
// the Recorder numbers every apply from 1, so a gap or offset means rows
// were renumbered or moved between events.
func compareApplyRows(eventID, eventType, status string, got []string, seqs []int32) *Violation {
	var want []string
	if status == "done" {
		want = expectedApplyTables(eventType)
	}
	detail := fmt.Sprintf("%s %s: logged [%s], want [%s]",
		status, eventType, strings.Join(got, " "), strings.Join(want, " "))
	switch {
	case len(got) < len(want):
		return &Violation{KindChangeLogMissing, eventID, detail}
	case len(got) > len(want):
		return &Violation{KindChangeLogUnexpected, eventID, detail}
	}
	for i := range got {
		if got[i] != want[i] {
			return &Violation{KindChangeLogUnexpected, eventID, detail}
		}
	}
	for i, s := range seqs {
		if s != int32(i+1) {
			return &Violation{KindChangeLogUnexpected, eventID,
				fmt.Sprintf("%s %s: seq %v, want 1..%d with no gaps", status, eventType, seqs, len(seqs))}
		}
	}
	return nil
}

// checkApplyCounts compares every in-scope event's source='apply' rows of
// its CURRENT generation (change_log.generation = inbox.replay_count) with
// expectedApplyTables. Undo rows (phase 6 step 6) are not an event's effect
// and are excluded.
//
// Apply rows of any other generation are reported as changelog_unexpected,
// separately from the current-generation comparison. Replay only starts from
// dead or stale events, which have no apply rows, so earlier generations are
// always empty; and a generation above replay_count was never fenced. Either
// kind of row means the log does not describe what the event did.
func checkApplyCounts(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope) ([]Violation, error) {
	rows, err := pool.Query(ctx, `
		SELECT i.event_id, i.event_type, i.status, i.replay_count,
		       COALESCE(array_agg(cl.table_name ORDER BY cl.seq) FILTER (WHERE cl.generation = i.replay_count), '{}'),
		       COALESCE(array_agg(cl.seq ORDER BY cl.seq) FILTER (WHERE cl.generation = i.replay_count), '{}'),
		       COALESCE(array_agg(DISTINCT cl.generation) FILTER (WHERE cl.generation <> i.replay_count), '{}')
		FROM inbox i
		LEFT JOIN change_log cl ON cl.inbox_id = i.id AND cl.source = 'apply'
		WHERE left(i.event_id, length($1)) = $1
		GROUP BY i.id
		ORDER BY i.id`, sc.events)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var v []Violation
	for rows.Next() {
		var eventID, eventType, status string
		var generation int32
		var tables []string
		var seqs, otherGens []int32
		if err := rows.Scan(&eventID, &eventType, &status, &generation, &tables, &seqs, &otherGens); err != nil {
			return nil, err
		}
		if bad := compareApplyRows(eventID, eventType, status, tables, seqs); bad != nil {
			v = append(v, *bad)
		}
		if bad := otherGenerationRows(eventID, generation, otherGens); bad != nil {
			v = append(v, *bad)
		}
	}
	return v, rows.Err()
}

// otherGenerationRows reports apply rows filed under a generation other than
// the event's current one (see checkApplyCounts). One violation per event.
func otherGenerationRows(eventID string, current int32, others []int32) *Violation {
	if len(others) == 0 {
		return nil
	}
	return &Violation{KindChangeLogUnexpected, eventID,
		fmt.Sprintf("apply rows in generation(s) %v, but the event is at generation %d (replay_count)", others, current)}
}

// scopeCTE defines seed_inbox and scope (table_name, row_id) for $1 events,
// $2 customers, $3 subscriptions. left(...) = prefix, not LIKE: '_' in the
// prefixes is a LIKE wildcard.
const scopeCTE = `
	seed_inbox AS (
		SELECT id FROM inbox WHERE left(event_id, length($1)) = $1
	),
	seed_customers AS (
		SELECT id FROM customers WHERE left(stripe_customer_id, length($2)) = $2
	),
	scope AS (
		SELECT 'customers'::text AS table_name, id AS row_id FROM seed_customers
		UNION
		SELECT 'credit_grants', g.id FROM credit_grants g WHERE g.customer_id IN (SELECT id FROM seed_customers)
		UNION
		SELECT 'subscriptions', s.id FROM subscriptions s
		WHERE left(s.stripe_subscription_id, length($3)) = $3 OR s.customer_id IN (SELECT id FROM seed_customers)
		UNION
		SELECT cl.table_name, cl.row_id FROM change_log cl WHERE cl.inbox_id IN (SELECT id FROM seed_inbox)
	)`

// imagesEqualSQL compares two jsonb row images of the row's own table as
// typed records, not as JSON text, so the result does not depend on how a
// value happened to be rendered (e.g. a timestamptz in another TimeZone).
//
// Limits of jsonb_populate_record, by design:
//   - keys that are not columns of the table are ignored, so an image with
//     an extra key still compares equal;
//   - a column missing from an image becomes NULL. After a future migration
//     adds a column, images written before it lack that key, so a row not
//     rewritten since would show as changelog_final_mismatch (and an old
//     after image vs a new before image as changelog_chain_break). Such a
//     migration must backfill change_log images or teach this check the
//     column. A key-set comparison was considered and left out: it would
//     turn exactly that migration case into false positives, while every
//     image RelayGuard writes comes from to_jsonb(row) and has all columns.
func imagesEqualSQL(table, a, b string) string {
	return fmt.Sprintf(`CASE %[1]s
		WHEN 'customers'     THEN jsonb_populate_record(NULL::customers, %[2]s)     IS NOT DISTINCT FROM jsonb_populate_record(NULL::customers, %[3]s)
		WHEN 'credit_grants' THEN jsonb_populate_record(NULL::credit_grants, %[2]s) IS NOT DISTINCT FROM jsonb_populate_record(NULL::credit_grants, %[3]s)
		WHEN 'subscriptions' THEN jsonb_populate_record(NULL::subscriptions, %[2]s) IS NOT DISTINCT FROM jsonb_populate_record(NULL::subscriptions, %[3]s)
	END`, table, a, b)
}

// chainEntry is one change_log entry of a row, as checkChain needs it.
// ID 0 means the row has no entries at all.
// The before image's row_version needs no separate check: Chained already
// requires the whole before image to equal the previous after image.
type chainEntry struct {
	ID      int64
	Op      string
	AfterRV *int64 // after_image->row_version
	Chained bool   // before_image equals the previous entry's after_image
}

// checkChain judges one row's entries (ordered by id) and reports the first
// problem only: after a break every later entry would be suspect too, and
// one violation per row keeps reports readable.
func checkChain(subject string, entries []chainEntry) *Violation {
	if len(entries) == 0 || entries[0].ID == 0 {
		return &Violation{KindChangeLogMissing, subject, "app row has no change_log entry"}
	}
	brk := func(format string, args ...any) *Violation {
		return &Violation{KindChangeLogChainBreak, subject, fmt.Sprintf(format, args...)}
	}
	first := entries[0]
	if first.Op != changelog.OpInsert {
		return brk("entry %d: first entry is %s, want insert", first.ID, first.Op)
	}
	if first.AfterRV == nil || *first.AfterRV != 1 {
		return brk("entry %d: insert has row_version %s, want 1", first.ID, rvText(first.AfterRV))
	}
	prevRV := *first.AfterRV
	for i, e := range entries[1:] {
		prevID := entries[i].ID
		if !e.Chained {
			return brk("entry %d: before_image differs from entry %d's after_image (unlogged write in between?)", e.ID, prevID)
		}
		switch e.Op {
		case changelog.OpUpdate:
			if e.AfterRV == nil || *e.AfterRV != prevRV+1 {
				return brk("entry %d: row_version %d -> %s, want %d", e.ID, prevRV, rvText(e.AfterRV), prevRV+1)
			}
			prevRV = *e.AfterRV
		case changelog.OpDelete:
			if i+2 != len(entries) {
				return brk("entry %d: entries follow a delete", e.ID)
			}
		default:
			return brk("entry %d: %s after the row already existed", e.ID, e.Op)
		}
	}
	return nil
}

func rvText(rv *int64) string {
	if rv == nil {
		return "missing"
	}
	return fmt.Sprint(*rv)
}

func checkChains(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope) ([]Violation, error) {
	rows, err := pool.Query(ctx, `
		WITH `+scopeCTE+`
		SELECT s.table_name, s.row_id, COALESCE(cl.id, 0), COALESCE(cl.op, ''),
		       (cl.after_image->>'row_version')::bigint,
		       COALESCE(`+imagesEqualSQL("s.table_name", "cl.before_image", "lag(cl.after_image) OVER w")+`, false)
		FROM scope s
		LEFT JOIN change_log cl ON cl.table_name = s.table_name AND cl.row_id = s.row_id
		WINDOW w AS (PARTITION BY s.table_name, s.row_id ORDER BY cl.id)
		ORDER BY s.table_name, s.row_id, cl.id`, sc.events, sc.customers, sc.subscriptions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var v []Violation
	var subject string
	var entries []chainEntry
	flush := func() {
		if subject == "" {
			return
		}
		if bad := checkChain(subject, entries); bad != nil {
			v = append(v, *bad)
		}
	}
	for rows.Next() {
		var table string
		var rowID int64
		var e chainEntry
		if err := rows.Scan(&table, &rowID, &e.ID, &e.Op, &e.AfterRV, &e.Chained); err != nil {
			return nil, err
		}
		s := fmt.Sprintf("%s/%d", table, rowID)
		if s != subject {
			flush()
			subject, entries = s, nil
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return v, nil
}

// checkFinalState folds each in-scope row's log: the last entry's after image
// must equal the stored row (compared as typed records via
// jsonb_populate_record), or the row must be gone if the last op was delete.
// Rows with no entries are reported by checkChains as changelog_missing.
func checkFinalState(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope) ([]Violation, error) {
	rows, err := pool.Query(ctx, `
		WITH `+scopeCTE+`,
		last AS (
			SELECT DISTINCT ON (cl.table_name, cl.row_id) cl.table_name, cl.row_id, cl.op, cl.after_image
			FROM change_log cl JOIN scope s USING (table_name, row_id)
			ORDER BY cl.table_name, cl.row_id, cl.id DESC
		),
		stored AS (
			SELECT l.*, CASE l.table_name
				WHEN 'customers'     THEN (SELECT to_jsonb(c.*) FROM customers c WHERE c.id = l.row_id)
				WHEN 'credit_grants' THEN (SELECT to_jsonb(g.*) FROM credit_grants g WHERE g.id = l.row_id)
				WHEN 'subscriptions' THEN (SELECT to_jsonb(x.*) FROM subscriptions x WHERE x.id = l.row_id)
			END AS row_image
			FROM last l
		)
		SELECT table_name, row_id, op, after_image::text, row_image::text,
		       COALESCE(`+imagesEqualSQL("table_name", "after_image", "row_image")+`, false)
		FROM stored
		ORDER BY table_name, row_id`, sc.events, sc.customers, sc.subscriptions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var v []Violation
	for rows.Next() {
		var table, op string
		var rowID int64
		var after, stored *string
		var equal bool
		if err := rows.Scan(&table, &rowID, &op, &after, &stored, &equal); err != nil {
			return nil, err
		}
		subject := fmt.Sprintf("%s/%d", table, rowID)
		switch {
		case op == changelog.OpDelete && stored != nil:
			v = append(v, Violation{KindChangeLogFinalMismatch, subject, "last entry is a delete but the row exists"})
		case op == changelog.OpDelete:
		case stored == nil:
			v = append(v, Violation{KindChangeLogFinalMismatch, subject, "logged but the row does not exist"})
		case !equal:
			v = append(v, Violation{KindChangeLogFinalMismatch, subject, "stored row differs from the last after_image in: " + diffColumns(after, stored)})
		}
	}
	return v, rows.Err()
}

// diffColumns names the columns that differ, for the violation detail only
// (the verdict itself comes from the typed comparison in SQL).
func diffColumns(after, stored *string) string {
	if after == nil || stored == nil {
		return "?"
	}
	diffs, err := changelog.Diff([]byte(*after), []byte(*stored))
	if err != nil {
		return "? (" + err.Error() + ")"
	}
	names := make([]string, len(diffs))
	for i, d := range diffs {
		names[i] = fmt.Sprintf("%s %s->%s", d.Field, d.Before, d.After)
	}
	return strings.Join(names, ", ")
}
