// Package outbox records side effects (emails, API calls) as rows written in
// the same transaction as the event's effect. If that transaction commits, the
// message exists and a relay will deliver it at least once; if it rolls back,
// the apply fails or crashes, the message never existed. That removes the
// "sent but not committed" and "committed but not sent" windows of sending
// inline.
//
// Outbox rows are append-only facts, not undoable state: they are not in
// change_log, and undo never touches them (an email cannot be unsent).
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Kinds, as stored in outbox.kind (CHECK in migration 00007).
const (
	KindWelcomeEmail       = "welcome_email"
	KindReceiptEmail       = "receipt_email"
	KindSubscriptionNotice = "subscription_notice"
	idempotencyKeyPrefix   = "rg:stripe:"
)

// Message is one enqueued side effect, as stored. Unused in 9a; the relay
// (Phase 9 step 9b) reads claimed rows into it.
type Message struct {
	InboxID        int64
	Generation     int
	Kind           string
	IdempotencyKey string
	Payload        []byte
}

// IdempotencyKey is the stable key a consumer dedupes on. It is the business
// identity of the effect (event + kind), not the outbox row id: ids restart
// after a data reset, and a real provider would answer a new message with the
// stored response of an old key. No generation: an inbox row has at most one
// committed apply, so (event, kind) is already unique.
func IdempotencyKey(eventID, kind string) string {
	return idempotencyKeyPrefix + eventID + ":" + kind
}

// Writer enqueues the messages of ONE apply of one inbox event. Like the
// changelog Recorder, create a fresh one per attempt.
type Writer struct {
	inboxID    int64
	generation int
	n          int
}

// NewWriter returns a Writer for inbox row inboxID, applied at generation
// (the row's replay_count, the same value the change recorder uses).
func NewWriter(inboxID int64, generation int) *Writer {
	return &Writer{inboxID: inboxID, generation: generation}
}

// Len is how many messages this Writer has enqueued.
func (w *Writer) Len() int { return w.n }

// Enqueue inserts a pending message through tx, which must be the apply's own
// transaction or savepoint (never the pool: a message written outside it would
// survive a rollback of the effect). payload is marshalled to JSON now, so
// every later delivery attempt sends identical bytes.
func (w *Writer) Enqueue(ctx context.Context, tx pgx.Tx, kind, eventID string, payload any) error {
	if w == nil {
		return errors.New("outbox: nil writer")
	}
	if eventID == "" {
		return errors.New("outbox: empty event id")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal %s payload: %w", kind, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (inbox_id, generation, kind, idempotency_key, payload)
		VALUES ($1, $2, $3, $4, $5::text::jsonb)`,
		w.inboxID, w.generation, kind, IdempotencyKey(eventID, kind), string(body)); err != nil {
		return fmt.Errorf("enqueue %s for inbox %d: %w", kind, w.inboxID, err)
	}
	w.n++
	return nil
}
