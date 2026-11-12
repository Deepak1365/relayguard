// worker applies inbox events to the app in two steps:
//
//  1. CLAIM (its own short statement, committed immediately): increment
//     attempts and push next_attempt_at forward by a lease. Other workers skip
//     the event until the lease expires. Because the attempt is committed first,
//     an event that crashes the worker still uses up attempts and is
//     dead-lettered after maxAttempts, instead of looping forever and blocking
//     every event behind it.
//  2. PROCESS (one transaction): re-check the claim (fencing), apply the effect,
//     and mark the row done. A crash here rolls everything back; the event is
//     retried once its lease expires.
//
// Row locks on inbox are FOR NO KEY UPDATE, not FOR UPDATE: still exclusive
// between claims and fences, but compatible with the FOR KEY SHARE that
// foreign keys to inbox (change_log, admin_actions) take when an admin
// transaction inserts rows for the same event. FOR UPDATE would make the
// fence wait for such a transaction and the claim skip the row.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/app"
	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/fault"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

const (
	maxAttempts  = 8
	maxBackoff   = time.Minute
	pollInterval = 500 * time.Millisecond
	defaultLease = 10 * time.Second
)

type worker struct {
	pool   *pgxpool.Pool
	hooks  app.Hooks
	faults *fault.Injector
	lease  time.Duration
}

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is not set")
	}
	lease := defaultLease
	if v := os.Getenv("WORKER_LEASE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			log.Fatalf("invalid WORKER_LEASE %q (use e.g. 10s)", v)
		}
		lease = d
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	pool, err := db.Connect(ctx, url)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	// Fault points: "after_grant" (between the two invoice writes) and
	// "before_commit" (effect applied, inbox row not yet marked done).
	faults := fault.FromEnv()
	if os.Getenv("WORKER_CRASH_AFTER_GRANT") == "1" {
		faults = fault.Always("after_grant")
	}
	if faults.Enabled() {
		log.Printf("FAULT INJECTION ON: %s", faults)
	}
	if fault.PoisonEnabled() {
		log.Printf("FAULT INJECTION ON: %s events crash the worker", fault.PoisonType)
	}
	w := newWorker(pool, faults, lease)

	log.Printf("RelayGuard worker started (lease %s, max attempts %d)", lease, maxAttempts)
	for {
		processed, err := w.processOne(ctx)
		if ctx.Err() != nil {
			log.Println("worker stopping")
			return
		}
		if err != nil {
			log.Printf("worker error: %v", err)
		}
		if !processed || err != nil {
			select {
			case <-ctx.Done():
				log.Println("worker stopping")
				return
			case <-time.After(pollInterval):
			}
		}
	}
}

// newWorker wires the fault injector into the app hooks. Shared by main and
// the crash test's child process, so that test exercises the real wiring.
func newWorker(pool *pgxpool.Pool, faults *fault.Injector, lease time.Duration) *worker {
	return &worker{
		pool:   pool,
		faults: faults,
		lease:  lease,
		hooks: app.Hooks{AfterGrant: func(string) error {
			faults.Maybe("after_grant") // exits the process on a hit
			return nil
		}},
	}
}

// claimToken identifies one claim of an inbox row. Both parts are needed:
// attempts alone repeats after a replay (which resets attempts to 0), so a
// claim that stalled before the replay could otherwise pass the fence of a
// later claim with the same attempts value (ABA). replay_count only grows.
type claimToken struct {
	attempts   int
	generation int // inbox.replay_count at claim time; also change_log.generation
}

// processOne claims and processes at most one ready event.
func (w *worker) processOne(ctx context.Context) (bool, error) {
	id, tok, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return false, err
	}
	if tok.attempts > maxAttempts {
		// Every earlier attempt ended without a recorded result: most likely the
		// event crashes the worker (a poison event). Stop retrying it.
		_, err := w.deadLetterCrashed(ctx, id, tok)
		return true, err
	}
	return true, w.process(ctx, id, tok)
}

// deadLetterCrashed marks a row dead after a claim found its attempts above
// maxAttempts. It records maxAttempts, not claimedAttempts-1: only attempts
// 1..maxAttempts ever ran the event. Claims beyond that only lead here, and
// one can be lost (killed or stalled before this UPDATE), so claimedAttempts-1
// could overstate the real count.
// The (attempts, replay_count) guard is a fence like the one in process: if
// this worker stalled long enough for its lease to expire and another worker
// re-claimed the row (possibly after an admin replay reset attempts), the row
// no longer carries our token and we must not touch it.
// Reports whether the row was updated.
func (w *worker) deadLetterCrashed(ctx context.Context, id int64, tok claimToken) (bool, error) {
	tag, err := w.pool.Exec(ctx, `
		UPDATE inbox
		SET status = 'dead', attempts = $2, processed_at = now(),
		    last_error = $5::text || format('%s attempts; last error: %s', $2::int, COALESCE(last_error, $6::text))
		WHERE id = $1 AND status = 'pending' AND attempts = $3 AND replay_count = $4`,
		id, maxAttempts, tok.attempts, tok.generation, app.GaveUpPrefix, app.NoResultRecorded)
	if err != nil {
		return false, fmt.Errorf("dead-letter: %w", err)
	}
	if tag.RowsAffected() == 0 {
		log.Printf("lost claim on inbox row %d before dead-lettering (lease expired and it was re-claimed)", id)
		return false, nil
	}
	log.Printf("DEAD inbox row %d after %d attempts (possible poison event)", id, maxAttempts)
	return true, nil
}

// claim records an attempt and leases the oldest ready event, in one statement
// that commits immediately. SKIP LOCKED skips rows a fence (or another claim)
// holds. FOR NO KEY UPDATE, unlike FOR UPDATE, does not count a foreign-key
// KEY SHARE lock (an admin transaction writing change_log or admin_actions
// rows for the event) as "locked", so such a row is still claimed
// (TestDBKeyShareDoesNotBlockClaimOrFence).
func (w *worker) claim(ctx context.Context) (int64, claimToken, bool, error) {
	var (
		id  int64
		tok claimToken
	)
	err := w.pool.QueryRow(ctx, `
		WITH next AS (
			SELECT id FROM inbox
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY id
			LIMIT 1
			FOR NO KEY UPDATE SKIP LOCKED
		)
		UPDATE inbox AS i
		SET attempts = i.attempts + 1,
		    next_attempt_at = now() + make_interval(secs => $1)
		FROM next
		WHERE i.id = next.id
		RETURNING i.id, i.attempts, i.replay_count`, w.lease.Seconds()).Scan(&id, &tok.attempts, &tok.generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, claimToken{}, false, nil
	}
	if err != nil {
		return 0, claimToken{}, false, fmt.Errorf("claim: %w", err)
	}
	return id, tok, true, nil
}

func (w *worker) process(ctx context.Context, id int64, tok claimToken) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after a successful commit

	// Fencing: only proceed if our claim is still the latest one, i.e. the row
	// still carries our (attempts, replay_count) token. FOR NO KEY UPDATE
	// holds the row until commit, so claim (SKIP LOCKED) cannot re-claim it
	// mid-apply even if our lease expires (TestDBFenceLockBlocksReclaimMidApply),
	// while an admin transaction's foreign-key KEY SHARE on the row does not
	// make us wait (TestDBKeyShareDoesNotBlockClaimOrFence).
	var payload string
	err = tx.QueryRow(ctx, `
		SELECT payload::text FROM inbox
		WHERE id = $1 AND status = 'pending' AND attempts = $2 AND replay_count = $3
		FOR NO KEY UPDATE`, id, tok.attempts, tok.generation).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Printf("lost claim on inbox row %d (lease expired and it was re-claimed)", id)
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock claimed row: %w", err)
	}

	var evt stripeevent.Event
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		return finish(ctx, tx, id, "dead", app.DecodePayloadPrefix+err.Error())
	}
	if fault.PoisonEnabled() && evt.Type == fault.PoisonType {
		log.Printf("POISON: %s crashes the worker (attempt %d)", evt.ID, tok.attempts)
		os.Exit(1)
	}

	// Savepoint: if the effect fails, undo only the effect and keep the row
	// locked so the failure is recorded in the same transaction. The change
	// log rows are written inside the savepoint too, so they share the
	// effect's fate: a stale, skipped or failed apply leaves none behind.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	// The generation the fence just confirmed: a replayed event's changes are
	// filed under its new generation, so they neither collide with the unique
	// index nor mix with what an earlier generation logged.
	rec := changelog.NewApply(id, tok.generation)
	applyErr := app.Apply(ctx, sp, rec, evt, w.hooks)

	switch {
	case applyErr == nil:
		if err := sp.Commit(ctx); err != nil {
			return fmt.Errorf("release savepoint: %w", err)
		}
		w.faults.Maybe("before_commit")
		log.Printf("applied %s (%s), %d changes logged", evt.ID, evt.Type, rec.Len())
		return finish(ctx, tx, id, "done", "")

	case errors.Is(applyErr, app.ErrUnhandledType):
		sp.Rollback(ctx)
		log.Printf("skipped %s (%s)", evt.ID, evt.Type)
		return finish(ctx, tx, id, "skipped", "")

	case errors.Is(applyErr, app.ErrStale):
		sp.Rollback(ctx)
		log.Printf("stale %s (%s): %v", evt.ID, evt.Type, applyErr)
		return finish(ctx, tx, id, "stale", applyErr.Error())

	default:
		if err := sp.Rollback(ctx); err != nil {
			return fmt.Errorf("rollback savepoint: %w", err)
		}
		if tok.attempts >= maxAttempts {
			log.Printf("DEAD %s (%s) after %d attempts: %v", evt.ID, evt.Type, tok.attempts, applyErr)
			return finish(ctx, tx, id, "dead", applyErr.Error())
		}
		delay := backoff(tok.attempts)
		log.Printf("retry %s (%s) in %s, attempt %d/%d: %v",
			evt.ID, evt.Type, delay, tok.attempts, maxAttempts, applyErr)
		// Keyed on id only: safe because this tx holds the fence lock.
		if _, err := tx.Exec(ctx, `
			UPDATE inbox
			SET last_error = $2, next_attempt_at = now() + make_interval(secs => $3)
			WHERE id = $1`, id, applyErr.Error(), delay.Seconds()); err != nil {
			return fmt.Errorf("schedule retry: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}
}

// finish records the final status and commits the whole transaction,
// including the event's effect. Attempts were already counted by claim.
// Keyed on id only: safe because tx holds the fence lock (no other claim or
// replay can change the row's token before this commits).
func finish(ctx context.Context, tx pgx.Tx, id int64, status, lastErr string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE inbox
		SET status = $2, last_error = NULLIF($3, ''), processed_at = now()
		WHERE id = $1`, id, status, lastErr); err != nil {
		return fmt.Errorf("mark %s: %w", status, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// backoff doubles the wait after each failure: 1s, 2s, 4s ... capped at maxBackoff.
func backoff(attempt int) time.Duration {
	// A negative shift count panics; claims make attempts >= 1, but a bad
	// value must not crash the worker.
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second << (attempt - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}
