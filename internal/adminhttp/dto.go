package adminhttp

// Response shapes. internal/admin's result structs carry no JSON tags and hold
// JSON as []byte or string, so encoding them directly would give Go field
// names, base64 images and double-encoded text. These DTOs fix the wire format
// in one explicit place: snake_case keys, images and audit detail embedded as
// JSON, and lists that are [] and never null. They live here, not in
// internal/admin, so the CLI and the library stay untouched.

import (
	"encoding/json"
	"time"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/changelog"
)

// rawJSON embeds stored JSON text as JSON. Empty or invalid text becomes null
// rather than failing the whole response.
func rawJSON[T ~string | ~[]byte](b T) json.RawMessage {
	if len(b) == 0 || !json.Valid([]byte(b)) {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

type changeDTO struct {
	Table  string          `json:"table"`
	RowID  int64           `json:"row_id"`
	Op     string          `json:"op"`
	Before json.RawMessage `json:"before"` // null for an insert
	After  json.RawMessage `json:"after"`  // null for a delete
}

func toChange(c changelog.Change) changeDTO {
	return changeDTO{Table: c.Table, RowID: c.RowID, Op: c.Op, Before: rawJSON(c.Before), After: rawJSON(c.After)}
}

func toChanges(cs []changelog.Change) []changeDTO {
	out := make([]changeDTO, 0, len(cs))
	for _, c := range cs {
		out = append(out, toChange(c))
	}
	return out
}

func conflicts(cs []admin.Conflict) []admin.Conflict {
	if cs == nil {
		return []admin.Conflict{} // admin.Conflict already has snake_case tags
	}
	return cs
}

func warnings(ws []string) []string {
	if ws == nil {
		return []string{}
	}
	return ws
}

type loggedChangeDTO struct {
	ID            int64     `json:"id"`
	Generation    int       `json:"generation"`
	Source        string    `json:"source"`
	AdminActionID *int64    `json:"admin_action_id"`
	Seq           int       `json:"seq"`
	CreatedAt     time.Time `json:"created_at"`
	changeDTO
}

type actionDTO struct {
	ID        int64           `json:"id"`
	InboxID   int64           `json:"inbox_id"`
	Action    string          `json:"action"`
	Actor     string          `json:"actor"`
	Reason    string          `json:"reason"`
	Outcome   string          `json:"outcome"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt time.Time       `json:"created_at"`
}

func toAction(a admin.AdminAction) actionDTO {
	return actionDTO{ID: a.ID, InboxID: a.InboxID, Action: a.Action, Actor: a.Actor, Reason: a.Reason,
		Outcome: a.Outcome, Detail: rawJSON(a.Detail), CreatedAt: a.CreatedAt}
}

func toActions(as []admin.AdminAction) []actionDTO {
	out := make([]actionDTO, 0, len(as))
	for _, a := range as {
		out = append(out, toAction(a))
	}
	return out
}

type logDTO struct {
	InboxID   int64             `json:"inbox_id"`
	EventID   string            `json:"event_id"`
	EventType string            `json:"event_type"`
	Status    string            `json:"status"`
	Attempts  int               `json:"attempts"`
	LastError string            `json:"last_error"`
	Changes   []loggedChangeDTO `json:"changes"`
	Actions   []actionDTO       `json:"actions"`
}

func toLog(r admin.LogResult) logDTO {
	d := logDTO{InboxID: r.InboxID, EventID: r.EventID, EventType: r.EventType, Status: r.Status,
		Attempts: r.Attempts, LastError: r.LastError,
		Changes: make([]loggedChangeDTO, 0, len(r.Changes)), Actions: toActions(r.Actions)}
	for _, c := range r.Changes {
		d.Changes = append(d.Changes, loggedChangeDTO{ID: c.ID, Generation: c.Generation, Source: c.Source,
			AdminActionID: c.AdminActionID, Seq: c.Seq, CreatedAt: c.CreatedAt, changeDTO: toChange(c.Change)})
	}
	return d
}

type eventSummaryDTO struct {
	ID           int64     `json:"id"`
	EventID      string    `json:"event_id"`
	EventType    string    `json:"event_type"`
	Status       string    `json:"status"`
	Attempts     int       `json:"attempts"`
	LastError    string    `json:"last_error"`
	ReceivedAt   time.Time `json:"received_at"`
	EventCreated *int64    `json:"event_created"`
	CustomerID   string    `json:"customer_id"`
}

type eventPageDTO struct {
	Events     []eventSummaryDTO `json:"events"`
	NextBefore int64             `json:"next_before"`
}

func toEventPage(p admin.EventPage) eventPageDTO {
	d := eventPageDTO{Events: make([]eventSummaryDTO, 0, len(p.Events)), NextBefore: p.NextBefore}
	for _, e := range p.Events {
		d.Events = append(d.Events, eventSummaryDTO{ID: e.ID, EventID: e.EventID, EventType: e.EventType,
			Status: e.Status, Attempts: e.Attempts, LastError: e.LastError, ReceivedAt: e.ReceivedAt,
			EventCreated: e.EventCreated, CustomerID: e.CustomerID})
	}
	return d
}

type actionPageDTO struct {
	Actions    []actionDTO `json:"actions"`
	NextBefore int64       `json:"next_before"`
}

func toActionPage(p admin.ActionPage) actionPageDTO {
	return actionPageDTO{Actions: toActions(p.Actions), NextBefore: p.NextBefore}
}

type previewDTO struct {
	InboxID    int64       `json:"inbox_id"`
	EventID    string      `json:"event_id"`
	EventType  string      `json:"event_type"`
	Status     string      `json:"status"`
	Attempts   int         `json:"attempts"`
	Generation int         `json:"generation"`
	Outcome    string      `json:"outcome"`
	Reason     string      `json:"reason"`
	Changes    []changeDTO `json:"changes"`
}

func toPreview(r admin.PreviewResult) previewDTO {
	return previewDTO{InboxID: r.InboxID, EventID: r.EventID, EventType: r.EventType, Status: r.Status,
		Attempts: r.Attempts, Generation: r.Generation, Outcome: r.Outcome, Reason: r.Reason,
		Changes: toChanges(r.Changes)}
}

type replayDTO struct {
	InboxID         int64      `json:"inbox_id"`
	EventID         string     `json:"event_id"`
	EventType       string     `json:"event_type"`
	Outcome         string     `json:"outcome"`
	Reason          string     `json:"reason"`
	ActionID        int64      `json:"action_id"`
	FromStatus      string     `json:"from_status"`
	FromAttempts    int        `json:"from_attempts"`
	FromLastError   string     `json:"from_last_error"`
	FromProcessedAt *time.Time `json:"from_processed_at"`
	Generation      int        `json:"generation"`
}

func toReplay(r admin.ReplayResult) replayDTO {
	return replayDTO{InboxID: r.InboxID, EventID: r.EventID, EventType: r.EventType, Outcome: r.Outcome,
		Reason: r.Reason, ActionID: r.ActionID, FromStatus: r.FromStatus, FromAttempts: r.FromAttempts,
		FromLastError: r.FromLastError, FromProcessedAt: r.FromProcessedAt, Generation: r.Generation}
}

type undoDTO struct {
	InboxID    int64            `json:"inbox_id"`
	EventID    string           `json:"event_id"`
	EventType  string           `json:"event_type"`
	Status     string           `json:"status"`
	Generation int              `json:"generation"`
	Outcome    string           `json:"outcome"`
	Reason     string           `json:"reason"`
	ActionID   int64            `json:"action_id"`
	Changes    []changeDTO      `json:"changes"`
	Conflicts  []admin.Conflict `json:"conflicts"`
	Warnings   []string         `json:"warnings"`
}

func toUndo(r admin.UndoResult) undoDTO {
	return undoDTO{InboxID: r.InboxID, EventID: r.EventID, EventType: r.EventType, Status: r.Status,
		Generation: r.Generation, Outcome: r.Outcome, Reason: r.Reason, ActionID: r.ActionID,
		Changes: toChanges(r.Changes), Conflicts: conflicts(r.Conflicts), Warnings: warnings(r.Warnings)}
}
