package adminhttp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Deepak1365/relayguard/internal/admin"
)

// errorBody is the body of every error response. Result is the admin result
// (audit action id, conflicts), present only when the outcome was audited.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Result  any    `json:"result,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The header is sent; an encode failure (client gone) cannot be reported.
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string, result any) {
	writeJSON(w, status, errorBody{Error: code, Message: message, Result: result})
}

// jsonErrors makes the mux's own 404 and 405 answers JSON like every other
// error. The mux writes plain text through the ResponseWriter; this wrapper
// notices those (a text/plain error status with no handler involvement) and
// replaces them. Handler-written errors are already application/json, so they
// pass through untouched.
type jsonErrorWriter struct {
	http.ResponseWriter
	replaced bool
}

func (w *jsonErrorWriter) WriteHeader(status int) {
	if (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) &&
		w.Header().Get("Content-Type") != "application/json" {
		w.replaced = true
		// Keep Allow (405); drop the text headers the mux set.
		w.Header().Del("Content-Length")
		code, msg := "no_route", "no such route"
		if status == http.StatusMethodNotAllowed {
			code, msg = "method_not_allowed", "method not allowed for this route"
		}
		writeError(w.ResponseWriter, status, code, msg, nil)
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *jsonErrorWriter) Write(b []byte) (int, error) {
	if w.replaced {
		return len(b), nil // discard the mux's plain-text body
	}
	return w.ResponseWriter.Write(b)
}

func jsonErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&jsonErrorWriter{ResponseWriter: w}, r)
	})
}

// mapError turns an admin error into status and machine-readable code. The
// order matters only where one error wraps two sentinels; ErrConflict goes
// before ErrNotUndoable for that reason.
func mapError(err error) (status int, code string) {
	switch {
	case errors.Is(err, admin.ErrInvalidInput):
		return http.StatusBadRequest, "invalid_input"
	case errors.Is(err, admin.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, admin.ErrNotPreviewable):
		return http.StatusConflict, "not_previewable"
	case errors.Is(err, admin.ErrUndone):
		return http.StatusConflict, "undone"
	case errors.Is(err, admin.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, admin.ErrNotReplayable):
		return http.StatusConflict, "not_replayable"
	case errors.Is(err, admin.ErrNotUndoable):
		return http.StatusConflict, "not_undoable"
	case errors.Is(err, admin.ErrChanged):
		return http.StatusConflict, "changed"
	case errors.Is(err, admin.ErrBusy):
		return http.StatusServiceUnavailable, "busy"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// fail writes the error response. result is attached only when reportable:
// otherwise no audit row is guaranteed and "refused, action #0" would be a
// lie. Unknown errors are logged here and never sent to the client.
func (s *Server) fail(w http.ResponseWriter, op string, err error, result any, reportable bool) {
	status, code := mapError(err)
	msg := err.Error()
	switch {
	case status == http.StatusInternalServerError:
		s.logf("adminhttp: %s: %v", op, err)
		msg = "internal error; details are in the server log"
	case status == http.StatusServiceUnavailable:
		// The wrapped text carries the raw Postgres error; keep it in the log.
		s.logf("adminhttp: %s: %v", op, err)
		msg = admin.ErrBusy.Error()
		w.Header().Set("Retry-After", "1")
	case code == "changed":
		// Wraps the apply error and Postgres text (a payload value, at worst).
		s.logf("adminhttp: %s: %v", op, err)
		msg = admin.ErrChanged.Error()
	}
	if !reportable {
		result = nil
	}
	writeError(w, status, code, msg, result)
}
