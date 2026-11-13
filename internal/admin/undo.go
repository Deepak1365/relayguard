package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/changelog"
)

var (
	// ErrNotUndoable: the event's state does not allow an undo (only done
	// events with recorded changes, never one already undone). Audited.
	ErrNotUndoable = errors.New("event cannot be undone")
	// ErrConflict: rows the event changed were changed again later, by an
	// event that is not undone or outside RelayGuard. The result lists every
	// conflict with an explanation. Audited.
	ErrConflict = errors.New("undo conflicts with later changes")
)

// Undo outcomes, as stored in admin_actions.outcome.
const (
	UndoDone    = "done"
	UndoRefused = "refused"
)

// Conflict kinds (rules 1-5 of the step 6 plan), stored in the refusal's
// audit detail and explained by FormatUndo.
const (
	ConflictMissing    = "missing"          // rule 1: the row is gone
	ConflictLaterEvent = "later_event"      // rule 2: a later event that is not undone changed the row
	ConflictUnpaired   = "unpaired"         // rule 2: a later event says undone, but nothing reverted its change here
	ConflictVersion    = "changed_outside"  // rule 3: row_version is not what the log says
	ConflictChainBreak = "chain_break"      // rule 3b: an unlogged write sits between two logged changes
	ConflictContent    = "content_mismatch" // rule 4: version matches, content does not
	ConflictReferenced = "referenced"       // rule 5: another row still points at a customer to delete
)

// Conflict is one reason an undo was refused, about one row.
type Conflict struct {
	Kind        string `json:"kind"`
	Table       string `json:"table"`
	RowID       int64  `json:"row_id"`
	ChangeLogID int64  `json:"change_log_id,omitempty"` // the later change_log row involved, if any
	InboxID     int64  `json:"inbox_id,omitempty"`      // the later event involved, if any
	EventID     string `json:"event_id,omitempty"`
	EventType   string `json:"event_type,omitempty"`
	// Generation of the later change involved; nil when no event is involved
	// (a write outside RelayGuard), so it never reads as "generation 0".
	Generation  *int   `json:"generation,omitempty"`
	Explanation string `json:"explanation"`
}

// UndoRequest asks to undo one inbox event. Actor and Reason are required,
// as for replay.
type UndoRequest struct {
	InboxID int64
	Actor   string
	Reason  string
}

// UndoResult describes an undo that was carried out or refused.
type UndoResult struct {
	InboxID    int64
	EventID    string
	EventType  string
	Status     string             // the inbox status before the request
	Generation int                // replay_count: the generation whose apply is (or would be) reverted
	Outcome    string             // UndoDone or UndoRefused; "" if not reported
	Reason     string             // why it was refused
	ActionID   int64              // the admin_actions row written for this request
	Changes    []changelog.Change // done: the undo change_log rows, in seq order
	Conflicts  []Conflict         // refused with ErrConflict
	Warnings   []string           // done: consequences the operator should know about
}

// NormalizeUndoRequest trims and checks the request exactly like
// NormalizeReplayRequest. Pure.
func NormalizeUndoRequest(r UndoRequest) (UndoRequest, error) {
	var err error
	r.Actor, r.Reason, err = normalizeAudit(r.InboxID, r.Actor, r.Reason)
	return r, err
}

// restoreColumns lists, per app table, every column an undo restores from a
// before image: all of them except id (the row identity, never changed) and
// row_version (owned by the trigger, which bumps it on the restoring UPDATE,
// so the version chain keeps growing). Explicit instead of derived at run
// time so the SQL is fixed and reviewable; TestDBUndoColumnsMatchSchema fails
// if a migration adds a column that is missing here (it would silently not
// be restored). The keys are also the allow-list of table names that may
// appear in the SQL below.
var restoreColumns = map[string][]string{
	"customers":     {"stripe_customer_id", "email", "credit_balance", "created_at"},
	"credit_grants": {"customer_id", "stripe_invoice_id", "amount", "created_at"},
	"subscriptions": {"stripe_subscription_id", "customer_id", "status", "updated_at", "last_event_created", "last_event_id"},
}

// fkRef is a foreign key column pointing at an app table.
type fkRef struct{ table, column string }

// referencedBy lists, per app table, the foreign keys that point at it.
// Rule 5 needs it: inserting a referencing row takes only KEY SHARE on the
// parent and does not bump its row_version, so no version check notices it;
// deleting the parent would then fail with 23503. TestDBUndoColumnsMatchSchema
// keeps it in step with pg_constraint.
var referencedBy = map[string][]fkRef{
	"customers": {{"credit_grants", "customer_id"}, {"subscriptions", "customer_id"}},
}

// applyEntry is one change_log apply row of the event being undone.
type applyEntry struct {
	id     int64
	seq    int
	table  string
	rowID  int64
	op     string
	before []byte
	after  []byte
}

// rowState is one row the undo touches, as read under its lock.
type rowState struct {
	table   string
	id      int64
	deletes bool   // the event inserted it, so the undo deletes it (FOR UPDATE)
	exists  bool   // false: rule 1
	image   []byte // to_jsonb of the current row
	version int64
	own     []applyEntry // the event's entries on this row, in seq order
	last    applyEntry   // the event's last entry on this row
}

func rowKey(table string, id int64) string { return fmt.Sprintf("%s/%d", table, id) }

// Undo reverts the changes a done event's current generation recorded in
// change_log, if nothing later depends on them, and audits the request in
// admin_actions in the same transaction, whether it is carried out or
// refused. On success the event's status becomes 'undone' (terminal: the
// worker only claims pending, preview and replay refuse it).
//
// Conflict rule (user decision Q1, rule B): an event can be undone only if
// every later change to its rows is accounted for in change_log and is itself
// undone. In practice that means newest first per row; the refusal names the
// event to undo first, or says the row was changed outside RelayGuard.
//
// Locks, in this order (the deadlock argument is in the step 6 plan note):
//  1. the inbox row, FOR NO KEY UPDATE, taken FIRST while nothing else is
//     held, like Replay. It is exclusive against the worker's claim and fence,
//     against Replay and against a second Undo of the same event, so the
//     status and undo checks below cannot go stale before the commit.
//  2. the app rows the event wrote, in reverse seq order (the order they are
//     reverted in): FOR UPDATE for rows the undo deletes (it also blocks new
//     foreign-key references to a customer, which rule 5 relies on), FOR NO
//     KEY UPDATE for rows it restores (the same lock the worker's pre-reads
//     take, so it does not conflict with a concurrent grant's KEY SHARE).
//     Reverse seq order is deadlock-free only because every apply writes at
//     most one row that existed before it (a customer or a subscription, the
//     rows other events lock too) and writes it LAST; its other entries insert
//     rows of its own. So the undo's first row lock is its only contended one,
//     taken while it holds just the inbox row, which no worker waits for.
//     TestDBApplyWritesExistingRowLast pins that property; a new event type
//     that breaks it needs a new argument (e.g. locks sorted by table, id).
//
// All conflict checks read change_log and the references AFTER the row locks
// are held: in READ COMMITTED each statement sees what committed before it,
// so a worker that changed a row and committed while the undo waited for it
// is seen.
//
// Errors: ErrInvalidInput (nothing touched), ErrNotFound (no audit), ErrBusy
// (lock_timeout; nothing written), ErrNotUndoable / ErrConflict (refusal
// audited and committed). Any other error means the outcome was not reported
// (res.Outcome is ""); after a commit error check `admin log <id>`.
func Undo(ctx context.Context, pool *pgxpool.Pool, req UndoRequest, opts Options) (UndoResult, error) {
	req, err := NormalizeUndoRequest(req)
	if err != nil {
		return UndoResult{InboxID: req.InboxID}, err
	}
	opts = opts.withDefaults()
	res := UndoResult{InboxID: req.InboxID}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after commit

	// SET cannot take bind parameters; the values are integers we format.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", opts.LockTimeout.Milliseconds())); err != nil {
		return res, fmt.Errorf("set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", opts.StatementTimeout.Milliseconds())); err != nil {
		return res, fmt.Errorf("set statement_timeout: %w", err)
	}

	err = tx.QueryRow(ctx, `
		SELECT event_id, event_type, status, replay_count
		FROM inbox WHERE id = $1
		FOR NO KEY UPDATE`, req.InboxID).
		Scan(&res.EventID, &res.EventType, &res.Status, &res.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("%w: id %d", ErrNotFound, req.InboxID)
	}
	if err != nil {
		return res, classify("lock inbox", err)
	}
	if opts.afterLock != nil {
		opts.afterLock()
	}

	// Checked in addition to the status: the audit trail, not the status
	// column, is the record of what an admin did (same guard as Replay).
	var undoneBy *int64
	if err := tx.QueryRow(ctx, `
		SELECT min(id) FROM admin_actions
		WHERE inbox_id = $1 AND action = 'undo' AND outcome = 'done'`, req.InboxID).Scan(&undoneBy); err != nil {
		return res, classify("check undo", err)
	}
	var entries []applyEntry
	switch {
	case undoneBy != nil:
		res.Reason = fmt.Sprintf("the event was already undone (admin action #%d)", *undoneBy)
	case res.Status != "done":
		// pending: the worker has it; stale and skipped changed nothing; dead
		// never applied; undone: see above.
		res.Reason = fmt.Sprintf("status %s is not undoable (only done)", res.Status)
	default:
		if entries, err = readApplyEntries(ctx, tx, req.InboxID, res.Generation); err != nil {
			return res, classify("read change_log", err)
		}
		if len(entries) == 0 {
			res.Reason = fmt.Sprintf("no change_log apply rows in generation %d: nothing recorded to undo "+
				"(applied before change capture?)", res.Generation)
		}
		for _, e := range entries {
			if e.op == changelog.OpDelete {
				// The app never deletes; undoing a delete would need an insert
				// with the old id, which nothing here supports.
				res.Reason = fmt.Sprintf("change_log #%d is a delete; deletes cannot be undone", e.id)
				break // name the first one
			}
		}
	}
	if res.Reason != "" {
		return refuse(ctx, tx, req, res, ErrNotUndoable)
	}

	rows, order, err := lockRows(ctx, tx, entries)
	if err != nil {
		return res, classify("lock rows", err)
	}
	conflicts, err := findConflicts(ctx, tx, order)
	if err != nil {
		return res, classify("check conflicts", err)
	}
	if len(conflicts) > 0 {
		res.Conflicts = conflicts
		res.Reason = fmt.Sprintf("%d conflict(s) with later changes", len(conflicts))
		return refuse(ctx, tx, req, res, ErrConflict)
	}

	// The audit row comes first: undo change_log rows must reference it
	// (CHECK source='undo' <=> admin_action_id IS NOT NULL).
	var actionID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome, detail)
		VALUES ('undo', $1, $2, $3, 'done',
		        jsonb_build_object('from_status', $4::text, 'generation', $5::int, 'changes', $6::int))
		RETURNING id`,
		req.InboxID, req.Actor, req.Reason, res.Status, res.Generation, len(entries)).Scan(&actionID); err != nil {
		return res, classify("audit undo", err)
	}
	rec := changelog.NewUndo(req.InboxID, res.Generation, actionID)
	var changes []changelog.Change
	var warnings []string
	for i := len(entries) - 1; i >= 0; i-- {
		c, err := revert(ctx, tx, rec, entries[i], rows[rowKey(entries[i].table, entries[i].rowID)])
		if err != nil {
			return res, classify("revert", err)
		}
		changes = append(changes, c)
		if c.Table == "customers" && c.Op == changelog.OpDelete {
			warnings = append(warnings, customerDeletedWarning(c))
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE inbox SET status = 'undone' WHERE id = $1`, req.InboxID); err != nil {
		return res, classify("mark undone", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit undo: %w", err)
	}
	res.Outcome, res.ActionID, res.Changes, res.Warnings = UndoDone, actionID, changes, warnings
	return res, nil
}

// refuse writes the refusal's audit row, commits it, and returns errKind.
// Outcome and ActionID are set only after the commit (audit honesty, as in
// Replay): until then nothing is audited.
func refuse(ctx context.Context, tx pgx.Tx, req UndoRequest, res UndoResult, errKind error) (UndoResult, error) {
	conflicts := []byte("[]")
	if len(res.Conflicts) > 0 {
		var err error
		if conflicts, err = json.Marshal(res.Conflicts); err != nil {
			return res, fmt.Errorf("encode conflicts: %w", err)
		}
	}
	var actionID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome, detail)
		VALUES ('undo', $1, $2, $3, 'refused',
		        jsonb_build_object('status', $4::text, 'generation', $5::int, 'refused', $6::text,
		                           'conflicts', $7::text::jsonb))
		RETURNING id`,
		req.InboxID, req.Actor, req.Reason, res.Status, res.Generation, res.Reason, string(conflicts)).
		Scan(&actionID); err != nil {
		return res, fmt.Errorf("audit refusal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit refusal audit: %w", err)
	}
	res.Outcome, res.ActionID = UndoRefused, actionID
	return res, fmt.Errorf("inbox %d: %w: %s", req.InboxID, errKind, res.Reason)
}

// readApplyEntries reads the event's apply rows of one generation in seq
// order. They are immutable once committed, so no lock is needed.
func readApplyEntries(ctx context.Context, tx pgx.Tx, inboxID int64, generation int) ([]applyEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, seq, table_name, row_id, op, before_image::text, after_image::text
		FROM change_log
		WHERE inbox_id = $1 AND generation = $2 AND source = 'apply'
		ORDER BY seq`, inboxID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []applyEntry
	for rows.Next() {
		var e applyEntry
		var before, after *string
		if err := rows.Scan(&e.id, &e.seq, &e.table, &e.rowID, &e.op, &before, &after); err != nil {
			return nil, err
		}
		if _, ok := restoreColumns[e.table]; !ok {
			return nil, fmt.Errorf("change_log #%d: unknown table %q", e.id, e.table)
		}
		e.before, e.after = textBytes(before), textBytes(after)
		out = append(out, e)
	}
	return out, rows.Err()
}

// lockRows locks every row the entries touch, in reverse seq order, and
// reads its current image. order is the rows in locking order.
func lockRows(ctx context.Context, tx pgx.Tx, entries []applyEntry) (map[string]*rowState, []*rowState, error) {
	rows := map[string]*rowState{}
	var order []*rowState
	for _, e := range entries {
		k := rowKey(e.table, e.rowID)
		r := rows[k]
		if r == nil {
			r = &rowState{table: e.table, id: e.rowID}
			rows[k] = r
		}
		r.deletes = r.deletes || e.op == changelog.OpInsert
		r.own = append(r.own, e)
		r.last = e // entries are in seq order: the last one wins
	}
	seen := map[string]bool{}
	for i := len(entries) - 1; i >= 0; i-- {
		k := rowKey(entries[i].table, entries[i].rowID)
		if seen[k] {
			continue
		}
		seen[k] = true
		r := rows[k]
		mode := "FOR NO KEY UPDATE"
		if r.deletes {
			mode = "FOR UPDATE"
		}
		// r.table passed readApplyEntries' allow-list check.
		var img string
		err := tx.QueryRow(ctx, fmt.Sprintf(
			`SELECT to_jsonb(t.*)::text, t.row_version FROM %s t WHERE t.id = $1 %s`, r.table, mode), r.id).
			Scan(&img, &r.version)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			r.exists = false
		case err != nil:
			return nil, nil, fmt.Errorf("lock %s %d: %w", r.table, r.id, err)
		default:
			r.exists, r.image = true, []byte(img)
		}
		order = append(order, r)
	}
	return rows, order, nil
}

// laterEntry is a change_log row on the same app row, written after the
// event's last entry on it, with the state of the inbox event that wrote it.
type laterEntry struct {
	id          int64
	inboxID     int64
	source      string
	generation  int
	before      []byte
	after       []byte
	eventID     string
	eventType   string
	inboxStatus string
}

// findConflicts applies rules 1-5 to every locked row.
func findConflicts(ctx context.Context, tx pgx.Tx, order []*rowState) ([]Conflict, error) {
	var out []Conflict
	for _, r := range order {
		base := Conflict{Table: r.table, RowID: r.id}
		if !r.exists {
			c := base
			c.Kind = ConflictMissing
			c.Explanation = fmt.Sprintf("%s %d no longer exists: it was deleted outside RelayGuard", r.table, r.id)
			out = append(out, c)
			continue
		}
		later, err := readLater(ctx, tx, r)
		if err != nil {
			return nil, err
		}
		// Rule 2: every later apply entry must come from an undone event
		// whose undo reverted it on this row (undo rows carry the generation
		// of the apply they revert, so (inbox, generation) pairs them).
		for i, l := range later {
			if l.source != changelog.SourceApply {
				continue
			}
			c := base
			gen := l.generation
			c.ChangeLogID, c.InboxID, c.EventID, c.EventType, c.Generation = l.id, l.inboxID, l.eventID, l.eventType, &gen
			if l.inboxStatus != "undone" {
				c.Kind = ConflictLaterEvent
				c.Explanation = fmt.Sprintf("change_log #%d: inbox #%d (%s, %s, generation %d) changed this row later "+
					"and is not undone: undo inbox #%d first", l.id, l.inboxID, l.eventID, l.eventType, l.generation, l.inboxID)
				out = append(out, c)
				continue
			}
			if !pairedUndo(later[i+1:], l) {
				c.Kind = ConflictUnpaired
				c.Explanation = fmt.Sprintf("change_log #%d: inbox #%d is marked undone but no undo row reverts its change "+
					"to this row: changed outside RelayGuard", l.id, l.inboxID)
				out = append(out, c)
			}
		}
		// Rule 3, chain part: rules 3 and 4 below only see writes made after
		// the LAST logged change. A write between two logged changes is
		// carried forward by every later before/after image (and an undo of
		// the later event restores it), so the final comparison cannot see
		// it; undoing this event would then silently wipe it. Each logged
		// change must start from exactly the row the previous one left.
		breaks, err := chainBreaks(base, r, later)
		if err != nil {
			return nil, err
		}
		out = append(out, breaks...)
		// Rules 3 and 4: the row must be exactly what the log says it is
		// now: the after image of the last entry on it (a later one, or the
		// event's own).
		expected, expectedID := r.last.after, r.last.id
		if len(later) > 0 {
			expected, expectedID = later[len(later)-1].after, later[len(later)-1].id
		}
		if c, ok, err := checkImage(base, r, expected, expectedID); err != nil {
			return nil, err
		} else if !ok {
			out = append(out, c)
		}
		// Rule 5.
		if r.deletes {
			refs, err := readReferences(ctx, tx, base)
			if err != nil {
				return nil, err
			}
			out = append(out, refs...)
		}
	}
	return out, nil
}

func readLater(ctx context.Context, tx pgx.Tx, r *rowState) ([]laterEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.inbox_id, c.source, c.generation, c.before_image::text, c.after_image::text,
		       i.event_id, i.event_type, i.status
		FROM change_log c JOIN inbox i ON i.id = c.inbox_id
		WHERE c.table_name = $1 AND c.row_id = $2 AND c.id > $3
		ORDER BY c.id`, r.table, r.id, r.last.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []laterEntry
	for rows.Next() {
		var l laterEntry
		var before, after *string
		if err := rows.Scan(&l.id, &l.inboxID, &l.source, &l.generation, &before, &after,
			&l.eventID, &l.eventType, &l.inboxStatus); err != nil {
			return nil, err
		}
		l.before, l.after = textBytes(before), textBytes(after)
		out = append(out, l)
	}
	return out, rows.Err()
}

// chainBreaks checks every link in the row's log from the event's first entry
// on: each entry's before image must equal the previous entry's after image
// (row_version and content). The event's own links cannot break today (the
// apply holds the row lock between them), but checking them costs nothing
// and keeps the rule independent of that. The first entry's before image is
// the state the undo restores; nothing before it matters.
func chainBreaks(base Conflict, r *rowState, later []laterEntry) ([]Conflict, error) {
	type link struct {
		id            int64
		before, after []byte
	}
	var links []link
	for _, e := range r.own {
		links = append(links, link{e.id, e.before, e.after})
	}
	for _, l := range later {
		links = append(links, link{l.id, l.before, l.after})
	}
	var out []Conflict
	for i := 1; i < len(links); i++ {
		prev, cur := links[i-1], links[i]
		same, err := sameImage(prev.after, cur.before)
		if err != nil {
			return nil, fmt.Errorf("change_log #%d/#%d images: %w", prev.id, cur.id, err)
		}
		if !same {
			c := base
			c.Kind, c.ChangeLogID = ConflictChainBreak, cur.id
			c.Explanation = fmt.Sprintf("changed outside RelayGuard between change_log #%d and #%d "+
				"(#%d starts from a different row than #%d left)", prev.id, cur.id, cur.id, prev.id)
			out = append(out, c)
		}
	}
	return out, nil
}

// sameImage compares two logged images including row_version. nil (no row)
// equals only nil.
func sameImage(a, b []byte) (bool, error) {
	if a == nil || b == nil {
		return a == nil && b == nil, nil
	}
	x, err := decodeImage(a)
	if err != nil {
		return false, err
	}
	y, err := decodeImage(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(x, y), nil
}

// pairedUndo reports whether an undo row after apply entry l reverts it.
func pairedUndo(after []laterEntry, l laterEntry) bool {
	for _, u := range after {
		if u.source == changelog.SourceUndo && u.inboxID == l.inboxID && u.generation == l.generation {
			return true
		}
	}
	return false
}

// checkImage applies rule 3 (row_version) and, if that passes, rule 4
// (content apart from row_version) against the expected image.
func checkImage(base Conflict, r *rowState, expected []byte, expectedID int64) (Conflict, bool, error) {
	c := base
	c.ChangeLogID = expectedID
	if expected == nil {
		c.Kind = ConflictVersion
		c.Explanation = fmt.Sprintf("change_log #%d deleted this row, but it exists: changed outside RelayGuard", expectedID)
		return c, false, nil
	}
	want, err := decodeImage(expected)
	if err != nil {
		return c, false, fmt.Errorf("change_log #%d image: %w", expectedID, err)
	}
	got, err := decodeImage(r.image)
	if err != nil {
		return c, false, fmt.Errorf("%s %d image: %w", r.table, r.id, err)
	}
	if v := fmt.Sprint(want["row_version"]); v != fmt.Sprint(r.version) {
		c.Kind = ConflictVersion
		c.Explanation = fmt.Sprintf("row_version is %d, but change_log #%d left it at %s: changed outside RelayGuard",
			r.version, expectedID, v)
		return c, false, nil
	}
	delete(want, "row_version")
	delete(got, "row_version")
	if !reflect.DeepEqual(want, got) {
		c.Kind = ConflictContent
		c.Explanation = fmt.Sprintf("row content differs from change_log #%d's after image although row_version matches: "+
			"changed outside RelayGuard", expectedID)
		return c, false, nil
	}
	return c, true, nil
}

// decodeImage keeps numbers as their JSON text, so bigints compare exactly.
func decodeImage(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// readReferences lists rows that still reference a row the undo would
// delete, each attributed to the event whose apply inserted it, if any.
// Reliable because the row is held FOR UPDATE: a new reference needs KEY
// SHARE on it, which conflicts, so none can appear before the commit.
func readReferences(ctx context.Context, tx pgx.Tx, base Conflict) ([]Conflict, error) {
	var out []Conflict
	for _, ref := range referencedBy[base.Table] {
		// ref comes from the fixed referencedBy map, never input.
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT r.id, cl.id, cl.inbox_id, cl.generation, i.event_id, i.event_type
			FROM %s r
			LEFT JOIN LATERAL (
				SELECT id, inbox_id, generation FROM change_log
				WHERE table_name = $2 AND row_id = r.id AND source = 'apply' AND op = 'insert'
				ORDER BY id DESC LIMIT 1) cl ON true
			LEFT JOIN inbox i ON i.id = cl.inbox_id
			WHERE r.%s = $1
			ORDER BY r.id`, ref.table, ref.column), base.RowID, ref.table)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var clID, inboxID *int64
			var gen *int
			var eventID, eventType *string
			if err := rows.Scan(&id, &clID, &inboxID, &gen, &eventID, &eventType); err != nil {
				rows.Close()
				return nil, err
			}
			c := base
			c.Kind = ConflictReferenced
			if inboxID == nil {
				c.Explanation = fmt.Sprintf("%s %d references this row and was not written by RelayGuard: changed outside RelayGuard",
					ref.table, id)
			} else {
				c.ChangeLogID, c.InboxID, c.Generation, c.EventID, c.EventType = *clID, *inboxID, gen, *eventID, *eventType
				c.Explanation = fmt.Sprintf("%s %d references this row (inserted by inbox #%d, %s, %s): undo inbox #%d first",
					ref.table, id, *inboxID, *eventID, *eventType, *inboxID)
			}
			out = append(out, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// revert undoes one apply entry and records it. Both statements carry
// WHERE row_version = <the version read under the lock>: the lock makes a
// mismatch impossible, so a mismatch means a bug, and the undo fails rather
// than overwrite a version it did not check.
func revert(ctx context.Context, tx pgx.Tx, rec *changelog.Recorder, e applyEntry, r *rowState) (changelog.Change, error) {
	switch e.op {
	case changelog.OpInsert:
		var before string
		err := tx.QueryRow(ctx, fmt.Sprintf(
			`DELETE FROM %s t WHERE t.id = $1 AND t.row_version = $2 RETURNING to_jsonb(t.*)::text`, e.table),
			e.rowID, r.version).Scan(&before)
		if err != nil {
			return changelog.Change{}, fmt.Errorf("delete %s %d at row_version %d: %w", e.table, e.rowID, r.version, err)
		}
		c := changelog.Change{Table: e.table, RowID: e.rowID, Op: changelog.OpDelete, Before: []byte(before)}
		r.exists, r.image = false, nil
		return c, rec.Record(ctx, tx, c)
	case changelog.OpUpdate:
		cols := restoreColumns[e.table]
		var after string
		var version int64
		err := tx.QueryRow(ctx, fmt.Sprintf(`
			UPDATE %[1]s t SET (%[2]s) = (SELECT %[3]s FROM jsonb_populate_record(NULL::%[1]s, $3::text::jsonb) r)
			WHERE t.id = $1 AND t.row_version = $2
			RETURNING to_jsonb(t.*)::text, t.row_version`,
			e.table, strings.Join(cols, ", "), "r."+strings.Join(cols, ", r.")),
			e.rowID, r.version, string(e.before)).Scan(&after, &version)
		if err != nil {
			return changelog.Change{}, fmt.Errorf("restore %s %d at row_version %d: %w", e.table, e.rowID, r.version, err)
		}
		c := changelog.Change{Table: e.table, RowID: e.rowID, Op: changelog.OpUpdate, Before: r.image, After: []byte(after)}
		r.image, r.version = c.After, version
		return c, rec.Record(ctx, tx, c)
	default:
		return changelog.Change{}, fmt.Errorf("change_log #%d: cannot undo op %q", e.id, e.op)
	}
}

// customerDeletedWarning is the Q4 warning: deleting a customer is allowed
// when nothing references it, but it has consequences beyond this event.
func customerDeletedWarning(c changelog.Change) string {
	stripeID := "?"
	if img, err := decodeImage(c.Before); err == nil {
		if s, ok := img["stripe_customer_id"].(string); ok {
			stripeID = s
		}
	}
	return fmt.Sprintf("customer %s (id %d) was deleted: later events for it will fail with unknown customer and dead-letter "+
		"until it is created again; this undo itself cannot be reversed (undone events are not replayable)", stripeID, c.RowID)
}
