package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotReplayable: the event's state does not allow a replay (only dead
	// and stale events can be replayed, and never one that was undone). The
	// refusal itself is audited.
	ErrNotReplayable = errors.New("event cannot be replayed")
	// ErrInvalidInput: actor or reason missing or too long (replay or undo).
	// Checked before the database is touched, so nothing is written or audited.
	ErrInvalidInput = errors.New("invalid admin request")
)

// Limits on the audit fields, in characters (runes), after trimming.
const (
	maxActorLen  = 200
	maxReasonLen = 1000
)

// Replay outcomes, as stored in admin_actions.outcome.
const (
	ReplayDone    = "done"
	ReplayRefused = "refused"
)

// ReplayRequest asks to replay one inbox event. Actor and Reason are
// required (user decision Q1: no defaults): every replay must say who did it
// and why.
type ReplayRequest struct {
	InboxID int64
	Actor   string
	Reason  string
}

// ReplayResult describes a replay that was carried out or refused. The From*
// fields are the inbox row as it was before (also stored in the audit
// detail).
type ReplayResult struct {
	InboxID         int64
	EventID         string
	EventType       string
	Outcome         string // ReplayDone or ReplayRefused
	Reason          string // why it was refused
	ActionID        int64  // the admin_actions row written for this request
	FromStatus      string
	FromAttempts    int
	FromLastError   string // "" if NULL
	FromProcessedAt *time.Time
	Generation      int // done: the new replay_count; refused: the unchanged one
}

// NormalizeReplayRequest trims actor and reason and checks they are present
// and within limits. Pure: the CLI uses it to reject bad input before it
// even loads settings, and Replay calls it before touching the database.
func NormalizeReplayRequest(r ReplayRequest) (ReplayRequest, error) {
	var err error
	r.Actor, r.Reason, err = normalizeAudit(r.InboxID, r.Actor, r.Reason)
	return r, err
}

// normalizeAudit is the validation shared by replay and undo: both write the
// same audit fields, so they must accept exactly the same input.
func normalizeAudit(inboxID int64, actor, reason string) (string, string, error) {
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	switch {
	case inboxID <= 0:
		return actor, reason, fmt.Errorf("%w: inbox id must be positive, got %d", ErrInvalidInput, inboxID)
	case actor == "":
		return actor, reason, fmt.Errorf("%w: actor is required", ErrInvalidInput)
	case reason == "":
		return actor, reason, fmt.Errorf("%w: reason is required", ErrInvalidInput)
	case utf8.RuneCountInString(actor) > maxActorLen:
		return actor, reason, fmt.Errorf("%w: actor is longer than %d characters", ErrInvalidInput, maxActorLen)
	case utf8.RuneCountInString(reason) > maxReasonLen:
		return actor, reason, fmt.Errorf("%w: reason is longer than %d characters", ErrInvalidInput, maxReasonLen)
	}
	return actor, reason, nil
}

// replayable is the allow-list of statuses a replay may start from (user
// decision Q2). dead: the worker gave up. stale: a newer event won, but an
// operator may know better (e.g. after fixing the ordering data). Not
// pending (the worker has it), done (applying it again would double its
// effect; undo is the tool for that), or skipped (unhandled type: a replay
// would only skip it again).
func replayable(status string) bool {
	return status == "dead" || status == "stale"
}

// Replay puts a dead or stale event back in the worker's queue as a new
// generation, and audits the request in admin_actions in the same
// transaction, whether it is carried out or refused.
//
// Locks: only the inbox row, FOR NO KEY UPDATE, taken by the first
// statement while the transaction holds nothing else. The audit insert's
// foreign-key check takes FOR KEY SHARE on that same row, which this
// transaction already holds more strongly, so it never waits. A transaction
// that waits at most once, and only before it holds anything, cannot be part
// of a deadlock cycle. The lock is exclusive against the worker's fence and
// claim (both FOR NO KEY UPDATE), so the status read here cannot change
// before the UPDATE; it does not conflict with a preview's KEY SHARE, whose
// final re-read catches a replay instead (replay_count changes).
//
// Errors: ErrInvalidInput (nothing touched), ErrNotFound (no audit: there
// is no row to attach it to), ErrBusy (lock_timeout; nothing written),
// ErrNotReplayable (refusal audited and committed). Any other error means
// the outcome was not reported (res.Outcome is ""); after a commit error the
// commit may still have succeeded, so check `admin log <id>`.
func Replay(ctx context.Context, pool *pgxpool.Pool, req ReplayRequest, opts Options) (ReplayResult, error) {
	req, err := NormalizeReplayRequest(req)
	if err != nil {
		return ReplayResult{InboxID: req.InboxID}, err
	}
	opts = opts.withDefaults()
	res := ReplayResult{InboxID: req.InboxID}

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

	var lastErr *string
	err = tx.QueryRow(ctx, `
		SELECT event_id, event_type, status, attempts, last_error, processed_at, replay_count
		FROM inbox WHERE id = $1
		FOR NO KEY UPDATE`, req.InboxID).
		Scan(&res.EventID, &res.EventType, &res.FromStatus, &res.FromAttempts, &lastErr, &res.FromProcessedAt, &res.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("%w: id %d", ErrNotFound, req.InboxID)
	}
	if err != nil {
		return res, classify("lock inbox", err)
	}
	if lastErr != nil {
		res.FromLastError = *lastErr
	}
	if opts.afterLock != nil {
		opts.afterLock()
	}

	// Undone guard (user decision Q5, ahead of step 6): an event whose
	// effect an admin reverted must not be re-applied by a replay, whatever
	// its status says. A refused undo changed nothing, so only 'done' counts.
	var undoneBy *int64
	if err := tx.QueryRow(ctx, `
		SELECT min(id) FROM admin_actions
		WHERE inbox_id = $1 AND action = 'undo' AND outcome = 'done'`, req.InboxID).Scan(&undoneBy); err != nil {
		return res, classify("check undo", err)
	}
	switch {
	case undoneBy != nil:
		res.Reason = fmt.Sprintf("the event was undone (admin action #%d); undone events are not replayable", *undoneBy)
	case !replayable(res.FromStatus):
		res.Reason = fmt.Sprintf("status %s is not replayable (only dead and stale)", res.FromStatus)
	}

	if res.Reason != "" {
		// Outcome is set only after the commit: until then nothing is audited,
		// and the CLI must not claim "refused, audited".
		if err := tx.QueryRow(ctx, `
			INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome, detail)
			VALUES ('replay', $1, $2, $3, 'refused',
			        jsonb_build_object('status', $4::text, 'attempts', $5::int, 'generation', $6::int, 'refused', $7::text))
			RETURNING id`,
			req.InboxID, req.Actor, req.Reason, res.FromStatus, res.FromAttempts, res.Generation, res.Reason).
			Scan(&res.ActionID); err != nil {
			return res, fmt.Errorf("audit refusal: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return res, fmt.Errorf("commit refusal audit: %w", err)
		}
		res.Outcome = ReplayRefused
		return res, fmt.Errorf("inbox %d: %w: %s", req.InboxID, ErrNotReplayable, res.Reason)
	}

	// One UPDATE resets everything a replay resets and opens the new
	// generation: replay_count+1 is what fences out any claim from an earlier
	// generation (see cmd/worker claimToken). next_attempt_at = now() makes it
	// claimable at once.
	if err := tx.QueryRow(ctx, `
		UPDATE inbox
		SET status = 'pending', attempts = 0, last_error = NULL, processed_at = NULL,
		    next_attempt_at = now(), replay_count = replay_count + 1
		WHERE id = $1
		RETURNING replay_count`, req.InboxID).Scan(&res.Generation); err != nil {
		return res, fmt.Errorf("reset inbox row: %w", err)
	}
	// The from_* values come from the locked SELECT above; the lock means
	// nothing changed them in between. (Postgres 17 cannot return old values
	// from UPDATE ... RETURNING.)
	if err := tx.QueryRow(ctx, `
		INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome, detail)
		VALUES ('replay', $1, $2, $3, 'done',
		        jsonb_build_object('from_status', $4::text, 'from_attempts', $5::int, 'from_last_error', $6::text,
		                           'from_processed_at', $7::timestamptz, 'generation', $8::int))
		RETURNING id`,
		req.InboxID, req.Actor, req.Reason, res.FromStatus, res.FromAttempts, lastErr, res.FromProcessedAt, res.Generation).
		Scan(&res.ActionID); err != nil {
		return res, fmt.Errorf("audit replay: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit replay: %w", err)
	}
	res.Outcome = ReplayDone
	return res, nil
}
