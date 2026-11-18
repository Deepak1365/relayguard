package admin

import "errors"

// ReplayReportable reports whether a Replay result may be shown as a report:
// only when it succeeded or the refusal was audited and committed. After any
// other error no audit row is guaranteed, and "refused, audit action #0"
// would be false. Shared by the CLI and the HTTP API so both make the same
// "safe to report" decision.
func ReplayReportable(err error) bool {
	return err == nil || errors.Is(err, ErrNotReplayable)
}

// UndoReportable is ReplayReportable for Undo: both refusal kinds (not
// undoable, conflicts) are audited and committed before they return.
func UndoReportable(err error) bool {
	return err == nil || errors.Is(err, ErrNotUndoable) || errors.Is(err, ErrConflict)
}
