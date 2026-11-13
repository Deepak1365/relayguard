package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Undo shares replay's audit-field rules (normalizeAudit), so one table of
// cases covers the shared code; this checks the undo entry point uses it.
func TestNormalizeUndoRequest(t *testing.T) {
	got, err := NormalizeUndoRequest(UndoRequest{InboxID: 4, Actor: " bob\t", Reason: "\nwrong grant "})
	if err != nil || got != (UndoRequest{InboxID: 4, Actor: "bob", Reason: "wrong grant"}) {
		t.Fatalf("trim: %+v, %v", got, err)
	}
	if _, err := NormalizeUndoRequest(UndoRequest{InboxID: 4, Actor: strings.Repeat("é", 200), Reason: strings.Repeat("ü", 1000)}); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
	for _, r := range []UndoRequest{
		{InboxID: 0, Actor: "a", Reason: "r"},
		{InboxID: -1, Actor: "a", Reason: "r"},
		{InboxID: 4, Actor: " ", Reason: "r"},
		{InboxID: 4, Actor: "a", Reason: ""},
		{InboxID: 4, Actor: strings.Repeat("a", 201), Reason: "r"},
		{InboxID: 4, Actor: "a", Reason: strings.Repeat("r", 1001)},
	} {
		if _, err := NormalizeUndoRequest(r); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%.40v: err %v, want ErrInvalidInput", r, err)
		}
	}
}

// With a nil pool any database access would panic, so ErrInvalidInput
// proves validation runs first.
func TestUndoValidatesBeforeDB(t *testing.T) {
	if _, err := Undo(context.Background(), nil, UndoRequest{InboxID: 1, Actor: "alice"}, Options{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err %v, want ErrInvalidInput without touching the (nil) pool", err)
	}
}

// Every table an undo can touch has a column list and every referenced table
// is one of them (the DB test checks both against the live schema).
func TestUndoTablesConsistent(t *testing.T) {
	for parent, refs := range referencedBy {
		if _, ok := restoreColumns[parent]; !ok {
			t.Errorf("referencedBy[%s]: not an undoable table", parent)
		}
		for _, r := range refs {
			if _, ok := restoreColumns[r.table]; !ok {
				t.Errorf("referencedBy[%s]: %s is not an undoable table", parent, r.table)
			}
		}
	}
}
