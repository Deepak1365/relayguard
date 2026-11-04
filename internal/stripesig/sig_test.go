package stripesig

import (
	"errors"
	"testing"
	"time"
)

var (
	testPayload = []byte(`{"id":"evt_1","type":"invoice.paid"}`)
	testSecret  = "whsec_test_secret"
	testNow     = time.Unix(1_800_000_000, 0)
)

func TestSignThenVerify(t *testing.T) {
	header := Sign(testPayload, testSecret, testNow)
	if err := Verify(testPayload, header, testSecret, DefaultTolerance, testNow); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifyAcceptsAnyMatchingSignature(t *testing.T) {
	// During secret rotation Stripe sends several v1 signatures.
	header := Sign(testPayload, "whsec_old_secret", testNow) + "," + Sign(testPayload, testSecret, testNow)
	if err := Verify(testPayload, header, testSecret, DefaultTolerance, testNow); err != nil {
		t.Fatalf("rotation header rejected: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	good := Sign(testPayload, testSecret, testNow)
	cases := []struct {
		name    string
		payload []byte
		header  string
		secret  string
		now     time.Time
		want    error
	}{
		{"tampered body", []byte(`{"id":"evt_2","type":"invoice.paid"}`), good, testSecret, testNow, ErrNoMatch},
		{"wrong secret", testPayload, good, "whsec_attacker", testNow, ErrNoMatch},
		{"replayed too late", testPayload, good, testSecret, testNow.Add(10 * time.Minute), ErrTooOld},
		{"timestamp from the future", testPayload, good, testSecret, testNow.Add(-10 * time.Minute), ErrTooOld},
		{"missing v1", testPayload, "t=1800000000", testSecret, testNow, ErrInvalidHeader},
		{"empty header", testPayload, "", testSecret, testNow, ErrInvalidHeader},
		{"garbage header", testPayload, "not-a-signature", testSecret, testNow, ErrInvalidHeader},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.payload, tc.header, tc.secret, DefaultTolerance, tc.now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
