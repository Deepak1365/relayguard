package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/admin"
)

// Undo-all modes (chaos -undo-all, phase 6 step 7a / experiment E4).
const (
	UndoAllNewest = "newest"
	UndoAllRandom = "random"
)

// Violation kinds reported by UndoAll. They all start with "undo_" so a
// results file separates them from the forward run's kinds. The change_log
// check after the undo phase reports its usual kinds with the same prefix
// (e.g. undo_changelog_chain_break), for the same reason.
const (
	// KindUndoStuck: a random-mode pass made no progress, so the remaining
	// events can never be undone in any order.
	KindUndoStuck = "undo_stuck"
	// KindUndoPassCap: the pass cap (candidates+1) was reached. Unreachable
	// while stuck detection works (each pass with progress removes at least
	// one event); separate from undo_stuck so losing stuck detection shows.
	KindUndoPassCap = "undo_pass_cap"
	// KindUndoUnexpectedConflict: a refusal named a conflict kind other than
	// later_event or referenced, i.e. something outside the event order
	// (an unlogged write, a missing row, a version mismatch).
	KindUndoUnexpectedConflict = "undo_unexpected_conflict"
	// KindUndoError: an undo that was neither done nor refused for a
	// conflict, but ended in one of admin's own sentinel errors
	// (ErrNotUndoable for a done event, ErrBusy, ErrNotFound, ...). Any other
	// error (context, database) aborts the phase instead: it says the
	// harness could not run, not that the system misbehaved.
	KindUndoError = "undo_error"
	// KindUndoRefusedNewestFirst: newest mode, where rule B guarantees every
	// undo succeeds, saw a refusal.
	KindUndoRefusedNewestFirst = "undo_refused_newest_first"
	// KindUndoProbe: after the loop, an undo or replay that must be refused
	// was not refused, or was refused from the wrong status.
	KindUndoProbe = "undo_probe"
	// KindUndoRowsLeft: an app table still has rows after undoing everything.
	KindUndoRowsLeft = "undo_rows_left"
	// KindUndoInboxCounts: inbox statuses are not what undoing every done
	// event implies (done 0, undone + done before, the rest unchanged).
	KindUndoInboxCounts = "undo_inbox_counts"
	// KindUndoAudit: admin_actions does not hold exactly the audit rows the
	// undo phase's own results imply.
	KindUndoAudit = "undo_audit"
	// KindUndoSkipped: cmd/chaos did not run the undo phase because events
	// were still pending after the wait, so the worker could still be
	// applying them while undos run (the phase assumes an idle worker).
	KindUndoSkipped = "undo_skipped"
)

// The harness identifies itself in every audit row it writes, so an
// operator reading admin_actions can tell a property run from a real admin.
const (
	undoAllActor       = "chaos-undo-all"
	undoAllProbeReason = "chaos -undo-all probe: this request must be refused"
)

// allowedRandomConflicts are the only conflict kinds random order may meet:
// both just mean "undo something newer first". Anything else means the data
// or the log is not what RelayGuard wrote.
var allowedRandomConflicts = map[string]bool{
	admin.ConflictLaterEvent: true,
	admin.ConflictReferenced: true,
}

// Attempt outcomes, as the pass loop sees one admin.Undo call.
const (
	attemptDone        = "done"
	attemptConflict    = "conflict"     // refused with ErrConflict (audited)
	attemptNotUndoable = "not_undoable" // refused with ErrNotUndoable (audited)
	attemptError       = "error"        // an admin sentinel error (e.g. ErrBusy, not audited) or outcome not reported
	attemptFatal       = "fatal"        // context or database error: abort the phase, not a violation
)

// isAdminSentinel says whether err is one of admin's own answers. Only those
// are judged as the system's behaviour (violation or not); anything else
// (a cancelled context, a lost connection) is the harness failing to run,
// which must abort the run rather than be counted as a violation.
func isAdminSentinel(err error) bool {
	for _, s := range []error{admin.ErrConflict, admin.ErrNotUndoable, admin.ErrBusy, admin.ErrNotFound, admin.ErrNotReplayable} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// classifyUndo turns one admin.Undo result into a loop outcome. Pure, so the
// context and unknown-error paths are unit tested.
func classifyUndo(ctx context.Context, res admin.UndoResult, err error) (string, []string, error) {
	// Checked first: once the context is gone every later call fails too,
	// and its error says "cancelled", not anything about the undo.
	if cerr := ctx.Err(); cerr != nil {
		return attemptFatal, nil, fmt.Errorf("undo inbox %d: %w", res.InboxID, cerr)
	}
	switch {
	case err == nil && res.Outcome == admin.UndoDone:
		return attemptDone, nil, nil
	case errors.Is(err, admin.ErrConflict) && res.Outcome == admin.UndoRefused:
		kinds := make([]string, len(res.Conflicts))
		expl := make([]string, len(res.Conflicts))
		for i, k := range res.Conflicts {
			kinds[i] = k.Kind
			expl[i] = fmt.Sprintf("%s %s/%d: %s", k.Kind, k.Table, k.RowID, k.Explanation)
		}
		return attemptConflict, kinds, fmt.Errorf("%w [%s]", err, strings.Join(expl, "; "))
	case errors.Is(err, admin.ErrNotUndoable) && res.Outcome == admin.UndoRefused:
		return attemptNotUndoable, nil, err
	case err == nil:
		return attemptError, nil, fmt.Errorf("undo returned outcome %q without an error", res.Outcome)
	case isAdminSentinel(err):
		return attemptError, nil, err
	default:
		return attemptFatal, nil, fmt.Errorf("undo inbox %d: %w", res.InboxID, err)
	}
}

// probeFatal says whether a probe call's error must abort the phase (the
// context is gone, or the error is not one of admin's answers) rather than
// be judged as a refusal or a violation.
func probeFatal(ctx context.Context, what string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("probe %s: %w", what, cerr)
	}
	if err != nil && !isAdminSentinel(err) {
		return fmt.Errorf("probe %s: %w", what, err)
	}
	return nil
}

// UndoReport summarises one seed's undo phase for the console and results.
type UndoReport struct {
	Mode         string
	Candidates   int            // done events of the seed before the phase
	Undone       int            // undone by this phase
	Refusals     int            // audited refusals in the loop (probes not included)
	RefusalKinds map[string]int // conflict kinds over all refusals (+ not_undoable)
	Passes       int
	Probes       int // undo and replay calls that must be refused
}

// undoCandidate is one done event the phase tries to undo.
type undoCandidate struct {
	InboxID int64
	EventID string
	Created int64 // Stripe "created"; NOT the order used (see sortNewest)
	ApplyID int64 // max change_log.id of its current-generation apply rows; 0 if none
}

// tryUndo is one undo attempt. kinds are the conflict kinds of a conflict
// refusal; err explains anything but done.
type tryUndo func(c undoCandidate) (outcome string, kinds []string, err error)

// loopResult is what runUndoLoop decided, without touching the database.
type loopResult struct {
	Undone     []undoCandidate
	Refusals   int
	Kinds      map[string]int
	Passes     int
	Violations []Violation
}

// sortNewest orders candidates by apply order, newest first: the max
// change_log.id of the current-generation apply. Not by Stripe "created":
// under -reorder an older event can be applied later, and rule B is about
// which change came later in the log, not in Stripe's clock. Max, not min:
// an event's own entries are all newer than those of any event that wrote
// the same row before it (row locks are held to commit), so both give the
// same relative order for every pair that conflicts; max states the intent.
func sortNewest(cands []undoCandidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].ApplyID != cands[j].ApplyID {
			return cands[i].ApplyID > cands[j].ApplyID
		}
		return cands[i].InboxID > cands[j].InboxID
	})
}

// shuffleRandom sorts by event id first, so the result depends only on the
// set of events and rng, not on the order the database returned them in.
func shuffleRandom(cands []undoCandidate, rng *rand.Rand) {
	sort.Slice(cands, func(i, j int) bool { return cands[i].EventID < cands[j].EventID })
	rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
}

// runUndoLoop tries every candidate, in order, with try. Pure, so the pass
// logic is unit tested without a database.
//
// newest: one pass; rule B guarantees each undo succeeds in this order, so
// any refusal is a violation (undo_refused_newest_first).
//
// random: passes over the remaining set until it is empty. A conflict keeps
// the event for the next pass; a done removes it. Progress argument: the
// remaining event with the largest apply id is always undoable (every later
// change to its rows came from an event with a larger apply id, which is
// already undone), so each pass removes at least one event while the data
// is what RelayGuard wrote. A pass without progress is therefore a violation
// (undo_stuck), and the loop stops. maxPasses is a safety net only.
//
// Both modes: attemptFatal stops the loop at once and returns the error; the
// partial result is returned too, but is not a verdict.
func runUndoLoop(mode string, order []undoCandidate, try tryUndo, maxPasses int) (loopResult, error) {
	res := loopResult{Kinds: map[string]int{}}
	if mode == UndoAllNewest {
		if len(order) > 0 {
			res.Passes = 1
		}
		for _, c := range order {
			outcome, kinds, err := try(c)
			switch outcome {
			case attemptFatal:
				return res, fmt.Errorf("%s: %w", c.EventID, err)
			case attemptDone:
				res.Undone = append(res.Undone, c)
			case attemptConflict, attemptNotUndoable:
				res.countRefusal(outcome, kinds)
				res.Violations = append(res.Violations, Violation{KindUndoRefusedNewestFirst, c.EventID,
					fmt.Sprintf("refused in newest-first order (apply id %d): %v", c.ApplyID, err)})
			default:
				res.Violations = append(res.Violations, Violation{KindUndoError, c.EventID, fmt.Sprint(err)})
			}
		}
		return res, nil
	}

	remaining := order
	reported := map[string]bool{} // event+kind already reported as unexpected
	for len(remaining) > 0 {
		if res.Passes >= maxPasses {
			res.Violations = append(res.Violations, Violation{KindUndoPassCap, "undo-all",
				fmt.Sprintf("%d pass(es) done, %d event(s) left: %s", res.Passes, len(remaining), candidateIDs(remaining))})
			break
		}
		res.Passes++
		var keep []undoCandidate
		progress := false
		for _, c := range remaining {
			outcome, kinds, err := try(c)
			switch outcome {
			case attemptFatal:
				return res, fmt.Errorf("%s (pass %d): %w", c.EventID, res.Passes, err)
			case attemptDone:
				res.Undone = append(res.Undone, c)
				progress = true
			case attemptConflict:
				res.countRefusal(outcome, kinds)
				for _, k := range kinds {
					if !allowedRandomConflicts[k] && !reported[c.EventID+" "+k] {
						reported[c.EventID+" "+k] = true
						res.Violations = append(res.Violations, Violation{KindUndoUnexpectedConflict, c.EventID,
							fmt.Sprintf("conflict kind %s (pass %d): %v", k, res.Passes, err)})
					}
				}
				keep = append(keep, c)
			case attemptNotUndoable:
				// Audited, so counted; but a done event must be undoable, and
				// retrying would only repeat the refusal.
				res.countRefusal(outcome, kinds)
				res.Violations = append(res.Violations, Violation{KindUndoError, c.EventID, fmt.Sprint(err)})
			default:
				res.Violations = append(res.Violations, Violation{KindUndoError, c.EventID, fmt.Sprint(err)})
			}
		}
		remaining = keep
		if !progress && len(remaining) > 0 {
			res.Violations = append(res.Violations, Violation{KindUndoStuck, "undo-all",
				fmt.Sprintf("pass %d undid nothing; %d event(s) left: %s", res.Passes, len(remaining), candidateIDs(remaining))})
			break
		}
	}
	return res, nil
}

func (r *loopResult) countRefusal(outcome string, kinds []string) {
	r.Refusals++
	if outcome == attemptNotUndoable {
		r.Kinds[attemptNotUndoable]++
		return
	}
	for _, k := range kinds {
		r.Kinds[k]++
	}
}

func candidateIDs(cs []undoCandidate) string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.EventID
	}
	return strings.Join(ids, " ")
}

// undoAllHooks are test seams for the negative controls; nil in production.
type undoAllHooks struct {
	order     func([]undoCandidate) // runs after the mode's ordering
	afterLoop func()                // runs after the loop, before the probes
}

// UndoAll undoes every done event of seed's history with the real
// admin.Undo, then checks that the result is exactly "nothing ever
// happened" (phase 6 step 7a, experiment E4). It runs after the seed's
// forward checks, against the same database, with the worker idle; it does
// not deliver anything. Violations use undo_* kinds; every answer admin gives
// (done, or one of its sentinel errors) is a violation or not. The error is
// for the harness failing to run: a cancelled context, or a database error
// in the phase's own reads or inside an undo/replay call (see
// isAdminSentinel). Then the report and violations are partial, not a verdict.
func UndoAll(ctx context.Context, pool *pgxpool.Pool, seed int64, mode string) (UndoReport, []Violation, error) {
	return undoAll(ctx, pool, seedScope(seed), mode, rand.New(rand.NewSource(seed)), undoAllHooks{})
}

func undoAll(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope, mode string, rng *rand.Rand, hooks undoAllHooks) (UndoReport, []Violation, error) {
	if mode != UndoAllNewest && mode != UndoAllRandom {
		return UndoReport{}, nil, fmt.Errorf("undo-all mode %q: want %s or %s", mode, UndoAllNewest, UndoAllRandom)
	}
	pre, err := readUndoPhase(ctx, pool, sc)
	if err != nil {
		return UndoReport{}, nil, fmt.Errorf("undo-all: read before: %w", err)
	}
	cands := append([]undoCandidate(nil), pre.done...)
	if mode == UndoAllNewest {
		sortNewest(cands)
	} else {
		shuffleRandom(cands, rng)
	}
	if hooks.order != nil {
		hooks.order(cands)
	}

	reason := fmt.Sprintf("chaos -undo-all property check (mode %s)", mode)
	try := func(c undoCandidate) (string, []string, error) {
		res, err := admin.Undo(ctx, pool, admin.UndoRequest{InboxID: c.InboxID, Actor: undoAllActor, Reason: reason}, admin.Options{})
		return classifyUndo(ctx, res, err)
	}
	loop, err := runUndoLoop(mode, cands, try, len(cands)+1)
	rep := UndoReport{Mode: mode, Candidates: len(pre.done), Undone: len(loop.Undone),
		Refusals: loop.Refusals, RefusalKinds: loop.Kinds, Passes: loop.Passes}
	v := loop.Violations
	if err != nil {
		return rep, v, fmt.Errorf("undo-all: loop: %w", err)
	}
	if hooks.afterLoop != nil {
		hooks.afterLoop()
	}

	pr, err := runProbes(ctx, pool, pre, loop.Undone)
	rep.Probes = pr.probes
	v = append(v, pr.violations...)
	if err != nil {
		return rep, v, fmt.Errorf("undo-all: %w", err)
	}

	post, err := checkAfterUndo(ctx, pool, sc, pre, loop, pr)
	if err != nil {
		return rep, v, fmt.Errorf("undo-all: check after: %w", err)
	}
	return rep, append(v, post...), nil
}

// undoPhaseState is the scope's inbox and audit state before the loop,
// read in one snapshot.
type undoPhaseState struct {
	done    []undoCandidate
	undone  []undoCandidate // already undone before the phase (not in chaos runs; possible in tests)
	notDone []probeTarget   // stale, skipped, dead: must stay not undoable
	inbox   map[string]int  // status -> count
	audit   map[string]int  // "action outcome" -> count
}

type probeTarget struct {
	InboxID int64
	EventID string
	Status  string
}

func readUndoPhase(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope) (undoPhaseState, error) {
	// One REPEATABLE READ snapshot: the candidates, the counts they are
	// compared against later, and the audit baseline must describe the same
	// moment.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return undoPhaseState{}, fmt.Errorf("begin snapshot: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only, nothing to commit
	st := undoPhaseState{}
	rows, err := tx.Query(ctx, `
		SELECT i.id, i.event_id, i.status, COALESCE((i.payload->>'created')::bigint, 0),
		       COALESCE((SELECT max(cl.id) FROM change_log cl
		                 WHERE cl.inbox_id = i.id AND cl.source = 'apply' AND cl.generation = i.replay_count), 0)
		FROM inbox i
		WHERE left(i.event_id, length($1)) = $1
		ORDER BY i.id`, sc.events)
	if err != nil {
		return st, fmt.Errorf("read inbox: %w", err)
	}
	for rows.Next() {
		var c undoCandidate
		var status string
		if err := rows.Scan(&c.InboxID, &c.EventID, &status, &c.Created, &c.ApplyID); err != nil {
			rows.Close()
			return st, fmt.Errorf("scan inbox: %w", err)
		}
		switch status {
		case "done":
			st.done = append(st.done, c)
		case "undone":
			st.undone = append(st.undone, c)
		case "stale", "skipped", "dead":
			st.notDone = append(st.notDone, probeTarget{c.InboxID, c.EventID, status})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("read inbox: %w", err)
	}
	if st.inbox, err = scopeInboxCounts(ctx, tx, sc); err != nil {
		return st, err
	}
	if st.audit, err = scopeAuditCounts(ctx, tx, sc); err != nil {
		return st, err
	}
	return st, nil
}

// querier is what both a pool and a transaction offer.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func scopeInboxCounts(ctx context.Context, q querier, sc changeLogScope) (map[string]int, error) {
	return countBy(ctx, q, `
		SELECT status, count(*) FROM inbox
		WHERE left(event_id, length($1)) = $1 GROUP BY status`, sc.events)
}

func scopeAuditCounts(ctx context.Context, q querier, sc changeLogScope) (map[string]int, error) {
	return countBy(ctx, q, `
		SELECT a.action || ' ' || a.outcome, count(*)
		FROM admin_actions a JOIN inbox i ON i.id = a.inbox_id
		WHERE left(i.event_id, length($1)) = $1 GROUP BY 1`, sc.events)
}

func countBy(ctx context.Context, q querier, sql string, args ...any) (map[string]int, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = int(n)
	}
	return out, rows.Err()
}

// probeResult counts the probes and the audited refusals they produced, so
// the audit check can compare admin_actions with what really happened.
type probeResult struct {
	probes        int
	undoRefused   int // audited undo refusals from probes
	undoDone      int // probes that wrongly undid something
	replayRefused int
	replayDone    int
	violations    []Violation
}

// runProbes asks for what must be refused after the loop: a second undo and
// a replay of every undone event (from status undone: refused for the right
// reason, not just refused), and an undo of every stale, skipped and dead
// event (they changed nothing). No replay probe for those: dead and stale
// are replayable by design, and a replay would make the worker apply them.
// A context or database error (probeFatal) stops the probes with an error.
func runProbes(ctx context.Context, pool *pgxpool.Pool, pre undoPhaseState, loopUndone []undoCandidate) (probeResult, error) {
	var pr probeResult
	undone := append(append([]undoCandidate(nil), pre.undone...), loopUndone...)
	sort.Slice(undone, func(i, j int) bool { return undone[i].InboxID < undone[j].InboxID })
	probeUndo := func(id int64, eventID, wantStatus string) error {
		pr.probes++
		res, err := admin.Undo(ctx, pool, admin.UndoRequest{InboxID: id, Actor: undoAllActor, Reason: undoAllProbeReason}, admin.Options{})
		if ferr := probeFatal(ctx, "undo "+eventID, err); ferr != nil {
			return ferr
		}
		switch res.Outcome {
		case admin.UndoRefused:
			pr.undoRefused++
		case admin.UndoDone:
			pr.undoDone++
		}
		if !errors.Is(err, admin.ErrNotUndoable) || res.Status != wantStatus {
			pr.violations = append(pr.violations, Violation{KindUndoProbe, eventID,
				fmt.Sprintf("undo: outcome %q from status %q, err %v; want ErrNotUndoable from status %s", res.Outcome, res.Status, err, wantStatus)})
		}
		return nil
	}
	for _, c := range undone {
		if err := probeUndo(c.InboxID, c.EventID, "undone"); err != nil {
			return pr, err
		}
		pr.probes++
		res, err := admin.Replay(ctx, pool, admin.ReplayRequest{InboxID: c.InboxID, Actor: undoAllActor, Reason: undoAllProbeReason}, admin.Options{})
		if ferr := probeFatal(ctx, "replay "+c.EventID, err); ferr != nil {
			return pr, ferr
		}
		switch res.Outcome {
		case admin.ReplayRefused:
			pr.replayRefused++
		case admin.ReplayDone:
			pr.replayDone++
		}
		if !errors.Is(err, admin.ErrNotReplayable) || res.FromStatus != "undone" {
			pr.violations = append(pr.violations, Violation{KindUndoProbe, c.EventID,
				fmt.Sprintf("replay: outcome %q from status %q, err %v; want ErrNotReplayable from status undone", res.Outcome, res.FromStatus, err)})
		}
	}
	for _, p := range pre.notDone {
		if err := probeUndo(p.InboxID, p.EventID, p.Status); err != nil {
			return pr, err
		}
	}
	return pr, nil
}

// checkAfterUndo asserts the end state: every done event undone and nothing
// else moved, app tables empty, change_log consistent, and admin_actions
// holding exactly the audit rows this phase's own results imply.
func checkAfterUndo(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope, pre undoPhaseState, loop loopResult, pr probeResult) ([]Violation, error) {
	var v []Violation

	// Inbox: done -> undone, every other status unchanged.
	want := map[string]int{}
	for k, n := range pre.inbox {
		want[k] = n
	}
	want["undone"] += want["done"]
	delete(want, "done")
	got, err := scopeInboxCounts(ctx, pool, sc)
	if err != nil {
		return nil, fmt.Errorf("inbox counts: %w", err)
	}
	if !sameCounts(got, want) {
		v = append(v, Violation{KindUndoInboxCounts, "inbox",
			fmt.Sprintf("statuses [%s], want [%s]", countsText(got), countsText(want))})
	}

	// App tables: count(*) over the WHOLE table, not the scope: chaos resets
	// every table per seed, so any row left is a leak, including one
	// CheckState would skip as foreign.
	for _, table := range []string{"customers", "credit_grants", "subscriptions"} {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		if n != 0 {
			v = append(v, Violation{KindUndoRowsLeft, table, fmt.Sprintf("%d row(s) left after undoing everything", n)})
		}
	}
	// And the scoped model check against an empty history, so a leftover row
	// of this seed is named.
	sv, _, err := checkState(ctx, pool, sc, Expected{})
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	for _, x := range sv {
		v = append(v, Violation{KindUndoRowsLeft, x.Subject, x.Kind + ": " + x.Detail})
	}

	cl, err := checkChangeLog(ctx, pool, sc)
	if err != nil {
		return nil, fmt.Errorf("change log: %w", err)
	}
	for _, x := range cl {
		v = append(v, Violation{"undo_" + x.Kind, x.Subject, x.Detail})
	}

	av, err := checkUndoAudit(ctx, pool, sc, pre, loop, pr)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	return append(v, av...), nil
}

// checkUndoAudit compares admin_actions with what the phase saw: per event,
// exactly one done undo for every undone event and none for the others;
// in total, one new refused undo row per audited refusal (loop and probes),
// one refused replay row per refused replay probe, no new done replay, and
// no change at all under any other (action, outcome) key.
func checkUndoAudit(ctx context.Context, pool *pgxpool.Pool, sc changeLogScope, pre undoPhaseState, loop loopResult, pr probeResult) ([]Violation, error) {
	var v []Violation
	undone := map[int64]bool{}
	for _, c := range pre.undone {
		undone[c.InboxID] = true
	}
	for _, c := range loop.Undone {
		undone[c.InboxID] = true
	}
	rows, err := pool.Query(ctx, `
		SELECT i.id, i.event_id,
		       count(a.id) FILTER (WHERE a.action = 'undo' AND a.outcome = 'done')
		FROM inbox i LEFT JOIN admin_actions a ON a.inbox_id = i.id
		WHERE left(i.event_id, length($1)) = $1
		GROUP BY i.id ORDER BY i.id`, sc.events)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, n int64
		var eventID string
		if err := rows.Scan(&id, &eventID, &n); err != nil {
			rows.Close()
			return nil, err
		}
		want := int64(0)
		if undone[id] {
			want = 1
		}
		if n != want {
			v = append(v, Violation{KindUndoAudit, eventID, fmt.Sprintf("%d done undo audit row(s), want %d", n, want)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	post, err := scopeAuditCounts(ctx, pool, sc)
	if err != nil {
		return nil, err
	}
	want := map[string]auditWant{
		"undo done":      {len(loop.Undone) + pr.undoDone, "undos carried out"},
		"undo refused":   {loop.Refusals + pr.undoRefused, "audited undo refusals (loop + probes)"},
		"replay refused": {pr.replayRefused, "refused replay probes"},
		"replay done":    {pr.replayDone, "replays carried out (probes only; want 0)"},
	}
	return append(v, auditDeltaViolations(pre.audit, post, want)...), nil
}

type auditWant struct {
	n    int
	what string
}

// auditDeltaViolations compares the per-"action outcome" growth of
// admin_actions with want, over EVERY key seen before, after or in want: a
// key the phase does not expect (today the schema's CHECKs allow only the
// four in want, but a later migration could add an action or an outcome)
// must not change at all. Pure, so the "other key" rule is unit tested;
// the schema makes it unreachable from a DB control.
func auditDeltaViolations(pre, post map[string]int, want map[string]auditWant) []Violation {
	keys := map[string]bool{}
	for _, m := range []map[string]int{pre, post} {
		for k := range m {
			keys[k] = true
		}
	}
	for k := range want {
		keys[k] = true
	}
	var v []Violation
	for _, k := range sortedKeys(keys) {
		w, ok := want[k]
		if !ok {
			w = auditWant{0, "not written by the undo phase"}
		}
		if got := post[k] - pre[k]; got != w.n {
			v = append(v, Violation{KindUndoAudit, "admin_actions",
				fmt.Sprintf("%d new '%s' row(s), want %d (%s)", got, k, w.n, w.what)})
		}
	}
	return v
}

func sameCounts(a, b map[string]int) bool {
	for k, n := range a {
		if b[k] != n {
			return false
		}
	}
	for k, n := range b {
		if a[k] != n {
			return false
		}
	}
	return true
}

func countsText(m map[string]int) string {
	keys := sortedKeys(m)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if m[k] != 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
		}
	}
	return strings.Join(parts, " ")
}
