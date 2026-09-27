package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticAllowlist(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/", "/index.html", "/assets/style.css", "/src/app.mjs", "/src/history-data.mjs", "/src/history-transport.mjs", "/src/history-view.mjs"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatal("missing CSP")
		}
		if strings.HasSuffix(path, ".mjs") && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
			t.Fatal("module MIME type")
		}
	}
	for _, path := range []string{"/README.md", "/handler.go", "/assets/", "/assets/../handler.go", "/src/", "/tests/format.test.mjs", "/.env", "/admin", "/api/public/v1/nodes"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: exposed with status %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("POST accepted")
	}
}
