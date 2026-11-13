package sim

import (
	"strings"
	"testing"
)

// A well-formed undone invoice: apply seq 1 grant insert (#10), seq 2
// customer update (#11); undo seq 1 reverts #11, seq 2 deletes the grant.
func undoneInvoice() undoEvent {
	act := int64(7)
	return undoEvent{
		EventID: "evt", Status: "undone", Generation: 1, DoneAction: &act, ApplyRows: 2,
		Rows: []undoRow{
			{ID: 20, Seq: 1, Generation: 1, ActionID: &act, Table: "customers", RowID: 1, Op: "update",
				Pair: &undoPair{ID: 11, Table: "customers", RowID: 1, Op: "update", Restored: true}},
			{ID: 21, Seq: 2, Generation: 1, ActionID: &act, Table: "credit_grants", RowID: 5, Op: "delete",
				Pair: &undoPair{ID: 10, Table: "credit_grants", RowID: 5, Op: "insert"}},
		},
	}
}

// One case per rule (and per condition within presence), each changing one
// field of a clean event, so every reason text is attributable to its rule.
func TestJudgeUndo(t *testing.T) {
	other := int64(8)
	cases := []struct {
		name     string
		damage   func(e *undoEvent)
		wantKind string // "" = no violation
		wantText string
	}{
		{"clean", func(e *undoEvent) {}, "", ""},
		{"done event without undo rows", func(e *undoEvent) { e.Status, e.Rows, e.DoneAction = "done", nil, nil }, "", ""},
		{"done undo action but no undo rows", func(e *undoEvent) { e.Status, e.Rows = "done", nil }, KindChangeLogMissing, "done undo action #7 but no undo rows (status done)"},
		{"undone without undo rows", func(e *undoEvent) { e.Rows = nil }, KindChangeLogMissing, "no undo rows"},
		{"undo rows on a done event", func(e *undoEvent) { e.Status = "done" }, KindChangeLogUnexpected, "not undone"},
		{"undo rows without a done action", func(e *undoEvent) { e.DoneAction = nil }, KindChangeLogUnexpected, "no done undo action"},
		{"action link", func(e *undoEvent) { e.Rows[1].ActionID = &other }, KindChangeLogBadUndo, "admin_action_id 8"},
		{"generation", func(e *undoEvent) { e.Rows[0].Generation = 0 }, KindChangeLogBadUndo, "generation 0, want 1"},
		{"pairing count", func(e *undoEvent) { e.ApplyRows = 3 }, KindChangeLogBadUndo, "2 undo row(s) for 3 apply row(s)"},
		{"pairing seq", func(e *undoEvent) { e.Rows[1].Seq = 3 }, KindChangeLogBadUndo, "seq 3, want 2"},
		{"pairing row", func(e *undoEvent) { e.Rows[0].Pair = e.Rows[1].Pair }, KindChangeLogBadUndo, "does not revert apply seq 2"},
		{"pairing none", func(e *undoEvent) { e.Rows[0].Pair = nil }, KindChangeLogBadUndo, "(none)"},
		{"restore insert", func(e *undoEvent) { e.Rows[1].Op = "update" }, KindChangeLogBadUndo, "want delete"},
		{"restore update op", func(e *undoEvent) { e.Rows[0].Op = "delete" }, KindChangeLogBadUndo, "want update"},
		{"restore content", func(e *undoEvent) { e.Rows[0].Pair.Restored = false }, KindChangeLogBadUndo, "after image differs"},
		{"restore of a delete", func(e *undoEvent) { e.Rows[0].Pair.Op = "delete" }, KindChangeLogBadUndo, "never reverts"},
	}
	for _, c := range cases {
		e := undoneInvoice()
		c.damage(&e)
		got := judgeUndo(e)
		if c.wantKind == "" {
			if got != nil {
				t.Errorf("%s: %+v, want no violation", c.name, *got)
			}
			continue
		}
		if got == nil || got.Kind != c.wantKind || got.Subject != "evt" || !strings.Contains(got.Detail, c.wantText) {
			t.Errorf("%s: %+v, want %s containing %q", c.name, got, c.wantKind, c.wantText)
		}
	}
}
