// Package app holds the demo subscription app's business rules: what each
// webhook event changes in the database. RelayGuard's worker calls Apply
// inside a transaction, so all writes for one event commit together or not at all.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

var (
	ErrUnknownCustomer = errors.New("unknown customer")
	ErrUnhandledType   = errors.New("unhandled event type")
	// ErrStale means a newer event has already been applied to the same object.
	// The event is recorded but deliberately not applied.
	ErrStale = errors.New("stale event")
)

// Hooks lets experiments inject faults at precise points.
type Hooks struct {
	AfterGrant func(eventID string) // called between the two invoice writes
}

// Apply performs evt's effect using tx. The caller owns the transaction.
func Apply(ctx context.Context, tx pgx.Tx, evt stripeevent.Event, hooks Hooks) error {
	switch evt.Type {
	case "customer.created":
		var c stripeevent.Customer
		if err := json.Unmarshal(evt.Data.Object, &c); err != nil {
			return fmt.Errorf("decode customer: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO customers (stripe_customer_id, email) VALUES ($1, $2)`,
			c.ID, c.Email); err != nil {
			return fmt.Errorf("insert customer: %w", err)
		}
		return nil

	case "invoice.paid":
		var inv stripeevent.Invoice
		if err := json.Unmarshal(evt.Data.Object, &inv); err != nil {
			return fmt.Errorf("decode invoice: %w", err)
		}
		customerID, err := lookupCustomer(ctx, tx, inv.Customer)
		if err != nil {
			return err
		}
		credits := inv.AmountPaid / 100
		if _, err := tx.Exec(ctx,
			`INSERT INTO credit_grants (customer_id, stripe_invoice_id, amount) VALUES ($1, $2, $3)`,
			customerID, inv.ID, credits); err != nil {
			return fmt.Errorf("insert grant: %w", err)
		}
		if hooks.AfterGrant != nil {
			hooks.AfterGrant(evt.ID)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE customers SET credit_balance = credit_balance + $1 WHERE id = $2`,
			credits, customerID); err != nil {
			return fmt.Errorf("update balance: %w", err)
		}
		return nil

	case "customer.subscription.created",
		"customer.subscription.updated",
		"customer.subscription.deleted":
		var sub stripeevent.Subscription
		if err := json.Unmarshal(evt.Data.Object, &sub); err != nil {
			return fmt.Errorf("decode subscription: %w", err)
		}
		customerID, err := lookupCustomer(ctx, tx, sub.Customer)
		if err != nil {
			return err
		}
		// Apply only if this event is not older than the one the row already
		// reflects. ON CONFLICT ... WHERE false updates nothing, so a stale event
		// affects 0 rows. Postgres locks the conflicting row and re-checks the
		// WHERE against the latest committed version, so this is also safe with
		// several workers. Equal timestamps (same second) still apply: Stripe's
		// "created" cannot order them, which is a documented limitation.
		tag, err := tx.Exec(ctx, `
			INSERT INTO subscriptions
				(stripe_subscription_id, customer_id, status, last_event_created, last_event_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (stripe_subscription_id) DO UPDATE
			SET status             = EXCLUDED.status,
			    last_event_created = EXCLUDED.last_event_created,
			    last_event_id      = EXCLUDED.last_event_id,
			    updated_at         = now()
			WHERE subscriptions.last_event_created <= EXCLUDED.last_event_created`,
			sub.ID, customerID, sub.Status, evt.Created, evt.ID)
		if err != nil {
			return fmt.Errorf("upsert subscription: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var newerID string
			var newerCreated int64
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE(last_event_id, ''), last_event_created
				FROM subscriptions WHERE stripe_subscription_id = $1`,
				sub.ID).Scan(&newerID, &newerCreated); err != nil {
				return fmt.Errorf("read current subscription: %w", err)
			}
			return fmt.Errorf("%w: %s already reflects %s (created %d), this event created %d",
				ErrStale, sub.ID, newerID, newerCreated, evt.Created)
		}
		return nil

	default:
		return ErrUnhandledType
	}
}

func lookupCustomer(ctx context.Context, tx pgx.Tx, stripeID string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM customers WHERE stripe_customer_id = $1`, stripeID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w %s", ErrUnknownCustomer, stripeID)
	}
	if err != nil {
		return 0, fmt.Errorf("lookup customer: %w", err)
	}
	return id, nil
}
