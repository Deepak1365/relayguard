// Smoke-test receiver: confirms Stripe CLI -> your machine -> Go works.
// It does NOT verify signatures or store events yet. That comes in phase 2.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/Deepak1365/relayguard/internal/config"
)

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}

	// 127.0.0.1 (not ":8080") keeps the receiver private to this laptop and
	// avoids a Windows Firewall prompt every time `go run` rebuilds it.
	addr := os.Getenv("RECEIVER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("POST /webhooks/stripe", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // cap at 1 MB
		if err != nil {
			http.Error(w, "could not read body", http.StatusBadRequest)
			return
		}
		var evt struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body, &evt); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		log.Printf("received %s (%s), signature header present: %t",
			evt.ID, evt.Type, r.Header.Get("Stripe-Signature") != "")
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("receiver listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
