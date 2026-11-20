package sim

// DB tests for CheckState, the model check every chaos run ends with. Before
// these, no Go test called it: CI's red proof ci-red/8b-checker (CheckState
// returning nil, 0, nil) passed every unit and DB test, and only the chaos
// job's naive negative control went red. Each case makes one change, to the
// stored rows or to the expected state, and asserts the exact set of
// violations that change produces, next to a positive control (an undamaged
// state is clean), so a checker that reports nothing and one that reports
// everything both fail.
//
// The rows are written with plain SQL in seed checkSeed's scope (customer and
// subscription ids only; no inbox rows, so dbtest.Truncate's foreign-inbox
// guard is not involved) to call the exported CheckState itself.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Deepak1365/relayguard/internal/dbtest"
)

const checkSeed = 990001

const (
	chkCus = "cus_s990001_1"
	chkSub = "sub_s990001_1"
)

// seedCheckState writes one in-scope customer with two grants (10 + 20, so
// balance 30) and an active subscription, plus a customer and subscription of
// seed 9900010, whose ids start with "cus_s990001" but not with the scope's
// "cus_s990001_": they must count as foreign, not as this seed's rows.
func seedCheckState(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, `
		WITH c AS (INSERT INTO customers (stripe_customer_id, credit_balance) VALUES ($1, 30) RETURNING id),
		     g AS (INSERT INTO credit_grants (customer_id, stripe_invoice_id, amount)
		           SELECT id, inv, amt FROM c, (VALUES ('in_chk_1', 10), ('in_chk_2', 20)) AS v(inv, amt))
		INSERT INTO subscriptions (stripe_subscription_id, customer_id, status) SELECT $2, id, 'active' FROM c`,
		chkCus, chkSub)
	mustExec(t, pool, `
		WITH c AS (INSERT INTO customers (stripe_customer_id, credit_balance) VALUES ('cus_s9900010_1', 5) RETURNING id)
		INSERT INTO subscriptions (stripe_subscription_id, customer_id, status) SELECT 'sub_s9900010_1', id, 'canceled' FROM c`)
}

func checkExpected() Expected {
	return Expected{
		Customers:     map[string]*CustomerState{chkCus: {Credits: 30, Grants: 2}},
		Subscriptions: map[string]string{chkSub: "active"},
	}
}

func TestDBCheckStateRules(t *testing.T) {
	pool := dbtest.Pool(t)
	cases := []struct {
		name       string
		damage     string            // SQL run after seeding; "" for none
		expect     func(*Expected)   // change to the expected state; nil for none
		want       []string          // exact "kind subject" violations
		wantDetail map[string]string // kind -> substring its Detail must contain
	}{
		{name: "positive control: undamaged state is clean"},
		{
			name:   "balance changed without a grant",
			damage: `UPDATE customers SET credit_balance = 31 WHERE stripe_customer_id = '` + chkCus + `'`,
			want:   []string{"ledger_mismatch " + chkCus, "wrong_balance " + chkCus},
			wantDetail: map[string]string{
				"wrong_balance": "expected 30, got 31", "ledger_mismatch": "balance 31 but grants total 30"},
		},
		{
			name: "invoice applied twice (grant and balance both doubled up)",
			damage: `WITH c AS (UPDATE customers SET credit_balance = credit_balance + 20
			           WHERE stripe_customer_id = '` + chkCus + `' RETURNING id)
			         INSERT INTO credit_grants (customer_id, stripe_invoice_id, amount) SELECT id, 'in_chk_2', 20 FROM c`,
			want:       []string{"wrong_balance " + chkCus, "wrong_grant_count " + chkCus},
			wantDetail: map[string]string{"wrong_grant_count": "expected 2, got 3 (duplicate grants)"},
		},
		{
			name:       "grant lost, balance kept",
			damage:     `DELETE FROM credit_grants WHERE stripe_invoice_id = 'in_chk_1'`,
			want:       []string{"ledger_mismatch " + chkCus, "wrong_grant_count " + chkCus},
			wantDetail: map[string]string{"wrong_grant_count": "expected 2, got 1 (lost grants)"},
		},
		{
			name:       "subscription in the wrong status",
			damage:     `UPDATE subscriptions SET status = 'canceled' WHERE stripe_subscription_id = '` + chkSub + `'`,
			want:       []string{"wrong_subscription_status " + chkSub},
			wantDetail: map[string]string{"wrong_subscription_status": "expected active, got canceled"},
		},
		{
			name:   "expected customer never created",
			expect: func(e *Expected) { e.Customers["cus_s990001_2"] = &CustomerState{} },
			want:   []string{"missing_customer cus_s990001_2"},
		},
		{
			name:   "expected subscription never created",
			expect: func(e *Expected) { e.Subscriptions["sub_s990001_2"] = "active" },
			want:   []string{"missing_subscription sub_s990001_2"},
		},
		{
			name:   "customer not in the history",
			damage: `INSERT INTO customers (stripe_customer_id) VALUES ('cus_s990001_3')`,
			want:   []string{"unexpected_customer cus_s990001_3"},
		},
		{
			name: "subscription not in the history",
			damage: `INSERT INTO subscriptions (stripe_subscription_id, customer_id, status)
			         SELECT 'sub_s990001_3', id, 'active' FROM customers WHERE stripe_customer_id = '` + chkCus + `'`,
			want: []string{"unexpected_subscription sub_s990001_3"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dbtest.Truncate(t, pool)
			seedCheckState(t, pool)
			if c.damage != "" {
				mustExec(t, pool, c.damage)
			}
			exp := checkExpected()
			if c.expect != nil {
				c.expect(&exp)
			}
			got, foreign, err := CheckState(context.Background(), pool, checkSeed, exp)
			if err != nil {
				t.Fatalf("CheckState: %v", err)
			}
			wantViolations(t, got, c.want...)
			if foreign != 2 {
				t.Errorf("foreign rows: %d, want 2 (seed 9900010's customer and subscription)", foreign)
			}
			for kind, sub := range c.wantDetail {
				found := false
				for _, v := range got {
					if v.Kind == kind && strings.Contains(v.Detail, sub) {
						found = true
					}
				}
				if !found {
					t.Errorf("no %s violation with detail containing %q: %+v", kind, sub, got)
				}
			}
		})
	}
}
