package sim

import (
	"math/rand"
	"sort"
)

// ChaosOptions describe delivery faults, like a real provider's network.
type ChaosOptions struct {
	DuplicateRate float64 // probability each event is delivered a second time
	ReorderWindow int     // max positions an event can move from its true order
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
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })

	plan := make([]Event, len(items))
	for i, it := range items {
		plan[i] = it.e
	}
	return plan
}
