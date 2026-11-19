// Package adminhttp is the localhost dashboard's JSON API. It is a thin layer
// over internal/admin: handlers decode the request, call the same audited
// functions the CLI calls, and map the error to a status. All business rules
// and all locking stay in internal/admin.
//
// Threat model: the API can replay and undo events, and it has no
// authentication (out of scope: single operator, same machine). What it must
// stop is a web page in the operator's own browser reaching it: CSRF (any
// site can make the browser POST to localhost) and DNS rebinding (a hostile
// name that resolves to 127.0.0.1 makes the page same-origin). Hence the
// loopback-only bind, the Host allow-list, cross-origin protection, and the
// JSON-only content type on POSTs.
package adminhttp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/admin"
)

const (
	// maxBodyBytes caps a POST body. A replay or undo body is two short
	// strings (actor <= 100, reason <= a few hundred chars).
	maxBodyBytes = 16 << 10
	// callTimeout bounds one admin call, as the CLI does.
	callTimeout = 30 * time.Second
)

// Server holds what the handlers need.
type Server struct {
	pool *pgxpool.Pool
	opts admin.Options // lock/statement timeouts passed to Preview, Replay, Undo
	logf func(format string, args ...any)
}

// ParseAddr validates the listen address and returns host and port. Only
// 127.0.0.1 is accepted, with an explicit port 1-65535. Refused: 0.0.0.0,
// ":8090" (all interfaces), hostnames (even "localhost": it can resolve to
// something else, and the Host allow-list wants one known literal), and IPv6
// "::1" (the Host allow-list in the plan is IPv4 only; refusing is the
// simpler thing to defend than half-supporting it).
func ParseAddr(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("listen address %q: %w", addr, err)
	}
	if host != "127.0.0.1" {
		return "", "", fmt.Errorf("listen address %q: host must be 127.0.0.1 (the dashboard has no authentication, so it only listens on loopback)", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("listen address %q: port must be a number from 1 to 65535", addr)
	}
	// Normalized: "08090" listens on 8090, and a browser sends "8090" in Host,
	// so the allow-list must be built from the number, not the typed text.
	return host, strconv.Itoa(n), nil
}

// Handler builds the API for a listener on 127.0.0.1:port. opts is passed to
// every Preview, Replay and Undo (tests use a short LockTimeout). logf may be
// nil.
func Handler(pool *pgxpool.Pool, port string, opts admin.Options, logf func(string, ...any)) http.Handler {
	if logf == nil {
		logf = log.Printf
	}
	s := &Server{pool: pool, opts: opts, logf: logf}

	mux := http.NewServeMux()
	// Method patterns: a GET on a POST route is answered 405 by the mux.
	mux.HandleFunc("GET /api/events", s.listEvents)
	mux.HandleFunc("GET /api/events/unattributed", s.listUnattributed) // more specific than {id}
	mux.HandleFunc("GET /api/events/{id}", s.eventLog)
	mux.HandleFunc("GET /api/customers/{stripe_id}/timeline", s.customerTimeline)
	mux.HandleFunc("GET /api/actions", s.listActions)
	mux.HandleFunc("POST /api/events/{id}/preview", s.preview)
	mux.HandleFunc("POST /api/events/{id}/replay", s.replay)
	mux.HandleFunc("POST /api/events/{id}/undo", s.undo)
	// Placeholder until the static UI (step 7c).
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "RelayGuard admin API (UI not built yet)")
	})

	// Outermost first: headers, Host, cross-origin, content type + body cap.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request refused", nil)
	}))
	h := postRules(jsonErrors(mux))
	h = cop.Handler(h)
	h = hostAllowList(h, port)
	return securityHeaders(h)
}

// securityHeaders sets the headers on every response, including rejections.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// hostAllowList answers 403 unless the Host header is exactly one of the two
// names a browser on this machine uses. Origin checks alone do not stop DNS
// rebinding: after the rebind the attacker's page IS same-origin, but the
// browser still sends its own hostname in Host.
func hostAllowList(next http.Handler, port string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if host != "127.0.0.1:"+port && host != "localhost:"+port {
			writeError(w, http.StatusForbidden, "bad_host", "unexpected Host header", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// postRules: every POST must be application/json (a cross-site form or a
// simple fetch cannot send that without a CORS preflight, which this server
// never approves) and is capped at maxBodyBytes.
func postRules(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
					"Content-Type must be application/json", nil)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// Serve listens on addr (validated by ParseAddr) until ctx is cancelled, then
// shuts down gracefully so an in-flight replay or undo finishes. onListening
// (may be nil) is called once the socket is bound, so callers announce the
// address only when it is true.
func Serve(ctx context.Context, pool *pgxpool.Pool, addr string, opts admin.Options, onListening func(addr string)) error {
	_, port, err := ParseAddr(addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{
		Handler:           Handler(pool, port, opts, nil),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if onListening != nil {
		onListening(ln.Addr().String())
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), callTimeout+5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
