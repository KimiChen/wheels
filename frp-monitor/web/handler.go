// Package web serves the public and admin applications' embedded static assets.
package web

import (
	"bytes"
	"embed"
	"net/http"
	"time"
)

//go:embed index.html admin.html assets src
var content embed.FS

// This list is intentionally explicit: adding documentation, tests, or Go source
// beside an asset must never make that file publicly accessible.
var publicFiles = map[string]string{
	"/":                          "index.html",
	"/index.html":                "index.html",
	"/admin/":                    "admin.html",
	"/assets/admin.css":          "assets/admin.css",
	"/src/admin.mjs":             "src/admin.mjs",
	"/src/admin-data.mjs":        "src/admin-data.mjs",
	"/assets/style.css":          "assets/style.css",
	"/assets/script.js":          "assets/script.js",
	"/assets/app.css":            "assets/app.css",
	"/assets/theme.js":           "assets/theme.js",
	"/assets/favicon.svg":        "assets/favicon.svg",
	"/src/app.mjs":               "src/app.mjs",
	"/src/format.mjs":            "src/format.mjs",
	"/src/store.mjs":             "src/store.mjs",
	"/src/transport.mjs":         "src/transport.mjs",
	"/src/history-data.mjs":      "src/history-data.mjs",
	"/src/history-transport.mjs": "src/history-transport.mjs",
	"/src/history-view.mjs":      "src/history-view.mjs",
}

// Handler provides the static application only. The monitor owns API/SSE routes.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-cache")
		if r.URL.Path == "/admin/" {
			w.Header().Set("Cache-Control", "no-store")
		}
		name, ok := publicFiles[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, err := content.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if len(name) > 4 && name[len(name)-4:] == ".mjs" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}
