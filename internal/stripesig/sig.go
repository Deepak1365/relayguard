// Package stripesig signs and verifies Stripe webhook signatures.
//
// Stripe sends a header like:  Stripe-Signature: t=1700000000,v1=<hex>[,v1=<hex>]
// v1 is HMAC-SHA256(secret, "<t>.<raw body>"). Several v1 values can appear
// while a secret is being rotated; any one matching is enough.
package stripesig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidHeader = errors.New("invalid signature header")
	ErrNoMatch       = errors.New("no matching signature")
	ErrTooOld        = errors.New("timestamp outside tolerance")
)

// DefaultTolerance limits how old a signed request may be, which blocks
// replaying a captured webhook later.
const DefaultTolerance = 5 * time.Minute

func computeSignature(payload []byte, secret string, ts int64) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	return mac.Sum(nil)
}

// Sign returns a Stripe-Signature header value for payload, signed at time t.
func Sign(payload []byte, secret string, t time.Time) string {
	ts := t.Unix()
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(computeSignature(payload, secret, ts)))
}

// Verify checks that header is a valid signature of payload made with secret,
// no further than tolerance away from now.
func Verify(payload []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	var (
		ts     int64
		haveTS bool
		sigs   [][]byte
	)
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("%w: bad timestamp", ErrInvalidHeader)
			}
			ts, haveTS = n, true
		case "v1":
			if b, err := hex.DecodeString(value); err == nil {
				sigs = append(sigs, b)
			}
		}
	}
	if !haveTS || len(sigs) == 0 {
		return fmt.Errorf("%w: missing t or v1", ErrInvalidHeader)
	}

	expected := computeSignature(payload, secret, ts)
	matched := false
	for _, s := range sigs {
		if hmac.Equal(s, expected) { // constant-time comparison
			matched = true
			break
		}
	}
	if !matched {
		return ErrNoMatch
	}

	age := now.Sub(time.Unix(ts, 0))
	if tolerance > 0 && (age > tolerance || age < -tolerance) {
		return ErrTooOld
	}
	return nil
}
