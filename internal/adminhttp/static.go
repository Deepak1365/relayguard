package adminhttp

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The UI is three plain files compiled into the binary, so `admin serve` has
// nothing to deploy and the page always matches the API it ships with.
//
//go:embed static/index.html static/app.js static/style.css
var staticFiles embed.FS

// contentTypes is explicit instead of mime.TypeByExtension: on Windows that
// reads the registry, and a wrong type for .js would break the page under
// nosniff.
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
}

func serveStatic(w http.ResponseWriter, r *http.Request, name string) {
	ext := name[strings.LastIndex(name, "."):]
	ct, ok := contentTypes[ext]
	b, err := fs.ReadFile(staticFiles, "static/"+name)
	if !ok || err != nil {
		// A plain-text 404 from the stdlib; jsonErrors turns it into the JSON
		// no_route body like every other unknown path. No directory listings:
		// only the three embedded names are ever served.
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func serveIndex(w http.ResponseWriter, r *http.Request) { serveStatic(w, r, "index.html") }

func serveAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.ContainsAny(name, "/\\") || !strings.Contains(name, ".") {
		http.NotFound(w, r)
		return
	}
	serveStatic(w, r, name)
}
