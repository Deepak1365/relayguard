// Package app holds the demo subscription app's business rules: what each
// webhook event changes in the database. RelayGuard's worker calls Apply
// inside a transaction, so all writes for one event commit together or not at all.
//
// Every row Apply writes is also recorded in change_log (internal/changelog)
// through the same transaction, right after the write. The images come from
// the database itself (to_jsonb of the row, read in the writing statement or
// under a row lock just before it), not rebuilt in Go, so they show exactly
// what was stored, including defaults and the trigger-maintained row_version.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

var (
	ErrUnknownCustomer = errors.New("unknown customer")
	ErrUnhandledType   = errors.New("unhandled event type")
	// ErrStale means a newer event has already been applied to the same object.
	// The event is recorded but deliberately not applied.
	ErrStale = errors.New("stale event")
	// ErrConcurrentInsert: another transaction inserted the subscription
	// between our locked pre-read (which found nothing) and our upsert (which
	// then hit the conflict). The before image we hold would be wrong, so the
	// apply fails and the worker retries; next time the pre-read finds the row.
	// Exported so admin.Preview can report it as an outcome of the event.
	ErrConcurrentInsert = errors.New("subscription was inserted concurrently after the pre-read; retrying")
)

// Hooks lets experiments inject faults at precise points.
type Hooks struct {
	// AfterGrant is called between the two invoice writes, after the grant
	// and its change_log row are written. A non-nil error fails the apply,
	// so tests can make an apply fail after a logged write.
	AfterGrant func(eventID string) error
}

// Apply performs evt's effect using tx and records each row it writes in rec,
// through the same tx. The caller owns the transaction and must use a fresh
// Recorder per apply. If Apply returns an error, the caller must roll tx
// back: some writes and their log rows may already be in it.
func Apply(ctx context.Context, tx pgx.Tx, rec *changelog.Recorder, evt stripeevent.Event, hooks Hooks) error {
	if rec == nil {
		// No silent "apply without logging" mode: a write missing from the
		// log would break the audit trail and later undo.
		return errors.New("apply: nil change recorder")
	}
	switch evt.Type {
	case "customer.created":
		var c stripeevent.Customer
		if err := json.Unmarshal(evt.Data.Object, &c); err != nil {
			return fmt.Errorf("decode customer: %w", err)
		}
		var id int64
		var after []byte
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers AS c (stripe_customer_id, email) VALUES ($1, $2)
			RETURNING c.id, to_jsonb(c.*)`,
			c.ID, c.Email).Scan(&id, &after); err != nil {
			return fmt.Errorf("insert customer: %w", err)
		}
		return rec.Record(ctx, tx, changelog.Change{
			Table: "customers", RowID: id, Op: changelog.OpInsert, After: after})

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
		var grantID int64
		var grant []byte
		if err := tx.QueryRow(ctx, `
			INSERT INTO credit_grants AS g (customer_id, stripe_invoice_id, amount) VALUES ($1, $2, $3)
			RETURNING g.id, to_jsonb(g.*)`,
			customerID, inv.ID, credits).Scan(&grantID, &grant); err != nil {
			return fmt.Errorf("insert grant: %w", err)
		}
		// Logged before the hook on purpose: the crash and error tests rely
		// on a log row already being in the transaction when AfterGrant fires.
		if err := rec.Record(ctx, tx, changelog.Change{
			Table: "credit_grants", RowID: grantID, Op: changelog.OpInsert, After: grant}); err != nil {
			return err
		}
		if hooks.AfterGrant != nil {
			if err := hooks.AfterGrant(evt.ID); err != nil {
				return fmt.Errorf("after grant: %w", err)
			}
		}
		// Logged even when credits is 0: the row_version trigger bumps the
		// row on every UPDATE, so an unlogged no-op would break the
		// "+1 per logged update" chain that undo relies on.
		return addCredits(ctx, tx, rec, customerID, credits)

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
		return upsertSubscription(ctx, tx, rec, evt, sub, customerID)

	default:
		return ErrUnhandledType
	}
}

// addCredits adds credits to a customer's balance and logs the update.
// The before image is read under a row lock so no other transaction can
// change the row between that read and our UPDATE: in READ COMMITTED an
// unlocked read could return a version older than the one we then modify.
//
// Why FOR NO KEY UPDATE and not FOR UPDATE: it is the lock the UPDATE itself
// takes (it changes no key column), and it conflicts with itself, so the
// before image stays exact. FOR UPDATE would also conflict with FOR KEY
// SHARE, which the credit_grants foreign key check took on this customer
// when we inserted the grant (held until commit). Two workers paying
// invoices for the same customer would each hold KEY SHARE and wait for the
// other's: a deadlock (40P01). TestDBCustomerLockAllowsConcurrentGrant
// covers this.
func addCredits(ctx context.Context, tx pgx.Tx, rec *changelog.Recorder, customerID, credits int64) error {
	var before, after []byte
	if err := tx.QueryRow(ctx,
		`SELECT to_jsonb(c.*) FROM customers c WHERE c.id = $1 FOR NO KEY UPDATE`,
		customerID).Scan(&before); err != nil {
		return fmt.Errorf("lock customer %d: %w", customerID, err)
	}
	if err := tx.QueryRow(ctx, `
		UPDATE customers AS c SET credit_balance = c.credit_balance + $1
		WHERE c.id = $2
		RETURNING to_jsonb(c.*)`,
		credits, customerID).Scan(&after); err != nil {
		return fmt.Errorf("update balance: %w", err)
	}
	return rec.Record(ctx, tx, changelog.Change{
		Table: "customers", RowID: customerID, Op: changelog.OpUpdate, Before: before, After: after})
}

// upsertSubscription inserts or (if not stale) updates the subscription and
// logs what it did.
func upsertSubscription(ctx context.Context, tx pgx.Tx, rec *changelog.Recorder,
	evt stripeevent.Event, sub stripeevent.Subscription, customerID int64) error {
	// Pre-read under a row lock: if the row exists, this is the exact before
	// image the upsert will replace, because nobody else can change it now.
	// FOR NO KEY UPDATE, like addCredits, for consistency: no table references
	// subscriptions today, so FOR UPDATE would not deadlock here, but the
	// weaker lock is all the image needs. (The upsert's ON CONFLICT DO UPDATE
	// then takes its own lock on the row, as it always did.)
	var before []byte
	err := tx.QueryRow(ctx, `
		SELECT to_jsonb(s.*) FROM subscriptions s
		WHERE s.stripe_subscription_id = $1 FOR NO KEY UPDATE`, sub.ID).Scan(&before)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock subscription: %w", err)
	}

	// Apply only if this event is not older than the one the row already
	// reflects. ON CONFLICT ... WHERE false updates nothing, so a stale event
	// returns no row (and fires no trigger, so nothing to log). Postgres locks
	// the conflicting row and re-checks the WHERE against the latest
	// committed version, so this is also safe with several workers. Equal
	// timestamps (same second) still apply: Stripe's "created" cannot order
	// them, which is a documented limitation.
	// (xmax = 0) is true only for a freshly inserted row version; an updated
	// one carries the updater's lock in xmax. It tells insert from update.
	var (
		id       int64
		inserted bool
		after    []byte
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO subscriptions
			(stripe_subscription_id, customer_id, status, last_event_created, last_event_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (stripe_subscription_id) DO UPDATE
		SET status             = EXCLUDED.status,
		    last_event_created = EXCLUDED.last_event_created,
		    last_event_id      = EXCLUDED.last_event_id,
		    updated_at         = now()
		WHERE subscriptions.last_event_created <= EXCLUDED.last_event_created
		RETURNING subscriptions.id, (subscriptions.xmax = 0), to_jsonb(subscriptions.*)`,
		sub.ID, customerID, sub.Status, evt.Created, evt.ID).Scan(&id, &inserted, &after)
	returned := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("upsert subscription: %w", err)
	}

	op, err := classifyUpsert(found, returned, inserted)
	if err != nil {
		return fmt.Errorf("subscription %s: %w", sub.ID, err)
	}
	switch op {
	case changelog.OpInsert:
		return rec.Record(ctx, tx, changelog.Change{
			Table: "subscriptions", RowID: id, Op: changelog.OpInsert, After: after})
	case changelog.OpUpdate:
		return rec.Record(ctx, tx, changelog.Change{
			Table: "subscriptions", RowID: id, Op: changelog.OpUpdate, Before: before, After: after})
	}

	// Stale: nothing was written.
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

// classifyUpsert decides what the subscription upsert did and therefore what
// to log. found: the locked pre-read saw the row. returned: the upsert
// returned a row (it wrote). inserted: that row was freshly inserted.
// It returns "" for a stale event (no write, nothing to log).
//
// Split out so the decision, including the rare race branch, is unit tested
// without having to stage a concurrent insert.
func classifyUpsert(found, returned, inserted bool) (string, error) {
	switch {
	case !returned:
		return "", nil
	case inserted && !found:
		return changelog.OpInsert, nil
	case !inserted && found:
		return changelog.OpUpdate, nil
	case !inserted && !found:
		// The upsert updated a row our pre-read did not see: we have no
		// before image for it. Logging the update without one is impossible
		// (CHECK) and inventing one would be wrong, so fail and retry.
		return "", ErrConcurrentInsert
	default: // inserted && found
		// The row we hold locked cannot vanish (nothing deletes
		// subscriptions), so an insert here means something is badly wrong.
		return "", errors.New("upsert inserted a row although the locked pre-read found one")
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
