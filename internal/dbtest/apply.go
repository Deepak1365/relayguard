package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/app"
	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

// InvoiceObject is a minimal Stripe invoice object for an invoice.paid event.
func InvoiceObject(id, customer string, amountPaid int64) map[string]any {
	return map[string]any{"id": id, "object": "invoice", "customer": customer, "amount_paid": amountPaid}
}

// SubscriptionObject is a minimal Stripe subscription object for the
// customer.subscription.* events.
func SubscriptionObject(id, customer, status string) map[string]any {
	return map[string]any{"id": id, "object": "subscription", "customer": customer, "status": status}
}

// ApplyInbox processes one inbox row the way cmd/worker does, minus claim
// and fence (these tests run single-threaded): app.Apply with a fresh
// recorder (generation = inbox.replay_count) inside a savepoint, then mark
// the row done, stale or skipped, and return that status. Any other apply
// error fails the test.
// Kept minimal on purpose; the worker's own DB tests cover claim, lease and
// retry, and use the real worker instead of this.
func ApplyInbox(t *testing.T, pool *pgxpool.Pool, inboxID int64) string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Same lock and generation as the worker's fence: a replayed event's
	// changes go under its current replay_count.
	var payload string
	var generation int
	if err := tx.QueryRow(ctx, `SELECT payload::text, replay_count FROM inbox WHERE id = $1 FOR NO KEY UPDATE`, inboxID).
		Scan(&payload, &generation); err != nil {
		t.Fatalf("read inbox %d: %v", inboxID, err)
	}
	var evt stripeevent.Event
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		t.Fatal(err)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := "done"
	switch err := app.Apply(ctx, sp, changelog.NewApply(inboxID, generation), evt, app.Hooks{}); {
	case err == nil:
		if err := sp.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	case errors.Is(err, app.ErrStale):
		status = "stale"
	case errors.Is(err, app.ErrUnhandledType):
		status = "skipped"
	default:
		t.Fatalf("apply %s: %v", evt.ID, err)
	}
	if status != "done" {
		if err := sp.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE inbox SET status = $2, attempts = 1, processed_at = now() WHERE id = $1`,
		inboxID, status); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return status
}
