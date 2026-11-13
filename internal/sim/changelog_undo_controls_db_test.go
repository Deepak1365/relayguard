package sim

// Negative controls for checkUndoRows: ONE control per rule of judgeUndo
// (presence, action link, generation, pairing, restore). Each starts from a
// clean undoneHistory (or history), damages exactly the field its rule
// reads, and asserts the checker's verdict for that event: exactly one
// violation, its kind and the rule's reason text. Rules are evaluated in
// order and the first failure is reported, so a control that also touched
// an earlier rule's field would be masked by it; checking the reason, not
// just the count, catches that.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

// wantUndoViolation asserts exactly one violation for event suffix, of kind
// with a detail containing text.
func wantUndoViolation(t *testing.T, vs []Violation, suffix, kind, text string) {
	t.Helper()
	got := undoViolations(vs, suffix)
	if len(got) != 1 || got[0].Kind != kind || !strings.Contains(got[0].Detail, text) {
		t.Fatalf("violations for %s: %+v\nwant exactly one %s containing %q\nall: %v",
			suffix, got, kind, text, keys(vs))
	}
}

// Presence (rule 1), one subtest per condition. None of these damages a row
// or an image, so the whole report must be exactly that one violation.
func TestDBChangeLogCheckUndoPresence(t *testing.T) {
	t.Run("undone without undo rows", func(t *testing.T) {
		pool := dbtest.Pool(t)
		history(t, pool, "a")
		mustExec(t, pool, `UPDATE inbox SET status = 'undone' WHERE id = $1`, inboxIDOf(t, pool, "a_sub_u"))
		v := check(t, pool, "a")
		wantViolations(t, v, KindChangeLogMissing+" "+dbtest.EventPrefix+"a_sub_u")
		wantUndoViolation(t, v, "a_sub_u", KindChangeLogMissing, "no undo rows")
	})
	t.Run("undo rows but the event is not undone", func(t *testing.T) {
		pool := dbtest.Pool(t)
		u := undoneHistory(t, pool)
		mustExec(t, pool, `UPDATE inbox SET status = 'done' WHERE id = $1`, u.invoice)
		v := check(t, pool, "a")
		wantViolations(t, v, KindChangeLogUnexpected+" "+dbtest.EventPrefix+"a_inv")
		wantUndoViolation(t, v, "a_inv", KindChangeLogUnexpected, "is done, not undone")
	})
	t.Run("undo rows without a done undo action", func(t *testing.T) {
		pool := dbtest.Pool(t)
		u := undoneHistory(t, pool)
		mustExec(t, pool, `UPDATE admin_actions SET outcome = 'refused' WHERE id = $1`, u.invAction)
		v := check(t, pool, "a")
		wantViolations(t, v, KindChangeLogUnexpected+" "+dbtest.EventPrefix+"a_inv")
		wantUndoViolation(t, v, "a_inv", KindChangeLogUnexpected, "no done undo action")
	})
	t.Run("done undo action but no undo rows", func(t *testing.T) {
		pool := dbtest.Pool(t)
		history(t, pool, "a")
		id := inboxIDOf(t, pool, "a_sub_u")
		var actionID int64
		if err := pool.QueryRow(context.Background(), `INSERT INTO admin_actions (inbox_id, action, outcome, actor, reason)
			VALUES ($1, 'undo', 'done', 'dbtest', 'orphan') RETURNING id`, id).Scan(&actionID); err != nil {
			t.Fatal(err)
		}
		v := check(t, pool, "a")
		wantViolations(t, v, KindChangeLogMissing+" "+dbtest.EventPrefix+"a_sub_u")
		wantUndoViolation(t, v, "a_sub_u", KindChangeLogMissing,
			fmt.Sprintf("done undo action #%d but no undo rows (status done)", actionID))
	})
}

// Action link (rule 2): the invoice's undo rows point at a real undo audit
// row of the same event, but the refused one (a second undo attempt), not
// the one that did the undo.
func TestDBChangeLogCheckUndoActionLink(t *testing.T) {
	pool := dbtest.Pool(t)
	u := undoneHistory(t, pool)
	res, err := admin.Undo(context.Background(), pool,
		admin.UndoRequest{InboxID: u.invoice, Actor: "dbtest", Reason: "again"}, admin.Options{})
	if !errors.Is(err, admin.ErrNotUndoable) || res.ActionID == 0 {
		t.Fatalf("second undo: %+v, %v; want a refused audit row", res, err)
	}
	mustExec(t, pool, `UPDATE change_log SET admin_action_id = $2 WHERE inbox_id = $1 AND source = 'undo'`, u.invoice, res.ActionID)
	v := check(t, pool, "a")
	wantViolations(t, v, KindChangeLogBadUndo+" "+dbtest.EventPrefix+"a_inv")
	wantUndoViolation(t, v, "a_inv", KindChangeLogBadUndo,
		fmt.Sprintf("admin_action_id %d, want the event's done undo action #%d", res.ActionID, u.invAction))
}

// Generation (rule 3): undo rows filed under a generation the event never
// applied in.
func TestDBChangeLogCheckUndoGeneration(t *testing.T) {
	pool := dbtest.Pool(t)
	u := undoneHistory(t, pool)
	mustExec(t, pool, `UPDATE change_log SET generation = 1 WHERE inbox_id = $1 AND source = 'undo'`, u.invoice)
	v := check(t, pool, "a")
	wantViolations(t, v, KindChangeLogBadUndo+" "+dbtest.EventPrefix+"a_inv")
	wantUndoViolation(t, v, "a_inv", KindChangeLogBadUndo, "generation 1, want 0")
}

// Pairing (rule 4): the invoice's undo rows in forward instead of reverse
// order (seq swapped; ids, and so the chains, unchanged). Swapped through
// seq 99 because the unique index (admin_action_id, seq) is checked per row.
func TestDBChangeLogCheckUndoPairing(t *testing.T) {
	pool := dbtest.Pool(t)
	u := undoneHistory(t, pool)
	for _, step := range [][2]int{{1, 99}, {2, 1}, {99, 2}} {
		mustExec(t, pool, `UPDATE change_log SET seq = $3 WHERE inbox_id = $1 AND source = 'undo' AND seq = $2`,
			u.invoice, step[0], step[1])
	}
	v := check(t, pool, "a")
	wantViolations(t, v, KindChangeLogBadUndo+" "+dbtest.EventPrefix+"a_inv")
	wantUndoViolation(t, v, "a_inv", KindChangeLogBadUndo, "does not revert apply seq 2")
}

// Restore (rule 5), one subtest per condition. Damaging an undo image also
// breaks the row's chain or final state (row subjects); only the event's
// verdict is asserted here, the row kinds are the chain/final checks' job.
func TestDBChangeLogCheckUndoRestore(t *testing.T) {
	cases := []struct {
		name, damage, want string // damage: SQL, $1 = the invoice's inbox id
	}{
		{"update restores other content",
			`UPDATE change_log SET after_image = jsonb_set(after_image, '{email}', '"tampered@example.test"')
			 WHERE inbox_id = $1 AND source = 'undo' AND table_name = 'customers'`,
			"after image differs"},
		{"insert reverted by an update, not a delete",
			`UPDATE change_log SET op = 'update', after_image = before_image
			 WHERE inbox_id = $1 AND source = 'undo' AND table_name = 'credit_grants'`,
			"want delete"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := dbtest.Pool(t)
			u := undoneHistory(t, pool)
			mustExec(t, pool, c.damage, u.invoice)
			wantUndoViolation(t, check(t, pool, "a"), "a_inv", KindChangeLogBadUndo, c.want)
		})
	}
}
