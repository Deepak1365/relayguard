package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/changelog"
)

// LoggedChange is one change_log row.
type LoggedChange struct {
	ID            int64
	Generation    int
	Source        string // apply or undo
	AdminActionID *int64
	Seq           int
	CreatedAt     time.Time
	Change        changelog.Change
}

// AdminAction is one admin_actions row.
type AdminAction struct {
	ID        int64
	InboxID   int64
	Action    string
	Actor     string
	Reason    string
	Outcome   string
	Detail    string // JSON text
	CreatedAt time.Time
}

// LogResult is everything recorded about one inbox event.
type LogResult struct {
	InboxID   int64
	EventID   string
	EventType string
	Status    string
	Attempts  int
	LastError string
	Changes   []LoggedChange // every generation and source, by id
	Actions   []AdminAction  // by id
}

// Log reads an event's inbox row, all its change_log rows and all admin
// actions on it. Read-only; it takes no locks. All three reads run in one
// REPEATABLE READ READ ONLY transaction, so they share one snapshot: without
// it a replay or worker commit between the reads could show a status that
// disagrees with the change and action rows beside it.
func Log(ctx context.Context, pool *pgxpool.Pool, inboxID int64) (LogResult, error) {
	return logEvent(ctx, pool, inboxID, Options{})
}

// logEvent is Log with the test hook (opts.beforeReread runs after the inbox
// read and before the other reads, the point where a concurrent commit would
// show up without the snapshot).
func logEvent(ctx context.Context, pool *pgxpool.Pool, inboxID int64, opts Options) (LogResult, error) {
	res := LogResult{InboxID: inboxID}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return res, fmt.Errorf("begin log snapshot: %w", err)
	}
	// A read-only transaction has nothing to commit; rollback just ends it.
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		SELECT event_id, event_type, status, attempts, COALESCE(last_error, '')
		FROM inbox WHERE id = $1`, inboxID).
		Scan(&res.EventID, &res.EventType, &res.Status, &res.Attempts, &res.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("%w: id %d", ErrNotFound, inboxID)
	}
	if err != nil {
		return res, fmt.Errorf("read inbox: %w", err)
	}
	if opts.beforeReread != nil {
		opts.beforeReread()
	}

	rows, err := tx.Query(ctx, `
		SELECT id, generation, source, admin_action_id, seq, created_at,
		       table_name, row_id, op, before_image::text, after_image::text
		FROM change_log WHERE inbox_id = $1 ORDER BY id`, inboxID)
	if err != nil {
		return res, fmt.Errorf("read change_log: %w", err)
	}
	for rows.Next() {
		var c LoggedChange
		var before, after *string
		if err := rows.Scan(&c.ID, &c.Generation, &c.Source, &c.AdminActionID, &c.Seq, &c.CreatedAt,
			&c.Change.Table, &c.Change.RowID, &c.Change.Op, &before, &after); err != nil {
			rows.Close()
			return res, fmt.Errorf("scan change_log: %w", err)
		}
		c.Change.Before, c.Change.After = textBytes(before), textBytes(after)
		res.Changes = append(res.Changes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("read change_log: %w", err)
	}

	rows, err = tx.Query(ctx, `
		SELECT id, inbox_id, action, actor, reason, outcome, detail::text, created_at
		FROM admin_actions WHERE inbox_id = $1 ORDER BY id`, inboxID)
	if err != nil {
		return res, fmt.Errorf("read admin_actions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.ID, &a.InboxID, &a.Action, &a.Actor, &a.Reason, &a.Outcome, &a.Detail, &a.CreatedAt); err != nil {
			return res, fmt.Errorf("scan admin_actions: %w", err)
		}
		res.Actions = append(res.Actions, a)
	}
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("read admin_actions: %w", err)
	}
	return res, nil
}
