package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Deepak1365/relayguard/internal/admin"
)

// fakeUndo simulates rule B over a dependency graph: an event is undoable
// once every event in deps[id] is undone; otherwise it is refused with the
// given conflict kind. calls records the order of attempts.
type fakeUndo struct {
	deps   map[string][]string
	kind   map[string]string // conflict kind to report for an event (default later_event)
	undone map[string]bool
	calls  []string
}

func (f *fakeUndo) try(c undoCandidate) (string, []string, error) {
	f.calls = append(f.calls, c.EventID)
	if f.undone == nil {
		f.undone = map[string]bool{}
	}
	for _, d := range f.deps[c.EventID] {
		if !f.undone[d] {
			k := f.kind[c.EventID]
			if k == "" {
				k = admin.ConflictLaterEvent
			}
			return attemptConflict, []string{k}, fmt.Errorf("%w: %s first", admin.ErrConflict, d)
		}
	}
	f.undone[c.EventID] = true
	return attemptDone, nil, nil
}

func cands(ids ...string) []undoCandidate {
	out := make([]undoCandidate, len(ids))
	for i, id := range ids {
		out[i] = undoCandidate{InboxID: int64(i + 1), EventID: id, ApplyID: int64(i + 1)}
	}
	return out
}

// mustLoop is runUndoLoop for tries that never return attemptFatal.
func mustLoop(t *testing.T, mode string, order []undoCandidate, try tryUndo, maxPasses int) loopResult {
	t.Helper()
	res, err := runUndoLoop(mode, order, try, maxPasses)
	if err != nil {
		t.Fatalf("runUndoLoop: %v", err)
	}
	return res
}

func violationKinds(vs []Violation) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Kind
	}
	sort.Strings(out)
	return out
}

// A chain worst-case for one pass: in order a, b, c where a needs b and b
// needs c, a pass undoes only the tail. Random mode must keep passing until
// everything is undone, count every refusal, and report nothing.
func TestUndoLoopRandomMakesProgress(t *testing.T) {
	f := &fakeUndo{deps: map[string][]string{"a": {"b"}, "b": {"c"}}}
	res := mustLoop(t, UndoAllRandom, cands("a", "b", "c"), f.try, 4)
	if len(res.Violations) != 0 {
		t.Fatalf("violations: %+v", res.Violations)
	}
	// pass 1: a refused, b refused, c done; pass 2: a refused, b done; pass 3: a done.
	if len(res.Undone) != 3 || res.Passes != 3 || res.Refusals != 3 || res.Kinds[admin.ConflictLaterEvent] != 3 {
		t.Fatalf("undone %d passes %d refusals %d kinds %v; want 3 3 3 later_event=3", len(res.Undone), res.Passes, res.Refusals, res.Kinds)
	}
	want := []string{"a", "b", "c", "a", "b", "a"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
}

// A cycle can never be undone in any order: the loop must say so after the
// first pass without progress, list what is left, and stop.
func TestUndoLoopRandomStuck(t *testing.T) {
	f := &fakeUndo{deps: map[string][]string{"a": {"b"}, "b": {"a"}}}
	res := mustLoop(t, UndoAllRandom, cands("a", "b", "c"), f.try, 4)
	if got := violationKinds(res.Violations); !reflect.DeepEqual(got, []string{KindUndoStuck}) {
		t.Fatalf("kinds %v, want [undo_stuck]; %+v", got, res.Violations)
	}
	if res.Passes != 2 || len(res.Undone) != 1 {
		t.Fatalf("passes %d undone %d, want 2 and 1 (c)", res.Passes, len(res.Undone))
	}
	if d := res.Violations[0].Detail; !strings.Contains(d, "a b") {
		t.Fatalf("stuck detail %q does not list the remaining events", d)
	}
}

// The pass cap is a safety net behind stuck detection, with its own kind so
// that losing stuck detection cannot hide behind it.
func TestUndoLoopRandomPassCap(t *testing.T) {
	f := &fakeUndo{deps: map[string][]string{"a": {"b"}, "b": {"c"}}}
	res := mustLoop(t, UndoAllRandom, cands("a", "b", "c"), f.try, 1)
	if got := violationKinds(res.Violations); !reflect.DeepEqual(got, []string{KindUndoPassCap}) {
		t.Fatalf("kinds %v, want [undo_pass_cap]; %+v", got, res.Violations)
	}
	if res.Passes != 1 || len(res.Undone) != 1 {
		t.Fatalf("passes %d undone %d, want 1 and 1", res.Passes, len(res.Undone))
	}
}

// Only later_event and referenced are expected in random order. Any other
// kind is reported once per event and kind, not once per pass, and the event
// stays (it may still be undone later, or end in undo_stuck).
func TestUndoLoopRandomDisallowedKind(t *testing.T) {
	for _, k := range []string{admin.ConflictChainBreak, admin.ConflictVersion, admin.ConflictMissing,
		admin.ConflictContent, admin.ConflictUnpaired} {
		t.Run(k, func(t *testing.T) {
			f := &fakeUndo{deps: map[string][]string{"a": {"x"}}, kind: map[string]string{"a": k}}
			res := mustLoop(t, UndoAllRandom, cands("a", "b"), f.try, 3)
			if got := violationKinds(res.Violations); !reflect.DeepEqual(got, []string{KindUndoStuck, KindUndoUnexpectedConflict}) {
				t.Fatalf("kinds %v; %+v", got, res.Violations)
			}
			if res.Kinds[k] != 2 { // refused in pass 1 and pass 2, reported once
				t.Fatalf("kinds %v, want %s=2", res.Kinds, k)
			}
		})
	}
	for _, k := range []string{admin.ConflictLaterEvent, admin.ConflictReferenced} {
		t.Run("allowed "+k, func(t *testing.T) {
			f := &fakeUndo{deps: map[string][]string{"a": {"b"}}, kind: map[string]string{"a": k}}
			res := mustLoop(t, UndoAllRandom, cands("a", "b"), f.try, 3)
			if len(res.Violations) != 0 || res.Kinds[k] != 1 {
				t.Fatalf("violations %+v kinds %v", res.Violations, res.Kinds)
			}
		})
	}
}

// ErrBusy (not audited) and ErrNotUndoable (audited) on a done event are
// undo_error in both modes; only the audited one counts as a refusal.
func TestUndoLoopErrors(t *testing.T) {
	for _, mode := range []string{UndoAllNewest, UndoAllRandom} {
		t.Run(mode, func(t *testing.T) {
			try := func(c undoCandidate) (string, []string, error) {
				switch c.EventID {
				case "busy":
					return attemptError, nil, admin.ErrBusy
				case "nu":
					return attemptNotUndoable, nil, admin.ErrNotUndoable
				}
				return attemptDone, nil, nil
			}
			res := mustLoop(t, mode, cands("busy", "ok"), try, 3)
			if got := violationKinds(res.Violations); !reflect.DeepEqual(got, []string{KindUndoError}) || res.Refusals != 0 {
				t.Fatalf("busy: kinds %v refusals %d; want [undo_error] 0", got, res.Refusals)
			}
			if !strings.Contains(res.Violations[0].Detail, "locked") {
				t.Fatalf("detail %q does not carry ErrBusy's text", res.Violations[0].Detail)
			}
			res = mustLoop(t, mode, cands("nu", "ok"), try, 3)
			want := []string{KindUndoError}
			if mode == UndoAllNewest {
				want = []string{KindUndoRefusedNewestFirst}
			}
			if got := violationKinds(res.Violations); !reflect.DeepEqual(got, want) || res.Refusals != 1 || res.Kinds[attemptNotUndoable] != 1 {
				t.Fatalf("not undoable: kinds %v refusals %d kinds %v; want %v 1 not_undoable=1", got, res.Refusals, res.Kinds, want)
			}
		})
	}
}

// Newest mode makes exactly one pass and reports every refusal as
// undo_refused_newest_first, with the conflicts in the detail.
func TestUndoLoopNewestRefusal(t *testing.T) {
	f := &fakeUndo{deps: map[string][]string{"a": {"b"}}}
	res := mustLoop(t, UndoAllNewest, cands("a", "b"), f.try, 3)
	if got := violationKinds(res.Violations); !reflect.DeepEqual(got, []string{KindUndoRefusedNewestFirst}) {
		t.Fatalf("kinds %v", got)
	}
	if res.Passes != 1 || res.Refusals != 1 || len(res.Undone) != 1 || res.Violations[0].Subject != "a" {
		t.Fatalf("passes %d refusals %d undone %d subject %s", res.Passes, res.Refusals, len(res.Undone), res.Violations[0].Subject)
	}
	if !strings.Contains(res.Violations[0].Detail, "b first") {
		t.Fatalf("detail %q lacks the conflict", res.Violations[0].Detail)
	}
}

// Newest first means apply order (change_log id), not Stripe's created time:
// under -reorder they differ, and only apply order is safe under rule B.
func TestSortNewestByApplyIDNotCreated(t *testing.T) {
	cs := []undoCandidate{
		{InboxID: 1, EventID: "older_applied_last", Created: 100, ApplyID: 30},
		{InboxID: 2, EventID: "newer_applied_first", Created: 300, ApplyID: 10},
		{InboxID: 3, EventID: "middle", Created: 200, ApplyID: 20},
	}
	sortNewest(cs)
	got := []string{cs[0].EventID, cs[1].EventID, cs[2].EventID}
	want := []string{"older_applied_last", "middle", "newer_applied_first"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// The random order depends only on the set of events and the seed: the same
// seed gives the same order whatever order the rows came in, and different
// seeds give different orders (else "random" would test one order only).
func TestShuffleRandomDeterministic(t *testing.T) {
	ids := []string{"e01", "e02", "e03", "e04", "e05", "e06", "e07", "e08", "e09", "e10"}
	order := func(seed int64, in []string) []string {
		cs := cands(in...)
		shuffleRandom(cs, rand.New(rand.NewSource(seed)))
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.EventID
		}
		return out
	}
	reversed := make([]string, len(ids))
	for i, id := range ids {
		reversed[len(ids)-1-i] = id
	}
	a, b := order(7, ids), order(7, reversed)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("seed 7: %v vs %v for the same set in another input order", a, b)
	}
	if reflect.DeepEqual(a, ids) {
		t.Fatalf("seed 7 left the order unchanged: %v", a)
	}
	if c := order(8, ids); reflect.DeepEqual(a, c) {
		t.Fatalf("seeds 7 and 8 gave the same order %v", a)
	}
}

// A context or database error is the harness failing, not the system: the
// loop must stop at once (no further undo calls, no undo_error, no
// undo_stuck) and return it as an error the caller can unwrap.
func TestUndoLoopFatalStops(t *testing.T) {
	for _, mode := range []string{UndoAllNewest, UndoAllRandom} {
		t.Run(mode, func(t *testing.T) {
			var calls []string
			try := func(c undoCandidate) (string, []string, error) {
				calls = append(calls, c.EventID)
				if c.EventID == "b" {
					return attemptFatal, nil, context.Canceled
				}
				return attemptDone, nil, nil
			}
			res, err := runUndoLoop(mode, cands("a", "b", "c"), try, 4)
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "b") {
				t.Fatalf("err %v, want context.Canceled naming b", err)
			}
			if !reflect.DeepEqual(calls, []string{"a", "b"}) || len(res.Violations) != 0 || len(res.Undone) != 1 {
				t.Fatalf("calls %v violations %+v undone %d; want [a b], none, 1", calls, res.Violations, len(res.Undone))
			}
		})
	}
}

// classifyUndo: a cancelled context is fatal whatever admin answered; admin's
// sentinel errors are judged (undo_error); anything else is fatal.
func TestClassifyUndo(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	refusedConflict := admin.UndoResult{InboxID: 7, Outcome: admin.UndoRefused,
		Conflicts: []admin.Conflict{{Kind: admin.ConflictLaterEvent, Table: "customers", RowID: 1, Explanation: "x"}}}
	dbErr := errors.New("conn reset by peer")
	cases := []struct {
		name      string
		ctx       context.Context
		res       admin.UndoResult
		err       error
		want      string
		wantCause error
	}{
		{"done", live, admin.UndoResult{Outcome: admin.UndoDone}, nil, attemptDone, nil},
		{"conflict", live, refusedConflict, fmt.Errorf("inbox 7: %w", admin.ErrConflict), attemptConflict, admin.ErrConflict},
		{"not undoable", live, admin.UndoResult{Outcome: admin.UndoRefused}, admin.ErrNotUndoable, attemptNotUndoable, admin.ErrNotUndoable},
		{"busy", live, admin.UndoResult{}, fmt.Errorf("lock rows: %w (55P03)", admin.ErrBusy), attemptError, admin.ErrBusy},
		{"not found", live, admin.UndoResult{}, fmt.Errorf("%w: id 9", admin.ErrNotFound), attemptError, admin.ErrNotFound},
		{"no outcome, no error", live, admin.UndoResult{}, nil, attemptError, nil},
		{"db error", live, admin.UndoResult{InboxID: 7}, fmt.Errorf("begin: %w", dbErr), attemptFatal, dbErr},
		{"cancelled, admin said busy", cancelled, admin.UndoResult{}, fmt.Errorf("lock rows: %w", admin.ErrBusy), attemptFatal, context.Canceled},
		{"cancelled, undo done", cancelled, admin.UndoResult{Outcome: admin.UndoDone}, nil, attemptFatal, context.Canceled},
	}
	for _, c := range cases {
		got, kinds, err := classifyUndo(c.ctx, c.res, c.err)
		if got != c.want {
			t.Errorf("%s: outcome %s, want %s (err %v)", c.name, got, c.want, err)
		}
		if c.wantCause != nil && !errors.Is(err, c.wantCause) {
			t.Errorf("%s: err %v, want it to wrap %v", c.name, err, c.wantCause)
		}
		if c.name == "conflict" && !reflect.DeepEqual(kinds, []string{admin.ConflictLaterEvent}) {
			t.Errorf("conflict kinds %v", kinds)
		}
	}
}

// probeFatal: only a gone context or a non-admin error aborts the probes;
// admin's answers (the expected refusals and e.g. ErrBusy) are judged.
func TestProbeFatal(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{nil, admin.ErrNotUndoable, fmt.Errorf("inbox 3: %w: undone", admin.ErrNotReplayable), fmt.Errorf("x: %w", admin.ErrBusy)} {
		if f := probeFatal(live, "undo e", err); f != nil {
			t.Errorf("%v: fatal %v, want judged", err, f)
		}
	}
	if f := probeFatal(live, "undo e", errors.New("conn reset")); f == nil || !strings.Contains(f.Error(), "probe undo e") {
		t.Errorf("db error: %v, want fatal naming the probe", f)
	}
	if f := probeFatal(cancelled, "replay e", admin.ErrNotReplayable); !errors.Is(f, context.Canceled) {
		t.Errorf("cancelled: %v, want context.Canceled", f)
	}
}

// The audit delta covers every (action, outcome) key, not only the four the
// phase writes: an unexpected key that grew (or shrank) is undo_audit. The
// schema's CHECKs make such a key impossible today, so this rule has no DB
// control.
func TestAuditDeltaViolations(t *testing.T) {
	want := map[string]auditWant{"undo done": {2, "u"}, "undo refused": {1, "r"}}
	pre := map[string]int{"undo done": 1, "replay done": 1}
	post := map[string]int{"undo done": 3, "undo refused": 1, "replay done": 1}
	if v := auditDeltaViolations(pre, post, want); len(v) != 0 {
		t.Fatalf("clean: %+v", v)
	}
	post["undo weird"] = 1  // a key nobody expects
	post["replay done"] = 2 // a key seen before, not expected to grow
	v := auditDeltaViolations(pre, post, want)
	var details []string
	for _, x := range v {
		if x.Kind != KindUndoAudit || x.Subject != "admin_actions" {
			t.Fatalf("violation %+v", x)
		}
		details = append(details, x.Detail)
	}
	if len(v) != 2 || !strings.Contains(details[0], "'replay done'") || !strings.Contains(details[1], "'undo weird'") {
		t.Fatalf("details %q, want one for 'replay done' and one for 'undo weird'", details)
	}
	delete(post, "undo refused") // an expected key that did not grow
	if v := auditDeltaViolations(pre, post, want); len(v) != 3 {
		t.Fatalf("missing refusal: %+v, want 3 violations", v)
	}
}
