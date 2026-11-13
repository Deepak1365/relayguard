package sim

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/changelog"
)

// KindChangeLogBadUndo: an event's undo rows exist where they should, but do
// not describe a faithful revert of its apply rows (wrong audit link, wrong
// generation, wrong pairing or order, or content that is not the apply's
// before image). Separate from changelog_unexpected because the rows are
// not extra; they are wrong, and undo is exactly what an operator trusts to
// put data back.
const KindChangeLogBadUndo = "changelog_bad_undo"

// undoEvent is one in-scope inbox event with what checkUndoRows needs.
type undoEvent struct {
	EventID    string
	Status     string
	Generation int32  // inbox.replay_count
	DoneAction *int64 // the done 'undo' admin_actions row (unique per event, 00006)
	ApplyRows  int    // apply rows of the current generation
	Rows       []undoRow
}

// undoRow is one source='undo' change_log row of the event, in seq order,
// with the apply row it should revert (seq n+1-k for undo seq k), if any.
type undoRow struct {
	ID         int64
	Seq        int32
	Generation int32
	ActionID   *int64
	Table      string
	RowID      int64
	Op         string
	Pair       *undoPair
}

// undoPair is the apply row an undo row is positioned against.
type undoPair struct {
	ID       int64
	Table    string
	RowID    int64
	Op       string
	Restored bool // the undo's after image equals this apply's before image, except row_version
}

// judgeUndo reports the first problem with one event's undo rows, in rule
// order; one violation per event, because after the first broken rule the
// later ones are suspect too. Rules:
//  1. presence: an undone event has undo rows (else changelog_missing); undo
//     rows exist only for an undone event with a done undo audit row (else
//     changelog_unexpected);
//  2. action link: every row's admin_action_id is that done audit row;
//  3. generation: every row's generation is the event's replay_count, i.e.
//     the generation of the apply it reverts (undone events cannot be
//     replayed, so replay_count is frozen at that generation);
//  4. pairing: as many undo rows as current-generation apply rows, seq 1..n,
//     and undo seq k is on the same table/row as apply seq n+1-k (reverse
//     order, as Undo reverts);
//  5. restore: the undo of an insert is a delete; the undo of an update is
//     an update whose after image equals the apply's before image except
//     row_version (the BEFORE UPDATE trigger bumps it; the chain check
//     verifies the bump).
//
// Rules 3 and 4 are checked in this order on purpose: pairing reads the
// apply rows of the current generation, so a wrong undo generation is named
// as such instead of surfacing as a confusing pairing failure.
func judgeUndo(ev undoEvent) *Violation {
	bad := func(kind, format string, args ...any) *Violation {
		return &Violation{kind, ev.EventID, fmt.Sprintf(format, args...)}
	}
	n := len(ev.Rows)
	switch {
	case n == 0 && ev.Status == "undone":
		return bad(KindChangeLogMissing, "undone but has no undo rows")
	case n == 0 && ev.DoneAction != nil:
		// The audit row makes Undo and Replay refuse this event forever, yet
		// the effect is still applied: nothing can ever fix it.
		return bad(KindChangeLogMissing, "done undo action #%d but no undo rows (status %s)", *ev.DoneAction, ev.Status)
	case n == 0:
		return nil
	case ev.Status != "undone":
		return bad(KindChangeLogUnexpected, "%d undo row(s) but the event is %s, not undone", n, ev.Status)
	case ev.DoneAction == nil:
		return bad(KindChangeLogUnexpected, "%d undo row(s) but no done undo action in admin_actions", n)
	}
	// Rule 1 guarantees DoneAction != nil here; the check keeps the checker
	// from panicking if the rule order ever changes (it inspects corrupt state).
	if ev.DoneAction == nil {
		return bad(KindChangeLogUnexpected, "%d undo row(s) but no done undo action in admin_actions", n)
	}
	for _, r := range ev.Rows {
		if r.ActionID == nil || *r.ActionID != *ev.DoneAction {
			return bad(KindChangeLogBadUndo, "undo row #%d: admin_action_id %s, want the event's done undo action #%d",
				r.ID, rvText(r.ActionID), *ev.DoneAction)
		}
	}
	for _, r := range ev.Rows {
		if r.Generation != ev.Generation {
			return bad(KindChangeLogBadUndo, "undo row #%d: generation %d, want %d (the reverted apply's, = replay_count)",
				r.ID, r.Generation, ev.Generation)
		}
	}
	if n != ev.ApplyRows {
		return bad(KindChangeLogBadUndo, "pairing: %d undo row(s) for %d apply row(s) in generation %d", n, ev.ApplyRows, ev.Generation)
	}
	for i, r := range ev.Rows {
		want := int32(i + 1)
		if r.Seq != want {
			return bad(KindChangeLogBadUndo, "pairing: undo row #%d has seq %d, want %d (1..n with no gaps)", r.ID, r.Seq, want)
		}
		if p := r.Pair; p == nil || p.Table != r.Table || p.RowID != r.RowID {
			return bad(KindChangeLogBadUndo, "pairing: undo seq %d (%s/%d) does not revert apply seq %d (%s) (reverse order)",
				r.Seq, r.Table, r.RowID, n-i, pairText(p))
		}
	}
	for _, r := range ev.Rows {
		p := r.Pair
		if p == nil { // unreachable: rule 4 returned on a nil pair; kept so a rule edit cannot panic
			continue
		}
		switch {
		case p.Op == changelog.OpInsert && r.Op != changelog.OpDelete:
			return bad(KindChangeLogBadUndo, "restore: undo row #%d is %s, but apply #%d inserted the row (want delete)", r.ID, r.Op, p.ID)
		case p.Op == changelog.OpUpdate && r.Op != changelog.OpUpdate:
			return bad(KindChangeLogBadUndo, "restore: undo row #%d is %s, but apply #%d updated the row (want update)", r.ID, r.Op, p.ID)
		case p.Op == changelog.OpUpdate && !r.Restored():
			return bad(KindChangeLogBadUndo, "restore: undo row #%d's after image differs from apply #%d's before image", r.ID, p.ID)
		case p.Op != changelog.OpInsert && p.Op != changelog.OpUpdate:
			return bad(KindChangeLogBadUndo, "restore: apply #%d is a %s, which undo never reverts", p.ID, p.Op)
		}
	}
	return nil
}

// Restored reports whether the row restored its pair's before image.
func (r undoRow) Restored() bool { return r.Pair != nil && r.Pair.Restored }

func pairText(p *undoPair) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprintf("#%d %s/%d", p.ID, p.Table, p.RowID)
}

// checkUndoRows applies judgeUndo to every in-scope event (phase 6 step 6b).
func checkUndoRows(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope) ([]Violation, error) {
	// One REPEATABLE READ snapshot for both reads: with two READ COMMITTED
	// statements an inbox row committed between them has undo rows in the
	// second read but no entry in the first.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin undo snapshot: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only, nothing to commit
	events, order, err := readUndoEvents(ctx, tx, sc)
	if err != nil {
		return nil, err
	}
	if err := readUndoRows(ctx, tx, sc, events); err != nil {
		return nil, err
	}
	var v []Violation
	for _, id := range order {
		if bad := judgeUndo(*events[id]); bad != nil {
			v = append(v, *bad)
		}
	}
	return v, nil
}

func readUndoEvents(ctx context.Context, tx pgx.Tx, sc changeLogScope) (map[int64]*undoEvent, []int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.id, i.event_id, i.status, i.replay_count,
		       (SELECT min(a.id) FROM admin_actions a
		        WHERE a.inbox_id = i.id AND a.action = 'undo' AND a.outcome = 'done'),
		       (SELECT count(*) FROM change_log cl
		        WHERE cl.inbox_id = i.id AND cl.source = 'apply' AND cl.generation = i.replay_count)
		FROM inbox i
		WHERE left(i.event_id, length($1)) = $1
		ORDER BY i.id`, sc.events)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	events := map[int64]*undoEvent{}
	var order []int64
	for rows.Next() {
		var id int64
		e := &undoEvent{}
		if err := rows.Scan(&id, &e.EventID, &e.Status, &e.Generation, &e.DoneAction, &e.ApplyRows); err != nil {
			return nil, nil, err
		}
		events[id] = e
		order = append(order, id)
	}
	return events, order, rows.Err()
}

// readUndoRows attaches each in-scope undo row, in seq order, to its event,
// with the apply row of the current generation at the mirrored position.
// The restore comparison is done in SQL as typed records (imagesEqualSQL),
// with row_version removed from both images.
func readUndoRows(ctx context.Context, tx pgx.Tx, sc changeLogScope, events map[int64]*undoEvent) error {
	rows, err := tx.Query(ctx, `
		WITH ev AS (
			SELECT id, replay_count FROM inbox WHERE left(event_id, length($1)) = $1
		),
		ap AS (
			SELECT cl.*, count(*) OVER (PARTITION BY cl.inbox_id) AS n
			FROM change_log cl JOIN ev ON ev.id = cl.inbox_id AND cl.generation = ev.replay_count
			WHERE cl.source = 'apply'
		)
		SELECT u.inbox_id, u.id, u.seq, u.generation, u.admin_action_id, u.table_name, u.row_id, u.op,
		       a.id, a.table_name, a.row_id, a.op,
		       COALESCE(`+imagesEqualSQL("u.table_name", "u.after_image - 'row_version'", "a.before_image - 'row_version'")+`, false)
		FROM change_log u
		JOIN ev ON ev.id = u.inbox_id
		LEFT JOIN ap a ON a.inbox_id = u.inbox_id AND a.seq = a.n + 1 - u.seq
		WHERE u.source = 'undo'
		ORDER BY u.inbox_id, u.seq, u.id`, sc.events)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var inboxID int64
		var r undoRow
		var pID, pRow *int64
		var pTable, pOp *string
		var restored bool
		if err := rows.Scan(&inboxID, &r.ID, &r.Seq, &r.Generation, &r.ActionID, &r.Table, &r.RowID, &r.Op,
			&pID, &pTable, &pRow, &pOp, &restored); err != nil {
			return err
		}
		if pID != nil {
			r.Pair = &undoPair{ID: *pID, Table: *pTable, RowID: *pRow, Op: *pOp, Restored: restored}
		}
		ev := events[inboxID]
		if ev == nil {
			return fmt.Errorf("undo row #%d: inbox %d not in the event read", r.ID, inboxID)
		}
		ev.Rows = append(ev.Rows, r)
	}
	return rows.Err()
}
