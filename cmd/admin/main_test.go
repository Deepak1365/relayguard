package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Deepak1365/relayguard/internal/admin"
)

func TestParseArgs(t *testing.T) {
	good := []struct {
		args []string
		want command
	}{
		{[]string{"preview", "12"}, command{name: "preview", inboxID: 12}},
		{[]string{"log", "1"}, command{name: "log", inboxID: 1}},
		{[]string{"replay", "5", "-actor", "alice", "-reason", "customer created late"},
			command{name: "replay", inboxID: 5, actor: "alice", reason: "customer created late"}},
		{[]string{"replay", "5", "-reason", "  padded  ", "-actor", " bob "}, // any flag order; trimmed
			command{name: "replay", inboxID: 5, actor: "bob", reason: "padded"}},
		{[]string{"replay", "5", "--actor=alice", "--reason=x"},
			command{name: "replay", inboxID: 5, actor: "alice", reason: "x"}},
		{[]string{"undo", "8", "-actor", "alice", "-reason", "wrong grant"},
			command{name: "undo", inboxID: 8, actor: "alice", reason: "wrong grant"}},
		{[]string{"undo", "8", "-reason", " r ", "-actor", " bob "},
			command{name: "undo", inboxID: 8, actor: "bob", reason: "r"}},
	}
	for _, c := range good {
		got, err := parseArgs(c.args)
		if err != nil || got != c.want {
			t.Errorf("parseArgs(%q) = %+v, %v; want %+v", c.args, got, err, c.want)
		}
	}
	bad := [][]string{
		nil,
		{"preview"},
		{"preview", "12", "extra"},
		{"undo", "12"}, // flags required, as for replay
		{"preview", "abc"},
		{"preview", "0"},
		{"preview", "-3"},
		{"log", "1.5"},
		{"preview", "12", "-actor", "alice"}, // flags only for replay
		// Replay: both flags required (user decision Q1), non-blank, within limits.
		{"replay", "12"},
		{"replay", "12", "-actor", "alice"},
		{"replay", "12", "-reason", "why"},
		{"replay", "12", "-actor", "   ", "-reason", "why"},
		{"replay", "12", "-actor", "alice", "-reason", ""},
		{"replay", "12", "-actor", strings.Repeat("a", 201), "-reason", "why"},
		{"replay", "12", "-actor", "alice", "-reason", strings.Repeat("r", 1001)},
		{"replay", "12", "-actor", "alice", "-reason", "unquoted", "words"},
		{"replay", "12", "-actor", "alice", "-reason", "why", "-force"},
		{"replay", "-actor", "alice", "-reason", "why", "12"}, // id comes first
		// Undo: same rules as replay.
		{"undo", "12", "-actor", "alice"},
		{"undo", "12", "-reason", "why"},
		{"undo", "12", "-actor", " ", "-reason", "why"},
		{"undo", "12", "-actor", strings.Repeat("a", 201), "-reason", "why"},
		{"undo", "12", "-actor", "alice", "-reason", "un", "quoted"},
		{"undo", "0", "-actor", "alice", "-reason", "why"},
		{"undo", "-actor", "alice", "-reason", "why", "12"},
		{"undo", "12", "-actor", "alice", "-reason", "why", "-force"},
	}
	for _, args := range bad {
		if got, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) = %+v, want an error", args, got)
		}
	}
}

// Usage errors exit 2 and print the usage, before touching .env or the DB.
// A replay without -reason is one: the audit fields are checked up front.
// A failed audit insert or commit must not be printed as "refused, audited".
func TestReplayReportable(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"done", nil, true},
		{"refused and audited", fmt.Errorf("inbox 1: %w: status done", admin.ErrNotReplayable), true},
		{"audit failed", fmt.Errorf("audit refusal: %w", errors.New("boom")), false},
		{"busy", admin.ErrBusy, false},
		{"invalid", admin.ErrInvalidInput, false},
	} {
		if got := replayReportable(c.err); got != c.want {
			t.Errorf("%s: replayReportable = %v, want %v", c.name, got, c.want)
		}
	}
}

// Both undo refusal kinds are audited before Undo returns; nothing else is.
func TestUndoReportable(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"done", nil, true},
		{"not undoable", fmt.Errorf("inbox 1: %w: status pending", admin.ErrNotUndoable), true},
		{"conflict", fmt.Errorf("inbox 1: %w: 1 conflict(s)", admin.ErrConflict), true},
		{"audit failed", fmt.Errorf("audit refusal: %w", errors.New("boom")), false},
		{"busy", admin.ErrBusy, false},
		{"not found", admin.ErrNotFound, false},
		{"replay refusal", admin.ErrNotReplayable, false},
	} {
		if got := undoReportable(c.err); got != c.want {
			t.Errorf("%s: undoReportable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRunUsageError(t *testing.T) {
	for _, args := range [][]string{{"nope"}, {"replay", "3", "-actor", "alice"}, {"undo", "3", "-actor", "alice"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("%q: exit code %d, want 2", args, code)
		}
		if out.Len() != 0 || !strings.Contains(errOut.String(), "usage:") {
			t.Fatalf("%q: stdout %q, stderr %q: want usage on stderr only", args, out.String(), errOut.String())
		}
	}
}
