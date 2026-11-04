// Package stripeevent holds the subset of Stripe's event format that RelayGuard reads.
package stripeevent

import "encoding/json"

// Event is a Stripe "snapshot" webhook event.
type Event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"` // Unix seconds when Stripe created the event
	Data    struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type Customer struct {
	ID    string  `json:"id"`
	Email *string `json:"email"` // pointer: Stripe may send null
}

type Invoice struct {
	ID         string `json:"id"`
	Customer   string `json:"customer"`
	AmountPaid int64  `json:"amount_paid"` // in cents
}

type Subscription struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Status   string `json:"status"`
}
