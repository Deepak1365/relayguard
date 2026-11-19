package adminhttp

import (
	"net/http/httptest"
	"testing"

	"github.com/Deepak1365/relayguard/internal/admin"
)

func TestParseAddr(t *testing.T) {
	good := map[string]string{"127.0.0.1:8090": "8090", "127.0.0.1:1": "1", "127.0.0.1:65535": "65535",
		"127.0.0.1:08090": "8090"} // normalized: browsers send 8090 in Host
	for addr, port := range good {
		host, p, err := ParseAddr(addr)
		if err != nil || host != "127.0.0.1" || p != port {
			t.Errorf("ParseAddr(%q) = %q, %q, %v; want 127.0.0.1, %s", addr, host, p, err, port)
		}
	}
	bad := []string{
		"0.0.0.0:8090",     // all IPv4 interfaces
		":8090",            // empty host = all interfaces
		"[::]:8090",        // all IPv6 interfaces
		"[::1]:8090",       // IPv6 loopback: refused on purpose (see ParseAddr)
		"localhost:8090",   // hostname
		"example.com:8090", // hostname
		"192.168.1.5:8090", // LAN address
		"127.0.0.2:8090",   // loopback range, but not the allow-listed literal
		"127.0.0.1",        // no port
		"127.0.0.1:",       // empty port
		"127.0.0.1:0",      // port 0: the Host allow-list could not know the port
		"127.0.0.1:65536",
		"127.0.0.1:http",
		"",
	}
	for _, addr := range bad {
		if host, port, err := ParseAddr(addr); err == nil {
			t.Errorf("ParseAddr(%q) = %q, %q, nil; want an error", addr, host, port)
		}
	}
}

// Every response carries the CSP and nosniff headers: success, host
// rejection, and mux errors (404). Uses routes that need no database.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := Handler(nil, "8090", admin.Options{}, nil)
	cases := []struct {
		name, method, path, host string
		want                     int
	}{
		{"placeholder 200", "GET", "/", "127.0.0.1:8090", 200},
		{"bad host 403", "GET", "/", "evil.example:8090", 403},
		{"unknown path 404", "GET", "/nope", "127.0.0.1:8090", 404},
		{"get on post route 405", "GET", "/api/events/1/replay", "127.0.0.1:8090", 405},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Host = c.host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
			t.Errorf("%s: CSP %q, want default-src 'self'", c.name, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q, want nosniff", c.name, got)
		}
	}
}
