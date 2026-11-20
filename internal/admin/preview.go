// Package admin holds operator tools on top of the inbox and change_log:
// preview (what would applying this event change?), log (what did it
// change?), replay (audited: run a dead or stale event again) and undo
// (audited: revert a done event's recorded changes).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/app"
	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/outbox"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

var (
	ErrNotFound = errors.New("inbox event not found")
	// ErrNotPreviewable: the event is done, so applying it again is not what
	// would happen; its real changes are in the log.
	ErrNotPreviewable = errors.New("event is done and cannot be previewed; use log <id> to see what it changed")
	// ErrBusy: a row the preview needs is locked by a worker (or another
	// admin) for longer than LockTimeout. Nothing was written; try again.
	ErrBusy = errors.New("rows are locked by another transaction; try again")
	// ErrChanged: the event was processed while the preview ran, so the
	// preview no longer describes what would happen.
	ErrChanged = errors.New("event changed during the preview; run it again")
	// ErrUndone: the event was undone; it will never be applied again, so
	// there is nothing to preview.
	ErrUndone = errors.New("event was undone and will not be applied again; use log <id> to see its changes and the undo")
)

// Options bounds how long a preview, replay or undo may wait or run. Zero values
// use the defaults. Short limits matter because both take row locks the
// worker also needs: they must never hold up the worker for long.
type Options struct {
	LockTimeout      time.Duration // default 2s
	StatementTimeout time.Duration // default 5s

	// beforeReread, if set, runs just before the final inbox re-read. Test
	// hook only (unexported, so only this package's tests can set it): it
	// lets a test commit a concurrent change at the one point the re-read
	// exists to catch.
	beforeReread func()
	// afterLock, if set, runs in Replay and Undo right after the inbox row is
	// locked and read. Test hook only, for the concurrency tests.
	afterLock func()
}

func (o Options) withDefaults() Options {
	if o.LockTimeout <= 0 {
		o.LockTimeout = 2 * time.Second
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = 5 * time.Second
	}
	return o
}

// Outcomes of a completed preview.
const (
	OutcomeApply = "apply" // the event would apply; Changes lists the rows
	OutcomeStale = "stale" // a newer event already applied; nothing changes
	OutcomeSkip  = "skip"  // unhandled event type; nothing changes
	OutcomeError = "error" // the apply would fail now (the worker retries)
)

// PreviewResult is what applying the event now would do.
type PreviewResult struct {
	InboxID    int64
	EventID    string
	EventType  string
	Status     string
	Attempts   int
	Generation int
	Outcome    string
	Reason     string             // stale / error explanation
	Changes    []changelog.Change // in seq order; ids of inserted rows are provisional
}

// Preview runs the real app.Apply for inbox event inboxID in a transaction
// that is ALWAYS rolled back, and reports the change_log rows it produced.
//
// Why the real Apply and not a simulation: a dry-run reimplementation could
// drift from what the worker does; this cannot, because it is the same code
// writing the same change_log rows, read back before the rollback. The cost
// is that a preview takes the same row locks as an apply for a few ms,
// bounded by Options. Inserted row ids are provisional: sequences are not
// rolled back, so the real apply will get different ids.
//
// This file must never call Commit (TestPreviewSourceHasNoCommit).
func Preview(ctx context.Context, pool *pgxpool.Pool, inboxID int64, opts Options) (PreviewResult, error) {
	opts = opts.withDefaults()
	res := PreviewResult{InboxID: inboxID}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // the only way this transaction ends

	// SET cannot take bind parameters; the values are integers we format.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", opts.LockTimeout.Milliseconds())); err != nil {
		return res, fmt.Errorf("set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", opts.StatementTimeout.Milliseconds())); err != nil {
		return res, fmt.Errorf("set statement_timeout: %w", err)
	}

	// Lock the inbox row FIRST, with FOR KEY SHARE (the same lock the
	// change_log foreign key takes later anyway). Why first: any transaction
	// holding FOR UPDATE on this row (the worker's fence before step 5a) would
	// otherwise be a deadlock risk. If the preview took its app-row locks
	// first (an uncommitted customer insert, a subscription pre-read) and only
	// then needed the inbox row for the change_log FK, it could wait on that
	// transaction while it waits on the preview, and Postgres might abort the
	// WORKER. Taking the inbox lock before any other means the preview waits
	// (bounded by lock_timeout, then ErrBusy) while holding nothing, so no
	// cycle can form (TestDBPreviewWaitsForFenceWithoutDeadlock). Since 5a
	// the fence is FOR NO KEY UPDATE, which KEY SHARE does not conflict with,
	// so the preview no longer waits for the worker here at all; the ordering
	// stays as a guard. KEY SHARE does not block the worker's status UPDATE
	// either, which is why the status is re-read below.
	var payload string
	err = tx.QueryRow(ctx, `
		SELECT event_id, event_type, status, attempts, replay_count, payload::text FROM inbox WHERE id = $1
		FOR KEY SHARE`, inboxID).
		Scan(&res.EventID, &res.EventType, &res.Status, &res.Attempts, &res.Generation, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("%w: id %d", ErrNotFound, inboxID)
	}
	if err != nil {
		return res, classify("read inbox", err)
	}
	switch res.Status {
	case "done":
		return res, fmt.Errorf("inbox %d: %w", inboxID, ErrNotPreviewable)
	case "undone":
		// Its effect was reverted and it is never applied again (the worker
		// only claims pending, replay refuses it), so "what would applying it
		// do" describes nothing that can happen.
		return res, fmt.Errorf("inbox %d: %w", inboxID, ErrUndone)
	}

	var evt stripeevent.Event
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		res.Outcome, res.Reason = OutcomeError, fmt.Sprintf("decode payload: %v", err)
		return res, nil
	}

	// res.Generation is the event's current replay_count: the generation the
	// worker would log this apply under. Recording under the same generation
	// also makes a concurrent real apply collide with the preview on
	// change_log_apply_once (ErrChanged) instead of passing unnoticed.
	// Savepoint: a failing statement aborts only the savepoint, so the outer
	// transaction can still re-read the inbox status below. (Same structure
	// as the worker's process.)
	sp, err := tx.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("savepoint: %w", err)
	}
	applyErr := app.Apply(ctx, sp, changelog.NewApply(inboxID, res.Generation),
		outbox.NewWriter(inboxID, res.Generation), evt, app.Hooks{})
	switch {
	case applyErr == nil:
		res.Outcome = OutcomeApply
		changes, err := readChanges(ctx, sp, inboxID, res.Generation)
		if err != nil {
			return res, classify("read back changes", err)
		}
		res.Changes = changes
	case errors.Is(applyErr, app.ErrStale):
		res.Outcome, res.Reason = OutcomeStale, applyErr.Error()
	case errors.Is(applyErr, app.ErrUnhandledType):
		res.Outcome, res.Reason = OutcomeSkip, applyErr.Error()
	case isBusinessError(applyErr):
		// A failure of the event itself that the worker would hit too: a
		// real outcome of the preview.
		res.Outcome, res.Reason = OutcomeError, applyErr.Error()
	default:
		// Anything else (locks, timeouts, database errors, a concurrent apply)
		// says nothing reliable about the event: the preview failed. But first
		// check whether a worker finished the event meanwhile, which since 5a
		// is the usual cause: the preview no longer queues behind the fence, so
		// it overlaps the apply, waits (well within lock_timeout) on a key the
		// worker inserted, and gets a unique violation (23505) once the worker
		// commits. That is "the event changed", not a failure
		// (TestDBPreviewWorkerCommitsDuringPreview).
		//
		// The failed statement aborted the savepoint; rolling it back makes
		// the transaction usable for the re-read. If either step fails (e.g.
		// the context was cancelled), report the original error unchanged,
		// never the re-read's.
		if err := sp.Rollback(ctx); err == nil {
			if moved, err := stateMoved(ctx, tx, inboxID, res); err == nil && moved != "" {
				return res, fmt.Errorf("apply: %w (%s): %w", ErrChanged, moved, applyErr)
			}
		}
		return res, classify("apply", applyErr)
	}
	// After a failed apply the savepoint may be aborted; roll it back so the
	// transaction can still run the status query. After a successful apply
	// it stays open on purpose: the deferred tx.Rollback is the ONE place
	// that discards the preview's writes, and TestDBPreviewWritesNothing
	// tests exactly that (a second, untested safety net would hide a
	// regression in the first).
	if res.Outcome != OutcomeApply {
		if err := sp.Rollback(ctx); err != nil {
			return res, fmt.Errorf("rollback savepoint: %w", err)
		}
	}

	// If a worker processed the event meanwhile, the preview describes a
	// state that no longer exists. Status alone is not enough: a replay that
	// fails again during the preview goes dead -> pending -> dead, the same
	// status in a newer generation (ABA). change_log_apply_once does not
	// catch that either, because the generations differ. replay_count only
	// grows, so comparing it closes the gap (TestDBPreviewDetectsReplayDuringPreview).
	if opts.beforeReread != nil {
		opts.beforeReread()
	}
	moved, err := stateMoved(ctx, tx, inboxID, res)
	if err != nil {
		return res, classify("re-read inbox", err)
	}
	if moved != "" {
		return res, fmt.Errorf("%w: %s", ErrChanged, moved)
	}
	return res, nil
}

// stateMoved re-reads the inbox row (READ COMMITTED: it sees commits made
// since the preview started) and describes how its status or replay_count
// differ from what the preview started from, or returns "" if neither did.
func stateMoved(ctx context.Context, tx pgx.Tx, inboxID int64, res PreviewResult) (string, error) {
	var status string
	var generation int
	if err := tx.QueryRow(ctx, `SELECT status, replay_count FROM inbox WHERE id = $1`, inboxID).
		Scan(&status, &generation); err != nil {
		return "", err
	}
	if status == res.Status && generation == res.Generation {
		return "", nil
	}
	return fmt.Sprintf("status %s -> %s, generation %d -> %d", res.Status, status, res.Generation, generation), nil
}

// readChanges reads this apply's change_log rows back, inside the still-open
// transaction (they are visible only there and vanish on rollback).
func readChanges(ctx context.Context, tx pgx.Tx, inboxID int64, generation int) ([]changelog.Change, error) {
	rows, err := tx.Query(ctx, `
		SELECT table_name, row_id, op, before_image::text, after_image::text
		FROM change_log
		WHERE inbox_id = $1 AND generation = $2 AND source = 'apply'
		ORDER BY seq`, inboxID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var changes []changelog.Change
	for rows.Next() {
		var c changelog.Change
		var before, after *string
		if err := rows.Scan(&c.Table, &c.RowID, &c.Op, &before, &after); err != nil {
			return nil, err
		}
		c.Before, c.After = textBytes(before), textBytes(after)
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

func textBytes(s *string) []byte {
	if s == nil {
		return nil
	}
	return []byte(*s)
}

// isBusinessError reports apply errors that are a property of the event and
// the current data, i.e. what the worker would also hit, so a preview can
// show them as OutcomeError (exit 0):
//   - app.ErrUnknownCustomer: the event refers to a customer not created yet;
//   - app.ErrConcurrentInsert: the subscription race the worker retries;
//   - JSON decode errors of the event object (malformed payload).
//
// An allow-list on purpose: anything unknown (lock and statement timeouts,
// deadlocks, constraint violations, connection errors, cancellation, a
// concurrent apply) means the preview could not answer, and must surface as
// a failed command rather than look like a normal "would fail" result.
func isBusinessError(err error) bool {
	if err == nil {
		return false
	}
	// A database or context error anywhere in the chain is never the
	// event's fault, whatever else the chain says.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, app.ErrUnknownCustomer) || errors.Is(err, app.ErrConcurrentInsert) {
		return true
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
}

// classify maps lock timeouts to ErrBusy and a concurrent apply of the same
// event (unique change_log_apply_once) to ErrChanged; anything else is a
// plain error.
func classify(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "55P03":
			return fmt.Errorf("%s: %w (%v)", what, ErrBusy, err)
		case pgErr.Code == "23505" && pgErr.ConstraintName == "change_log_apply_once":
			return fmt.Errorf("%s: %w (a worker applied this event meanwhile: %v)", what, ErrChanged, err)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
