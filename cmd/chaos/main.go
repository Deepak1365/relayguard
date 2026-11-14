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
	"debug/buildinfo"
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

// workerMaxAttempts must match maxAttempts in cmd/worker: a dead-lettered
// poison event should record exactly this many attempts.
const workerMaxAttempts = 8

type runResult struct {
	seed        int64
	events      int
	deliveries  int
	stats       sim.DeliveryStats
	inbox       map[string]int
	violations  []sim.Violation
	elapsed     time.Duration
	undo        *sim.UndoReport // nil unless -undo-all
	undoViol    int             // violations found by the undo phase (included in violations)
	undoSkipped bool            // -undo-all set, but skipped: events still pending (undo_skipped)
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
	poison := flag.Int("poison", 0, "events per seed that crash the worker every time (relayguard, needs -supervise)")
	lease := flag.Duration("lease", time.Second, "WORKER_LEASE for the supervised worker (short keeps runs fast)")
	supervise := flag.String("supervise", "", "built worker (relayguard) or naive receiver (naive) for the harness to run, crash and restart")
	crashRate := flag.Float64("crash-rate", 0, "probability of crashing at each fault point (needs -supervise)")
	killInterval := flag.Duration("kill-interval", 0, "average time between hard kills, e.g. 2s (needs -supervise)")
	url := flag.String("url", "http://127.0.0.1:8080/webhooks/stripe", "receiver URL")
	health := flag.String("health", "http://127.0.0.1:8080/healthz", "receiver health URL")
	timeout := flag.Duration("timeout", 3*time.Minute, "max wait per seed for the worker to finish (relayguard)")
	out := flag.String("out", "", "also write a markdown results file, e.g. results/relayguard-crash.md")
	verbose := flag.Bool("v", false, "print violation examples for every failing seed")
	changeLogCheck := flag.Bool("changelog-check", true,
		"check change_log completeness and consistency (relayguard only); set false for workers built before phase 6, e.g. bin/worker-phase4.exe, which write no change_log")
	undoAll := flag.String("undo-all", "",
		"after each seed's forward checks, undo every done event with admin undo and check that nothing is left: newest or random (relayguard, needs -supervise)")
	flag.Parse()

	if *target != "relayguard" && *target != "naive" {
		log.Fatalf("-target must be relayguard or naive, got %q", *target)
	}
	if (*crashRate > 0 || *killInterval > 0 || *poison > 0) && *supervise == "" {
		log.Fatal("-crash-rate, -kill-interval and -poison need -supervise")
	}
	if *poison > 0 && *target != "relayguard" {
		log.Fatal("-poison only applies to -target relayguard")
	}
	if err := validateUndoAll(*undoAll, *target, *changeLogCheck, *supervise); err != nil {
		log.Fatal(err)
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
			ExtraEnv: []string{"WORKER_LEASE=" + lease.String()},
		}
		if *poison > 0 {
			sup.ExtraEnv = append(sup.ExtraEnv, "FAULT_POISON=1")
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
	opts := sim.ChaosOptions{DuplicateRate: *dup, ReorderWindow: *reorder, Poison: *poison}

	fmt.Printf("target=%s seeds=%d-%d customers=%d dup=%.2f reorder=%d poison=%d crash-rate=%.2f kill-interval=%s supervise=%q\n",
		*target, seeds[0], seeds[len(seeds)-1], *customers, *dup, *reorder, *poison, *crashRate, *killInterval, *supervise)
	fmt.Println(strings.TrimPrefix(changeLogCheckLine(*target, *changeLogCheck), "- "))
	if *undoAll != "" {
		fmt.Println(strings.TrimPrefix(undoAllLine(*undoAll), "- "))
	}

	var results []runResult
	for _, s := range seeds {
		r, err := runOne(ctx, pool, sender, *target, s, cfg, opts, *providerRetries, *timeout, *changeLogCheck, *undoAll)
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
	failing, total, undone, undoRefusals, undoSkipped := 0, 0, 0, 0, 0
	for _, r := range results {
		total += len(r.violations)
		if len(r.violations) > 0 {
			failing++
		}
		if r.undo != nil {
			undone += r.undo.Undone
			undoRefusals += r.undo.Refusals
		}
		if r.undoSkipped {
			undoSkipped++
		}
	}
	fmt.Println("---")
	// The undo fields appear only for -undo-all runs, so RESULT lines of
	// other runs keep the format earlier results files quote.
	undoResult := ""
	if *undoAll != "" {
		undoResult = undoResultFields(undone, undoRefusals, undoSkipped)
	}
	fmt.Printf("RESULT target=%s seeds=%d failing_seeds=%d violations=%d restarts=%d hard_kills=%d%s\n",
		*target, len(results), failing, total, restarts, kills, undoResult)

	if *out != "" {
		// -lease only reaches a worker the harness starts itself; with an
		// externally run worker, printing it would describe a setting that was
		// never applied.
		leaseLine := "- Worker lease: n/a (worker not supervised by the harness)"
		if *supervise != "" && *target == "relayguard" {
			leaseLine = fmt.Sprintf("- Worker lease: %s", *lease)
		}
		meta := []string{
			fmt.Sprintf("%s, poison events per seed: %d", leaseLine, *poison),
			changeLogCheckLine(*target, *changeLogCheck),
		}
		if *undoAll != "" {
			meta = append(meta, undoAllLine(*undoAll))
		}
		if *supervise != "" {
			meta = append(meta, "- Worker binary: "+binaryBuildInfo(*supervise))
		}
		if err := writeMarkdown(*out, *target, meta, results, failing, total, restarts, kills, *undoAll != ""); err != nil {
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
	cfg sim.Config, opts sim.ChaosOptions, retries int, timeout time.Duration, changeLogCheck bool, undoAll string) (runResult, error) {

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
		// Exactly the poison events must be dead-lettered; nothing else.
		dead, err := sim.DeadEvents(ctx, pool)
		if err != nil {
			return r, fmt.Errorf("dead events: %w", err)
		}
		poisonIDs := map[string]bool{}
		for k := 0; k < opts.Poison; k++ {
			poisonIDs[sim.PoisonID(seed, k)] = true
		}
		deadSet := map[string]bool{}
		for _, d := range dead {
			deadSet[d.ID] = true
			if !poisonIDs[d.ID] {
				extra = append(extra, sim.Violation{Kind: "unexpected_dead", Subject: d.ID, Detail: "dead-lettered but not a poison event"})
				continue
			}
			// A poison event must be given up on after exactly the worker's
			// maximum, with a reason, however many claims crashes and kills cost.
			if d.Attempts != workerMaxAttempts || d.LastError == "" {
				extra = append(extra, sim.Violation{Kind: "poison_wrong_attempts", Subject: d.ID,
					Detail: fmt.Sprintf("dead with attempts=%d (want %d), last_error empty=%v", d.Attempts, workerMaxAttempts, d.LastError == "")})
			}
		}
		// Loop over k, not the map, so violations come out in a stable order.
		for k := 0; k < opts.Poison; k++ {
			if id := sim.PoisonID(seed, k); !deadSet[id] {
				extra = append(extra, sim.Violation{Kind: "poison_not_dead", Subject: id, Detail: "poison event was not dead-lettered"})
			}
		}
		// The change log must be a complete, consistent record of this
		// seed's effects. Only RelayGuard writes one: the naive receiver has
		// no change_log by design, so it is not checked, and
		// -changelog-check=false skips it for pre-phase-6 workers (see
		// changeLogCheckLine).
		if changeLogCheck {
			clv, err := sim.CheckChangeLog(ctx, pool, seed)
			if err != nil {
				return r, fmt.Errorf("change log check: %w", err)
			}
			extra = append(extra, clv...)
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

	// Undo-all (experiment E4) runs after the forward checks, on the same
	// database, even if they found violations: its undo_* kinds keep the two
	// apart. It assumes an idle worker. WaitIdle does not guarantee that: on
	// timeout it returns with events still pending, which the supervised
	// worker may still be applying. Then the phase is skipped and the seed
	// fails with undo_skipped, rather than undoing a moving target.
	if skip, ok := undoAllSkip(undoAll, r.inbox); ok {
		r.undoSkipped = true
		r.undoViol = 1
		r.violations = append(r.violations, skip)
	} else if undoAll != "" {
		rep, uv, err := sim.UndoAll(ctx, pool, seed, undoAll)
		if err != nil {
			return r, fmt.Errorf("undo-all: %w", err)
		}
		r.undo, r.undoViol = &rep, len(uv)
		r.violations = append(r.violations, uv...)
	}
	r.elapsed = time.Since(start)
	return r, nil
}

// validateUndoAll rejects -undo-all where its result could not mean what it
// claims: the naive receiver writes no change_log to undo from; with the
// change_log check off, the run would say "check off" while the undo phase
// asserts "changelog 0"; and without -supervise the harness cannot vouch for
// which worker produced the state being undone (and it relies on the worker
// having drained the inbox, which WaitIdle only checks for relayguard).
func validateUndoAll(mode, target string, changeLogCheck bool, supervise string) error {
	switch {
	case mode == "":
		return nil
	case mode != sim.UndoAllNewest && mode != sim.UndoAllRandom:
		return fmt.Errorf("-undo-all must be %s or %s, got %q", sim.UndoAllNewest, sim.UndoAllRandom, mode)
	case target != "relayguard":
		return fmt.Errorf("-undo-all only applies to -target relayguard (the naive receiver writes no change_log)")
	case !changeLogCheck:
		return fmt.Errorf("-undo-all needs the change_log check: drop -changelog-check=false")
	case supervise == "":
		return fmt.Errorf("-undo-all needs -supervise")
	}
	return nil
}

// undoAllSkip says whether a seed's undo phase must be skipped: it needs the
// worker idle, and pending events after WaitIdle (a timeout) mean it may not
// be. The returned violation makes the seed fail, so a skipped phase can
// never read as a passed one.
func undoAllSkip(mode string, inbox map[string]int) (sim.Violation, bool) {
	if mode == "" || inbox["pending"] == 0 {
		return sim.Violation{}, false
	}
	return sim.Violation{Kind: sim.KindUndoSkipped, Subject: "inbox",
		Detail: fmt.Sprintf("undo-all %s not run: %d events still pending after the wait, so the worker may still be applying events", mode, inbox["pending"])}, true
}

// undoAllLine is the header line for -undo-all runs, saying what the undo
// phase asserts, so "0 violations" cannot be read as covering less, and what
// the refusal counts count.
func undoAllLine(mode string) string {
	return fmt.Sprintf("- Undo-all: %s (after forward checks; asserts empty tables, all undone, changelog 0, probes refused; "+
		"undo_refusals and Refusals count refusals in the undo loop only, not the probes' expected refusals)", mode)
}

// undoResultFields is the -undo-all suffix of the RESULT line. undo_skipped
// is there so a run whose undo phase never ran cannot read as a quiet success
// from undone alone (a skipped seed adds 0 to it).
func undoResultFields(undone, refusals, skipped int) string {
	return fmt.Sprintf(" undone=%d undo_refusals=%d undo_skipped=%d", undone, refusals, skipped)
}

// undoSkippedLine is the console line for a seed whose undo phase was skipped.
func undoSkippedLine() string {
	return "undo: skipped (events still pending; see undo_skipped) undone=0"
}

// undoLine is the console summary of one seed's undo phase.
func undoLine(r *sim.UndoReport, violations int) string {
	kinds := ""
	if len(r.RefusalKinds) > 0 {
		kinds = " " + formatCounts(r.RefusalKinds)
	}
	return fmt.Sprintf("undo: mode=%s undone=%d refusals=%d%s passes=%d probes=%d violations=%d",
		r.Mode, r.Undone, r.Refusals, kinds, r.Passes, r.Probes, violations)
}

// changeLogCheckLine says whether the change_log check ran, for the console
// and the results file, so a file with 0 violations cannot be read as
// covering a check it never made.
func changeLogCheckLine(target string, enabled bool) string {
	switch {
	case target != "relayguard":
		return "- Change log check: n/a (the naive receiver writes no change_log)"
	case !enabled:
		return "- Change log check: off (flag -changelog-check=false)"
	default:
		return "- Change log check: on (changelog_* violation kinds)"
	}
}

func printRun(r runResult, examples bool) {
	inbox := ""
	if r.inbox != nil {
		inbox = " inbox[" + formatCounts(r.inbox) + "]"
	}
	fmt.Printf("seed=%-4d events=%-4d deliveries=%-4d provider_retries=%-3d gave_up=%-3d%s violations=%d (%s)\n",
		r.seed, r.events, r.deliveries, r.stats.ProviderRetries, r.stats.GaveUp, inbox,
		len(r.violations), r.elapsed.Round(time.Millisecond))
	if r.undo != nil {
		fmt.Printf("    %s\n", undoLine(r.undo, r.undoViol))
	}
	if r.undoSkipped {
		fmt.Printf("    %s\n", undoSkippedLine())
	}
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

// writeMarkdown writes a results file. meta holds extra header lines
// (each starting with "- "), placed after the command line.
func writeMarkdown(path, target string, meta []string, results []runResult, failing, total, restarts, kills int, undo bool) error {
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
	for _, line := range meta {
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "- Data: synthetic seeded histories on a local machine (not production traffic)\n\n")
	fmt.Fprintf(&b, "**Seeds: %d, failing seeds: %d, violations: %d, process restarts: %d (of which hard kills: %d)**\n\n",
		len(results), failing, total, restarts, kills)
	b.WriteString(tableHeader(undo))
	for _, r := range results {
		b.WriteString(tableRow(r, undo))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// tableHeader is the results table header. The undo columns exist only for
// -undo-all runs, so files of other runs keep their established format.
func tableHeader(undo bool) string {
	if undo {
		return "| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Undone | Refusals | Passes | Violations | Kinds |\n" +
			"|---|---|---|---|---|---|---|---|---|---|---|\n"
	}
	return "| Seed | Events | Deliveries | Provider retries | Gave up | Inbox | Violations | Kinds |\n" +
		"|---|---|---|---|---|---|---|---|\n"
}

func tableRow(r runResult, undo bool) string {
	inbox := "-"
	if r.inbox != nil {
		inbox = formatCounts(r.inbox)
	}
	kinds := "-"
	if len(r.violations) > 0 {
		kinds = kindsSummary(r.violations)
	}
	undoCols := ""
	if undo {
		undoCols = "- | - | - | "
		if r.undoSkipped {
			undoCols = "skipped | - | - | "
		}
		if r.undo != nil {
			undoCols = fmt.Sprintf("%d | %d | %d | ", r.undo.Undone, r.undo.Refusals, r.undo.Passes)
		}
	}
	return fmt.Sprintf("| %d | %d | %d | %d | %d | %s | %s%d | %s |\n",
		r.seed, r.events, r.deliveries, r.stats.ProviderRetries, r.stats.GaveUp, inbox, undoCols, len(r.violations), kinds)
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
	commit := strings.TrimSpace(string(out))
	// A results file must not claim a commit it was not built from: uncommitted
	// changes to tracked files mean the binary may differ from that commit.
	// Untracked files (new results, scratch notes) do not count.
	status, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return commit + "-unknown-status"
	}
	if strings.TrimSpace(string(status)) != "" {
		commit += "-dirty"
	}
	return commit
}

// binaryBuildInfo reports the VCS stamp Go embedded in the supervised binary.
// The harness's own commit says nothing about a binary built earlier, perhaps
// from another commit or a dirty tree, so the binary must speak for itself.
func binaryBuildInfo(path string) string {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	rev, ok := settings["vcs.revision"]
	if !ok {
		return "unknown"
	}
	modified := settings["vcs.modified"]
	if modified == "" {
		modified = "unknown"
	}
	return fmt.Sprintf("vcs.revision=%s vcs.modified=%s", rev, modified)
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
