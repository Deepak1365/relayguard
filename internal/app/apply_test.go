package app

import (
	"context"
	"errors"
	"testing"

	"github.com/Deepak1365/relayguard/internal/changelog"
	"github.com/Deepak1365/relayguard/internal/stripeevent"
)

// Every combination of (pre-read found the row, upsert returned a row, the
// returned row was inserted). The DB tests cover insert, update and stale;
// this also covers the race branch, which needs a concurrent insert to reach.
func TestClassifyUpsert(t *testing.T) {
	cases := []struct {
		found, returned, inserted bool
		wantOp                    string
		wantErr                   bool
	}{
		{found: false, returned: true, inserted: true, wantOp: changelog.OpInsert},
		{found: true, returned: true, inserted: false, wantOp: changelog.OpUpdate},
		{found: true, returned: false, wantOp: ""},  // stale: nothing written
		{found: false, returned: false, wantOp: ""}, // stale vs a row inserted after the pre-read
		{found: false, returned: true, inserted: false, wantErr: true},
		{found: true, returned: true, inserted: true, wantErr: true},
	}
	for _, c := range cases {
		op, err := classifyUpsert(c.found, c.returned, c.inserted)
		if (err != nil) != c.wantErr || op != c.wantOp {
			t.Errorf("classifyUpsert(found=%v, returned=%v, inserted=%v) = %q, %v; want %q, error=%v",
				c.found, c.returned, c.inserted, op, err, c.wantOp, c.wantErr)
		}
	}
	if _, err := classifyUpsert(false, true, false); !errors.Is(err, ErrConcurrentInsert) {
		t.Errorf("race branch error = %v, want ErrConcurrentInsert (a retryable apply error)", err)
	}
}

// Apply must refuse to run without a recorder rather than write unlogged
// rows. It checks before touching tx, so a nil tx is fine here.
func TestApplyRequiresRecorder(t *testing.T) {
	err := Apply(context.Background(), nil, nil, stripeevent.Event{Type: "customer.created"}, Hooks{})
	if err == nil {
		t.Fatal("Apply with a nil recorder: no error, want one")
	}
}
