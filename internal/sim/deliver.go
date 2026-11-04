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
	URL    string
	Secret string
	Client *http.Client
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
