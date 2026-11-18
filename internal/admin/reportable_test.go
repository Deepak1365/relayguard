package admin

import (
	"errors"
	"fmt"
	"testing"
)

// A failed audit insert or commit must not be printed as "refused, audited".
func TestReplayReportable(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"done", nil, true},
		{"refused and audited", fmt.Errorf("inbox 1: %w: status done", ErrNotReplayable), true},
		{"audit failed", fmt.Errorf("audit refusal: %w", errors.New("boom")), false},
		{"busy", ErrBusy, false},
		{"invalid", ErrInvalidInput, false},
	} {
		if got := ReplayReportable(c.err); got != c.want {
			t.Errorf("%s: ReplayReportable = %v, want %v", c.name, got, c.want)
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
		{"not undoable", fmt.Errorf("inbox 1: %w: status pending", ErrNotUndoable), true},
		{"conflict", fmt.Errorf("inbox 1: %w: 1 conflict(s)", ErrConflict), true},
		{"audit failed", fmt.Errorf("audit refusal: %w", errors.New("boom")), false},
		{"busy", ErrBusy, false},
		{"not found", ErrNotFound, false},
		{"replay refusal", ErrNotReplayable, false},
	} {
		if got := UndoReportable(c.err); got != c.want {
			t.Errorf("%s: UndoReportable = %v, want %v", c.name, got, c.want)
		}
	}
}
