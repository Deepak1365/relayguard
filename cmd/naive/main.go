// naive is the BASELINE receiver. It is written the way many tutorials write
// webhook handlers, and its flaws are deliberate:
//
//   - processes each event inside the HTTP request
//   - no duplicate detection, so redelivered events apply twice
//   - multiple writes without a transaction, so a crash leaves partial state
//   - acknowledges events it cannot apply yet (unknown customer), so they are lost
//   - applies events in arrival order, so a late, older event overwrites newer state
//
// RelayGuard is measured against this program. Do not "fix" it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

var (
	errUnknownCustomer = errors.New("unknown customer")
	errUnhandledType   = errors.New("unhandled event type")
)

type server struct {
	pool            *pgxpool.Pool
	crashAfterGrant bool // fault injection: exit between the two invoice writes
}

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is not set")
	}
	addr := os.Getenv("RECEIVER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	s := &server{
		pool:            pool,
		crashAfterGrant: os.Getenv("NAIVE_CRASH_AFTER_GRANT") == "1",
	}
	if s.crashAfterGrant {
		log.Println("FAULT INJECTION ON: will crash after inserting a credit grant")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /webhooks/stripe", s.handleWebhook)

	log.Printf("NAIVE receiver listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	var evt stripeevent.Event
	if err := json.Unmarshal(body, &evt); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	err = s.apply(r.Context(), evt)
	switch {
	case errors.Is(err, errUnhandledType):
		log.Printf("skipped %s (%s)", evt.ID, evt.Type)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, errUnknownCustomer):
		// Common tutorial behaviour: log and acknowledge. The provider never
		// retries an acknowledged event, so this event is silently lost.
		log.Printf("IGNORED %s (%s): %v", evt.ID, evt.Type, err)
		w.WriteHeader(http.StatusOK)
	case err != nil:
		log.Printf("FAILED %s (%s): %v", evt.ID, evt.Type, err)
		http.Error(w, "processing failed", http.StatusInternalServerError)
	default:
		log.Printf("applied %s (%s)", evt.ID, evt.Type)
		w.WriteHeader(http.StatusOK)
	}
}

func (s *server) apply(ctx context.Context, evt stripeevent.Event) error {
	switch evt.Type {
	case "customer.created":
		var c stripeevent.Customer
		if err := json.Unmarshal(evt.Data.Object, &c); err != nil {
			return fmt.Errorf("decode customer: %w", err)
		}
		_, err := s.pool.Exec(ctx,
			`INSERT INTO customers (stripe_customer_id, email) VALUES ($1, $2)`,
			c.ID, c.Email)
		return err

	case "invoice.paid":
		var inv stripeevent.Invoice
		if err := json.Unmarshal(evt.Data.Object, &inv); err != nil {
			return fmt.Errorf("decode invoice: %w", err)
		}
		customerID, err := s.lookupCustomer(ctx, inv.Customer)
		if err != nil {
			return err
		}
		credits := inv.AmountPaid / 100

		// Two separate writes with no transaction (deliberate flaw).
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO credit_grants (customer_id, stripe_invoice_id, amount) VALUES ($1, $2, $3)`,
			customerID, inv.ID, credits); err != nil {
			return fmt.Errorf("insert grant: %w", err)
		}
		if s.crashAfterGrant {
			log.Printf("FAULT: crashing after grant for %s, before balance update", evt.ID)
			os.Exit(1)
		}
		if _, err := s.pool.Exec(ctx,
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
		customerID, err := s.lookupCustomer(ctx, sub.Customer)
		if err != nil {
			return err
		}
		// Last write wins, in arrival order (deliberate flaw).
		_, err = s.pool.Exec(ctx, `
			INSERT INTO subscriptions (stripe_subscription_id, customer_id, status)
			VALUES ($1, $2, $3)
			ON CONFLICT (stripe_subscription_id)
			DO UPDATE SET status = EXCLUDED.status, updated_at = now()`,
			sub.ID, customerID, sub.Status)
		return err

	default:
		return errUnhandledType
	}
}

func (s *server) lookupCustomer(ctx context.Context, stripeID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM customers WHERE stripe_customer_id = $1`, stripeID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w %s", errUnknownCustomer, stripeID)
	}
	return id, err
}
