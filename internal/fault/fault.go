// Package fault injects crashes at named points, for experiments.
//
// Configuration comes from the environment so the chaos harness can control
// processes it starts:
//
//	FAULT_CRASH_RATE  probability (0..1) of crashing each time a fault point is reached
//	FAULT_SEED        seed for the random choices (defaults to the current time)
package fault

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"
)

type Injector struct {
	mu   sync.Mutex
	rate float64
	only string // if set, only this point can crash
	rng  *rand.Rand
}

// FromEnv reads FAULT_CRASH_RATE and FAULT_SEED. A missing rate means no faults.
func FromEnv() *Injector {
	inj := &Injector{}
	if v := os.Getenv("FAULT_CRASH_RATE"); v != "" {
		if r, err := strconv.ParseFloat(v, 64); err == nil {
			inj.rate = r
		}
	}
	seed := time.Now().UnixNano()
	if v := os.Getenv("FAULT_SEED"); v != "" {
		if s, err := strconv.ParseInt(v, 10, 64); err == nil {
			seed = s
		}
	}
	inj.rng = rand.New(rand.NewSource(seed))
	return inj
}

// Always crashes every time the given point is reached.
func Always(point string) *Injector {
	return &Injector{rate: 1, only: point, rng: rand.New(rand.NewSource(1))}
}

func (i *Injector) Enabled() bool { return i != nil && i.rate > 0 }

func (i *Injector) String() string {
	if !i.Enabled() {
		return "off"
	}
	if i.only != "" {
		return fmt.Sprintf("always at %s", i.only)
	}
	return fmt.Sprintf("crash rate %.2f at every fault point", i.rate)
}

// Maybe exits the process immediately (like a power cut) with the configured
// probability. Deferred functions do not run and nothing is cleaned up.
func (i *Injector) Maybe(point string) {
	if !i.Enabled() || (i.only != "" && point != i.only) {
		return
	}
	i.mu.Lock()
	hit := i.rng.Float64() < i.rate
	i.mu.Unlock()
	if hit {
		log.Printf("FAULT: crashing at %s", point)
		os.Exit(1)
	}
}
