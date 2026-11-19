package adminhttp

import (
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Deepak1365/relayguard/internal/admin"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "127.0.0.1:8090"
	rec := httptest.NewRecorder()
	Handler(nil, "8090", admin.Options{}, nil).ServeHTTP(rec, req)
	return rec
}

func TestStaticFilesServed(t *testing.T) {
	cases := []struct{ path, ctype, marker string }{
		{"/", "text/html; charset=utf-8", "<title>RelayGuard admin</title>"},
		{"/static/app.js", "text/javascript; charset=utf-8", "RelayGuard admin UI"},
		{"/static/style.css", "text/css; charset=utf-8", "prefers-color-scheme"},
	}
	for _, c := range cases {
		rec := get(t, c.path)
		if rec.Code != 200 {
			t.Errorf("%s: status %d, want 200", c.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.ctype)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
			t.Errorf("%s: CSP %q", c.path, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q", c.path, got)
		}
		if !strings.Contains(rec.Body.String(), c.marker) {
			t.Errorf("%s: body does not contain %q", c.path, c.marker)
		}
	}
}

// Anything that is not one of the embedded files is the JSON no_route 404:
// unknown names, directories, traversal attempts, wrong methods on assets.
func TestStaticMissingIsJSON404(t *testing.T) {
	for _, path := range []string{"/static/nope.js", "/static/", "/static", "/static/app", "/static/..%2fserver.go", "/static/static.go", "/static/a/b.js"} {
		rec := get(t, path)
		if rec.Code != 404 {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: Content-Type %q, want application/json", path, got)
		}
		var b struct{ Error string }
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || b.Error != "no_route" {
			t.Errorf("%s: body %q, want error no_route", path, rec.Body.String())
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: missing CSP on a 404", path)
		}
	}
}

func TestStaticEmbeddedFilesPresent(t *testing.T) {
	for _, name := range []string{"static/index.html", "static/app.js", "static/style.css"} {
		b, err := fs.ReadFile(staticFiles, name)
		if err != nil || len(b) == 0 {
			t.Errorf("%s: embedded file missing or empty (%v)", name, err)
		}
	}
}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(staticFiles, "static/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The CSP is the second line of defence; the first is that untrusted text is
// never parsed as markup. This pins the first: no API that turns a string into
// markup or code may appear in the UI source (same idea as
// TestPreviewSourceHasNoCommit in internal/admin).
// Note: `https?://` also fails on a harmless URL in a comment (accepted).
func TestUISourceHasNoMarkupOrCodeSinks(t *testing.T) {
	banned := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\s*\(|new\s+Function|setTimeout\s*\(\s*["'` + "`" + `]|setInterval\s*\(\s*["'` + "`" + `]|localStorage|sessionStorage|XMLHttpRequest|https?://|DOMParser|createContextualFragment|srcdoc|\.(href|src)\s*=|location(\.href)?\s*=`)
	for _, name := range []string{"app.js", "index.html"} {
		if m := banned.FindString(readStatic(t, name)); m != "" {
			t.Errorf("%s contains %q; the UI must render untrusted text with textContent only", name, m)
		}
	}
}

// el() is the one place that sets attributes from a name. It must refuse
// anything outside a short allow-list, so on*/style/srcdoc cannot be set by a
// later careless call. Checked on the source because testing el() at runtime
// would need a JS engine.
func TestElAttributeAllowList(t *testing.T) {
	js := readStatic(t, "app.js")
	m := regexp.MustCompile(`var ALLOWED_ATTRS = \[([^\]]*)\];`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js has no `var ALLOWED_ATTRS = [...]` allow-list")
	}
	names := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1)
	if len(names) == 0 {
		t.Fatal("ALLOWED_ATTRS is empty")
	}
	for _, n := range names {
		lower := strings.ToLower(n[1])
		if strings.HasPrefix(lower, "on") || lower == "style" || lower == "srcdoc" || lower == "src" {
			t.Errorf("ALLOWED_ATTRS contains %q; el() must not set handlers, styles or frames", n[1])
		}
	}
	if !strings.Contains(js, "ALLOWED_ATTRS.indexOf(k) >= 0) node.setAttribute(k, attrs[k]);") ||
		!strings.Contains(js, `throw new Error("el: attribute not allowed: " + k)`) {
		t.Error("el() must setAttribute only for allow-listed names and throw otherwise")
	}
	// A call site passing an on* key would only fail at runtime; catch it here.
	if m := regexp.MustCompile(`[{,]\s*"?on[a-z]+"?\s*:`).FindString(js); m != "" {
		t.Errorf("app.js passes an event-handler attribute %q", m)
	}
	// No other setAttribute call may bypass el().
	if n := strings.Count(js, "setAttribute("); n != 1 {
		t.Errorf("app.js has %d setAttribute calls, want exactly 1 (inside el)", n)
	}
}

// With CSP default-src 'self' none of these would run anyway; the test keeps
// the page honest so nobody "fixes" a blocked inline handler by loosening the CSP.
func TestIndexHasNoInlineCodeOrStyle(t *testing.T) {
	html := readStatic(t, "index.html")
	checks := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"<script> without src", regexp.MustCompile(`(?is)<script(?:\s+[^>]*)?>`)},
		{"<style> element", regexp.MustCompile(`(?i)<style\b`)},
		{"style= attribute", regexp.MustCompile(`(?i)\sstyle\s*=`)},
		{"on*= handler attribute", regexp.MustCompile(`(?i)\son[a-z]+\s*=`)},
		{"javascript: URL", regexp.MustCompile(`(?i)javascript:`)},
	}
	for _, c := range checks {
		if c.name == "<script> without src" {
			// Every <script> tag must carry src=.
			for _, tag := range c.re.FindAllString(html, -1) {
				if !regexp.MustCompile(`(?i)\ssrc\s*=`).MatchString(tag) {
					t.Errorf("index.html has %s: %s", c.name, tag)
				}
			}
			continue
		}
		if m := c.re.FindString(html); m != "" {
			t.Errorf("index.html has %s: %q", c.name, m)
		}
	}
	// app.js must not set style attributes either (CSP style-src would block it).
	if regexp.MustCompile(`\.style\b|setAttribute\(\s*["']style["']|\bstyle\s*:`).MatchString(readStatic(t, "app.js")) {
		t.Error("app.js sets inline styles; use CSS classes")
	}
}
