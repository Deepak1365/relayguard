package sim

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
)

// ChaosOptions describe delivery faults, like a real provider's network.
type ChaosOptions struct {
	DuplicateRate float64 // probability each event is delivered a second time
	ReorderWindow int     // max positions an event can move from its true order
	Poison        int     // extra events that crash the worker every time (RelayGuard only)
}

// Plan returns the delivery order. It uses its own random stream derived from
// the seed, so changing chaos options never changes the generated history.
func Plan(events []Event, seed int64, opts ChaosOptions) []Event {
	rng := rand.New(rand.NewSource(seed ^ 0x5eed))
	type item struct {
		e   Event
		key float64
	}
	var items []item
	pos := 0
	add := func(e Event) {
		// Sorting by position plus random jitter in [0, window) moves each
		// delivery at most ReorderWindow places from where it started.
		key := float64(pos) + rng.Float64()*float64(opts.ReorderWindow)
		items = append(items, item{e: e, key: key})
		pos++
	}
	for _, e := range events {
		add(e)
		if rng.Float64() < opts.DuplicateRate {
			add(e)
		}
	}
	// The poison events' random draws come after all the others, so adding
	// them never changes the rest of the plan. Their keys still place them at
	// random positions in the delivery order.
	for k := 0; k < opts.Poison; k++ {
		items = append(items, item{e: PoisonEvent(seed, k), key: rng.Float64() * float64(pos)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })

	plan := make([]Event, len(items))
	for i, it := range items {
		plan[i] = it.e
	}
	return plan
}

// PoisonEvent is not part of the history or the model: it should end up
// dead-lettered, and every other event should still be applied correctly.
func PoisonEvent(seed int64, k int) Event {
	id := PoisonID(seed, k)
	payload, err := json.Marshal(map[string]any{
		"id": id, "object": "event", "type": "test.poison", "created": baseCreated,
		"data": map[string]any{"object": map[string]any{"id": id, "object": "test"}},
	})
	if err != nil {
		panic(err)
	}
	return Event{ID: id, Type: "test.poison", Created: baseCreated, Payload: payload}
}

func PoisonID(seed int64, k int) string { return fmt.Sprintf("evt_s%d_poison_%d", seed, k) }
