package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func githubFixtureClient(login string) *http.Client {
	return &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"access_token":"github-test","token_type":"bearer"}`
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("code_verifier") == "" {
				return nil, errors.New("missing PKCE")
			}
		case "https://api.github.com/user":
			if r.Header.Get("Authorization") != "Bearer github-test" {
				return nil, errors.New("missing authorization")
			}
			body = `{"login":"` + login + `","id":42,"type":"User"}`
		default:
			return nil, errors.New("unexpected OAuth endpoint")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
}
func testAdmin(t *testing.T) (*Service, string, string) {
	t.Helper()
	agentToken, _ := randomToken()
	cfg := testControlConfig(t, "Public name", agentToken)
	secret := filepath.Join(filepath.Dir(cfg.DatabaseFile), "github.secret")
	if err := os.WriteFile(secret, []byte("synthetic-oauth-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubClientID = "synthetic-client"
	cfg.GitHubClientSecretFile = secret
	cfg.GitHubCallbackURL = "http://127.0.0.1:1/api/admin/v1/auth/github/callback"
	cfg.GitHubAdminUsers = []string{"operator"}
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.cfg.GitHubCallbackURL = "http://" + s.Address() + "/api/admin/v1/auth/github/callback"
	s.admin.github.callback = s.cfg.GitHubCallbackURL
	s.admin.github.client = githubFixtureClient("operator")
	return s, agentToken, "github-test"
}
func adminRequest(t *testing.T, s *Service, method, path, body string, cookie *http.Cookie, csrf string, headers http.Header) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, "http://"+s.Address()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	for k, v := range headers {
		r.Header[k] = v
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
func oauthStart(t *testing.T, s *Service) (*http.Cookie, string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get("http://" + s.Address() + "/api/admin/v1/auth/github")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 302 {
		t.Fatalf("OAuth start: %d", res.StatusCode)
	}
	u, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "github.com" || u.Query().Get("code_challenge_method") != "S256" || len(u.Query().Get("code_challenge")) != 43 || u.Query().Get("scope") != "" {
		t.Fatal("unsafe OAuth authorization request")
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].SameSite != http.SameSiteLaxMode || !cookies[0].HttpOnly {
		t.Fatal("OAuth cookie")
	}
	return cookies[0], u.Query().Get("state")
}
func oauthCallback(t *testing.T, s *Service, cookie *http.Cookie, state string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", "http://"+s.Address()+"/api/admin/v1/auth/github/callback?code=synthetic-code&state="+url.QueryEscape(state), nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}
func login(t *testing.T, s *Service, _ string) (*http.Cookie, adminSession) {
	t.Helper()
	oauthCookie, state := oauthStart(t, s)
	res := oauthCallback(t, s, oauthCookie, state)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/admin/" {
		t.Fatalf("OAuth callback %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == adminCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 28800 {
		t.Fatal("unsafe admin cookie")
	}
	response := adminRequest(t, s, "GET", "/api/admin/v1/session", "", cookie, "", nil)
	var session adminSession
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&session) != nil {
		t.Fatal("session unavailable")
	}
	return cookie, session
}
func expectStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status %d expected %d body %s", response.StatusCode, want, data)
	}
}

func TestAdminAuthenticationCSRFAndRedaction(t *testing.T) {
	s, agent, admin := testAdmin(t)
	dial(t, s, agent, "private-facts")
	for _, path := range []string{"/api/admin/v1/session", "/api/admin/v1/nodes", "/api/admin/v1/probes", "/events/admin"} {
		expectStatus(t, adminRequest(t, s, "GET", path, "", nil, "", nil), 401)
	}
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/login", `{"token":"`+admin+`"}`, nil, "", http.Header{"Origin": []string{"https://evil.invalid"}}), 404)
	cookie, session := login(t, s, admin)
	if len(session.CSRF) != 43 || time.Until(session.ExpiresAt) < 7*time.Hour {
		t.Fatal("bad session")
	}
	private := adminRequest(t, s, "GET", "/api/admin/v1/nodes", "", cookie, "", nil)
	expectStatus(t, private, 200)
	data, _ := io.ReadAll(private.Body)
	if !strings.Contains(string(data), `"hostname"`) || !strings.Contains(string(data), `"raw_client_id"`) {
		t.Fatal("private facts missing")
	}
	for _, secret := range []string{"token_sha256", agent, admin, tokenHash(agent), tokenHash(admin)} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("private read exposed credential %q", secret)
		}
	}
	public := adminRequest(t, s, "GET", "/api/public/v1/nodes", "", nil, "", nil)
	data, _ = io.ReadAll(public.Body)
	for _, secret := range []string{"hostname", "local_target", "frp_binding", "raw_client_id", "token"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("public private-field leak")
		}
	}
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/nodes", `{"name":"New"}`, cookie, "", nil), 403)
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/nodes", `{"name":"New"}`, cookie, session.CSRF, http.Header{"Sec-Fetch-Site": []string{"cross-site"}}), 403)
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/logout", "", cookie, session.CSRF, nil), 204)
	expectStatus(t, adminRequest(t, s, "GET", "/api/admin/v1/nodes", "", cookie, "", nil), 401)
}
func TestAdminSessionExpirySecureCookieAndLoginThrottle(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, _ := login(t, s, admin)
	s.admin.mu.Lock()
	session := s.admin.sessions[tokenHash(cookie.Value)]
	session.ExpiresAt = time.Now().Add(-time.Second)
	s.admin.sessions[tokenHash(cookie.Value)] = session
	s.admin.mu.Unlock()
	expectStatus(t, adminRequest(t, s, "GET", "/api/admin/v1/session", "", cookie, "", nil), 401)
	req := httptest.NewRequest("GET", "https://monitor.invalid/", nil)
	record := httptest.NewRecorder()
	if err := s.createAdminSession(record, req, "operator"); err != nil {
		t.Fatal(err)
	}
	if !record.Result().Cookies()[0].Secure {
		t.Fatal("TLS cookie not secure")
	}
	s.admin.mu.Lock()
	s.admin.tokens = 0
	s.admin.at = time.Now()
	s.admin.mu.Unlock()
	record = httptest.NewRecorder()
	s.githubStart(record, httptest.NewRequest("GET", "http://"+s.Address()+"/api/admin/v1/auth/github", nil))
	if record.Code != 429 {
		t.Fatal("OAuth attempts unbounded")
	}
}
func TestAdminRotationRevocationAndPendingHello(t *testing.T) {
	s, agent, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	established := dial(t, s, agent, "rotation-session")
	headers := http.Header{"Authorization": []string{"Bearer " + agent}}
	pending, _, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	response := adminRequest(t, s, "POST", "/api/admin/v1/nodes/1/rotate", "", cookie, session.CSRF, nil)
	expectStatus(t, response, 200)
	var rotated map[string]string
	if err = json.NewDecoder(response.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	if rotated["token"] == agent || len(rotated["token"]) != 43 {
		t.Fatal("rotation failed")
	}
	for _, c := range []*websocket.Conn{established, pending} {
		c.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := c.ReadMessage(); err == nil {
			t.Fatal("rotated socket remains open")
		}
	}
	failed, r, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", headers)
	if failed != nil {
		failed.Close()
	}
	if err == nil || r == nil || r.StatusCode != 401 {
		t.Fatal("old credential admitted")
	}
	r.Body.Close()
	dial(t, s, rotated["token"], "after-rotation")
	expectStatus(t, adminRequest(t, s, "DELETE", "/api/admin/v1/nodes/1", "", cookie, session.CSRF, nil), 204)
	creds, err := s.control.Nodes(context.Background())
	if err != nil || len(creds) != 0 {
		t.Fatalf("last credential revoke: %v", err)
	}
	expectStatus(t, adminRequest(t, s, "GET", "/api/public/v1/nodes/1/history", "", nil, "", nil), 404)
	if len(s.public.Load().Nodes) != 0 {
		t.Fatal("revoked node remains publicly cached")
	}
	response = adminRequest(t, s, "POST", "/api/admin/v1/nodes", `{"name":"Replacement","frp_binding":{"server_id":"example","user":"public-user","raw_client_id":"stable-client"}}`, cookie, session.CSRF, nil)
	expectStatus(t, response, 201)
	var created map[string]string
	_ = json.NewDecoder(response.Body).Decode(&created)
	if created["id"] != "2" || len(created["token"]) != 43 {
		t.Fatal("invalid new credential")
	}
	creds, err = s.control.Nodes(context.Background())
	if err != nil || len(creds) != 1 || creds[0].Binding == nil {
		t.Fatal("binding not persisted")
	}
	bytes, _ := os.ReadFile(s.cfg.DatabaseFile)
	if strings.Contains(string(bytes), created["token"]) {
		t.Fatal("plaintext token persisted")
	}
	expectStatus(t, adminRequest(t, s, "DELETE", "/api/admin/v1/nodes/"+created["id"]+"/binding", "", cookie, session.CSRF, nil), 204)
}
func TestUnavailableControlRejectsNewAuthentication(t *testing.T) {
	s, agent, _ := testAdmin(t)
	s.control.Close()
	s.configMu.Lock()
	s.reloadCredentials()
	s.configMu.Unlock()
	if !s.credentialError.Load() || s.authenticateCredential(&http.Request{Header: http.Header{"Authorization": []string{"Bearer " + agent}}}).AgentID != "" {
		t.Fatal("unavailable configuration allowed authentication")
	}
}
func TestStrictPrivateJSONAndProbeWrites(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	for _, body := range []string{`{"name":"A","name":"B"}`, `{"Name":"A"}`, `{"name":null}`, `{"name":"A","unknown":1}`} {
		expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/nodes", body, cookie, session.CSRF, nil), 400)
	}
	body := `{"version":2,"nodes":[{"agent_id":"1","tasks":[{"id":"reach","name":"Public","target":"example.invalid:443","interval":30}]}]}`
	expectStatus(t, adminRequest(t, s, "PUT", "/api/admin/v1/probes", body, cookie, session.CSRF, nil), 200)
	expectStatus(t, adminRequest(t, s, "PUT", "/api/admin/v1/probes", body, cookie, session.CSRF, nil), 409)
	expectStatus(t, adminRequest(t, s, "PUT", "/api/admin/v1/probes", `{"version":3,"nodes":[{"agent_id":"unknown-node","tasks":[]}]}`, cookie, session.CSRF, nil), 400)
	s.configMu.Lock()
	s.reloadTasks()
	s.configMu.Unlock()
	if s.taskError.Load() || s.tasks.Load().Version != 2 {
		t.Fatal("task write not durable")
	}
	expectStatus(t, adminRequest(t, s, "PUT", "/api/admin/v1/probes", `{"version":3,"nodes":[]}`, cookie, session.CSRF, nil), 200)
	if len(s.tasks.Load().Nodes) != 0 {
		t.Fatal("tasks not cleared")
	}
}

func TestRevokedNodeLateProbeAndAdminSSE(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	response := adminRequest(t, s, "GET", "/events/admin", "", cookie, "", nil)
	expectStatus(t, response, 200)
	if response.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("unsafe SSE headers")
	}
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/logout", "", cookie, session.CSRF, nil), 204)
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("revoked SSE did not terminate: %v", err)
	}
	s.configMu.Lock()
	s.applyCredentials([]credential{})
	s.configMu.Unlock()
	if s.acceptProbe("1", nil, time.Now(), shared.PingResult{}) {
		t.Fatal("late result from revoked node accepted")
	}
}
func TestGitHubRejectsMissingStateReplayAndNonAdmin(t *testing.T) {
	s, _, _ := testAdmin(t)
	cookie, state := oauthStart(t, s)
	if res := oauthCallback(t, s, nil, state); res.Header.Get("Location") != "/admin/?auth_error=failed" {
		t.Fatal("missing browser state accepted")
	}
	if res := oauthCallback(t, s, cookie, state); res.Header.Get("Location") != "/admin/" {
		t.Fatal("valid state rejected")
	}
	if res := oauthCallback(t, s, cookie, state); res.Header.Get("Location") != "/admin/?auth_error=failed" {
		t.Fatal("replayed state accepted")
	}
	cookie, state = oauthStart(t, s)
	s.admin.github.client = githubFixtureClient("outsider")
	res := oauthCallback(t, s, cookie, state)
	if res.Header.Get("Location") != "/admin/?auth_error=access_denied" {
		t.Fatal("non-admin GitHub identity accepted")
	}
	for _, c := range res.Cookies() {
		if c.Name == adminCookie && c.Value != "" {
			t.Fatal("non-admin received session")
		}
	}
}
func TestGitHubOnlyAndUnconfiguredAdmin(t *testing.T) {
	s, _ := testMonitor(t)
	expectStatus(t, adminRequest(t, s, "GET", "/api/admin/v1/auth", "", nil, "", nil), 200)
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/login", `{"token":"anything"}`, nil, "", nil), 404)
	expectStatus(t, adminRequest(t, s, "GET", "/api/admin/v1/auth/github", "", nil, "", nil), 404)
}
func TestPublicReconciliationCropsIdentityAndTraffic(t *testing.T) {
	s, _, _ := testAdmin(t)
	report := fixture(t, "hello").Hello.Extensions.FRP
	raw := *report.Association.RawClientID
	serverID := report.Association.ServerID
	online := true
	count := "123"
	s.serverSnapshot.Store(&shared.ServerSnapshot{State: "ready", ServerID: serverID, GeneratedAt: time.Now(), Clients: []shared.ServerClient{{RawClientID: &raw, ClientID: raw, Hostname: "secret-host", IP: "192.0.2.44", Online: online}}, Proxies: []shared.ServerProxy{{Name: "secret-tunnel", ClientID: raw, Online: true, TodayRXBytes: &count}}})
	s.mu.Lock()
	n := s.nodes["1"]
	n.frp = report
	n.credential.FRPBinding = &shared.FRPBinding{ServerID: serverID, RawClientID: raw}
	n.conn = &websocket.Conn{}
	n.metrics = fixture(t, "report-first").Report.Metrics
	n.metricsAt = time.Now()
	s.mu.Unlock()
	out := s.snapshot(time.Now())
	if out.Nodes[0].FRP.Reconciliation != "matched" || out.Nodes[0].FRP.Registered != 1 || out.Nodes[0].FRP.ServerOnline == nil || !*out.Nodes[0].FRP.ServerOnline {
		t.Fatalf("unexpected summary %+v", out.Nodes[0].FRP)
	}
	data, _ := json.Marshal(out)
	for _, private := range []string{raw, "secret-host", "192.0.2.44", "secret-tunnel"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("public reconciliation leaked %s", private)
		}
	}
	s.mu.Lock()
	n.conn = nil
	s.mu.Unlock()
}

func TestServerCacheExpiryAndFutureTimestampsFailClosed(t *testing.T) {
	s, _, _ := testAdmin(t)
	now := time.Now()
	server, _ := reconcileFixture()
	for _, age := range []time.Duration{6 * time.Second, -6 * time.Second} {
		server.GeneratedAt = now.Add(-age)
		s.serverSnapshot.Store(&server)
		view := s.currentServerSnapshot(now)
		if view.State != "unavailable" || len(view.Clients) != 0 || len(view.Proxies) != 0 {
			t.Fatalf("stale server reused at age %v", age)
		}
		if len(server.Clients) == 0 || server.State != "ready" {
			t.Fatal("cached snapshot mutated")
		}
		if snapshot := s.adminSnapshot(); snapshot.FRP.State != "unavailable" || len(snapshot.FRP.Clients) != 0 {
			t.Fatal("private API retained stale registry")
		}
	}
	server.GeneratedAt = time.Time{}
	s.serverSnapshot.Store(&server)
	if s.currentServerSnapshot(now).State != "unavailable" {
		t.Fatal("missing timestamp accepted")
	}
	server.GeneratedAt = now
	s.serverSnapshot.Store(&server)
	if s.currentServerSnapshot(now).State != "ready" {
		t.Fatal("fresh snapshot rejected")
	}
}
