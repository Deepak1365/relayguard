// sendevent signs an event file with STRIPE_WEBHOOK_SECRET exactly the way
// Stripe does and POSTs it to the receiver, optionally several times.
// Usage: go run ./cmd/sendevent -file testdata/events/invoice_paid.json -n 2
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/internal/stripesig"
)

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}
	file := flag.String("file", "", "path to an event JSON file")
	n := flag.Int("n", 1, "how many times to send it")
	url := flag.String("url", "http://127.0.0.1:8080/webhooks/stripe", "receiver URL")
	flag.Parse()

	if *file == "" {
		log.Fatal("usage: sendevent -file <event.json> [-n 2]")
	}
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("STRIPE_WEBHOOK_SECRET is not set")
	}
	payload, err := os.ReadFile(*file)
	if err != nil {
		log.Fatalf("read %s: %v", *file, err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for i := 1; i <= *n; i++ {
		req, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(payload))
		if err != nil {
			log.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Stripe-Signature", stripesig.Sign(payload, secret, time.Now()))

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("attempt %d -> no response (%v)\n", i, err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		fmt.Printf("attempt %d -> HTTP %d\n", i, resp.StatusCode)
	}
}
