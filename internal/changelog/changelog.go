// Package changelog records every row the worker writes, with before and
// after images, in the same transaction as the write itself. That gives an
// audit trail ("what did event X change?") and is what preview, replay and
// undo (phase 6, later steps) are built on.
//
// Why rows are inserted immediately instead of buffered and flushed at the
// end: the log row and the business write then share the same savepoint, so
// any rollback (an apply error, a stale event, a crash) removes both
// together. Nothing can be logged that did not happen, and nothing can
// happen without being logged, regardless of where the apply stops.
package changelog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Ops, as stored in change_log.op.
const (
	OpInsert = "insert"
	OpUpdate = "update"
	OpDelete = "delete"
)

// Change is one row-level write. Before is nil for an insert, After is nil
// for a delete. Images are the full row as to_jsonb(row) produced it inside
// the writing statement, so they are exactly what the database stored.
type Change struct {
	Table  string
	RowID  int64
	Op     string
	Before []byte
	After  []byte
}

// validate mirrors the change_log CHECK constraints, so a programming error
// fails with a readable message instead of a constraint name.
func (c Change) validate() error {
	switch c.Op {
	case OpInsert:
		if c.Before != nil || c.After == nil {
			return fmt.Errorf("insert on %s %d needs an after image and no before image", c.Table, c.RowID)
		}
	case OpUpdate:
		if c.Before == nil || c.After == nil {
			return fmt.Errorf("update on %s %d needs both images", c.Table, c.RowID)
		}
	case OpDelete:
		if c.Before == nil || c.After != nil {
			return fmt.Errorf("delete on %s %d needs a before image and no after image", c.Table, c.RowID)
		}
	default:
		return fmt.Errorf("unknown op %q", c.Op)
	}
	if c.Table == "" {
		return errors.New("change has no table")
	}
	return nil
}

// Recorder numbers and stores the changes of ONE apply of one inbox event.
// Create a new Recorder for every apply attempt: seq starts at 1 each time,
// and the unique index (inbox_id, generation, seq) WHERE source = 'apply'
// then guarantees that one generation of an event can commit only one set of
// changes. Not safe for concurrent use (one apply is sequential anyway).
type Recorder struct {
	inboxID    int64
	generation int
	seq        int
}

// NewApply returns a Recorder for source='apply' rows. generation is the
// event's replay generation: inbox.replay_count as confirmed by the caller's
// fence (0 for an event that was never replayed).
func NewApply(inboxID int64, generation int) *Recorder {
	return &Recorder{inboxID: inboxID, generation: generation}
}

// Record inserts c as the next change_log row through tx, which must be the
// same transaction (savepoint) that made the write. tx is passed per call
// rather than stored so the caller cannot accidentally log through a
// different transaction than the one it writes with.
func (r *Recorder) Record(ctx context.Context, tx pgx.Tx, c Change) error {
	if err := c.validate(); err != nil {
		return fmt.Errorf("record change: %w", err)
	}
	seq := r.seq + 1
	if _, err := tx.Exec(ctx, `
		INSERT INTO change_log
			(inbox_id, generation, source, admin_action_id, seq, table_name, row_id, op, before_image, after_image)
		VALUES ($1, $2, 'apply', NULL, $3, $4, $5, $6, $7::text::jsonb, $8::text::jsonb)`,
		r.inboxID, r.generation, seq, c.Table, c.RowID, c.Op, jsonbArg(c.Before), jsonbArg(c.After)); err != nil {
		return fmt.Errorf("record change %d (%s %s %d): %w", seq, c.Op, c.Table, c.RowID, err)
	}
	// Advance only after a successful insert. A failed insert aborts the
	// savepoint anyway, so this just keeps Len honest.
	r.seq = seq
	return nil
}

// Len is the number of changes recorded so far.
func (r *Recorder) Len() int { return r.seq }

// jsonbArg turns a missing image into SQL NULL and a present one into text
// that the query casts to jsonb ($n::text::jsonb, as the receiver does).
// Going through text means pgx never has to guess how to encode a []byte for
// a jsonb parameter, and a missing image can never become the JSON literal
// null, which is not the same as SQL NULL for the change_log CHECKs.
func jsonbArg(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}
