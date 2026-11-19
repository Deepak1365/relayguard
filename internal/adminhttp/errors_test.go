package adminhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/changelog"
)

// Each case wraps the sentinel exactly as internal/admin does, so a change in
// the wrapping style or the switch order shows up here, not in production.
func TestMapError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"invalid input", fmt.Errorf("%w: actor is required", admin.ErrInvalidInput), 400, "invalid_input"},
		{"not found", fmt.Errorf("inbox 1: %w", admin.ErrNotFound), 404, "not_found"},
		{"not previewable", fmt.Errorf("inbox 1: %w", admin.ErrNotPreviewable), 409, "not_previewable"},
		{"undone", fmt.Errorf("inbox 1: %w", admin.ErrUndone), 409, "undone"},
		{"conflict", fmt.Errorf("inbox 1: %w: r", admin.ErrConflict), 409, "conflict"},
		{"not replayable", fmt.Errorf("inbox 1: %w: r", admin.ErrNotReplayable), 409, "not_replayable"},
		{"not undoable", fmt.Errorf("inbox 1: %w: r", admin.ErrNotUndoable), 409, "not_undoable"},
		{"changed with apply error", fmt.Errorf("apply: %w (x): %w", admin.ErrChanged, errors.New("boom")), 409, "changed"},
		{"changed plain", fmt.Errorf("%w: moved", admin.ErrChanged), 409, "changed"},
		{"busy", fmt.Errorf("lock inbox: %w (pg)", admin.ErrBusy), 503, "busy"},
		{"unknown", errors.New("pool closed"), 500, "internal"},
		// Precedence pin: if one error ever carries both, the more specific
		// conflict (it has a result worth showing) wins.
		{"conflict beats not undoable", fmt.Errorf("%w: %w", admin.ErrNotUndoable, admin.ErrConflict), 409, "conflict"},
	}
	for _, c := range cases {
		status, code := mapError(c.err)
		if status != c.status || code != c.code {
			t.Errorf("%s: mapError(%v) = %d %q, want %d %q", c.name, c.err, status, code, c.status, c.code)
		}
	}
}

// 409 changed and 503 busy wrap raw Postgres and apply error text; the body
// carries only the fixed sentence and the full error goes to the log.
func TestFailHidesWrappedDetail(t *testing.T) {
	var logged []string
	s := &Server{logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }}
	cases := []struct {
		err  error
		code string
		msg  string
	}{
		{fmt.Errorf("apply: %w (x): %w", admin.ErrChanged, errors.New("boom")), "changed", admin.ErrChanged.Error()},
		{fmt.Errorf("lock inbox: %w (boom: SQLSTATE 55P03)", admin.ErrBusy), "busy", admin.ErrBusy.Error()},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		s.fail(rec, "op", c.err, nil, false)
		var e errResp
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Error != c.code || e.Message != c.msg || strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("%s: body %s, want message %q and no wrapped detail", c.code, rec.Body.String(), c.msg)
		}
	}
	if len(logged) != 2 || !strings.Contains(logged[0], "boom") || !strings.Contains(logged[1], "SQLSTATE") {
		t.Errorf("log %q, want both full errors", logged)
	}
}

// The mux's own 404 and 405 are JSON {error, message} too, with the security
// headers, and 405 keeps its Allow header.
func TestMuxErrorsAreJSON(t *testing.T) {
	h := Handler(nil, "8090", admin.Options{}, nil)
	cases := []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", "/nope", 404, "no_route"},
		{"GET", "/api/events/1/replay", 405, "method_not_allowed"},
		{"DELETE", "/api/events", 405, "method_not_allowed"},
	}
	for _, c := range cases {
		hr := httptest.NewRequest(c.method, c.path, nil)
		hr.Host = "127.0.0.1:8090"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, hr)
		var e errResp
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("%s %s: body %q is not JSON: %v", c.method, c.path, rec.Body.String(), err)
		}
		if rec.Code != c.status || e.Error != c.code || e.Message == "" {
			t.Errorf("%s %s: %d %+v, want %d %q", c.method, c.path, rec.Code, e, c.status, c.code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: Content-Type %q, want application/json", c.method, c.path, ct)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s %s: security headers missing", c.method, c.path)
		}
		if c.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") == "" {
			t.Errorf("%s %s: Allow header missing on 405", c.method, c.path)
		}
	}
}

// Raw key names, decoded into map[string]any: decoding into Go structs would
// hide a renamed key (encoding/json matches case-insensitively).
func TestDTOWireShape(t *testing.T) {
	asMap := func(v any) map[string]any {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	keys := func(m map[string]any, want ...string) {
		t.Helper()
		if len(m) != len(want) {
			t.Errorf("keys %v, want exactly %v", m, want)
		}
		for _, k := range want {
			if _, ok := m[k]; !ok {
				t.Errorf("key %q missing in %v", k, m)
			}
		}
	}

	// Empty collections are [] and never null.
	page := asMap(toEventPage(admin.EventPage{}))
	keys(page, "events", "next_before")
	if ev, ok := page["events"].([]any); !ok || len(ev) != 0 {
		t.Errorf("empty event page events = %#v, want []", page["events"])
	}
	ap := asMap(toActionPage(admin.ActionPage{}))
	if a, ok := ap["actions"].([]any); !ok || len(a) != 0 {
		t.Errorf("empty action page actions = %#v, want []", ap["actions"])
	}
	lg := asMap(toLog(admin.LogResult{}))
	keys(lg, "inbox_id", "event_id", "event_type", "status", "attempts", "last_error", "changes", "actions")
	for _, k := range []string{"changes", "actions"} {
		if a, ok := lg[k].([]any); !ok || len(a) != 0 {
			t.Errorf("empty log %s = %#v, want []", k, lg[k])
		}
	}
	un := asMap(toUndo(admin.UndoResult{}))
	keys(un, "inbox_id", "event_id", "event_type", "status", "generation", "outcome", "reason",
		"action_id", "changes", "conflicts", "warnings")
	for _, k := range []string{"changes", "conflicts", "warnings"} {
		if a, ok := un[k].([]any); !ok || len(a) != 0 {
			t.Errorf("empty undo %s = %#v, want []", k, un[k])
		}
	}
	pv := asMap(toPreview(admin.PreviewResult{}))
	keys(pv, "inbox_id", "event_id", "event_type", "status", "attempts", "generation", "outcome", "reason", "changes")
	if a, ok := pv["changes"].([]any); !ok || len(a) != 0 {
		t.Errorf("empty preview changes = %#v, want []", pv["changes"])
	}
	rp := asMap(toReplay(admin.ReplayResult{}))
	keys(rp, "inbox_id", "event_id", "event_type", "outcome", "reason", "action_id", "from_status",
		"from_attempts", "from_last_error", "from_processed_at", "generation")

	// Images and audit detail are embedded JSON, not base64 or a string.
	actionID := int64(7)
	now := time.Now()
	lg = asMap(toLog(admin.LogResult{
		Changes: []admin.LoggedChange{{ID: 1, Generation: 0, Source: "apply", AdminActionID: &actionID, Seq: 1, CreatedAt: now,
			Change: changelog.Change{Table: "customers", RowID: 3, Op: changelog.OpUpdate,
				Before: []byte(`{"id":3,"credit":1}`), After: []byte(`{"id":3,"credit":2}`)}}},
		Actions: []admin.AdminAction{{ID: 7, InboxID: 1, Action: "undo", Actor: "a", Reason: "r", Outcome: "done",
			Detail: `{"status":"done"}`, CreatedAt: now}},
	}))
	ch := lg["changes"].([]any)[0].(map[string]any)
	keys(ch, "id", "generation", "source", "admin_action_id", "seq", "created_at", "table", "row_id", "op", "before", "after")
	for _, k := range []string{"before", "after"} {
		if img, ok := ch[k].(map[string]any); !ok || img["id"] != float64(3) {
			t.Errorf("%s image = %#v, want a JSON object", k, ch[k])
		}
	}
	act := lg["actions"].([]any)[0].(map[string]any)
	keys(act, "id", "inbox_id", "action", "actor", "reason", "outcome", "detail", "created_at")
	if d, ok := act["detail"].(map[string]any); !ok || d["status"] != "done" {
		t.Errorf("action detail = %#v, want a JSON object", act["detail"])
	}

	// An insert has no before image: null, not an empty string.
	ins := asMap(toChange(changelog.Change{Table: "customers", RowID: 1, Op: changelog.OpInsert, After: []byte(`{"id":1}`)}))
	if v, ok := ins["before"]; !ok || v != nil {
		t.Errorf("insert before = %#v, want null", v)
	}
}
