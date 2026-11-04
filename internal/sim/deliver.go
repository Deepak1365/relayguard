package sim

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/Deepak1365/relayguard/internal/stripesig"
)

// Sender signs payloads like Stripe and POSTs them to a receiver.
type Sender struct {
	URL       string
	Secret    string
	HealthURL string // optional; used to wait while a crashed receiver restarts
	Client    *http.Client
}

// WaitHealthy polls HealthURL until it answers 200 or the timeout passes.
func (s *Sender) WaitHealthy(timeout time.Duration) bool {
	if s.HealthURL == "" {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		resp, err := s.Client.Get(s.HealthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Sender) Send(payload []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, s.URL, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", stripesig.Sign(payload, s.Secret, time.Now()))
	resp, err := s.Client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

type DeliveryStats struct {
	Posted          int // HTTP requests made
	Accepted        int // 2xx responses
	ProviderRetries int // resends after a non-2xx or network error
	GaveUp          int // deliveries abandoned after maxRetries resends
}

// Deliver sends the plan in order. Like a real provider, a delivery that is
// not acknowledged with 2xx is resent later (here: appended to the queue).
func Deliver(s *Sender, plan []Event, maxRetries int) DeliveryStats {
	type pending struct {
		e       Event
		retries int
	}
	queue := make([]pending, len(plan))
	for i, e := range plan {
		queue[i] = pending{e: e}
	}

	var stats DeliveryStats
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		stats.Posted++
		status, err := s.Send(p.e.Payload)
		if err == nil && status >= 200 && status < 300 {
			stats.Accepted++
			continue
		}
		if err != nil {
			// Receiver unreachable (e.g. crashed and restarting): a real provider
			// would retry later, so give it a moment before continuing.
			s.WaitHealthy(5 * time.Second)
		}
		if p.retries < maxRetries {
			p.retries++
			stats.ProviderRetries++
			queue = append(queue, p)
		} else {
			stats.GaveUp++
		}
	}
	return stats
}
