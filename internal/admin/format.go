package admin

import (
	"fmt"
	"strings"
	"time"

	"github.com/Deepak1365/relayguard/internal/app"
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

// FormatReplay renders a replay result (done or refused) for a terminal.
// Pure. For a done replay it adds a warning based on the previous last_error
// (see CrashedLastError): a strong one when no attempt ever recorded a result
// (likely poison), a hedged one when only the final attempt(s) ended without a
// result, and one for a payload decode failure (it will fail the same way).
func FormatReplay(r ReplayResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REPLAY inbox %d (%s, %s): %s, audit action #%d\n",
		r.InboxID, r.EventID, r.EventType, r.Outcome, r.ActionID)
	processed := "none"
	if r.FromProcessedAt != nil {
		processed = utc(*r.FromProcessedAt)
	}
	fmt.Fprintf(&b, "was: status %s, attempts %d, processed_at %s, generation %d\n",
		r.FromStatus, r.FromAttempts, processed, r.wasGeneration())
	if r.FromLastError != "" {
		fmt.Fprintf(&b, "was last_error: %s\n", r.FromLastError)
	}
	switch r.Outcome {
	case ReplayDone:
		fmt.Fprintf(&b, "now: status pending, attempts 0, generation %d; the worker will pick it up\n", r.Generation)
		switch CrashedLastError(r.FromLastError) {
		case CrashLikely:
			b.WriteString("warning: the last error says the worker crashed on this event (possible poison event); " +
				"the replay will likely crash it again and dead-letter it after the full attempt budget\n")
		case CrashPossible:
			b.WriteString("warning: the final attempt(s) ended without a result (crash, kill or lease expiry); " +
				"if it is a poison event it will dead-letter again\n")
		}
		// The worker stores this text for a payload it cannot decode; the
		// payload does not change on replay.
		if strings.HasPrefix(r.FromLastError, app.DecodePayloadPrefix) {
			b.WriteString("warning: the last error is a payload decode failure; a replay will most likely fail the same way\n")
		}
	case ReplayRefused:
		fmt.Fprintf(&b, "refused: %s (nothing changed; the refusal is audited)\n", r.Reason)
	}
	return b.String()
}

// wasGeneration is the generation before the request: a done replay raised
// it by one.
func (r ReplayResult) wasGeneration() int {
	if r.Outcome == ReplayDone {
		return r.Generation - 1
	}
	return r.Generation
}

// CrashSign says how strongly a last_error suggests a poison event.
type CrashSign int

const (
	CrashNone     CrashSign = iota // normal error, or none
	CrashPossible                  // the final attempt(s) ended without a result
	CrashLikely                    // no attempt ever recorded a result
)

// CrashedLastError classifies last_error written by the worker's
// deadLetterCrashed (texts shared via app.GaveUpPrefix / app.NoResultRecorded;
// the worker's normal dead path stores the apply error itself, without the
// prefix). The bare prefix is only "possible": a single kill or lease expiry
// on the last attempt also writes it, after earlier attempts recorded a real
// error. Only "none recorded" means every attempt crashed.
func CrashedLastError(lastErr string) CrashSign {
	switch {
	case !strings.HasPrefix(lastErr, app.GaveUpPrefix):
		return CrashNone
	case strings.HasSuffix(lastErr, "last error: "+app.NoResultRecorded):
		return CrashLikely
	default:
		return CrashPossible
	}
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
