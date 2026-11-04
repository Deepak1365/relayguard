package sim

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Violation is one way the database disagrees with the expected state.
type Violation struct {
	Kind    string
	Subject string
	Detail  string
}

// Reset empties the app tables and the inbox before a run.
func Reset(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `TRUNCATE inbox, subscriptions, credit_grants, customers RESTART IDENTITY`)
	return err
}

// InboxCounts returns the number of inbox rows per status.
func InboxCounts(ctx context.Context, pool *pgxpool.Pool) (map[string]int, error) {
	rows, err := pool.Query(ctx, `SELECT status, count(*) FROM inbox GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[status] = int(n)
	}
	return counts, rows.Err()
}

// WaitIdle polls until no inbox event is pending, or the timeout passes.
func WaitIdle(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) (map[string]int, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		counts, err := InboxCounts(ctx, pool)
		if err != nil {
			return nil, false, err
		}
		if counts["pending"] == 0 {
			return counts, false, nil
		}
		if time.Now().After(deadline) {
			return counts, true, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// CheckState compares app tables with exp. Rows not belonging to this seed's
// history (for example real Stripe test events) are counted and ignored.
func CheckState(ctx context.Context, pool *pgxpool.Pool, seed int64, exp Expected) ([]Violation, int, error) {
	cusPrefix := fmt.Sprintf("cus_s%d_", seed)
	subPrefix := fmt.Sprintf("sub_s%d_", seed)
	var v []Violation
	foreign := 0

	rows, err := pool.Query(ctx, `
		SELECT c.stripe_customer_id, c.credit_balance, count(g.id), COALESCE(sum(g.amount), 0)::bigint
		FROM customers c LEFT JOIN credit_grants g ON g.customer_id = c.id
		GROUP BY c.id`)
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		var balance, grants, ledger int64
		if err := rows.Scan(&id, &balance, &grants, &ledger); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if !strings.HasPrefix(id, cusPrefix) {
			foreign++
			continue
		}
		seen[id] = true
		want, ok := exp.Customers[id]
		if !ok {
			v = append(v, Violation{"unexpected_customer", id, "not in the generated history"})
			continue
		}
		if balance != want.Credits {
			v = append(v, Violation{"wrong_balance", id, fmt.Sprintf("expected %d, got %d", want.Credits, balance)})
		}
		if grants != int64(want.Grants) {
			note := "duplicate grants"
			if grants < int64(want.Grants) {
				note = "lost grants"
			}
			v = append(v, Violation{"wrong_grant_count", id, fmt.Sprintf("expected %d, got %d (%s)", want.Grants, grants, note)})
		}
		if ledger != balance {
			v = append(v, Violation{"ledger_mismatch", id, fmt.Sprintf("balance %d but grants total %d", balance, ledger)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for _, id := range sortedKeys(exp.Customers) {
		if !seen[id] {
			v = append(v, Violation{"missing_customer", id, "customer.created never applied"})
		}
	}

	rows, err = pool.Query(ctx, `SELECT stripe_subscription_id, status FROM subscriptions`)
	if err != nil {
		return nil, 0, err
	}
	seenSub := map[string]bool{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if !strings.HasPrefix(id, subPrefix) {
			foreign++
			continue
		}
		seenSub[id] = true
		want, ok := exp.Subscriptions[id]
		if !ok {
			v = append(v, Violation{"unexpected_subscription", id, "not in the generated history"})
			continue
		}
		if status != want {
			v = append(v, Violation{"wrong_subscription_status", id, fmt.Sprintf("expected %s, got %s", want, status)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for _, id := range sortedKeys(exp.Subscriptions) {
		if !seenSub[id] {
			v = append(v, Violation{"missing_subscription", id, "no subscription event applied"})
		}
	}
	return v, foreign, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
