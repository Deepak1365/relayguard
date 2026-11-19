package adminhttp

// DB tests for the HTTP layer, against httptest requests (no real socket).
// Every security rejection is checked for zero side effects: the audit table
// must not gain a row.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/admin"
	"github.com/Deepak1365/relayguard/internal/db"
	"github.com/Deepak1365/relayguard/internal/dbtest"
)

const (
	testPort     = "8090"
	testCustomer = "cus_dbtest_http"
)

const goodBody = `{"actor":"alice","reason":"because"}`

// req is one request to the handler. Zero Host means 127.0.0.1:testPort; a
// POST gets Content-Type application/json unless ctype is set.
type req struct {
	method, path, body string
	host, ctype        string
	headers            map[string]string
}

func do(h http.Handler, r req) *httptest.ResponseRecorder {
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	hr.Host = "127.0.0.1:" + testPort
	if r.host != "" {
		hr.Host = r.host
	}
	if r.method == http.MethodPost {
		hr.Header.Set("Content-Type", "application/json")
	}
	if r.ctype != "" {
		hr.Header.Set("Content-Type", r.ctype)
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)
	return rec
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	return do(h, req{method: "POST", path: path, body: body})
}

// errResp is the error body; Result is decoded per test.
type errResp struct {
	Error   string          `json:"error"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errResp {
	t.Helper()
	var e errResp
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q is not JSON: %v", rec.Body.String(), err)
	}
	return e
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) errResp {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	if code == "" {
		return errResp{}
	}
	e := decodeErr(t, rec)
	if e.Error != code || e.Message == "" {
		t.Fatalf("error body %+v, want code %q with a message", e, code)
	}
	return e
}

func audits(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return dbtest.Count(t, pool, "admin_actions")
}

func newHandler(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.Pool(t)
	return Handler(pool, testPort, admin.Options{}, t.Logf), pool
}

func deadEvent(t *testing.T, pool *pgxpool.Pool, suffix string) int64 {
	t.Helper()
	id := dbtest.InsertEvent(t, pool, suffix, "invoice.paid", dbtest.InvoiceObject("in_dbtest_"+suffix, testCustomer, 2000))
	if _, err := pool.Exec(context.Background(), `
		UPDATE inbox SET status = 'dead', attempts = 8, last_error = 'unknown customer', processed_at = now(),
			next_attempt_at = now() + interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func appliedEvent(t *testing.T, pool *pgxpool.Pool, suffix, typ string, obj map[string]any) int64 {
	t.Helper()
	id := dbtest.InsertEvent(t, pool, suffix, typ, obj)
	if got := dbtest.ApplyInbox(t, pool, id); got != "done" {
		t.Fatalf("apply %s: %s", suffix, got)
	}
	return id
}

func path(id int64, verb string) string { return fmt.Sprintf("/api/events/%d/%s", id, verb) }

func TestDBReplayOK(t *testing.T) {
	h, pool := newHandler(t)
	id := deadEvent(t, pool, "ok")
	rec := post(h, path(id, "replay"), goodBody)
	wantStatus(t, rec, 200, "")
	var res replayDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != admin.ReplayDone || res.ActionID == 0 || res.InboxID != id {
		t.Fatalf("result %+v, want a done replay with an action id", res)
	}
	log, err := admin.Log(context.Background(), pool, id)
	if err != nil || len(log.Actions) != 1 || log.Actions[0].ID != res.ActionID ||
		log.Actions[0].Actor != "alice" || log.Actions[0].Outcome != "done" {
		t.Fatalf("audit rows %+v, %v; want one done row for alice with the returned id", log.Actions, err)
	}
	if log.Status != "pending" {
		t.Fatalf("status %q after replay, want pending", log.Status)
	}
}

func TestDBReplayRefusedIsAudited(t *testing.T) {
	h, pool := newHandler(t)
	id := appliedEvent(t, pool, "done", "customer.created", dbtest.CustomerObject(testCustomer))
	rec := post(h, path(id, "replay"), goodBody)
	e := wantStatus(t, rec, 409, "not_replayable")
	var res replayDTO
	if err := json.Unmarshal(e.Result, &res); err != nil {
		t.Fatalf("result %s: %v", e.Result, err)
	}
	if res.Outcome != admin.ReplayRefused || res.ActionID == 0 {
		t.Fatalf("result %+v, want a refusal with an action id", res)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_actions WHERE id = $1 AND inbox_id = $2 AND outcome = 'refused'`,
		res.ActionID, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("admin_actions row for action_id %d: count %d, %v; want 1 refused row", res.ActionID, n, err)
	}
}

func TestDBInvalidInput400NoAudit(t *testing.T) {
	h, pool := newHandler(t)
	id := deadEvent(t, pool, "inv")
	before := audits(t, pool)
	cases := []struct{ name, path, body string }{
		{"missing actor", path(id, "replay"), `{"reason":"r"}`},
		{"blank reason", path(id, "undo"), `{"actor":"a","reason":"   "}`},
		{"not json", path(id, "replay"), `not json`},
		{"unknown field", path(id, "replay"), `{"actor":"a","reason":"r","extra":1}`},
		{"trailing data", path(id, "replay"), goodBody + `{}`},
		{"non-numeric id", "/api/events/abc/replay", goodBody},
		{"zero id", "/api/events/0/undo", goodBody},
		{"non-numeric id preview", "/api/events/x/preview", ""},
	}
	for _, c := range cases {
		wantStatus(t, post(h, c.path, c.body), 400, "invalid_input")
		if n := audits(t, pool); n != before {
			t.Fatalf("%s: admin_actions %d -> %d, want unchanged", c.name, before, n)
		}
	}
	for _, p := range []string{"/api/events/abc", "/api/events?limit=x", "/api/events?before=x",
		"/api/events?limit=-1", "/api/events?status=bogus", "/api/actions?before=-5"} {
		wantStatus(t, do(h, req{method: "GET", path: p}), 400, "invalid_input")
	}
}

func TestDBNotFound404(t *testing.T) {
	h, pool := newHandler(t)
	before := audits(t, pool)
	// The handler's own 404 must survive jsonErrors (which rewrites only the
	// mux's plain-text 404 to no_route): check code AND message.
	for _, rec := range []*httptest.ResponseRecorder{
		post(h, path(999999, "replay"), goodBody),
		post(h, path(999999, "undo"), goodBody),
		post(h, path(999999, "preview"), ""),
		do(h, req{method: "GET", path: "/api/events/999999"}),
	} {
		e := wantStatus(t, rec, 404, "not_found")
		if !strings.Contains(e.Message, admin.ErrNotFound.Error()) {
			t.Fatalf("404 message %q, want it to contain %q", e.Message, admin.ErrNotFound.Error())
		}
	}
	if n := audits(t, pool); n != before {
		t.Fatalf("admin_actions %d -> %d for unknown events, want unchanged", before, n)
	}
}

func TestDBGetOnPostRoute405(t *testing.T) {
	h, pool := newHandler(t)
	id := deadEvent(t, pool, "405")
	for _, verb := range []string{"replay", "undo", "preview"} {
		rec := do(h, req{method: "GET", path: path(id, verb)})
		if rec.Code != 405 {
			t.Fatalf("GET %s: status %d, want 405", verb, rec.Code)
		}
	}
	if audits(t, pool) != 0 || dbtest.ReadRow(t, pool, id).Status != "dead" {
		t.Fatal("a GET on a POST route changed something")
	}
}

func TestDBCrossSitePost403(t *testing.T) {
	h, pool := newHandler(t)
	id := deadEvent(t, pool, "csrf")
	attacks := []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "http://evil.example"},
		{"Origin": "http://127.0.0.1:" + testPort + ".evil.example"},
	}
	for _, hdr := range attacks {
		rec := do(h, req{method: "POST", path: path(id, "replay"), body: goodBody, headers: hdr})
		wantStatus(t, rec, 403, "cross_origin")
		if n := audits(t, pool); n != 0 {
			t.Fatalf("%v: %d audit rows after a cross-site POST, want 0", hdr, n)
		}
		if got := dbtest.ReadRow(t, pool, id).Status; got != "dead" {
			t.Fatalf("%v: status %q after a cross-site POST, want dead", hdr, got)
		}
	}
	// Control: the same request from the dashboard's own page succeeds.
	rec := do(h, req{method: "POST", path: path(id, "replay"), body: goodBody,
		headers: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:" + testPort}})
	wantStatus(t, rec, 200, "")
	if audits(t, pool) != 1 {
		t.Fatal("same-origin control: want one audit row")
	}
}

func TestDBWrongHost403(t *testing.T) {
	h, pool := newHandler(t)
	id := deadEvent(t, pool, "host")
	for _, host := range []string{"evil.example:" + testPort, "127.0.0.1:9999", "127.0.0.1", "localhost", "127.0.0.1." + "evil.example:" + testPort, ""} {
		hr := httptest.NewRequest("POST", path(id, "replay"), strings.NewReader(goodBody))
		hr.Host = host
		hr.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, hr)
		wantStatus(t, rec, 403, "bad_host")
		if n := audits(t, pool); n != 0 {
			t.Fatalf("host %q: %d audit rows, want 0", host, n)
		}
	}
	// A GET with a bad host must not read data either.
	hr := httptest.NewRequest("GET", "/api/events", nil)
	hr.Host = "evil.example:" + testPort
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)
	wantStatus(t, rec, 403, "bad_host")
	// Control: both allowed names work (localhost case-insensitively).
	wantStatus(t, do(h, req{method: "GET", path: "/api/events", host: "localhost:" + testPort}), 200, "")
	wantStatus(t, do(h, req{method: "GET", path: "/api/events", host: "LocalHost:" + testPort}), 200, "")
	wantStatus(t, do(h, req{method: "POST", path: path(id, "replay"), body: goodBody, host: "localhost:" + testPort}), 200, "")
	if audits(t, pool) != 1 {
		t.Fatal("control: want one audit row")
	}
}

func TestDBWrongContentType415(t *testing.T) {
	h, pool := newHandler(t)
	id := deadEvent(t, pool, "ctype")
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/jsonx", "bogus;;"} {
		wantStatus(t, do(h, req{method: "POST", path: path(id, "replay"), body: goodBody, ctype: ct}), 415, "unsupported_media_type")
		if n := audits(t, pool); n != 0 {
			t.Fatalf("content type %q: %d audit rows, want 0", ct, n)
		}
	}
	// No Content-Type at all.
	hr := httptest.NewRequest("POST", path(id, "replay"), strings.NewReader(goodBody))
	hr.Host = "127.0.0.1:" + testPort
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)
	wantStatus(t, rec, 415, "unsupported_media_type")
	if audits(t, pool) != 0 {
		t.Fatal("missing content type: want 0 audit rows")
	}
	// Control, with a charset parameter.
	wantStatus(t, do(h, req{method: "POST", path: path(id, "replay"), body: goodBody, ctype: "application/json; charset=utf-8"}), 200, "")
}

func TestDBBodyTooLarge413(t *testing.T) {
	h, pool := newHandler(t)
	id := deadEvent(t, pool, "big")
	big := fmt.Sprintf(`{"actor":"a","reason":%q}`, strings.Repeat("x", 20<<10))
	wantStatus(t, post(h, path(id, "replay"), big), 413, "body_too_large")
	wantStatus(t, post(h, path(id, "undo"), big), 413, "body_too_large")
	if n := audits(t, pool); n != 0 {
		t.Fatalf("%d audit rows after oversized bodies, want 0", n)
	}
	// Just under the cap still reaches admin validation (reason too long: 400).
	under := fmt.Sprintf(`{"actor":"a","reason":%q}`, strings.Repeat("x", 15<<10))
	wantStatus(t, post(h, path(id, "replay"), under), 400, "invalid_input")
}

func TestDBUndoConflict409(t *testing.T) {
	h, pool := newHandler(t)
	appliedEvent(t, pool, "cus", "customer.created", dbtest.CustomerObject(testCustomer))
	inv1 := appliedEvent(t, pool, "inv1", "invoice.paid", dbtest.InvoiceObject("in_dbtest_1", testCustomer, 2000))
	appliedEvent(t, pool, "inv2", "invoice.paid", dbtest.InvoiceObject("in_dbtest_2", testCustomer, 3000))

	rec := post(h, path(inv1, "undo"), goodBody)
	e := wantStatus(t, rec, 409, "conflict")
	var res undoDTO
	if err := json.Unmarshal(e.Result, &res); err != nil {
		t.Fatalf("result %s: %v", e.Result, err)
	}
	if res.Outcome != admin.UndoRefused || res.ActionID == 0 || len(res.Conflicts) == 0 {
		t.Fatalf("result %+v, want a refusal with conflicts and an action id", res)
	}
	for _, c := range res.Conflicts {
		if c.Kind == "" || strings.TrimSpace(c.Explanation) == "" {
			t.Fatalf("conflict %+v: kind and explanation must be non-empty", c)
		}
	}
	// The audit detail holds the same conflicts.
	log, err := admin.Log(context.Background(), pool, inv1)
	if err != nil || len(log.Actions) != 1 || log.Actions[0].ID != res.ActionID {
		t.Fatalf("audit rows %+v, %v; want the refusal with the returned id", log.Actions, err)
	}
	var detail struct {
		Conflicts []admin.Conflict `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(log.Actions[0].Detail), &detail); err != nil {
		t.Fatalf("detail %q: %v", log.Actions[0].Detail, err)
	}
	if len(detail.Conflicts) != len(res.Conflicts) {
		t.Fatalf("detail has %d conflicts, response %d", len(detail.Conflicts), len(res.Conflicts))
	}
	for i, c := range res.Conflicts {
		d := detail.Conflicts[i]
		if d.Kind != c.Kind || d.Explanation != c.Explanation {
			t.Fatalf("conflict %d: response {%s %q}, audit {%s %q}; want identical", i, c.Kind, c.Explanation, d.Kind, d.Explanation)
		}
	}
}

func TestDBPreviewAndReads(t *testing.T) {
	h, pool := newHandler(t)
	cus := appliedEvent(t, pool, "cus", "customer.created", dbtest.CustomerObject(testCustomer))
	inv := dbtest.InsertEvent(t, pool, "inv", "invoice.paid", dbtest.InvoiceObject("in_dbtest_p", testCustomer, 2000))
	poison := dbtest.InsertEvent(t, pool, "poison", "test.poison", map[string]any{"id": "x"})

	var pv previewDTO
	rec := post(h, path(inv, "preview"), "")
	wantStatus(t, rec, 200, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &pv); err != nil || pv.Outcome != admin.OutcomeApply || len(pv.Changes) == 0 {
		t.Fatalf("preview %+v, %v; want an apply with changes", pv, err)
	}
	if dbtest.ReadRow(t, pool, inv).Status != "pending" || audits(t, pool) != 0 {
		t.Fatal("preview wrote something")
	}
	wantStatus(t, post(h, path(cus, "preview"), ""), 409, "not_previewable")

	var page eventPageDTO
	rec = do(h, req{method: "GET", path: "/api/events?status=done"})
	wantStatus(t, rec, 200, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Events) != 1 || page.Events[0].ID != cus {
		t.Fatalf("status=done page %+v, %v", page, err)
	}
	rec = do(h, req{method: "GET", path: "/api/events/unattributed"})
	wantStatus(t, rec, 200, "")
	page = eventPageDTO{}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Events) != 1 || page.Events[0].ID != poison {
		t.Fatalf("unattributed page %+v, %v; want only the poison event %d", page, err, poison)
	}
	rec = do(h, req{method: "GET", path: "/api/customers/" + testCustomer + "/timeline"})
	wantStatus(t, rec, 200, "")
	page = eventPageDTO{}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Events) != 2 {
		t.Fatalf("timeline %+v, %v; want the customer's 2 events", page, err)
	}
	wantStatus(t, do(h, req{method: "GET", path: fmt.Sprintf("/api/events/%d", inv)}), 200, "")
	wantStatus(t, do(h, req{method: "GET", path: "/api/actions"}), 200, "")
}

// Another transaction holds the row lock (as a worker fence would): replay
// and undo give up after the short lock timeout with 503 + Retry-After, and
// write nothing. Timing dependent, so it is run with -count=20.
func TestDBBusy503(t *testing.T) {
	pool := dbtest.Pool(t)
	h := Handler(pool, testPort, admin.Options{LockTimeout: 200 * time.Millisecond, StatementTimeout: 3 * time.Second}, t.Logf)
	ctx := context.Background()
	id := deadEvent(t, pool, "busy")

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) // before the cleanup Truncate
	if _, err := other.Exec(ctx, `SELECT 1 FROM inbox WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"replay", "undo"} {
		start := time.Now()
		rec := post(h, path(id, verb), goodBody)
		e := wantStatus(t, rec, 503, "busy")
		if rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("%s: Retry-After %q, want 1", verb, rec.Header().Get("Retry-After"))
		}
		if len(e.Result) != 0 {
			t.Fatalf("%s: busy response carries a result %s, want none (nothing audited)", verb, e.Result)
		}
		// The wrapped error carries the raw Postgres text; the client gets the
		// fixed sentence only.
		if e.Message != admin.ErrBusy.Error() {
			t.Fatalf("%s: busy message %q, want exactly %q", verb, e.Message, admin.ErrBusy.Error())
		}
		if el := time.Since(start); el > 3*time.Second {
			t.Fatalf("%s: busy after %s, want about the 200ms lock timeout", verb, el)
		}
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := audits(t, pool); n != 0 {
		t.Fatalf("%d audit rows after busy requests, want 0", n)
	}
}

// An unexpected error is a 500 with generic text; the detail goes to the
// server log only. A closed pool is the unexpected error.
func TestDBInternalErrorNotLeaked(t *testing.T) {
	dbtest.Pool(t) // skips unless DB tests are enabled
	broken, err := db.Connect(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()
	var mu sync.Mutex
	var logged []string
	h := Handler(broken, testPort, admin.Options{}, func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(f, a...))
	})
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/events/5", ""},
		{"GET", "/api/events", ""},
		{"POST", "/api/events/5/replay", goodBody},
		{"POST", "/api/events/5/preview", ""},
	} {
		rec := do(h, req{method: c.method, path: c.path, body: c.body})
		e := wantStatus(t, rec, 500, "internal")
		if strings.Contains(strings.ToLower(rec.Body.String()), "pool") || len(e.Result) != 0 {
			t.Fatalf("%s %s: 500 body leaks detail: %s", c.method, c.path, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 4 || !strings.Contains(strings.ToLower(strings.Join(logged, "\n")), "pool") {
		t.Fatalf("server log %q, want 4 entries with the real error", logged)
	}
}

// Undoing an already-undone event is refused 409 not_undoable with an audited
// refusal; previewing it is 409 undone.
func TestDBUndoTwiceAndPreviewUndone(t *testing.T) {
	h, pool := newHandler(t)
	id := appliedEvent(t, pool, "twice", "customer.created", dbtest.CustomerObject(testCustomer))
	wantStatus(t, post(h, path(id, "undo"), goodBody), 200, "")

	rec := post(h, path(id, "undo"), goodBody)
	e := wantStatus(t, rec, 409, "not_undoable")
	var res undoDTO
	if err := json.Unmarshal(e.Result, &res); err != nil {
		t.Fatalf("result %s: %v", e.Result, err)
	}
	if res.Outcome != admin.UndoRefused || res.ActionID == 0 {
		t.Fatalf("result %+v, want a refusal with an action id", res)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_actions WHERE id = $1 AND inbox_id = $2 AND outcome = 'refused'`,
		res.ActionID, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("admin_actions row for action_id %d: count %d, %v; want 1 refused row", res.ActionID, n, err)
	}
	wantStatus(t, post(h, path(id, "preview"), ""), 409, "undone")
}

// Raw keys on real responses (decoded into maps, not structs).
func TestDBWireKeysOnRealResponses(t *testing.T) {
	h, pool := newHandler(t)
	cus := appliedEvent(t, pool, "cus", "customer.created", dbtest.CustomerObject(testCustomer))

	get := func(p string) map[string]any {
		t.Helper()
		rec := do(h, req{method: "GET", path: p})
		wantStatus(t, rec, 200, "")
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	lg := get(fmt.Sprintf("/api/events/%d", cus))
	changes, _ := lg["changes"].([]any)
	if len(changes) == 0 {
		t.Fatalf("log %v has no changes", lg)
	}
	ch := changes[0].(map[string]any)
	if _, ok := ch["after"].(map[string]any); !ok || ch["before"] != nil {
		t.Fatalf("insert change %v: want after as a JSON object and before null", ch)
	}
	for _, k := range []string{"table", "row_id", "op", "admin_action_id", "created_at"} {
		if _, ok := ch[k]; !ok {
			t.Fatalf("change key %q missing in %v", k, ch)
		}
	}
	wantStatus(t, post(h, path(cus, "undo"), goodBody), 200, "")
	lg = get(fmt.Sprintf("/api/events/%d", cus))
	acts, _ := lg["actions"].([]any)
	if len(acts) != 1 {
		t.Fatalf("log actions %v, want 1", lg["actions"])
	}
	if d, ok := acts[0].(map[string]any)["detail"].(map[string]any); !ok || len(d) == 0 {
		t.Fatalf("action detail %v, want a JSON object", acts[0])
	}
	empty := get("/api/events?status=dead")
	if ev, ok := empty["events"].([]any); !ok || len(ev) != 0 {
		t.Fatalf("empty page events = %#v, want []", empty["events"])
	}
	if _, ok := get("/api/events")["next_before"]; !ok {
		t.Fatal("next_before missing")
	}
}
