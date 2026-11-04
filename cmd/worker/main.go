// worker applies inbox events to the app. For each event it claims the inbox
// row, applies the effect, and marks the row done in ONE transaction:
// if it crashes anywhere in between, Postgres rolls everything back and the
// event is still pending, so it is applied exactly once in effect.
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
	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

const (
	maxAttempts  = 8
	maxBackoff   = time.Minute
	pollInterval = 500 * time.Millisecond
)

type worker struct {
	pool  *pgxpool.Pool
	hooks app.Hooks
}

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	pool, err := db.Connect(ctx, url)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	w := &worker{pool: pool}
	if os.Getenv("WORKER_CRASH_AFTER_GRANT") == "1" {
		log.Println("FAULT INJECTION ON: worker will crash after inserting a credit grant")
		w.hooks.AfterGrant = func(eventID string) {
			log.Printf("FAULT: crashing mid-transaction while applying %s", eventID)
			os.Exit(1)
		}
	}

	log.Println("RelayGuard worker started")
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

// processOne handles at most one ready event. It reports whether it found one.
func (w *worker) processOne(ctx context.Context) (bool, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after a successful commit

	// SKIP LOCKED: several workers can run at once without claiming the same row.
	var (
		id       int64
		payload  string
		attempts int
	)
	err = tx.QueryRow(ctx, `
		SELECT id, payload::text, attempts
		FROM inbox
		WHERE status = 'pending' AND next_attempt_at <= now()
		ORDER BY id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).Scan(&id, &payload, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}

	var evt stripeevent.Event
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		return true, finish(ctx, tx, id, "dead", fmt.Sprintf("decode payload: %v", err))
	}

	// Savepoint: if the effect fails, undo only the effect and keep the lock on
	// the inbox row, so the failure can be recorded in the same transaction.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("savepoint: %w", err)
	}
	applyErr := app.Apply(ctx, sp, evt, w.hooks)

	switch {
	case applyErr == nil:
		if err := sp.Commit(ctx); err != nil {
			return false, fmt.Errorf("release savepoint: %w", err)
		}
		log.Printf("applied %s (%s)", evt.ID, evt.Type)
		return true, finish(ctx, tx, id, "done", "")

	case errors.Is(applyErr, app.ErrUnhandledType):
		sp.Rollback(ctx)
		log.Printf("skipped %s (%s)", evt.ID, evt.Type)
		return true, finish(ctx, tx, id, "skipped", "")

	case errors.Is(applyErr, app.ErrStale):
		sp.Rollback(ctx) // nothing was written, but release the savepoint
		log.Printf("stale %s (%s): %v", evt.ID, evt.Type, applyErr)
		return true, finish(ctx, tx, id, "stale", applyErr.Error())

	default:
		if err := sp.Rollback(ctx); err != nil {
			return false, fmt.Errorf("rollback savepoint: %w", err)
		}
		attempts++
		if attempts >= maxAttempts {
			log.Printf("DEAD %s (%s) after %d attempts: %v", evt.ID, evt.Type, attempts, applyErr)
			return true, finish(ctx, tx, id, "dead", applyErr.Error())
		}
		delay := backoff(attempts)
		log.Printf("retry %s (%s) in %s, attempt %d/%d: %v",
			evt.ID, evt.Type, delay, attempts, maxAttempts, applyErr)
		if _, err := tx.Exec(ctx, `
			UPDATE inbox
			SET attempts = $2, last_error = $3,
			    next_attempt_at = now() + make_interval(secs => $4)
			WHERE id = $1`,
			id, attempts, applyErr.Error(), delay.Seconds()); err != nil {
			return false, fmt.Errorf("schedule retry: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit: %w", err)
		}
		return true, nil
	}
}

// finish records the final status and commits the whole transaction,
// including the event's effect.
func finish(ctx context.Context, tx pgx.Tx, id int64, status, lastErr string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE inbox
		SET status = $2, attempts = attempts + 1,
		    last_error = NULLIF($3, ''), processed_at = now()
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
	d := time.Second << (attempt - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}
