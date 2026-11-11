package main

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	want := map[int]time.Duration{
		-1: time.Second, // clamped to attempt 1 (a negative shift would panic)
		0:  time.Second,
		1:  time.Second,
		2:  2 * time.Second,
		3:  4 * time.Second,
		4:  8 * time.Second,
		5:  16 * time.Second,
		6:  32 * time.Second,
		// 64s would exceed the cap, so attempt 7 onward waits maxBackoff.
		7: maxBackoff,
		8: maxBackoff,
	}
	for attempt, w := range want {
		if got := backoff(attempt); got != w {
			t.Errorf("backoff(%d) = %s, want %s", attempt, got, w)
		}
	}
}

func TestBackoffLargeAttemptsStayCapped(t *testing.T) {
	// A shift this large overflows time.Duration (to 0 or negative). A broken
	// cap would schedule the retry immediately or in the past, turning a
	// failing event into a hot loop.
	for _, attempt := range []int{20, 34, 62, 63, 64, 65, 100, 1000} {
		got := backoff(attempt)
		if got != maxBackoff {
			t.Errorf("backoff(%d) = %s, want %s", attempt, got, maxBackoff)
		}
	}
}
