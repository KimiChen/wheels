// Package web serves the public and admin applications' embedded static assets.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed index.html node.html admin.html assets src
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
	"/src/admin-overview.mjs":    "src/admin-overview.mjs",
	"/src/admin-directory.mjs":   "src/admin-directory.mjs",
	"/src/admin-editor.mjs":      "src/admin-editor.mjs",
	"/assets/style.css":          "assets/style.css",
	"/assets/script.js":          "assets/script.js",
	"/assets/app.css":            "assets/app.css",
	"/assets/node.css":           "assets/node.css",
	"/assets/theme.js":           "assets/theme.js",
	"/assets/favicon.svg":        "assets/favicon.svg",
	"/src/app.mjs":               "src/app.mjs",
	"/src/home-data.mjs":         "src/home-data.mjs",
	"/src/node.mjs":              "src/node.mjs",
	"/src/node-data.mjs":         "src/node-data.mjs",
	"/src/node-settings.mjs":     "src/node-settings.mjs",
	"/src/format.mjs":            "src/format.mjs",
	"/src/store.mjs":             "src/store.mjs",
	"/src/transport.mjs":         "src/transport.mjs",
	"/src/connection-status.mjs": "src/connection-status.mjs",
	"/src/history-data.mjs":      "src/history-data.mjs",
	"/src/history-transport.mjs": "src/history-transport.mjs",
	"/src/history-view.mjs":      "src/history-view.mjs",
}

// A node page is a public shell; the live snapshot determines whether it exists.
// Do not turn arbitrary paths into a catch-all application route.
var nodePagePath = regexp.MustCompile(`^/node/[1-9][0-9]{0,18}/?$`)

// ETags are strong validators derived from embedded content, which is immutable
// for the lifetime of the process; Cache-Control: no-cache clients revalidate
// with them and receive 304 instead of the unchanged body.
var etags = func() map[string]string {
	names := make(map[string]struct{}, len(publicFiles)+1)
	for _, name := range publicFiles {
		names[name] = struct{}{}
	}
	names["node.html"] = struct{}{}
	result := make(map[string]string, len(names))
	for name := range names {
		data, err := content.ReadFile(name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		result[name] = `"` + hex.EncodeToString(sum[:16]) + `"`
	}
	return result
}()

// If-None-Match uses weak comparison; the embedded validators are strong.
func etagMatch(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
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
		if !ok && nodePagePath.MatchString(r.URL.Path) {
			if _, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/node/"), "/"), 10, 64); err == nil {
				name, ok = "node.html", true
			}
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if etag, ok := etags[name]; ok {
			w.Header().Set("ETag", etag)
			if etagMatch(r.Header.Get("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		data, err := content.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// Embedded assets have fixed formats; do not let host MIME mappings
		// change whether browsers accept scripts and styles with nosniff.
		switch path.Ext(name) {
		case ".html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case ".js", ".mjs":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case ".svg":
			w.Header().Set("Content-Type", "image/svg+xml")
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
}
