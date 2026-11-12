// Package db opens the shared Postgres connection pool.
package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionTimeZone is set on every connection Connect opens.
//
// Why: change_log stores row images made by to_jsonb(row), and to_jsonb
// renders a timestamptz as text in the session's TimeZone. The same instant
// would be "...+00:00" in one session and "...+01:00" in another, and jsonb
// equality compares that text. Pinning every RelayGuard session to UTC makes
// images canonical when they are written, so an image written by the worker
// can be compared with one written later by an admin undo or read by the
// chaos checker, whatever the server or client default is. The alternative,
// comparing via jsonb_populate_record in every query, would protect only the
// queries that remember to do it; this protects the stored data itself.
const SessionTimeZone = "UTC"

// forceSessionTimeZone sets timezone=SessionTimeZone in the startup
// parameters, overriding any timezone from the URL on purpose. pgx keeps URL
// parameter names as written, so a URL with TimeZone=... would otherwise
// leave two keys that Postgres treats as the same setting, and which one wins
// would depend on map iteration order. Every case variant is removed first.
func forceSessionTimeZone(params map[string]string) {
	for k := range params {
		if strings.EqualFold(k, "timezone") {
			delete(params, k)
		}
	}
	params["timezone"] = SessionTimeZone
}

// Connect creates a connection pool and fails fast if the database is unreachable.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	forceSessionTimeZone(cfg.ConnConfig.RuntimeParams)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
