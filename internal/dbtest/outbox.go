package dbtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OutboxRow is an outbox row as the tests assert on it. Payload is the stored
// JSON as text (jsonb normalises key order and spacing, so compare payloads in
// SQL or decode them).
type OutboxRow struct {
	InboxID    int64
	Generation int
	Kind       string
	Key        string
	Payload    string
	Status     string
	Attempts   int
}

// Outbox returns every outbox row in id order.
func Outbox(t *testing.T, pool *pgxpool.Pool) []OutboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT inbox_id, generation, kind, idempotency_key, payload::text, status, attempts
		FROM outbox ORDER BY id`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.InboxID, &r.Generation, &r.Kind, &r.Key, &r.Payload, &r.Status, &r.Attempts); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}
