package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLoadPublicRouteBlocklistDefault(t *testing.T) {
	t.Setenv("PUBLIC_ROUTE_BLOCKLIST_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("DATA_DIR", "")

	list, err := LoadPublicRouteBlocklist()
	require.NoError(t, err)
	require.True(t, list.Enabled)
	require.True(t, list.UsingDefault)
	require.False(t, list.Matches("/register"))
	require.False(t, list.Matches("/email-verify"))
	require.True(t, list.Matches("/home"))
	require.True(t, list.Matches("/api/v1/settings/public"))
	require.True(t, list.Matches("/api/v1/auth/oauth/github/start"))
	require.True(t, list.Allows("/responses"))
	require.True(t, list.Allows("/v1/responses"))
	require.True(t, list.Allows("/v1/usage"))
	require.True(t, list.Allows("/v1/chat/completions"))
	require.True(t, list.RequiresUnauthorized("/api/v1/keys"))
	require.False(t, list.RequiresUnauthorized("/v1/usage"))
	require.False(t, list.Matches("/login"))
}

func TestLoadPublicRouteBlocklistFromFile(t *testing.T) {
	path := writeBlocklistFile(t, `
enabled: false
not_found:
  rules:
    - match: exact
      path: /hidden
    - match: exact
      path: /hidden
    - match: prefix
      path: /private/
allow:
  rules:
    - match: exact
      path: /responses
unauthorized:
  rules:
    - match: prefix
      path: /api/v1/
`)
	t.Setenv("PUBLIC_ROUTE_BLOCKLIST_FILE", path)
	t.Setenv("DATA_DIR", "")

	list, err := LoadPublicRouteBlocklist()
	require.NoError(t, err)
	require.False(t, list.Enabled)
	require.False(t, list.UsingDefault)
	require.Equal(t, path, list.Source)
	require.Len(t, list.Rules, 2)
	require.Len(t, list.AllowRules, 1)
	require.Len(t, list.UnauthorizedRules, 1)
	require.False(t, list.Matches("/hidden"))
}

func TestLoadPublicRouteBlocklistFromLegacyFile(t *testing.T) {
	path := writeBlocklistFile(t, `
enabled: true
rules:
  - match: exact
    path: /hidden
  - match: exact
    path: /hidden
`)
	t.Setenv("PUBLIC_ROUTE_BLOCKLIST_FILE", path)
	t.Setenv("DATA_DIR", "")

	list, err := LoadPublicRouteBlocklist()
	require.NoError(t, err)
	require.Len(t, list.Rules, 1)
	require.True(t, list.Matches("/hidden"))
}

func TestLoadPublicRouteBlocklistRejectsInvalidRules(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "bad_match",
			body: `
enabled: true
not_found:
  rules:
    - match: glob
      path: /hidden
`,
		},
		{
			name: "bad_path",
			body: `
enabled: true
unauthorized:
  rules:
    - match: exact
      path: hidden
`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			path := writeBlocklistFile(t, tc.body)
			t.Setenv("PUBLIC_ROUTE_BLOCKLIST_FILE", path)
			t.Setenv("DATA_DIR", "")

			_, err := LoadPublicRouteBlocklist()
			require.Error(t, err)
		})
	}
}

func TestPublicRouteBlocklistMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	list, err := normalizePublicRouteBlocklist(publicRouteBlocklistFile{
		Enabled: boolPtr(true),
		NotFound: publicRouteRuleModule{
			Rules: []PublicRouteBlocklistRule{
				{Match: "exact", Path: "/hidden"},
				{Match: "prefix", Path: "/private/"},
				{Match: "prefix", Path: "/responses/hidden"},
			},
		},
		Allow: publicRouteRuleModule{
			Rules: []PublicRouteBlocklistRule{
				{Match: "exact", Path: "/responses"},
				{Match: "exact", Path: "/v1/usage"},
			},
		},
		Unauthorized: publicRouteRuleModule{
			Rules: []PublicRouteBlocklistRule{
				{Match: "prefix", Path: "/api/v1/"},
			},
		},
	}, "test", false)
	require.NoError(t, err)

	router := gin.New()
	router.Use(PublicRouteBlocklistMiddleware(list))
	router.Any("/*path", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	tests := []struct {
		name       string
		path       string
		headers    map[string]string
		wantStatus int
		wantBody   string
	}{
		{name: "not_found_exact", path: "/hidden?x=1", wantStatus: http.StatusNotFound, wantBody: "404 Not Found"},
		{name: "not_found_prefix", path: "/private/value", wantStatus: http.StatusNotFound, wantBody: "404 Not Found"},
		{name: "setup_exact", path: "/setup", wantStatus: http.StatusNotFound, wantBody: "404 Not Found"},
		{name: "setup_prefix", path: "/setup/status", wantStatus: http.StatusNotFound, wantBody: "404 Not Found"},
		{name: "public_login", path: "/login", wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "public_register", path: "/register", wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "public_email_verify", path: "/email-verify", wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "allow_overrides_not_found", path: "/responses", wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "allow_v1_usage", path: "/v1/usage", wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "api_v1_without_auth", path: "/api/v1/keys", wantStatus: http.StatusUnauthorized, wantBody: `{"code":"UNAUTHORIZED","message":"ERROR"}`},
		{name: "api_v1_with_bearer", path: "/api/v1/keys", headers: map[string]string{"Authorization": "Bearer token"}, wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "api_v1_with_admin_key", path: "/api/v1/admin/users", headers: map[string]string{"x-api-key": "admin"}, wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "static_assets", path: "/assets/app.js", wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "static_app_assets", path: "/static/app/res/app.js", wantStatus: http.StatusOK, wantBody: "ok"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			router.ServeHTTP(w, req)

			require.Equal(t, tc.wantStatus, w.Code)
			require.Equal(t, tc.wantBody, w.Body.String())
		})
	}
}

func TestPublicRouteBlocklistMiddlewareDisabledStillHidesSetup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	list, err := normalizePublicRouteBlocklist(publicRouteBlocklistFile{
		Enabled: boolPtr(false),
		NotFound: publicRouteRuleModule{
			Rules: []PublicRouteBlocklistRule{
				{Match: "exact", Path: "/hidden"},
			},
		},
		Unauthorized: publicRouteRuleModule{
			Rules: []PublicRouteBlocklistRule{
				{Match: "prefix", Path: "/api/v1/"},
			},
		},
	}, "test", false)
	require.NoError(t, err)

	router := gin.New()
	router.Use(PublicRouteBlocklistMiddleware(list))
	router.Any("/*path", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hidden", nil))
	require.Equal(t, http.StatusOK, w.Code)

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil))
	require.Equal(t, http.StatusOK, w.Code)

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/setup/status", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
}

func writeBlocklistFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "public-route-blocklist.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func boolPtr(v bool) *bool {
	return &v
}
