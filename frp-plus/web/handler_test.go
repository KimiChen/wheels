package web

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticMIMEIgnoresHostMappings(t *testing.T) {
	cases := []struct{ extension, path, contentType string }{
		{".html", "/", "text/html; charset=utf-8"},
		{".css", "/assets/style.css", "text/css; charset=utf-8"},
		{".js", "/assets/theme.js", "text/javascript; charset=utf-8"},
		{".mjs", "/src/app.mjs", "text/javascript; charset=utf-8"},
		{".svg", "/assets/favicon.svg", "image/svg+xml"},
	}
	for _, test := range cases {
		saved := mime.TypeByExtension(test.extension)
		if err := mime.AddExtensionType(test.extension, "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mime.AddExtensionType(test.extension, saved) })
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := httptest.NewRecorder()
			Handler().ServeHTTP(w, httptest.NewRequest(method, test.path, nil))
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != test.contentType {
				t.Errorf("%s %s: status=%d, type=%q", method, test.path, w.Code, w.Header().Get("Content-Type"))
			}
		}
	}
}

func TestStaticAllowlist(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/", "/index.html", "/admin/", "/assets/admin.css", "/src/admin.mjs", "/src/admin-data.mjs", "/src/admin-overview.mjs", "/src/admin-directory.mjs", "/assets/style.css", "/src/app.mjs", "/src/home-data.mjs", "/src/connection-status.mjs", "/src/history-data.mjs", "/src/history-transport.mjs", "/src/history-view.mjs"} {
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
	for _, required := range []string{"/src/admin.mjs", "id=\"github-login\"", "id=\"workspace\" hidden", "id=\"secret-token\""} {
		if !strings.Contains(body, required) {
			t.Fatalf("missing %s", required)
		}
	}
	for _, forbidden := range []string{"<script>", "onclick=", "token_sha256", "localStorage", "id=\"login-token\"", "id=\"login-form\""} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unexpected %s", forbidden)
		}
	}
}

func TestETagValidation(t *testing.T) {
	h := Handler()
	etags := map[string]string{}
	for _, path := range []string{"/", "/assets/style.css", "/src/app.mjs", "/node/1", "/admin/"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		etag := w.Header().Get("ETag")
		if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) != 34 {
			t.Fatalf("%s: malformed ETag %q", path, etag)
		}
		etags[path] = etag
	}
	if etags["/"] == etags["/src/app.mjs"] || etags["/"] == etags["/node/1"] {
		t.Fatal("distinct documents share an ETag")
	}
	for _, header := range []string{etags["/"], `W/` + etags["/"], `"other", ` + etags["/"], "*"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("If-None-Match", header)
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Fatalf("If-None-Match %q: status %d, body %d bytes", header, w.Code, w.Body.Len())
		}
		if w.Header().Get("ETag") != etags["/"] {
			t.Fatal("304 lost the validator")
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("If-None-Match", `"stale"`)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("stale validator: status %d", w.Code)
	}
}

func TestNodePageRoutes(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/node/1", "/node/1/", "/node/9223372036854775807"} {
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
	for _, path := range []string{"/node/0", "/node/01", "/node/-1", "/node/9223372036854775808", "/node/short", "/node/", "/node/1/extra", "/node/test%2Fnode1", "/node/../admin.html", "/node.html", "/node/" + strings.Repeat("a", 129)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("unexpected route %s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/node/1", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("node page accepted a write")
	}
	for _, path := range []string{"/src/node.mjs", "/src/node-data.mjs", "/src/node-settings.mjs", "/assets/node.css"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("missing node asset: %s", path)
		}
	}
}
