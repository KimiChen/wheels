package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticAllowlist(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/", "/index.html", "/admin/", "/assets/admin.css", "/src/admin.mjs", "/src/admin-data.mjs", "/assets/style.css", "/src/app.mjs", "/src/history-data.mjs", "/src/history-transport.mjs", "/src/history-view.mjs"} {
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
	for _, path := range []string{"/README.md", "/handler.go", "/assets/", "/assets/../handler.go", "/src/", "/tests/format.test.mjs", "/.env", "/admin", "/admin.html", "/admin/../.env", "/api/public/v1/nodes"} {
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

func TestAdminStaticShellContainsNoPrivateValues(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("admin shell caching")
	}
	body := w.Body.String()
	for _, required := range []string{"/src/admin.mjs", "id=\"login-token\"", "id=\"workspace\" hidden", "id=\"secret-token\""} {
		if !strings.Contains(body, required) {
			t.Fatalf("missing %s", required)
		}
	}
	for _, forbidden := range []string{"<script>", "onclick=", "token_sha256", "localStorage"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unexpected %s", forbidden)
		}
	}
}

func TestNodePageRoutes(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/node/test-node-1", "/node/test-node-1/"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
			if method == http.MethodGet && !strings.Contains(w.Body.String(), "/src/node.mjs") {
				t.Fatal("node page was not served")
			}
		}
	}
	for _, path := range []string{"/node/short", "/node/", "/node/test-node-1/extra", "/node/test%2Fnode1", "/node/../admin.html", "/node.html", "/node/" + strings.Repeat("a", 129)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("unexpected route %s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/node/test-node-1", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("node page accepted a write")
	}
	for _, path := range []string{"/src/node.mjs", "/src/node-data.mjs", "/assets/node.css"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("missing node asset: %s", path)
		}
	}
}
