// Package sim generates synthetic webhook histories, computes their correct
// outcome, delivers them with injected faults, and checks the database.
// Everything is driven by a seed, so any run can be reproduced exactly.
package sim

import (
	"encoding/json"
	"fmt"
	"math/rand"
)

// Event is one generated webhook in its true (provider-side) order.
type Event struct {
	ID         string
	Type       string
	Created    int64  // Unix seconds; strictly increasing across the history
	CustomerID string // owning customer
	ObjectID   string // customer, invoice or subscription id
	AmountPaid int64  // invoice.paid only, in cents
	Status     string // subscription events only
	Payload    []byte // Stripe-format JSON body
}

// Config controls the size and shape of a generated history.
type Config struct {
	Customers          int
	MaxInvoices        int     // per customer, 0..MaxInvoices
	SubscriptionChance float64 // probability a customer has a subscription
	MaxSubUpdates      int     // per subscription, 0..MaxSubUpdates
}

func DefaultConfig() Config {
	return Config{Customers: 20, MaxInvoices: 3, SubscriptionChance: 0.7, MaxSubUpdates: 2}
}

const baseCreated = 1_800_000_000

type step struct {
	typ      string
	objectID string
	email    string
	amount   int64
	status   string
}

// Generate builds a causally valid history: each customer is created before
// its invoices and subscription events, and each subscription is created
// before it is updated or deleted. Customers' streams are interleaved randomly.
// Timestamps are one second apart, so no two events share a "created" value
// (RelayGuard does not claim to order same-second events).
func Generate(seed int64, cfg Config) []Event {
	rng := rand.New(rand.NewSource(seed))

	customerIDs := make([]string, cfg.Customers)
	queues := make([][]step, cfg.Customers)
	for i := 0; i < cfg.Customers; i++ {
		cus := fmt.Sprintf("cus_s%d_%03d", seed, i)
		customerIDs[i] = cus

		var invoices []step
		n := rng.Intn(cfg.MaxInvoices + 1)
		for j := 0; j < n; j++ {
			invoices = append(invoices, step{
				typ:      "invoice.paid",
				objectID: fmt.Sprintf("in_s%d_%03d_%02d", seed, i, j),
				amount:   int64(rng.Intn(46)+5) * 100, // $5.00 .. $50.00
			})
		}

		var subs []step
		if rng.Float64() < cfg.SubscriptionChance {
			subID := fmt.Sprintf("sub_s%d_%03d", seed, i)
			subs = append(subs, step{typ: "customer.subscription.created", objectID: subID, status: "active"})
			updates := rng.Intn(cfg.MaxSubUpdates + 1)
			for k := 0; k < updates; k++ {
				status := "active"
				if rng.Intn(2) == 0 {
					status = "past_due"
				}
				subs = append(subs, step{typ: "customer.subscription.updated", objectID: subID, status: status})
			}
			if rng.Intn(3) == 0 {
				subs = append(subs, step{typ: "customer.subscription.deleted", objectID: subID, status: "canceled"})
			}
		}

		steps := []step{{typ: "customer.created", objectID: cus, email: fmt.Sprintf("user%03d@example.test", i)}}
		queues[i] = append(steps, merge(rng, invoices, subs)...)
	}

	// Interleave customers' streams while keeping each stream's own order.
	active := make([]int, cfg.Customers)
	for i := range active {
		active[i] = i
	}
	var events []Event
	seq := 0
	for len(active) > 0 {
		k := rng.Intn(len(active))
		ci := active[k]
		st := queues[ci][0]
		queues[ci] = queues[ci][1:]
		if len(queues[ci]) == 0 {
			active[k] = active[len(active)-1]
			active = active[:len(active)-1]
		}
		seq++
		events = append(events, build(seed, seq, customerIDs[ci], st))
	}
	return events
}

// merge interleaves a and b randomly, preserving the order within each.
func merge(rng *rand.Rand, a, b []step) []step {
	var out []step
	for len(a) > 0 || len(b) > 0 {
		if len(b) == 0 || (len(a) > 0 && rng.Intn(2) == 0) {
			out, a = append(out, a[0]), a[1:]
		} else {
			out, b = append(out, b[0]), b[1:]
		}
	}
	return out
}

func build(seed int64, seq int, customerID string, st step) Event {
	e := Event{
		ID:         fmt.Sprintf("evt_s%d_%05d", seed, seq),
		Type:       st.typ,
		Created:    baseCreated + int64(seq),
		CustomerID: customerID,
		ObjectID:   st.objectID,
		AmountPaid: st.amount,
		Status:     st.status,
	}

	var object map[string]any
	switch st.typ {
	case "customer.created":
		object = map[string]any{"id": st.objectID, "object": "customer", "email": st.email}
	case "invoice.paid":
		object = map[string]any{"id": st.objectID, "object": "invoice", "customer": customerID, "amount_paid": st.amount}
	default:
		object = map[string]any{"id": st.objectID, "object": "subscription", "customer": customerID, "status": st.status}
	}
	payload, err := json.Marshal(map[string]any{
		"id":      e.ID,
		"object":  "event",
		"type":    e.Type,
		"created": e.Created,
		"data":    map[string]any{"object": object},
	})
	if err != nil {
		panic(err) // only plain maps of strings and numbers; cannot fail
	}
	e.Payload = payload
	return e
}
