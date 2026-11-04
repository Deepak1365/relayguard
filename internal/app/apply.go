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
		// Still last-write-wins in processing order; ordering is fixed in phase 3.
		if _, err := tx.Exec(ctx, `
			INSERT INTO subscriptions (stripe_subscription_id, customer_id, status)
			VALUES ($1, $2, $3)
			ON CONFLICT (stripe_subscription_id)
			DO UPDATE SET status = EXCLUDED.status, updated_at = now()`,
			sub.ID, customerID, sub.Status); err != nil {
			return fmt.Errorf("upsert subscription: %w", err)
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
