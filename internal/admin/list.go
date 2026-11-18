package admin

// Read API for the dashboard: event lists, customer timelines and the audit
// log. Each function is ONE statement, so each page is one snapshot; keyset
// pagination (id DESC, "before" cursor) keeps pages stable while the worker
// moves rows between statuses. OFFSET would shift under the reader.

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultLimit and MaxLimit bound a page. A limit above MaxLimit is
	// capped rather than rejected; a negative one is invalid.
	DefaultLimit = 50
	MaxLimit     = 200
)

// EventStatuses are the values ListEvents accepts. "failed" is not a column
// value: it means status 'pending' with a recorded last_error, i.e. an event
// that has failed at least once and is still being retried. It does NOT
// include an event whose worker crashed mid-process (attempts > 0 but
// last_error still NULL): that one shows only under "pending".
var EventStatuses = []string{"pending", "failed", "done", "skipped", "stale", "dead", "undone"}

// EventSummary is one inbox row for a list. It carries no payload.
type EventSummary struct {
	ID         int64
	EventID    string
	EventType  string
	Status     string
	Attempts   int
	LastError  string
	ReceivedAt time.Time
	// EventCreated is the Stripe "created" time (unix seconds) from the
	// payload, nil if the payload has none. Shown beside arrival order so
	// out-of-order delivery is visible.
	EventCreated *int64
	// CustomerID is the Stripe customer the event belongs to, derived from
	// the payload; "" when it cannot be (see ListUnattributed).
	CustomerID string
}

// EventPage is one page of events, newest inbox id first. NextBefore is the
// cursor for the next page (pass it as before), 0 when there is no more.
type EventPage struct {
	Events     []EventSummary
	NextBefore int64
}

// ActionPage is one page of admin_actions, newest first; NextBefore as in
// EventPage.
type ActionPage struct {
	Actions    []AdminAction
	NextBefore int64
}

// customerKeySQL derives the Stripe customer id from an inbox payload the
// same way internal/app/apply.go does: data.object.id for customer.created,
// data.object.customer for invoices and subscriptions. The payload's own
// "type" is used (as Apply does), not the event_type column. NULL when the
// type is not one Apply handles, the payload is not the expected shape, or
// the id is not a non-empty string (Apply would fail to decode it). This
// matches Apply for well-formed Stripe payloads only: jsonb keys are
// case-sensitive, while Go's encoding/json matches field names
// case-insensitively, so a payload with "Customer" would decode in Apply but
// be unattributed here.
const customerKeySQL = `NULLIF(CASE payload->>'type'
	WHEN 'customer.created' THEN
		CASE WHEN jsonb_typeof(payload #> '{data,object,id}') = 'string' THEN payload #>> '{data,object,id}' END
	WHEN 'invoice.paid' THEN
		CASE WHEN jsonb_typeof(payload #> '{data,object,customer}') = 'string' THEN payload #>> '{data,object,customer}' END
	WHEN 'customer.subscription.created' THEN
		CASE WHEN jsonb_typeof(payload #> '{data,object,customer}') = 'string' THEN payload #>> '{data,object,customer}' END
	WHEN 'customer.subscription.updated' THEN
		CASE WHEN jsonb_typeof(payload #> '{data,object,customer}') = 'string' THEN payload #>> '{data,object,customer}' END
	WHEN 'customer.subscription.deleted' THEN
		CASE WHEN jsonb_typeof(payload #> '{data,object,customer}') = 'string' THEN payload #>> '{data,object,customer}' END
END, '')`

// eventCreatedSQL is payload.created as unix seconds, NULL unless it is a
// plain integer (a bad value must not make the whole list fail).
const eventCreatedSQL = `CASE WHEN jsonb_typeof(payload -> 'created') = 'number'
	AND (payload ->> 'created') ~ '^-?[0-9]{1,15}$' THEN (payload ->> 'created')::bigint END`

// cursorBound turns the "before" cursor into an exclusive upper id bound:
// 0 (no cursor) means "from the newest". A plain `id < $1` keeps the plan
// generic and lets the primary key be scanned backwards.
func cursorBound(before int64) int64 {
	if before == 0 {
		return math.MaxInt64
	}
	return before
}

func normalizePage(before int64, limit int) (int, error) {
	if before < 0 {
		return 0, fmt.Errorf("%w: before must not be negative, got %d", ErrInvalidInput, before)
	}
	if limit < 0 {
		return 0, fmt.Errorf("%w: limit must not be negative, got %d", ErrInvalidInput, limit)
	}
	if limit == 0 {
		return DefaultLimit, nil
	}
	if limit > MaxLimit {
		return MaxLimit, nil
	}
	return limit, nil
}

// ListEvents lists inbox events newest first, optionally only one status (""
// = all; see EventStatuses). before is the keyset cursor (0 = from the
// newest); limit 0 means DefaultLimit, above MaxLimit is capped.
func ListEvents(ctx context.Context, pool *pgxpool.Pool, status string, before int64, limit int) (EventPage, error) {
	n, err := normalizePage(before, limit)
	if err != nil {
		return EventPage{}, err
	}
	cond, args := "TRUE", []any{}
	switch status {
	case "":
	case "failed":
		cond = "status = 'pending' AND last_error IS NOT NULL"
	case "pending", "done", "skipped", "stale", "dead", "undone":
		cond, args = "status = $3", []any{status}
	default:
		return EventPage{}, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, status)
	}
	return listInbox(ctx, pool, cond, before, n, args...)
}

// CustomerTimeline lists the events of one Stripe customer in arrival (inbox
// id) order, newest first, each with its payload created time. The customer
// is derived from the payload (customerKeySQL); inbox has no customer column
// and a sequential scan is acceptable at this scale. Events whose customer
// cannot be derived appear in no timeline: see ListUnattributed.
func CustomerTimeline(ctx context.Context, pool *pgxpool.Pool, stripeCustomerID string, before int64, limit int) (EventPage, error) {
	if stripeCustomerID == "" {
		return EventPage{}, fmt.Errorf("%w: customer id is required", ErrInvalidInput)
	}
	n, err := normalizePage(before, limit)
	if err != nil {
		return EventPage{}, err
	}
	return listInbox(ctx, pool, customerKeySQL+" = $3", before, n, stripeCustomerID)
}

// ListUnattributed lists events no customer can be derived from (poison
// payloads, unhandled types, wrong shapes), so they are shown rather than
// hidden. Status-independent; same paging as ListEvents.
func ListUnattributed(ctx context.Context, pool *pgxpool.Pool, before int64, limit int) (EventPage, error) {
	n, err := normalizePage(before, limit)
	if err != nil {
		return EventPage{}, err
	}
	return listInbox(ctx, pool, customerKeySQL+" IS NULL", before, n)
}

// listInbox runs one statement. cond is a fixed fragment from this file (never
// input); its values go in extra and are bound from $3. It fetches n+1 rows
// to learn whether another page exists.
func listInbox(ctx context.Context, pool *pgxpool.Pool, cond string, before int64, n int, extra ...any) (EventPage, error) {
	args := append([]any{cursorBound(before), n + 1}, extra...)
	rows, err := pool.Query(ctx, `
		SELECT id, event_id, event_type, status, attempts, COALESCE(last_error, ''), received_at,
		       `+eventCreatedSQL+`, COALESCE(`+customerKeySQL+`, '')
		FROM inbox
		WHERE (`+cond+`) AND id < $1
		ORDER BY id DESC
		LIMIT $2`, args...)
	if err != nil {
		return EventPage{}, fmt.Errorf("list inbox: %w", err)
	}
	defer rows.Close()
	var page EventPage
	for rows.Next() {
		var e EventSummary
		if err := rows.Scan(&e.ID, &e.EventID, &e.EventType, &e.Status, &e.Attempts, &e.LastError,
			&e.ReceivedAt, &e.EventCreated, &e.CustomerID); err != nil {
			return EventPage{}, fmt.Errorf("scan inbox list: %w", err)
		}
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, fmt.Errorf("list inbox: %w", err)
	}
	if len(page.Events) > n {
		page.Events = page.Events[:n]
		page.NextBefore = page.Events[n-1].ID
	}
	return page, nil
}

// ListActions lists the admin_actions audit log newest first, all events.
func ListActions(ctx context.Context, pool *pgxpool.Pool, before int64, limit int) (ActionPage, error) {
	n, err := normalizePage(before, limit)
	if err != nil {
		return ActionPage{}, err
	}
	rows, err := pool.Query(ctx, `
		SELECT id, inbox_id, action, actor, reason, outcome, detail::text, created_at
		FROM admin_actions
		WHERE id < $1
		ORDER BY id DESC
		LIMIT $2`, cursorBound(before), n+1)
	if err != nil {
		return ActionPage{}, fmt.Errorf("list admin_actions: %w", err)
	}
	defer rows.Close()
	var page ActionPage
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.ID, &a.InboxID, &a.Action, &a.Actor, &a.Reason, &a.Outcome, &a.Detail, &a.CreatedAt); err != nil {
			return ActionPage{}, fmt.Errorf("scan admin_actions: %w", err)
		}
		page.Actions = append(page.Actions, a)
	}
	if err := rows.Err(); err != nil {
		return ActionPage{}, fmt.Errorf("list admin_actions: %w", err)
	}
	if len(page.Actions) > n {
		page.Actions = page.Actions[:n]
		page.NextBefore = page.Actions[n-1].ID
	}
	return page, nil
}
