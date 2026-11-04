// chaos runs one seeded experiment end to end:
//  1. empty the tables
//  2. generate a history and compute its correct outcome (the reference model)
//  3. deliver it, optionally with duplicates and reordering
//  4. wait for processing, then report every way the database disagrees
//
// Run it against whichever receiver is listening:
//
//	-target relayguard   receiver + worker running
//	-target naive        naive receiver running (no worker needed)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/sim"
)

func main() {
	target := flag.String("target", "relayguard", "relayguard or naive (must match the receiver that is running)")
	seed := flag.Int64("seed", 1, "random seed: the same seed always produces the same history")
	customers := flag.Int("customers", 20, "number of customers in the history")
	maxInvoices := flag.Int("max-invoices", 3, "max paid invoices per customer")
	dup := flag.Float64("dup", 0, "probability each event is delivered twice (0 to 1)")
	reorder := flag.Int("reorder", 0, "max positions an event can move from its true order (0 = in order)")
	providerRetries := flag.Int("provider-retries", 3, "how often a rejected delivery is resent, like a provider would")
	url := flag.String("url", "http://127.0.0.1:8080/webhooks/stripe", "receiver URL")
	timeout := flag.Duration("timeout", 3*time.Minute, "max wait for the worker to finish (relayguard)")
	flag.Parse()

	if *target != "relayguard" && *target != "naive" {
		log.Fatalf("-target must be relayguard or naive, got %q", *target)
	}
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("STRIPE_WEBHOOK_SECRET is not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	cfg := sim.DefaultConfig()
	cfg.Customers = *customers
	cfg.MaxInvoices = *maxInvoices
	events := sim.Generate(*seed, cfg)
	exp := sim.Model(events)
	plan := sim.Plan(events, *seed, sim.ChaosOptions{DuplicateRate: *dup, ReorderWindow: *reorder})

	if err := sim.Reset(ctx, pool); err != nil {
		log.Fatalf("reset: %v", err)
	}

	fmt.Printf("target=%s seed=%d customers=%d events=%d deliveries=%d dup=%.2f reorder=%d\n",
		*target, *seed, *customers, len(events), len(plan), *dup, *reorder)
	fmt.Printf("expected: %d customers, %d subscriptions, %d invoices, %d credits\n",
		len(exp.Customers), len(exp.Subscriptions), exp.Invoices, exp.TotalCredits)

	start := time.Now()
	sender := &sim.Sender{URL: *url, Secret: secret, Client: &http.Client{Timeout: 10 * time.Second}}
	stats := sim.Deliver(sender, plan, *providerRetries)
	fmt.Printf("delivery: posted=%d accepted=%d provider_retries=%d gave_up=%d\n",
		stats.Posted, stats.Accepted, stats.ProviderRetries, stats.GaveUp)

	var extra []sim.Violation
	if *target == "relayguard" {
		counts, timedOut, err := sim.WaitIdle(ctx, pool, *timeout)
		if err != nil {
			log.Fatalf("waiting for worker: %v", err)
		}
		if total(counts) == 0 {
			log.Fatal("inbox is empty after delivery: is the RelayGuard receiver (not the naive one) running?")
		}
		fmt.Printf("inbox: %s\n", formatCounts(counts))
		if timedOut {
			fmt.Println("WARNING: timed out with events still pending. Is the worker running?")
		}
		if n := counts["pending"]; n > 0 {
			extra = append(extra, sim.Violation{Kind: "unprocessed_event", Subject: "inbox", Detail: fmt.Sprintf("%d events still pending", n)})
		}
		if n := counts["dead"]; n > 0 {
			extra = append(extra, sim.Violation{Kind: "unprocessed_event", Subject: "inbox", Detail: fmt.Sprintf("%d events dead-lettered", n)})
		}
	} else {
		counts, err := sim.InboxCounts(ctx, pool)
		if err != nil {
			log.Fatalf("inbox: %v", err)
		}
		if total(counts) > 0 {
			log.Fatal("inbox has rows: the RelayGuard receiver seems to be running. Start the naive one for -target naive.")
		}
	}

	violations, foreign, err := sim.CheckState(ctx, pool, *seed, exp)
	if err != nil {
		log.Fatalf("check: %v", err)
	}
	violations = append(violations, extra...)
	if foreign > 0 {
		fmt.Printf("note: ignored %d rows not from this run (e.g. real Stripe test events)\n", foreign)
	}

	report(violations)
	fmt.Printf("RESULT target=%s seed=%d dup=%.2f reorder=%d violations=%d elapsed=%s\n",
		*target, *seed, *dup, *reorder, len(violations), time.Since(start).Round(time.Millisecond))
	if len(violations) > 0 {
		os.Exit(1)
	}
}

func report(violations []sim.Violation) {
	if len(violations) == 0 {
		fmt.Println("violations: none")
		return
	}
	byKind := map[string]int{}
	for _, v := range violations {
		byKind[v.Kind]++
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Println("violations by kind:")
	for _, k := range kinds {
		fmt.Printf("  %-26s %d\n", k, byKind[k])
	}
	fmt.Println("examples:")
	for i, v := range violations {
		if i == 10 {
			fmt.Printf("  ... and %d more\n", len(violations)-10)
			break
		}
		fmt.Printf("  %-26s %-16s %s\n", v.Kind, v.Subject, v.Detail)
	}
}

func total(counts map[string]int) int {
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}

func formatCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, counts[k])
	}
	return strings.Join(parts, " ")
}
