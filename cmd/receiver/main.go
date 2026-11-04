// receiver is RelayGuard's intake. It does as little as possible:
// verify the signature, store the raw event in the inbox, return 200.
// No business logic runs here, so a slow or crashing handler can never make
// the provider time out and resend.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/stripesig"
)

const maxBody = 1 << 20 // 1 MB

type server struct {
	pool   *pgxpool.Pool
	secret string
}

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is not set")
	}
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" || secret == "whsec_replace_me" {
		log.Fatal("set STRIPE_WEBHOOK_SECRET in .env (it is printed by `stripe listen`)")
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

	s := &server{pool: pool, secret: secret}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /webhooks/stripe", s.handleWebhook)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("RelayGuard receiver listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	// Verify against the exact raw bytes, before parsing anything.
	if err := stripesig.Verify(body, r.Header.Get("Stripe-Signature"), s.secret,
		stripesig.DefaultTolerance, time.Now()); err != nil {
		log.Printf("REJECTED request: %v", err)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var evt struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &evt); err != nil || evt.ID == "" {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}

	tag, err := s.pool.Exec(r.Context(), `
		INSERT INTO inbox (provider, event_id, event_type, payload)
		VALUES ('stripe', $1, $2, $3::text::jsonb)
		ON CONFLICT (provider, event_id) DO NOTHING`,
		evt.ID, evt.Type, string(body))
	if err != nil {
		// Not stored, so ask the provider to retry.
		log.Printf("FAILED to store %s (%s): %v", evt.ID, evt.Type, err)
		http.Error(w, "storage failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		log.Printf("duplicate %s (%s), already in inbox", evt.ID, evt.Type)
	} else {
		log.Printf("stored %s (%s)", evt.ID, evt.Type)
	}
	w.WriteHeader(http.StatusOK)
}
