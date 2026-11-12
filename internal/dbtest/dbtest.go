// Package dbtest holds helpers shared by the database integration tests
// (cmd/worker, internal/sim, and later internal/admin), so every package wipes
// the same tables with the same safety check.
//
// The tests are skipped unless RELAYGUARD_DB_TESTS=1 and DATABASE_URL are set
// (`dev.ps1 db-test` does both), because they TRUNCATE the inbox, the app
// tables (customers, credit_grants, subscriptions) and the change/audit tables
// (change_log, admin_actions) before and after each test. They refuse to do
// that if the inbox holds events they did not create. They never create, drop
// or migrate schema: run `dev.ps1 migrate` first.
//
// Before running:
//   - Stop any worker. It polls the inbox and could claim evt_dbtest_ rows,
//     racing the tests and making them fail or flake.
//   - Never run during an experiment (chaos or manual): the tests wipe the
//     tables the experiment is measuring.
//   - The inbox must contain only evt_dbtest_ rows (or be empty); otherwise
//     the tests refuse to truncate and fail.
//   - Packages using this share tables: run them with `go test -p 1`.
package dbtest

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/db"
)

// EventPrefix starts every inbox event_id these tests create, so the safety
// check can tell them apart from rows a real receiver stored.
const EventPrefix = "evt_dbtest_"

// truncateSQL lists every table the tests touch. change_log and admin_actions
// reference inbox, so TRUNCATE inbox without them is refused by Postgres.
const truncateSQL = `TRUNCATE change_log, admin_actions, inbox, subscriptions, credit_grants, customers RESTART IDENTITY`

// testLockKey is the pg_advisory_lock key every DB test holds for its whole
// run. The value is arbitrary ("RGDB" in ASCII); nothing else in RelayGuard
// takes advisory locks, so it cannot collide with production code.
const testLockKey int64 = 0x52474442

// lockWait bounds how long a test waits for another package's test to
// release testLockKey. A single test takes well under a second, so hitting
// this means something is stuck (or a crashed session still holds the lock).
const lockWait = 60 * time.Second

// Pool connects to the test database, or skips the test if DB tests are not
// enabled. It holds testLockKey for the whole test and empties the tables
// before the test and again in cleanup.
//
// Why the advisory lock: the DB-test packages share the same tables. `go test
// -p 1` already runs them one at a time, but only if every caller remembers
// the flag; the lock makes the database itself serialise them, so a plain
// `go test ./...` with DB tests enabled cannot let one package's TRUNCATE wipe
// another's rows mid-test.
//
// It cannot deadlock with Truncate's lock_timeout / ACCESS EXCLUSIVE path:
// the advisory lock lives on its own connection, outside any transaction, and
// that connection never touches a table. A test waiting for it holds no table
// locks, and the test holding it takes table locks only through the pool, so
// no wait cycle can form. Cleanups run last-in first-out, so the final
// Truncate finishes before the lock is released.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if os.Getenv("RELAYGUARD_DB_TESTS") != "1" || url == "" {
		t.Skip("set RELAYGUARD_DB_TESTS=1 and DATABASE_URL to run database tests (they truncate tables)")
	}
	ctx := context.Background()
	lockTestDB(t, url)
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	Truncate(t, pool)
	t.Cleanup(func() { Truncate(t, pool) })
	return pool
}

// lockTestDB takes the session-level advisory lock on a dedicated connection
// and registers a cleanup that releases it and closes the connection. Closing
// the session would release the lock on its own (also if the test process
// dies); unlocking explicitly first makes a lost lock visible as an error.
func lockTestDB(t *testing.T, url string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect for test lock: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	if _, err := conn.Exec(waitCtx, `SELECT pg_advisory_lock($1)`, testLockKey); err != nil {
		conn.Close(ctx)
		t.Fatalf("take DB test lock %d (another DB test running, or a stuck session holds it?): %v", testLockKey, err)
	}
	t.Cleanup(func() {
		var released bool
		if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, testLockKey).Scan(&released); err != nil {
			t.Errorf("release DB test lock: %v", err)
		} else if !released {
			t.Errorf("release DB test lock: it was not held by this session")
		}
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close DB test lock connection: %v", err)
		}
	})
}

// Truncate empties the inbox, app and change/audit tables, but refuses if the
// inbox holds events these tests did not create: those belong to someone (a
// receiver, a chaos run) and must not be silently destroyed. The check and the
// TRUNCATE run in one transaction under an exclusive lock on inbox, so a
// receiver cannot insert a row between the check and the wipe (its INSERT
// waits for the lock, then lands in the emptied table).
func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin truncate: %v", err)
	}
	defer tx.Rollback(ctx) // no-op after commit
	// Fail instead of hanging if something (a running worker, an open psql
	// transaction) holds a lock on inbox.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE inbox IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock inbox: %v", err)
	}
	var foreign int
	if err := tx.QueryRow(ctx,
		// Exact prefix match: in LIKE, '_' in the prefix is a wildcard.
		`SELECT count(*) FROM inbox WHERE left(event_id, length($1)) <> $1`, EventPrefix).Scan(&foreign); err != nil {
		t.Fatalf("count foreign inbox rows: %v", err)
	}
	if foreign > 0 {
		t.Fatalf("inbox has %d rows not created by these tests; refusing to truncate (run dev.ps1 reset-data yourself if they are disposable)", foreign)
	}
	if _, err := tx.Exec(ctx, truncateSQL); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit truncate: %v", err)
	}
}

// DefaultCreated is the Stripe "created" time InsertEvent uses.
const DefaultCreated int64 = 1793800000

// InsertEvent stores a pending inbox row for event EventPrefix+suffix and
// returns its id.
func InsertEvent(t *testing.T, pool *pgxpool.Pool, suffix, typ string, object map[string]any) int64 {
	t.Helper()
	return InsertEventAt(t, pool, suffix, typ, DefaultCreated, object)
}

// InsertEventAt is InsertEvent with an explicit Stripe "created" time, for
// ordering (stale event) tests.
func InsertEventAt(t *testing.T, pool *pgxpool.Pool, suffix, typ string, created int64, object map[string]any) int64 {
	t.Helper()
	eventID := EventPrefix + suffix
	payload, err := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "type": typ, "created": created,
		"data": map[string]any{"object": object},
	})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO inbox (provider, event_id, event_type, payload)
		VALUES ('stripe', $1, $2, $3::text::jsonb)
		RETURNING id`, eventID, typ, string(payload)).Scan(&id); err != nil {
		t.Fatalf("insert inbox row: %v", err)
	}
	return id
}

// SetAttempts makes a row look like it has already used n attempts and is
// ready to be claimed again.
func SetAttempts(t *testing.T, pool *pgxpool.Pool, id int64, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE inbox SET attempts = $2, next_attempt_at = now() - interval '1 second' WHERE id = $1`,
		id, n); err != nil {
		t.Fatalf("set attempts: %v", err)
	}
}

// ExpireLease simulates a claim's lease running out (the worker holding it
// stalled or died) without sleeping, so the tests do not depend on timing.
func ExpireLease(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE inbox SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

// InboxRow is the part of an inbox row the tests assert on.
type InboxRow struct {
	Status   string
	Attempts int
	LastErr  string
}

// ReadRow reads one inbox row's status, attempts and last_error.
func ReadRow(t *testing.T, pool *pgxpool.Pool, id int64) InboxRow {
	t.Helper()
	var r InboxRow
	if err := pool.QueryRow(context.Background(),
		`SELECT status, attempts, COALESCE(last_error, '') FROM inbox WHERE id = $1`, id).
		Scan(&r.Status, &r.Attempts, &r.LastErr); err != nil {
		t.Fatalf("read inbox row %d: %v", id, err)
	}
	return r
}

// Count returns the number of rows in table. table must be a literal from
// the tests, never input: it is concatenated into the SQL.
func Count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// InsertAuditRows writes one admin_actions row and two change_log rows (an
// 'apply' insert and an 'undo' update tied to the admin action) for inboxID,
// so tests can prove that a wipe still works while rows reference the inbox.
// The action's outcome is 'done' because undo change_log rows point at it: a
// refused action writes no changes. The images are placeholders: nothing
// reads them back.
func InsertAuditRows(t *testing.T, pool *pgxpool.Pool, inboxID int64) {
	t.Helper()
	ctx := context.Background()
	var actionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO admin_actions (action, inbox_id, actor, reason, outcome)
		VALUES ('undo', $1, 'dbtest', 'fk truncate test', 'done')
		RETURNING id`, inboxID).Scan(&actionID); err != nil {
		t.Fatalf("insert admin_actions row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO change_log (inbox_id, generation, source, admin_action_id, seq, table_name, row_id, op, before_image, after_image)
		VALUES ($1, 0, 'apply', NULL, 1, 'customers', 1, 'insert', NULL, '{"id": 1}'),
		       ($1, 0, 'undo',  $2,   1, 'customers', 1, 'update', '{"id": 1}', '{"id": 1}')`,
		inboxID, actionID); err != nil {
		t.Fatalf("insert change_log rows: %v", err)
	}
}

// ChangeRow is one change_log row as the tests assert on it. Images are
// decoded with UseNumber so numeric columns compare exactly (e.g.
// Before["credit_balance"] == json.Number("0")); a missing image is nil.
type ChangeRow struct {
	ID            int64
	Generation    int
	Source        string
	AdminActionID *int64
	Seq           int
	Table         string
	RowID         int64
	Op            string
	Before        map[string]any
	After         map[string]any
}

// ChangeLog returns inboxID's change_log rows in id order.
func ChangeLog(t *testing.T, pool *pgxpool.Pool, inboxID int64) []ChangeRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, generation, source, admin_action_id, seq, table_name, row_id, op,
		       before_image::text, after_image::text
		FROM change_log WHERE inbox_id = $1 ORDER BY id`, inboxID)
	if err != nil {
		t.Fatalf("query change_log: %v", err)
	}
	defer rows.Close()
	var out []ChangeRow
	for rows.Next() {
		var r ChangeRow
		var before, after *string
		if err := rows.Scan(&r.ID, &r.Generation, &r.Source, &r.AdminActionID, &r.Seq,
			&r.Table, &r.RowID, &r.Op, &before, &after); err != nil {
			t.Fatalf("scan change_log: %v", err)
		}
		r.Before = decodeImage(t, before)
		r.After = decodeImage(t, after)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read change_log: %v", err)
	}
	return out
}

func decodeImage(t *testing.T, img *string) map[string]any {
	t.Helper()
	if img == nil {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(*img))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode image %s: %v", *img, err)
	}
	return m
}

// CustomerObject is a minimal Stripe customer object for a customer.created
// event.
func CustomerObject(id string) map[string]any {
	return map[string]any{"id": id, "object": "customer", "email": id + "@example.test"}
}
