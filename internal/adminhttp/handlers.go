package adminhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/Deepak1365/relayguard/internal/admin"
)

// callCtx detaches the admin call from the request: a closed tab must not
// cancel a replay or undo half way and leave the operator unsure what
// happened. The timeout still bounds it.
func callCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), callTimeout)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{admin.ErrInvalidInput}, args...)...)
}

// pathID parses {id}; anything but a positive integer is invalid input, before
// the database is touched.
func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, invalid("inbox id must be a positive integer, got %q", raw)
	}
	return id, nil
}

// pageParams reads ?before= and ?limit=. Range rules (negative, cap) belong
// to internal/admin; this only rejects what is not a number.
func pageParams(r *http.Request) (before int64, limit int, err error) {
	q := r.URL.Query()
	if v := q.Get("before"); v != "" {
		if before, err = strconv.ParseInt(v, 10, 64); err != nil {
			return 0, 0, invalid("before must be an integer, got %q", v)
		}
	}
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil {
			return 0, 0, invalid("limit must be an integer, got %q", v)
		}
	}
	return before, limit, nil
}

// actionBody is the body of replay and undo. Unknown fields are rejected so a
// typo ("reasn") is a 400 and not a silently empty reason.
type actionBody struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

// decodeAction reads the body. A body over the cap is 413 (written here,
// returns false); any other problem is invalid input via the normal mapping.
func (s *Server) decodeAction(w http.ResponseWriter, r *http.Request, op string) (actionBody, bool) {
	var b actionBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&b)
	if err == nil {
		// Exactly one JSON value: trailing data is not silently ignored.
		if _, e := dec.Token(); !errors.Is(e, io.EOF) {
			err = e
			if err == nil {
				err = errors.New("unexpected data after the JSON object")
			}
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("request body is larger than %d bytes", maxBodyBytes), nil)
			return b, false
		}
		s.fail(w, op, invalid("body must be one JSON object {actor, reason}: %v", err), nil, false)
		return b, false
	}
	return b, true
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	before, limit, err := pageParams(r)
	if err != nil {
		s.fail(w, "list events", err, nil, false)
		return
	}
	ctx, cancel := callCtx(r)
	defer cancel()
	page, err := admin.ListEvents(ctx, s.pool, r.URL.Query().Get("status"), before, limit)
	if err != nil {
		s.fail(w, "list events", err, nil, false)
		return
	}
	writeJSON(w, http.StatusOK, toEventPage(page))
}

// listUnattributed is a separate route (not a query flag): the events no
// customer can be derived from (poison payloads and the like), so the
// dashboard can show them instead of hiding them.
func (s *Server) listUnattributed(w http.ResponseWriter, r *http.Request) {
	before, limit, err := pageParams(r)
	if err != nil {
		s.fail(w, "list unattributed", err, nil, false)
		return
	}
	ctx, cancel := callCtx(r)
	defer cancel()
	page, err := admin.ListUnattributed(ctx, s.pool, before, limit)
	if err != nil {
		s.fail(w, "list unattributed", err, nil, false)
		return
	}
	writeJSON(w, http.StatusOK, toEventPage(page))
}

func (s *Server) eventLog(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, "log", err, nil, false)
		return
	}
	ctx, cancel := callCtx(r)
	defer cancel()
	res, err := admin.Log(ctx, s.pool, id)
	if err != nil {
		s.fail(w, "log", err, nil, false)
		return
	}
	writeJSON(w, http.StatusOK, toLog(res))
}

func (s *Server) customerTimeline(w http.ResponseWriter, r *http.Request) {
	before, limit, err := pageParams(r)
	if err != nil {
		s.fail(w, "timeline", err, nil, false)
		return
	}
	ctx, cancel := callCtx(r)
	defer cancel()
	page, err := admin.CustomerTimeline(ctx, s.pool, r.PathValue("stripe_id"), before, limit)
	if err != nil {
		s.fail(w, "timeline", err, nil, false)
		return
	}
	writeJSON(w, http.StatusOK, toEventPage(page))
}

func (s *Server) listActions(w http.ResponseWriter, r *http.Request) {
	before, limit, err := pageParams(r)
	if err != nil {
		s.fail(w, "list actions", err, nil, false)
		return
	}
	ctx, cancel := callCtx(r)
	defer cancel()
	page, err := admin.ListActions(ctx, s.pool, before, limit)
	if err != nil {
		s.fail(w, "list actions", err, nil, false)
		return
	}
	writeJSON(w, http.StatusOK, toActionPage(page))
}

// preview is a POST although it writes nothing: it takes row locks, and a
// browser prefetch or crawler must not be able to trigger it with a GET. The
// body is not read.
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, "preview", err, nil, false)
		return
	}
	ctx, cancel := callCtx(r)
	defer cancel()
	res, err := admin.Preview(ctx, s.pool, id, s.opts)
	if err != nil {
		s.fail(w, "preview", err, nil, false)
		return
	}
	writeJSON(w, http.StatusOK, toPreview(res))
}

func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, "replay", err, nil, false)
		return
	}
	body, ok := s.decodeAction(w, r, "replay")
	if !ok {
		return
	}
	ctx, cancel := callCtx(r)
	defer cancel()
	res, err := admin.Replay(ctx, s.pool, admin.ReplayRequest{InboxID: id, Actor: body.Actor, Reason: body.Reason}, s.opts)
	if err != nil {
		s.fail(w, "replay", err, toReplay(res), admin.ReplayReportable(err))
		return
	}
	writeJSON(w, http.StatusOK, toReplay(res))
}

func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, "undo", err, nil, false)
		return
	}
	body, ok := s.decodeAction(w, r, "undo")
	if !ok {
		return
	}
	ctx, cancel := callCtx(r)
	defer cancel()
	res, err := admin.Undo(ctx, s.pool, admin.UndoRequest{InboxID: id, Actor: body.Actor, Reason: body.Reason}, s.opts)
	if err != nil {
		s.fail(w, "undo", err, toUndo(res), admin.UndoReportable(err))
		return
	}
	writeJSON(w, http.StatusOK, toUndo(res))
}
