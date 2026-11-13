package admin

// DB test for Undo on a replayed event (generation > 0): its apply rows, its
// undo rows and the pairing of later changes all live in generation 1, so a
// generation hard-coded to 0 anywhere in Undo shows up here.

import (
	"context"
	"strings"
	"testing"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

func TestDBUndoReplayedEvent(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	// The invoice arrives before its customer and dead-letters (generation 0,
	// no change_log rows); the customer and an older-applied invoice follow.
	replayed := deadInvoice(t, pool, "gen_replayed")
	cus := appliedCustomer(t, pool)
	older := appliedInvoice(t, pool, "gen_older", 1000)
	if _, err := Replay(ctx, pool, ReplayRequest{InboxID: replayed, Actor: "alice", Reason: "customer arrived late"}, Options{}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := dbtest.ApplyInbox(t, pool, replayed); got != "done" {
		t.Fatalf("replayed invoice: %s, want done", got)
	}

	// The generation-1 apply is the later change on the customer: undoing the
	// older invoice is refused and names it, with its generation.
	cs := wantConflict(t, pool, older)
	found := false
	for _, c := range cs {
		found = found || (c.Kind == ConflictLaterEvent && c.InboxID == replayed && c.Generation != nil && *c.Generation == 1)
	}
	if !found {
		t.Fatalf("conflicts %+v, want inbox #%d named as later event in generation 1", cs, replayed)
	}

	res := mustUndo(t, pool, replayed)
	if res.Generation != 1 {
		t.Fatalf("undo generation %d, want 1", res.Generation)
	}
	// Every apply and undo row of the event in generation 1 (checked by
	// wantUndoRows against res.Generation and each apply row's generation).
	wantUndoRows(t, pool, replayed, res, "update", "delete")
	var done *AdminAction
	for _, a := range actions(t, pool, replayed) {
		if a.ID == res.ActionID {
			a := a
			done = &a
		}
	}
	if done == nil || done.Outcome != "done" || !strings.Contains(done.Detail, `"generation": 1`) {
		t.Fatalf("undo audit row %+v, want a done undo with generation 1 in its detail", done)
	}

	// Pairing at generation 1: the older invoice is now undoable (the later
	// apply is paired with its undo by (inbox, generation 1)), then the
	// customer, leaving nothing behind.
	mustUndo(t, pool, older)
	mustUndo(t, pool, cus)
	for _, tb := range []string{"customers", "credit_grants"} {
		if n := dbtest.Count(t, pool, tb); n != 0 {
			t.Fatalf("%s: %d rows after undoing everything, want 0", tb, n)
		}
	}
}
