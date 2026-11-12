package admin

import (
	"fmt"
	"strings"
	"time"

	"github.com/Deepak1365/relayguard/internal/changelog"
)

// FormatPreview renders a preview for a terminal. Pure, so it is unit tested
// without a database.
func FormatPreview(r PreviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PREVIEW inbox %d (%s, %s): status %s, attempts %d, generation %d\n",
		r.InboxID, r.EventID, r.EventType, r.Status, r.Attempts, r.Generation)
	switch r.Outcome {
	case OutcomeApply:
		fmt.Fprintf(&b, "outcome: apply, %d change(s)\n", len(r.Changes))
		for i, c := range r.Changes {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, changeLine(c))
		}
		b.WriteString("note: nothing was written (rolled back); ids of inserted rows are provisional\n")
	case OutcomeStale:
		fmt.Fprintf(&b, "outcome: stale, no changes: %s\n", r.Reason)
	case OutcomeSkip:
		fmt.Fprintf(&b, "outcome: skip, no changes: %s\n", r.Reason)
	case OutcomeError:
		fmt.Fprintf(&b, "outcome: error, the apply would fail now (the worker retries): %s\n", r.Reason)
	default:
		fmt.Fprintf(&b, "outcome: %q (unknown)\n", r.Outcome)
	}
	return b.String()
}

// FormatLog renders an event's recorded history for a terminal. Pure.
func FormatLog(r LogResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LOG inbox %d (%s, %s): status %s, attempts %d\n",
		r.InboxID, r.EventID, r.EventType, r.Status, r.Attempts)
	if r.LastError != "" {
		fmt.Fprintf(&b, "last_error: %s\n", r.LastError)
	}
	fmt.Fprintf(&b, "change_log: %d row(s)\n", len(r.Changes))
	for _, c := range r.Changes {
		src := c.Source
		if c.AdminActionID != nil {
			src = fmt.Sprintf("%s (action %d)", c.Source, *c.AdminActionID)
		}
		fmt.Fprintf(&b, "  #%d %s gen %d %s seq %d: %s\n",
			c.ID, utc(c.CreatedAt), c.Generation, src, c.Seq, changeLine(c.Change))
	}
	fmt.Fprintf(&b, "admin_actions: %d row(s)\n", len(r.Actions))
	for _, a := range r.Actions {
		fmt.Fprintf(&b, "  #%d %s %s %s by %s, reason %q, detail %s\n",
			a.ID, utc(a.CreatedAt), a.Action, a.Outcome, a.Actor, a.Reason, a.Detail)
	}
	return b.String()
}

// changeLine is changelog.Format, falling back to a marker instead of
// failing the whole report on one unreadable image.
func changeLine(c changelog.Change) string {
	s, err := changelog.Format(c)
	if err != nil {
		return fmt.Sprintf("%s %s %d: (unreadable image: %v)", c.Op, c.Table, c.RowID, err)
	}
	return s
}

func utc(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") }
