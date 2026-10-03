package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
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
	for _, path := range []string{"/", "/index.html", "/admin/", "/assets/admin.css", "/src/admin.mjs", "/src/admin-data.mjs", "/src/admin-frp.mjs", "/src/admin-overview.mjs", "/src/admin-directory.mjs", "/src/admin-editor.mjs", "/assets/style.css", "/src/app.mjs", "/src/home-data.mjs", "/src/connection-status.mjs", "/src/history-data.mjs", "/src/history-transport.mjs", "/src/history-view.mjs"} {
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
	for _, path := range []string{"/README.md", "/handler.go", "/assets/", "/assets/../handler.go", "/src/", "/tests/format.test.mjs", "/.env", "/admin", "/admin.html", "/admin/../.env", "/api/public/v1/nodes", "/partials/", "/partials/header.html", "/partials/footer.html", "/partials/admin/dashboard.html", "/partials/admin/nodes.html", "/partials/admin/groups.html"} {
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
		sum := sha256.Sum256(w.Body.Bytes())
		if want := `"` + hex.EncodeToString(sum[:16]) + `"`; etag != want {
			t.Fatalf("%s: ETag %q does not describe served body; want %q", path, etag, want)
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

// These checks describe the DOM contract consumed by the page modules. They
// inspect the served document so accidentally serving template source also fails.
func TestComposedPageDOM(t *testing.T) {
	cases := []struct {
		path string
		ids  []string
	}{
		{"/", []string{"main", "search-toggle", "node-search-dialog", "node-search", "node-list", "node-template", "connection-notice", "fleet-rx", "fleet-tx"}},
		{"/node/1", []string{"main", "node-empty", "node-detail", "node-history", "connection-notice"}},
		{"/admin/", []string{"main", "notice", "login-panel", "github-login", "workspace", "logout", "snapshot-at", "node-list", "group-form", "probes-form", "server-registry", "create-form", "settings-form", "secret-token", "admin-node-row"}},
	}
	idAttribute := regexp.MustCompile(`\s+id="([^"]+)"`)
	h := Handler()
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, test.path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			body := w.Body.String()
			if strings.Contains(body, "{{") || strings.Contains(body, "}}") {
				t.Fatal("unrendered template action in page")
			}
			for tag, class := range map[string]string{"header": "fm-topbar", "footer": "fm-footer"} {
				count := 0
				for _, opening := range regexp.MustCompile(`<`+tag+`\b[^>]*>`).FindAllString(body, -1) {
					for _, token := range strings.Fields(htmlAttribute(opening, "class")) {
						if token == class {
							count++
						}
					}
				}
				if count != 1 {
					t.Errorf("site %s count = %d, want 1", tag, count)
				}
			}
			ids := map[string]bool{}
			for _, match := range idAttribute.FindAllStringSubmatch(body, -1) {
				if ids[match[1]] {
					t.Errorf("duplicate DOM id %q", match[1])
				}
				ids[match[1]] = true
			}
			for _, id := range test.ids {
				if !ids[id] {
					t.Errorf("missing DOM id %q", id)
				}
			}
			if count := strings.Count(body, "data-theme-toggle"); count != 1 {
				t.Errorf("theme toggle count = %d, want 1", count)
			}
		})
	}
}

func TestComposedAdminInitialState(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	body := w.Body.String()
	hidden := regexp.MustCompile(`\s+hidden(?:\s|=|>)`)
	for _, id := range []string{"workspace", "login-panel", "logout"} {
		if !hidden.MatchString(htmlElement(t, body, "", "id", id)) {
			t.Errorf("%s is exposed before authentication resolves", id)
		}
	}
	for _, panel := range []string{"dashboard", "nodes", "groups", "probes", "frp", "access"} {
		opening := htmlElement(t, body, "section", "data-admin-panel", panel)
		if hidden.MatchString(opening) != (panel != "dashboard") {
			t.Errorf("unexpected initial visibility of admin panel %s", panel)
		}
		link := htmlElement(t, body, "a", "data-admin-view", panel)
		if (htmlAttribute(link, "aria-current") == "page") != (panel == "dashboard") {
			t.Errorf("unexpected initial navigation selection for %s", panel)
		}
	}
	for _, id := range []string{"create-dialog", "credential-result", "node-panel"} {
		opening := htmlElement(t, body, "dialog", "id", id)
		if regexp.MustCompile(`\s+open(?:\s|=|>)`).MatchString(opening) {
			t.Errorf("dialog %s is initially open", id)
		}
		if id != "create-dialog" && !hidden.MatchString(opening) {
			t.Errorf("dialog %s lost its initial hidden state", id)
		}
	}
}

func TestFragmentsAreNotPublicRoutes(t *testing.T) {
	h := Handler()
	err := fs.WalkDir(content, "partials", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, "/"+name, nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s /%s exposes a fragment: status %d", method, name, w.Code)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSharedFragmentsRenderIntoEveryPage(t *testing.T) {
	source := templateSources(t)
	before, err := renderPages(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"header", "footer"} {
		name := "partials/" + part + ".html"
		original := source[name]
		if original == nil {
			t.Fatalf("missing shared fragment %s", name)
		}
		marker := "shared-" + part + "-regression-marker"
		updated := strings.Replace(string(original.Data), "</"+part+">", "<span>"+marker+"</span></"+part+">", 1)
		if updated == string(original.Data) {
			t.Fatalf("shared fragment has no closing %s tag", part)
		}
		source[name] = &fstest.MapFile{Data: []byte(updated)}
		after, err := renderPages(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, page := range []string{"index.html", "node.html", "admin.html"} {
			if bytes.Equal(before[page], after[page]) || bytes.Count(after[page], []byte(marker)) != 1 {
				t.Errorf("%s does not include exactly one updated shared %s", page, part)
			}
		}
		source[name] = original
	}
}

func TestPageRenderingRejectsBrokenFragments(t *testing.T) {
	for _, name := range []string{"partials/header.html", "partials/footer.html", "partials/admin/dashboard.html"} {
		t.Run("missing/"+name, func(t *testing.T) {
			source := templateSources(t)
			delete(source, name)
			if _, err := renderPages(source); err == nil {
				t.Fatal("missing required fragment did not fail rendering")
			}
		})
	}
	t.Run("invalid-template", func(t *testing.T) {
		source := templateSources(t)
		source["partials/header.html"] = &fstest.MapFile{Data: []byte("{{if}}")}
		if _, err := renderPages(source); err == nil {
			t.Fatal("invalid fragment did not fail rendering")
		}
	})
}

func templateSources(t *testing.T) fstest.MapFS {
	t.Helper()
	source := fstest.MapFS{}
	err := fs.WalkDir(content, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".html") {
			return err
		}
		data, err := content.ReadFile(name)
		if err == nil {
			source[name] = &fstest.MapFile{Data: data}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// The shell uses static, double-quoted HTML attributes; no parser dependency is
// needed to check the individual opening tags that form its module contract.
func htmlAttribute(opening, name string) string {
	match := regexp.MustCompile(`\s+` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(opening)
	if match == nil {
		return ""
	}
	return match[1]
}

func htmlElement(t *testing.T, body, tag, attribute, value string) string {
	t.Helper()
	if tag == "" {
		tag = `[a-z][a-z0-9-]*`
	}
	var matches []string
	for _, opening := range regexp.MustCompile(`<`+tag+`\b[^>]*>`).FindAllString(body, -1) {
		if htmlAttribute(opening, attribute) == value {
			matches = append(matches, opening)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected one element with %s=%q, found %d", attribute, value, len(matches))
	}
	return matches[0]
}
