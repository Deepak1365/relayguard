package admin

import (
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/Deepak1365/relayguard/internal/changelog"
)

func TestFormatPreview(t *testing.T) {
	base := PreviewResult{InboxID: 7, EventID: "evt_1", EventType: "invoice.paid", Status: "pending", Attempts: 1}
	apply := base
	apply.Outcome = OutcomeApply
	apply.Changes = []changelog.Change{
		{Table: "credit_grants", RowID: 3, Op: changelog.OpInsert, After: []byte(`{"amount":20}`)},
		{Table: "customers", RowID: 1, Op: changelog.OpUpdate,
			Before: []byte(`{"credit_balance":0,"row_version":1}`), After: []byte(`{"credit_balance":20,"row_version":2}`)},
	}
	stale := base
	stale.EventType, stale.Status, stale.Outcome, stale.Reason = "customer.subscription.updated", "stale", OutcomeStale, "stale event: newer"
	failed := base
	failed.Status, failed.Outcome, failed.Reason = "dead", OutcomeError, "unknown customer cus_x"
	bad := base
	bad.Outcome = OutcomeApply
	bad.Changes = []changelog.Change{{Table: "customers", RowID: 1, Op: changelog.OpInsert, After: []byte(`not json`)}}

	cases := []struct {
		name string
		r    PreviewResult
		want string
	}{
		{"apply", apply, `PREVIEW inbox 7 (evt_1, invoice.paid): status pending, attempts 1, generation 0
outcome: apply, 2 change(s)
  1. insert credit_grants 3: amount (none) -> 20
  2. update customers 1: credit_balance 0 -> 20, row_version 1 -> 2
note: nothing was written (rolled back); ids of inserted rows are provisional
`},
		{"stale", stale, `PREVIEW inbox 7 (evt_1, customer.subscription.updated): status stale, attempts 1, generation 0
outcome: stale, no changes: stale event: newer
`},
		{"error", failed, `PREVIEW inbox 7 (evt_1, invoice.paid): status dead, attempts 1, generation 0
outcome: error, the apply would fail now (the worker retries): unknown customer cus_x
`},
		{"unreadable image", bad, `PREVIEW inbox 7 (evt_1, invoice.paid): status pending, attempts 1, generation 0
outcome: apply, 1 change(s)
  1. insert customers 1: (unreadable image: format insert customers 1: after image: invalid character 'o' in literal null (expecting 'u'))
note: nothing was written (rolled back); ids of inserted rows are provisional
`},
	}
	for _, c := range cases {
		if got := FormatPreview(c.r); got != c.want {
			t.Errorf("%s:\n got:\n%s\nwant:\n%s", c.name, got, c.want)
		}
	}
}

func TestFormatLog(t *testing.T) {
	ts := time.Date(2026, 11, 12, 10, 30, 0, 0, time.FixedZone("CET", 3600))
	action := int64(4)
	r := LogResult{
		InboxID: 7, EventID: "evt_1", EventType: "invoice.paid", Status: "done", Attempts: 2,
		LastError: "",
		Changes: []LoggedChange{
			{ID: 11, Generation: 0, Source: "apply", Seq: 1, CreatedAt: ts,
				Change: changelog.Change{Table: "credit_grants", RowID: 3, Op: changelog.OpInsert, After: []byte(`{"amount":20}`)}},
			{ID: 15, Generation: 0, Source: "undo", AdminActionID: &action, Seq: 1, CreatedAt: ts,
				Change: changelog.Change{Table: "customers", RowID: 1, Op: changelog.OpUpdate,
					Before: []byte(`{"credit_balance":20}`), After: []byte(`{"credit_balance":0}`)}},
		},
		Actions: []AdminAction{{ID: 4, Action: "undo", Actor: "alice", Reason: "refund \"test\"", Outcome: "done",
			Detail: `{"x":1}`, CreatedAt: ts}},
	}
	want := `LOG inbox 7 (evt_1, invoice.paid): status done, attempts 2
change_log: 2 row(s)
  #11 2026-11-12 09:30:00Z gen 0 apply seq 1: insert credit_grants 3: amount (none) -> 20
  #15 2026-11-12 09:30:00Z gen 0 undo (action 4) seq 1: update customers 1: credit_balance 20 -> 0
admin_actions: 1 row(s)
  #4 2026-11-12 09:30:00Z undo done by alice, reason "refund \"test\"", detail {"x":1}
`
	if got := FormatLog(r); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	empty := LogResult{InboxID: 9, EventID: "evt_2", EventType: "charge.refunded", Status: "dead", Attempts: 8,
		LastError: "gave up after 8 attempts"}
	wantEmpty := `LOG inbox 9 (evt_2, charge.refunded): status dead, attempts 8
last_error: gave up after 8 attempts
change_log: 0 row(s)
admin_actions: 0 row(s)
`
	if got := FormatLog(empty); got != wantEmpty {
		t.Errorf("empty log:\n got:\n%s\nwant:\n%s", got, wantEmpty)
	}
}

func TestFormatReplay(t *testing.T) {
	ts := time.Date(2026, 11, 12, 10, 30, 0, 0, time.FixedZone("CET", 3600))
	done := ReplayResult{InboxID: 7, EventID: "evt_1", EventType: "invoice.paid", Outcome: ReplayDone, ActionID: 3,
		FromStatus: "dead", FromAttempts: 8, FromLastError: "unknown customer cus_x", FromProcessedAt: &ts, Generation: 1}
	poison := done
	poison.EventType, poison.Generation = "test.poison", 2
	poison.FromLastError = "gave up after 8 attempts; last error: none recorded (the worker crashed or was killed while processing)"
	possible := poison
	possible.EventType = "invoice.paid"
	possible.FromLastError = "gave up after 8 attempts; last error: unknown customer cus_x"
	decode := done
	decode.FromLastError = "decode payload: unexpected end of JSON input"
	refused := ReplayResult{InboxID: 7, EventID: "evt_1", EventType: "invoice.paid", Outcome: ReplayRefused, ActionID: 4,
		FromStatus: "done", FromAttempts: 1, Generation: 0, Reason: "status done is not replayable (only dead and stale)"}

	cases := []struct {
		name string
		r    ReplayResult
		want string
	}{
		{"done", done, `REPLAY inbox 7 (evt_1, invoice.paid): done, audit action #3
was: status dead, attempts 8, processed_at 2026-11-12 09:30:00Z, generation 0
was last_error: unknown customer cus_x
now: status pending, attempts 0, generation 1; the worker will pick it up
`},
		{"poison warning", poison, `REPLAY inbox 7 (evt_1, test.poison): done, audit action #3
was: status dead, attempts 8, processed_at 2026-11-12 09:30:00Z, generation 1
was last_error: gave up after 8 attempts; last error: none recorded (the worker crashed or was killed while processing)
now: status pending, attempts 0, generation 2; the worker will pick it up
warning: the last error says the worker crashed on this event (possible poison event); the replay will likely crash it again and dead-letter it after the full attempt budget
`},
		{"possible poison warning", possible, `REPLAY inbox 7 (evt_1, invoice.paid): done, audit action #3
was: status dead, attempts 8, processed_at 2026-11-12 09:30:00Z, generation 1
was last_error: gave up after 8 attempts; last error: unknown customer cus_x
now: status pending, attempts 0, generation 2; the worker will pick it up
warning: the final attempt(s) ended without a result (crash, kill or lease expiry); if it is a poison event it will dead-letter again
`},
		{"decode warning", decode, `REPLAY inbox 7 (evt_1, invoice.paid): done, audit action #3
was: status dead, attempts 8, processed_at 2026-11-12 09:30:00Z, generation 0
was last_error: decode payload: unexpected end of JSON input
now: status pending, attempts 0, generation 1; the worker will pick it up
warning: the last error is a payload decode failure; a replay will most likely fail the same way
`},
		{"refused", refused, `REPLAY inbox 7 (evt_1, invoice.paid): refused, audit action #4
was: status done, attempts 1, processed_at none, generation 0
refused: status done is not replayable (only dead and stale) (nothing changed; the refusal is audited)
`},
	}
	for _, c := range cases {
		if got := FormatReplay(c.r); got != c.want {
			t.Errorf("%s:\n got:\n%s\nwant:\n%s", c.name, got, c.want)
		}
	}
}

func TestCrashedLastError(t *testing.T) {
	for _, c := range []struct {
		lastErr string
		want    CrashSign
	}{
		{"gave up after 8 attempts; last error: none recorded (the worker crashed or was killed while processing)", CrashLikely},
		{"gave up after 8 attempts; last error: unknown customer cus_x", CrashPossible}, // one kill on the last attempt
		{"gave up after 8 attempts", CrashPossible},
		{"unknown customer cus_x", CrashNone}, // normal dead path
		{"the worker crashed", CrashNone},     // text alone, without the worker's prefix, proves nothing
		{"", CrashNone},
	} {
		if got := CrashedLastError(c.lastErr); got != c.want {
			t.Errorf("CrashedLastError(%q) = %v, want %v", c.lastErr, got, c.want)
		}
	}
}

// The preview's whole safety argument is "always rolled back". Guard it at
// the source level too, so a later edit cannot add a Commit unnoticed
// (TestDBPreviewWritesNothing is the behavioural check).
func TestPreviewSourceHasNoCommit(t *testing.T) {
	src, err := os.ReadFile("preview.go")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`\.Commit\(`).Match(src) {
		t.Fatal("preview.go calls Commit; a preview must always roll back")
	}
}
