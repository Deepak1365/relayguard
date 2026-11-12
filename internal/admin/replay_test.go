package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeReplayRequest(t *testing.T) {
	got, err := NormalizeReplayRequest(ReplayRequest{InboxID: 3, Actor: "  alice ", Reason: "\tlate customer\n"})
	if err != nil || got != (ReplayRequest{InboxID: 3, Actor: "alice", Reason: "late customer"}) {
		t.Fatalf("trim: %+v, %v", got, err)
	}
	// Limits count characters, not bytes: 200 two-byte runes are allowed.
	if _, err := NormalizeReplayRequest(ReplayRequest{InboxID: 3, Actor: strings.Repeat("é", 200), Reason: strings.Repeat("ü", 1000)}); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
	bad := []ReplayRequest{
		{InboxID: 0, Actor: "a", Reason: "r"},
		{InboxID: 3, Actor: "", Reason: "r"},
		{InboxID: 3, Actor: " \t", Reason: "r"},
		{InboxID: 3, Actor: "a", Reason: ""},
		{InboxID: 3, Actor: "a", Reason: "  "},
		{InboxID: 3, Actor: strings.Repeat("a", 201), Reason: "r"},
		{InboxID: 3, Actor: "a", Reason: strings.Repeat("r", 1001)},
	}
	for _, r := range bad {
		if _, err := NormalizeReplayRequest(r); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v: err %v, want ErrInvalidInput", r, err)
		}
	}
}

// Validation happens before the database is touched: with a nil pool, any
// database access would panic, so returning ErrInvalidInput proves it.
func TestReplayValidatesBeforeDB(t *testing.T) {
	_, err := Replay(context.Background(), nil, ReplayRequest{InboxID: 1, Actor: "alice"}, Options{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err %v, want ErrInvalidInput without touching the (nil) pool", err)
	}
}
