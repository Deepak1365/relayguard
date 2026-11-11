package sim

import (
	"bytes"
	"reflect"
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

func TestPoisonDoesNotChangeTheRestOfThePlan(t *testing.T) {
	events := Generate(3, DefaultConfig())
	base := Plan(events, 3, ChaosOptions{DuplicateRate: 0.3, ReorderWindow: 5})
	withPoison := Plan(events, 3, ChaosOptions{DuplicateRate: 0.3, ReorderWindow: 5, Poison: 2})

	var rest []Event
	poison := 0
	for _, e := range withPoison {
		if e.Type == "test.poison" {
			poison++
			continue
		}
		rest = append(rest, e)
	}
	if poison != 2 {
		t.Fatalf("got %d poison deliveries, want 2", poison)
	}
	if len(rest) != len(base) {
		t.Fatalf("non-poison deliveries: got %d, want %d", len(rest), len(base))
	}
	for i := range base {
		if rest[i].ID != base[i].ID {
			t.Fatalf("position %d changed: %s vs %s", i, rest[i].ID, base[i].ID)
		}
	}
}

func TestPoisonEvent(t *testing.T) {
	// Deterministic: the harness recomputes poison IDs after delivery to check
	// that exactly those events were dead-lettered.
	if PoisonID(5, 0) != PoisonID(5, 0) {
		t.Fatal("PoisonID is not deterministic")
	}
	a, b := PoisonEvent(5, 1), PoisonEvent(5, 1)
	if a.ID != b.ID || !bytes.Equal(a.Payload, b.Payload) {
		t.Fatal("PoisonEvent is not deterministic")
	}
	if a.Type != "test.poison" {
		t.Fatalf("type %q, want test.poison", a.Type)
	}
	if !bytes.Contains(a.Payload, []byte(`"type":"test.poison"`)) {
		t.Fatalf("payload does not carry the poison type: %s", a.Payload)
	}
	ids := map[string]bool{}
	for k := 0; k < 10; k++ {
		id := PoisonID(5, k)
		if ids[id] {
			t.Fatalf("PoisonID(5, %d) = %s repeats an earlier k", k, id)
		}
		ids[id] = true
	}

	events := Generate(5, DefaultConfig())
	before := Model(events)
	plan := Plan(events, 5, ChaosOptions{DuplicateRate: 0.3, ReorderWindow: 5, Poison: 3})

	seen := map[string]int{}
	for _, e := range plan {
		if e.Type == "test.poison" {
			seen[e.ID]++
		}
	}
	if len(seen) != 3 {
		t.Fatalf("got %d distinct poison events, want 3", len(seen))
	}
	for k := 0; k < 3; k++ {
		if n := seen[PoisonID(5, k)]; n != 1 {
			t.Fatalf("%s delivered %d times, want exactly once", PoisonID(5, k), n)
		}
	}

	// Poison events live only in the plan, never in the history, so the
	// expected state cannot change.
	if after := Model(events); !reflect.DeepEqual(after, before) {
		t.Fatal("reference model changed after planning poison events")
	}
	for _, e := range events {
		if e.Type == "test.poison" {
			t.Fatalf("poison event %s leaked into the generated history", e.ID)
		}
	}
}
