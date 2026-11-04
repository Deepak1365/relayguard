// chaos runs seeded experiments end to end. For each seed it:
//  1. empties the tables
//  2. generates a history and computes its correct outcome (the reference model)
//  3. delivers it, optionally with duplicates and reordering
//  4. waits for processing, then reports every way the database disagrees
//
// With -supervise it also runs the worker (or the naive receiver) itself,
// crashing it at fault points and hard-killing it at random moments.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/config"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/sim"
)

type runResult struct {
	seed       int64
	events     int
	deliveries int
	stats      sim.DeliveryStats
	inbox      map[string]int
	violations []sim.Violation
	elapsed    time.Duration
}

func main() {
	target := flag.String("target", "relayguard", "relayguard or naive (must match the receiver that is running)")
	seed := flag.Int64("seed", 1, "random seed: the same seed always produces the same history")
	seedsFlag := flag.String("seeds", "", "range of seeds, e.g. 1-20 (overrides -seed)")
	customers := flag.Int("customers", 20, "number of customers in each history")
	maxInvoices := flag.Int("max-invoices", 3, "max paid invoices per customer")
	dup := flag.Float64("dup", 0, "probability each event is delivered twice (0 to 1)")
	reorder := flag.Int("reorder", 0, "max positions an event can move from its true order (0 = in order)")
	providerRetries := flag.Int("provider-retries", 3, "how often a rejected delivery is resent, like a provider would")
	supervise := flag.String("supervise", "", "built worker (relayguard) or naive receiver (naive) for the harness to run, crash and restart")
	crashRate := flag.Float64("crash-rate", 0, "probability of crashing at each fault point (needs -supervise)")
	killInterval := flag.Duration("kill-interval", 0, "average time between hard kills, e.g. 2s (needs -supervise)")
	url := flag.String("url", "http://127.0.0.1:8080/webhooks/stripe", "receiver URL")
	health := flag.String("health", "http://127.0.0.1:8080/healthz", "receiver health URL")
	timeout := flag.Duration("timeout", 3*time.Minute, "max wait per seed for the worker to finish (relayguard)")
	out := flag.String("out", "", "also write a markdown results file, e.g. results/relayguard-crash.md")
	verbose := flag.Bool("v", false, "print violation examples for every failing seed")
	flag.Parse()

	if *target != "relayguard" && *target != "naive" {
		log.Fatalf("-target must be relayguard or naive, got %q", *target)
	}
	if (*crashRate > 0 || *killInterval > 0) && *supervise == "" {
		log.Fatal("-crash-rate and -kill-interval need -supervise")
	}
	seeds, err := parseSeeds(*seedsFlag, *seed)
	if err != nil {
		log.Fatal(err)
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

	var sup *sim.Supervisor
	if *supervise != "" {
		if _, err := os.Stat(*supervise); err != nil {
			log.Fatalf("cannot find %s: build it first with .\\dev.ps1 build", *supervise)
		}
		sup = &sim.Supervisor{
			Path: *supervise, LogPath: filepath.Join("bin", "supervised.log"),
			CrashRate: *crashRate, KillInterval: *killInterval, Seed: seeds[0],
		}
		if err := sup.Start(); err != nil {
			log.Fatalf("supervisor: %v", err)
		}
	}
	exit := func(code int) {
		if sup != nil {
			sup.Stop()
		}
		pool.Close()
		os.Exit(code)
	}

	sender := &sim.Sender{URL: *url, Secret: secret, HealthURL: *health, Client: &http.Client{Timeout: 10 * time.Second}}
	if !sender.WaitHealthy(15 * time.Second) {
		log.Printf("receiver not reachable at %s: is it running?", *health)
		exit(2)
	}

	cfg := sim.DefaultConfig()
	cfg.Customers = *customers
	cfg.MaxInvoices = *maxInvoices
	opts := sim.ChaosOptions{DuplicateRate: *dup, ReorderWindow: *reorder}

	fmt.Printf("target=%s seeds=%d-%d customers=%d dup=%.2f reorder=%d crash-rate=%.2f kill-interval=%s supervise=%q\n",
		*target, seeds[0], seeds[len(seeds)-1], *customers, *dup, *reorder, *crashRate, *killInterval, *supervise)

	var results []runResult
	for _, s := range seeds {
		r, err := runOne(ctx, pool, sender, *target, s, cfg, opts, *providerRetries, *timeout)
		if err != nil {
			log.Print(err)
			exit(2)
		}
		results = append(results, r)
		printRun(r, *verbose || len(seeds) == 1)
	}

	restarts, kills := 0, 0
	if sup != nil {
		restarts, kills = sup.Restarts(), sup.Kills()
	}
	failing, total := 0, 0
	for _, r := range results {
		total += len(r.violations)
		if len(r.violations) > 0 {
			failing++
		}
	}
	fmt.Println("---")
	fmt.Printf("RESULT target=%s seeds=%d failing_seeds=%d violations=%d restarts=%d hard_kills=%d\n",
		*target, len(results), failing, total, restarts, kills)

	if *out != "" {
		if err := writeMarkdown(*out, *target, results, failing, total, restarts, kills); err != nil {
			log.Printf("writing %s: %v", *out, err)
		} else {
			fmt.Printf("wrote %s\n", *out)
		}
	}
	if total > 0 {
		exit(1)
	}
	exit(0)
}

func runOne(ctx context.Context, pool *pgxpool.Pool, sender *sim.Sender, target string, seed int64,
	cfg sim.Config, opts sim.ChaosOptions, retries int, timeout time.Duration) (runResult, error) {

	events := sim.Generate(seed, cfg)
	exp := sim.Model(events)
	plan := sim.Plan(events, seed, opts)
	if err := sim.Reset(ctx, pool); err != nil {
		return runResult{}, fmt.Errorf("reset: %w", err)
	}

	start := time.Now()
	r := runResult{seed: seed, events: len(events), deliveries: len(plan)}
	r.stats = sim.Deliver(sender, plan, retries)

	var extra []sim.Violation
	if target == "relayguard" {
		counts, _, err := sim.WaitIdle(ctx, pool, timeout)
		if err != nil {
			return r, fmt.Errorf("waiting for worker: %w", err)
		}
		if sum(counts) == 0 {
			return r, fmt.Errorf("inbox is empty after delivery: is the RelayGuard receiver (not the naive one) running?")
		}
		r.inbox = counts
		if n := counts["pending"]; n > 0 {
			extra = append(extra, sim.Violation{Kind: "unprocessed_event", Subject: "inbox", Detail: fmt.Sprintf("%d events still pending (is a worker running?)", n)})
		}
		if n := counts["dead"]; n > 0 {
			extra = append(extra, sim.Violation{Kind: "unprocessed_event", Subject: "inbox", Detail: fmt.Sprintf("%d events dead-lettered", n)})
		}
	} else {
		counts, err := sim.InboxCounts(ctx, pool)
		if err != nil {
			return r, fmt.Errorf("inbox: %w", err)
		}
		if sum(counts) > 0 {
			return r, fmt.Errorf("inbox has rows: the RelayGuard receiver seems to be running; use the naive one for -target naive")
		}
	}

	v, _, err := sim.CheckState(ctx, pool, seed, exp)
	if err != nil {
		return r, fmt.Errorf("check: %w", err)
	}
	r.violations = append(v, extra...)
	r.elapsed = time.Since(start)
	return r, nil
}

func printRun(r runResult, examples bool) {
	inbox := ""
	if r.inbox != nil {
		inbox = " inbox[" + formatCounts(r.inbox) + "]"
	}
	fmt.Printf("seed=%-4d events=%-4d deliveries=%-4d provider_retries=%-3d gave_up=%-3d%s violations=%d (%s)\n",
		r.seed, r.events, r.deliveries, r.stats.ProviderRetries, r.stats.GaveUp, inbox,
		len(r.violations), r.elapsed.Round(time.Millisecond))
	if examples && len(r.violations) > 0 {
		fmt.Printf("    kinds: %s\n", kindsSummary(r.violations))
		for i, v := range r.violations {
			if i == 5 {
				fmt.Printf("    ... and %d more\n", len(r.violations)-5)
				break
			}
			fmt.Printf("    %-26s %-16s %s\n", v.Kind, v.Subject, v.Detail)
		}
	}
}

func writeMarkdown(path, target string, results []runResult, failing, total, restarts, kills int) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Chaos run: %s\n\n", target)
	fmt.Fprintf(&b, "- Date: %s\n", time.Now().Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "- Commit: `%s`\n", gitCommit())
	fmt.Fprintf(&b, "- Command: `chaos %s`\n", strings.Join(os.Args[1:], " "))
	fmt.Fprintf(&b, "- Data: synthetic seeded histories on a local machine (not production traffic)\n\n")
	fmt.Fprintf(&b, "**Seeds: %d, failing seeds: %d, violations: %d, process restarts: %d (of which hard kills: %d)**\n\n",
		len(results), failing, total, restarts, kills)
	b.WriteString("| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		inbox := "-"
		if r.inbox != nil {
			inbox = formatCounts(r.inbox)
		}
		kinds := "-"
		if len(r.violations) > 0 {
			kinds = kindsSummary(r.violations)
		}
		fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | %s | %d | %s |\n",
			r.seed, r.events, r.deliveries, r.stats.ProviderRetries, r.stats.GaveUp, inbox, len(r.violations), kinds)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func parseSeeds(spec string, single int64) ([]int64, error) {
	if spec == "" {
		return []int64{single}, nil
	}
	from, to, isRange := strings.Cut(spec, "-")
	a, err := strconv.ParseInt(strings.TrimSpace(from), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad -seeds %q", spec)
	}
	b := a
	if isRange {
		if b, err = strconv.ParseInt(strings.TrimSpace(to), 10, 64); err != nil || b < a {
			return nil, fmt.Errorf("bad -seeds %q (use e.g. 1-20)", spec)
		}
	}
	var seeds []int64
	for s := a; s <= b; s++ {
		seeds = append(seeds, s)
	}
	return seeds, nil
}

func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func kindsSummary(vs []sim.Violation) string {
	byKind := map[string]int{}
	for _, v := range vs {
		byKind[v.Kind]++
	}
	return formatCounts(byKind)
}

func sum(counts map[string]int) int {
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
