package sim

import (
	"bytes"
	"testing"
)

func TestGenerateIsDeterministic(t *testing.T) {
	a := Generate(42, DefaultConfig())
	b := Generate(42, DefaultConfig())
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || !bytes.Equal(a[i].Payload, b[i].Payload) {
			t.Fatalf("event %d differs between runs with the same seed", i)
		}
	}
}

func TestHistoryIsCausalAndOrdered(t *testing.T) {
	created := map[string]bool{}
	subStarted := map[string]bool{}
	var last int64
	for _, e := range Generate(7, DefaultConfig()) {
		if e.Created <= last {
			t.Fatalf("timestamps not strictly increasing at %s", e.ID)
		}
		last = e.Created
		switch e.Type {
		case "customer.created":
			created[e.CustomerID] = true
		case "invoice.paid":
			if !created[e.CustomerID] {
				t.Fatalf("%s: invoice before its customer", e.ID)
			}
		case "customer.subscription.created":
			if !created[e.CustomerID] {
				t.Fatalf("%s: subscription before its customer", e.ID)
			}
			subStarted[e.ObjectID] = true
		default:
			if !subStarted[e.ObjectID] {
				t.Fatalf("%s: %s before subscription.created", e.ID, e.Type)
			}
		}
	}
}

func TestModelMatchesHistory(t *testing.T) {
	events := Generate(7, DefaultConfig())
	exp := Model(events)

	var credits int64
	lastStatus := map[string]string{}
	for _, e := range events {
		switch e.Type {
		case "invoice.paid":
			credits += e.AmountPaid / 100
		case "customer.created":
		default:
			lastStatus[e.ObjectID] = e.Status
		}
	}
	if exp.TotalCredits != credits {
		t.Fatalf("total credits: model %d, history %d", exp.TotalCredits, credits)
	}
	var sum int64
	for _, c := range exp.Customers {
		sum += c.Credits
	}
	if sum != credits {
		t.Fatalf("per-customer credits sum to %d, want %d", sum, credits)
	}
	for id, status := range lastStatus {
		if exp.Subscriptions[id] != status {
			t.Fatalf("%s: model %q, last event %q", id, exp.Subscriptions[id], status)
		}
	}
}

func TestPlanWithoutChaosKeepsOrder(t *testing.T) {
	events := Generate(3, DefaultConfig())
	plan := Plan(events, 3, ChaosOptions{})
	if len(plan) != len(events) {
		t.Fatalf("plan has %d deliveries, want %d", len(plan), len(events))
	}
	for i := range events {
		if plan[i].ID != events[i].ID {
			t.Fatalf("position %d: got %s, want %s", i, plan[i].ID, events[i].ID)
		}
	}
}

func TestPlanWithChaosDeliversEverythingDeterministically(t *testing.T) {
	events := Generate(3, DefaultConfig())
	opts := ChaosOptions{DuplicateRate: 0.5, ReorderWindow: 5}
	plan := Plan(events, 3, opts)

	seen := map[string]int{}
	for _, p := range plan {
		seen[p.ID]++
	}
	for _, e := range events {
		if seen[e.ID] == 0 {
			t.Fatalf("%s never delivered", e.ID)
		}
	}
	if len(plan) <= len(events) {
		t.Fatalf("expected duplicates: %d deliveries for %d events", len(plan), len(events))
	}

	again := Plan(events, 3, opts)
	for i := range plan {
		if plan[i].ID != again[i].ID {
			t.Fatalf("plan differs at %d for the same seed", i)
		}
	}
}
