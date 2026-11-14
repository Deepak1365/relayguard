package main

import (
	"strings"
	"testing"

	"github.com/Deepak1365/relayguard/internal/sim"
)

// The results file must say whether the change_log check ran: "0
// violations" means something different with the check off.
func TestChangeLogCheckLine(t *testing.T) {
	cases := []struct {
		target  string
		enabled bool
		want    string
	}{
		{"relayguard", true, "- Change log check: on (changelog_* violation kinds)"},
		{"relayguard", false, "- Change log check: off (flag -changelog-check=false)"},
		{"naive", true, "- Change log check: n/a (the naive receiver writes no change_log)"},
		{"naive", false, "- Change log check: n/a (the naive receiver writes no change_log)"},
	}
	for _, c := range cases {
		if got := changeLogCheckLine(c.target, c.enabled); got != c.want {
			t.Errorf("changeLogCheckLine(%q, %v) = %q, want %q", c.target, c.enabled, got, c.want)
		}
	}
}

// -undo-all must be refused wherever its result could not mean what the
// header line claims. Each rejection case differs from the accepted one in
// one input only, so each rule is tested on its own.
func TestValidateUndoAll(t *testing.T) {
	cases := []struct {
		name, mode, target string
		changeLog          bool
		supervise          string
		wantErr            string // "" = accepted
	}{
		{"off", "", "naive", false, "", ""},
		{"newest", "newest", "relayguard", true, "bin/worker.exe", ""},
		{"random", "random", "relayguard", true, "bin/worker.exe", ""},
		{"bad mode", "oldest", "relayguard", true, "bin/worker.exe", "must be newest or random"},
		{"naive", "newest", "naive", true, "bin/naive.exe", "only applies to -target relayguard"},
		{"changelog off", "newest", "relayguard", false, "bin/worker.exe", "needs the change_log check"},
		{"unsupervised", "random", "relayguard", true, "", "needs -supervise"},
	}
	for _, c := range cases {
		err := validateUndoAll(c.mode, c.target, c.changeLog, c.supervise)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: %v, want accepted", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: %v, want an error containing %q", c.name, err, c.wantErr)
		}
	}
}

func TestUndoAllLine(t *testing.T) {
	want := "- Undo-all: random (after forward checks; asserts empty tables, all undone, changelog 0, probes refused; " +
		"undo_refusals and Refusals count refusals in the undo loop only, not the probes' expected refusals)"
	if got := undoAllLine("random"); got != want {
		t.Errorf("undoAllLine = %q, want %q", got, want)
	}
}

// The undo columns appear only for -undo-all runs (other results files keep
// their format), and line up with the header.
func TestResultsTableUndoColumns(t *testing.T) {
	r := runResult{seed: 3, events: 10, deliveries: 12, inbox: map[string]int{"undone": 9},
		undo: &sim.UndoReport{Undone: 9, Refusals: 4, Passes: 3}}
	if got, want := tableRow(r, true), "| 3 | 10 | 12 | 0 | 0 | undone=9 | 9 | 4 | 3 | 0 | - |\n"; got != want {
		t.Errorf("undo row = %q, want %q", got, want)
	}
	r.undo = nil
	if got, want := tableRow(r, false), "| 3 | 10 | 12 | 0 | 0 | undone=9 | 0 | - |\n"; got != want {
		t.Errorf("plain row = %q, want %q", got, want)
	}
	for _, undo := range []bool{false, true} {
		cols := strings.Count(strings.SplitN(tableHeader(undo), "\n", 2)[0], "|")
		r.undo = &sim.UndoReport{}
		if got := strings.Count(tableRow(r, undo), "|"); got != cols {
			t.Errorf("undo=%v: row has %d '|', header %d", undo, got, cols)
		}
	}
	if strings.Contains(tableHeader(false), "Undone") || !strings.Contains(tableHeader(true), "| Undone | Refusals | Passes |") {
		t.Errorf("headers: %q / %q", tableHeader(false), tableHeader(true))
	}
}

func TestUndoLine(t *testing.T) {
	r := &sim.UndoReport{Mode: "random", Undone: 40, Refusals: 7, Passes: 3, Probes: 95,
		RefusalKinds: map[string]int{"referenced": 2, "later_event": 6}}
	want := "undo: mode=random undone=40 refusals=7 later_event=6 referenced=2 passes=3 probes=95 violations=1"
	if got := undoLine(r, 1); got != want {
		t.Errorf("undoLine = %q, want %q", got, want)
	}
}

// With events still pending after WaitIdle (a timeout), the worker may still
// be applying them, so the undo phase must not run, and the seed must fail
// with undo_skipped naming the pending count. Without -undo-all, or with
// nothing pending, nothing is skipped.
func TestUndoAllSkip(t *testing.T) {
	if _, skip := undoAllSkip("", map[string]int{"pending": 3}); skip {
		t.Error("skipped without -undo-all")
	}
	if _, skip := undoAllSkip("newest", map[string]int{"done": 40, "dead": 2}); skip {
		t.Error("skipped with nothing pending")
	}
	if _, skip := undoAllSkip("newest", nil); skip {
		t.Error("skipped with no inbox counts")
	}
	v, skip := undoAllSkip("random", map[string]int{"done": 30, "pending": 3})
	if !skip || v.Kind != sim.KindUndoSkipped || v.Subject != "inbox" ||
		!strings.Contains(v.Detail, "3 events still pending") || !strings.Contains(v.Detail, "random") {
		t.Errorf("skip=%v violation %+v; want undo_skipped on inbox naming 3 pending and the mode", skip, v)
	}
}

func TestUndoResultFields(t *testing.T) {
	if got, want := undoResultFields(40, 7, 2), " undone=40 undo_refusals=7 undo_skipped=2"; got != want {
		t.Errorf("undoResultFields = %q, want %q", got, want)
	}
}

// A skipped seed shows as skipped, not as zeros of a phase that ran: the
// table says "skipped", the console says undone=0, and it counts as one
// violation of the seed.
func TestUndoSkippedDisplay(t *testing.T) {
	v, _ := undoAllSkip("newest", map[string]int{"pending": 2})
	r := runResult{seed: 4, events: 10, deliveries: 12, inbox: map[string]int{"done": 8, "pending": 2},
		violations: []sim.Violation{v}, undoSkipped: true, undoViol: 1}
	if got, want := tableRow(r, true), "| 4 | 10 | 12 | 0 | 0 | done=8 pending=2 | skipped | - | - | 1 | undo_skipped=1 |\n"; got != want {
		t.Errorf("row = %q, want %q", got, want)
	}
	if got := undoSkippedLine(); !strings.Contains(got, "skipped") || !strings.Contains(got, "undone=0") {
		t.Errorf("console line %q", got)
	}
}
