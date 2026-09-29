package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// Live facts join the shared management snapshot on its next publication.
	var data []byte
	eventually(t, func() bool {
		private := adminRequest(t, s, "GET", "/api/admin/v1/nodes", "", cookie, "", nil)
		expectStatus(t, private, 200)
		data, _ = io.ReadAll(private.Body)
		private.Body.Close()
		return strings.Contains(string(data), `"hostname"`) && strings.Contains(string(data), `"raw_client_id"`)
	})
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

func TestPublicAndAdminSSEQuotasAreIndependent(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, _ := login(t, s, admin)
	for i := 0; i < cap(s.streamsPublic); i++ {
		s.streamsPublic <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(s.streamsPublic); i++ {
			<-s.streamsPublic
		}
	}()
	expectStatus(t, adminRequest(t, s, "GET", "/events/public", "", nil, "", nil), 503)
	// The admin stream keeps its own quota and still revalidates the session.
	expectStatus(t, adminRequest(t, s, "GET", "/events/admin", "", cookie, "", nil), 200)
}

func TestAdminJSONContentTypeCaseInsensitive(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	mixed := http.Header{"Content-Type": []string{"Application/JSON; charset=utf-8"}}
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/nodes", `{"name":"Case"}`, cookie, session.CSRF, mixed), 201)
	wrong := http.Header{"Content-Type": []string{"text/json"}}
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/nodes", `{"name":"Other"}`, cookie, session.CSRF, wrong), 415)
}

func TestNodeIDRequiresInt64RoundTrip(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	if !validNodeID("1") || !validNodeID("9223372036854775807") {
		t.Fatal("valid ID rejected")
	}
	for _, id := range []string{"9223372036854775808", "9999999999999999999", "01", "0", "-1", "1.0"} {
		if validNodeID(id) {
			t.Fatalf("invalid ID accepted: %q", id)
		}
		expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/nodes/"+id+"/rotate", "", cookie, session.CSRF, nil), 404)
		expectStatus(t, adminRequest(t, s, "GET", "/api/public/v1/nodes/"+id+"/history", "", nil, "", nil), 404)
	}
}

func TestGitHubPendingSharedProxyEvictsOldestPeer(t *testing.T) {
	s, _, _ := testAdmin(t)
	a := s.admin
	// Other peers' older attempts must survive even when both the global
	// table and the shared proxy's share are full.
	a.mu.Lock()
	for i := 0; i < maxOAuthPending-maxOAuthPendingPerIP; i++ {
		a.github.pending[fmt.Sprintf("other-peer-%d", i)] = oauthAttempt{ip: fmt.Sprintf("198.51.100.%d", i+1), expires: time.Now().Add(time.Minute)}
	}
	a.mu.Unlock()
	var oldest string
	var oldestCookie, latestCookie *http.Cookie
	for i := 0; i <= maxOAuthPendingPerIP; i++ {
		a.mu.Lock()
		// Isolate pending capacity from the short-term initiation rate limit.
		a.tokens, a.at = 5, time.Now()
		a.mu.Unlock()
		req := httptest.NewRequest("GET", "http://"+s.Address()+"/api/admin/v1/auth/github", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i+1))
		record := httptest.NewRecorder()
		s.githubStart(record, req)
		if record.Code != 302 {
			t.Fatalf("proxy login %d: %d", i+1, record.Code)
		}
		latestCookie = record.Result().Cookies()[0]
		if i == 0 {
			oldestCookie = latestCookie
			oldest = tokenHash(oldestCookie.Value)
			a.mu.Lock()
			attempt := a.github.pending[oldest]
			attempt.expires = time.Now().Add(2 * time.Minute)
			a.github.pending[oldest] = attempt
			a.mu.Unlock()
		}
	}
	a.mu.Lock()
	_, keptOldest := a.github.pending[oldest]
	total := len(a.github.pending)
	peers := 0
	for _, attempt := range a.github.pending {
		if attempt.ip == "127.0.0.1" {
			peers++
		}
	}
	otherPeers := 0
	for i := 0; i < maxOAuthPending-maxOAuthPendingPerIP; i++ {
		if _, kept := a.github.pending[fmt.Sprintf("other-peer-%d", i)]; kept {
			otherPeers++
		}
	}
	a.mu.Unlock()
	if keptOldest || peers != maxOAuthPendingPerIP || total != maxOAuthPending || otherPeers != maxOAuthPending-maxOAuthPendingPerIP {
		t.Fatalf("wrong proxy eviction: oldest=%v peer=%d total=%d other=%d", keptOldest, peers, total, otherPeers)
	}
	if res := oauthCallback(t, s, oldestCookie, oldestCookie.Value); res.Header.Get("Location") != "/admin/?auth_error=failed" {
		t.Fatal("evicted OAuth attempt still accepted")
	}
	if res := oauthCallback(t, s, latestCookie, latestCookie.Value); res.Header.Get("Location") != "/admin/" {
		t.Fatal("new OAuth attempt behind shared proxy could not log in")
	}
}

func TestGitHubPendingCookieReplacementAtCapacity(t *testing.T) {
	for _, size := range []int{maxOAuthPendingPerIP, maxOAuthPending} {
		t.Run(fmt.Sprintf("pending_%d", size), func(t *testing.T) {
			s, _, _ := testAdmin(t)
			a := s.admin
			oldCookie := &http.Cookie{Name: oauthCookie, Value: strings.Repeat("a", 43)}
			oldKey := tokenHash(oldCookie.Value)
			a.mu.Lock()
			for i := 0; i < size; i++ {
				key, ip := fmt.Sprintf("attempt-%d", i), "127.0.0.1"
				if i == maxOAuthPendingPerIP-1 {
					key = oldKey
				} else if i >= maxOAuthPendingPerIP {
					ip = fmt.Sprintf("198.51.100.%d", i)
				}
				a.github.pending[key] = oauthAttempt{ip: ip, expires: time.Now().Add(time.Duration(i+1) * time.Minute)}
			}
			a.mu.Unlock()
			req := httptest.NewRequest("GET", "http://"+s.Address()+"/api/admin/v1/auth/github", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.AddCookie(oldCookie)
			record := httptest.NewRecorder()
			s.githubStart(record, req)
			if record.Code != 302 {
				t.Fatalf("cookie replacement rejected: %d", record.Code)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if _, kept := a.github.pending[oldKey]; kept {
				t.Fatal("old cookie attempt not removed")
			}
			if len(a.github.pending) != size {
				t.Fatalf("replacement removed extra attempts: pending=%d want=%d", len(a.github.pending), size)
			}
			for i := 0; i < size; i++ {
				if i == maxOAuthPendingPerIP-1 {
					continue
				}
				if _, kept := a.github.pending[fmt.Sprintf("attempt-%d", i)]; !kept {
					t.Fatalf("replacement removed unrelated attempt %d", i)
				}
			}
		})
	}
}

func TestGitHubPendingGlobalEvictionAndExpiredCleanup(t *testing.T) {
	for _, expired := range []int{0, 2} {
		t.Run(fmt.Sprintf("expired_%d", expired), func(t *testing.T) {
			s, _, _ := testAdmin(t)
			a := s.admin
			a.mu.Lock()
			for i := 0; i < maxOAuthPending; i++ {
				ttl := time.Duration(i+1) * time.Minute
				if i < expired {
					ttl = -time.Minute
				}
				a.github.pending[fmt.Sprintf("attempt-%d", i)] = oauthAttempt{ip: fmt.Sprintf("198.51.100.%d", i+1), expires: time.Now().Add(ttl)}
			}
			a.mu.Unlock()
			record := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "http://"+s.Address()+"/api/admin/v1/auth/github", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			s.githubStart(record, req)
			if record.Code != 302 {
				t.Fatalf("full table rejected login: %d", record.Code)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			removed := max(expired, 1)
			if len(a.github.pending) != maxOAuthPending-removed+1 {
				t.Fatalf("unexpected pending count: %d", len(a.github.pending))
			}
			for i := 0; i < maxOAuthPending; i++ {
				_, kept := a.github.pending[fmt.Sprintf("attempt-%d", i)]
				if kept != (i >= removed) {
					t.Fatalf("wrong eviction for attempt %d: kept=%v", i, kept)
				}
			}
		})
	}
}

func TestGitHubPendingThrottlePreservesExistingAttempt(t *testing.T) {
	s, _, _ := testAdmin(t)
	cookie, _ := oauthStart(t, s)
	s.admin.mu.Lock()
	s.admin.tokens, s.admin.at = 0, time.Now()
	s.admin.mu.Unlock()
	req := httptest.NewRequest("GET", "http://"+s.Address()+"/api/admin/v1/auth/github", nil)
	req.AddCookie(cookie)
	record := httptest.NewRecorder()
	s.githubStart(record, req)
	if record.Code != 429 || record.Header().Get("Retry-After") != "3" {
		t.Fatalf("throttle response: status=%d retry=%q", record.Code, record.Header().Get("Retry-After"))
	}
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	if _, kept := s.admin.github.pending[tokenHash(cookie.Value)]; !kept || len(s.admin.github.pending) != 1 {
		t.Fatal("throttled login changed pending attempts")
	}
}

func TestAdminSameOriginRejectsMissingCallback(t *testing.T) {
	s, _, _ := testAdmin(t)
	s.cfg.GitHubCallbackURL = ""
	record := httptest.NewRecorder()
	s.githubStart(record, httptest.NewRequest("GET", "http://"+s.Address()+"/api/admin/v1/auth/github", nil))
	if record.Code != 403 {
		t.Fatalf("missing callback tolerated: %d", record.Code)
	}
}

func TestAdminSameOriginNormalizesDefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		name, callback, origin, host string
		want                         bool
	}{
		{"https omitted", "https://monitor.example.invalid:443", "https://monitor.example.invalid", "monitor.example.invalid", true},
		{"https explicit", "https://monitor.example.invalid", "https://monitor.example.invalid:443", "monitor.example.invalid:443", true},
		{"http omitted", "http://127.0.0.1:80", "http://127.0.0.1", "127.0.0.1", true},
		{"http explicit", "http://127.0.0.1", "http://127.0.0.1:80", "127.0.0.1:80", true},
		{"hostname case", "https://MONITOR.example.invalid:443", "https://monitor.example.invalid", "monitor.example.invalid", true},
		{"ipv6", "http://[::1]:80", "http://[::1]", "[::1]", true},
		{"unbracketed ipv6", "http://[::1]:80", "http://::1:80", "[::1]", false},
		{"bracketed domain", "https://monitor.example.invalid", "https://[monitor.example.invalid]", "monitor.example.invalid", false},
		{"custom port", "https://monitor.example.invalid:8443", "https://monitor.example.invalid:8443", "monitor.example.invalid:8443", true},
		{"wrong origin port", "https://monitor.example.invalid", "https://monitor.example.invalid:8443", "monitor.example.invalid", false},
		{"wrong host port", "https://monitor.example.invalid", "https://monitor.example.invalid", "monitor.example.invalid:8443", false},
		{"wrong host", "https://monitor.example.invalid", "https://monitor.example.invalid", "other.example.invalid", false},
		{"wrong scheme", "https://monitor.example.invalid", "http://monitor.example.invalid", "monitor.example.invalid", false},
		{"userinfo", "https://monitor.example.invalid", "https://user@monitor.example.invalid", "monitor.example.invalid", false},
		{"path", "https://monitor.example.invalid", "https://monitor.example.invalid/", "monitor.example.invalid", false},
		{"query", "https://monitor.example.invalid", "https://monitor.example.invalid?x=1", "monitor.example.invalid", false},
		{"empty query", "https://monitor.example.invalid", "https://monitor.example.invalid?", "monitor.example.invalid", false},
		{"fragment", "https://monitor.example.invalid", "https://monitor.example.invalid#x", "monitor.example.invalid", false},
		{"empty fragment", "https://monitor.example.invalid", "https://monitor.example.invalid#", "monitor.example.invalid", false},
		{"empty port", "https://monitor.example.invalid", "https://monitor.example.invalid:", "monitor.example.invalid", false},
		{"overflow port", "https://monitor.example.invalid", "https://monitor.example.invalid:65536", "monitor.example.invalid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{cfg: shared.MonitorConfig{GitHubCallbackURL: tc.callback + "/api/admin/v1/auth/github/callback"}}
			r := httptest.NewRequest(http.MethodPatch, "https://monitor.example.invalid/api/admin/v1/nodes/1/settings", nil)
			r.Host = tc.host
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			if got := s.adminSameOrigin(r); got != tc.want {
				t.Fatalf("same origin = %v, want %v", got, tc.want)
			}
			r.Header.Add("Origin", tc.origin)
			if s.adminSameOrigin(r) {
				t.Fatal("multiple Origin headers accepted")
			}
		})
	}
}

// Network callbacks must never run with the configuration lock held. Wait for
// another goroutine rather than TryLock so an unrelated brief publisher is OK.
func assertConfigUnlocked(t *testing.T, s *Service) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.configMu.Lock(); s.configMu.Unlock(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("configuration lock held during HTTP body I/O")
	}
}

type configUnlockedRecorder struct {
	*httptest.ResponseRecorder
	t *testing.T
	s *Service
}

func (w *configUnlockedRecorder) WriteHeader(status int) {
	assertConfigUnlocked(w.t, w.s)
	w.ResponseRecorder.WriteHeader(status)
}
func (w *configUnlockedRecorder) Write(data []byte) (int, error) {
	assertConfigUnlocked(w.t, w.s)
	return w.ResponseRecorder.Write(data)
}

type configUnlockedBody struct {
	io.Reader
	t *testing.T
	s *Service
}

func (b configUnlockedBody) Read(data []byte) (int, error) {
	assertConfigUnlocked(b.t, b.s)
	return b.Reader.Read(data)
}

func TestAdminProbeBodiesStayOutsideConfigLock(t *testing.T) {
	s, _, _ := testAdmin(t)
	for _, tc := range []struct {
		method, body string
		status       int
	}{
		{http.MethodPut, `{"version":2,"nodes":[]}`, 200},
		{http.MethodPut, `{"version":2,"nodes":[]}`, 409},
		{http.MethodPut, `{"version":3,"nodes":[{"agent_id":"999","tasks":[]}]}`, 400},
		{http.MethodPut, `{"version":`, 400},
		{http.MethodGet, "", 200},
	} {
		r := httptest.NewRequest(tc.method, "/api/admin/v1/probes", configUnlockedBody{strings.NewReader(tc.body), t, s})
		r.Header.Set("Content-Type", "application/json")
		w := &configUnlockedRecorder{httptest.NewRecorder(), t, s}
		s.handleAdminProbes(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d, want %d", tc.method, tc.body, w.Code, tc.status)
		}
	}
}

func TestAdminMutationsWriteResponsesOutsideConfigLock(t *testing.T) {
	for _, action := range []string{"create", "invalid create", "rotate", "binding", "delete", "missing", "storage error"} {
		t.Run(action, func(t *testing.T) {
			s, _, _ := testAdmin(t)
			w := &configUnlockedRecorder{httptest.NewRecorder(), t, s}
			method, path, body, want := http.MethodPost, "1/rotate", "", 200
			switch action {
			case "create":
				body, want = `{"name":"Second"}`, 201
			case "invalid create":
				body, want = `{"name":""}`, 400
			case "binding":
				method, path, body, want = http.MethodPut, "1/binding", `{"server_id":"example","user":"","raw_client_id":"bound"}`, 204
			case "delete":
				method, path, want = http.MethodDelete, "1", 204
			case "missing":
				method, path, want = http.MethodDelete, "999", 404
			case "storage error":
				s.control.Close()
				want = 503
			}
			r := httptest.NewRequest(method, "/api/admin/v1/nodes/"+path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			if action == "create" || action == "invalid create" {
				s.createNode(w, r)
			} else {
				s.mutateNode(w, r, path)
			}
			if w.Code != want {
				t.Fatalf("status %d, want %d", w.Code, want)
			}
		})
	}
}

func TestAdminHEADHasAllowAndCannotMutateWithoutCSRF(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, _ := login(t, s, admin)
	for _, tc := range []struct{ path, allow string }{
		{"auth", "GET"}, {"session", "GET"}, {"logout", "POST"}, {"nodes", "GET, POST"},
		{"nodes/1", "DELETE"}, {"nodes/1/rotate", "POST"}, {"nodes/1/binding", "PUT, DELETE"},
		{"nodes/1/settings", "PATCH"}, {"nodes/1/reset-traffic", "POST"}, {"probes", "GET, PUT"},
	} {
		res := adminRequest(t, s, http.MethodHead, "/api/admin/v1/"+tc.path, "", cookie, "", nil)
		if res.StatusCode != 405 || res.Header.Get("Allow") != tc.allow {
			t.Fatalf("%s: status %d Allow %q", tc.path, res.StatusCode, res.Header.Get("Allow"))
		}
	}
	if _, err := s.control.Get(context.Background(), "1"); err != nil {
		t.Fatal("HEAD removed node", err)
	}
	expectStatus(t, adminRequest(t, s, http.MethodGet, "/api/admin/v1/session", "", cookie, "", nil), 200)
}

func TestAdminCacheDoesNotServeDeletedNode(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	s.publishAdminSnapshot()
	before := s.adminJSON.Load()
	if before == nil || len(s.adminSnapshotBytes()) == 0 {
		t.Fatal("admin cache was not published")
	}
	expectStatus(t, adminRequest(t, s, http.MethodDelete, "/api/admin/v1/nodes/1", "", cookie, session.CSRF, nil), 204)
	// Model an old encoder completing after invalidation. The generation check
	// must reject these bytes, not leak the deleted node until the next tick.
	s.adminJSON.Store(before)
	res := adminRequest(t, s, http.MethodGet, "/api/admin/v1/nodes", "", cookie, "", nil)
	defer res.Body.Close()
	var snapshot adminSnapshot
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&snapshot) != nil || len(snapshot.Nodes) != 0 {
		t.Fatal("GET reused a revoked admin snapshot")
	}
}

func TestAdminSnapshotEncodingSharedByConcurrentReaders(t *testing.T) {
	fixture, _, _ := testAdmin(t)
	n, err := fixture.control.Get(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	// An independent service has no periodic publisher, making concurrent cache
	// misses deterministic without sleeping across ticker boundaries.
	s := &Service{cfg: shared.MonitorConfig{ReportIntervalSeconds: 1}, location: time.UTC,
		admin: &adminState{}, nodes: map[string]*node{"1": {credential: credential{AgentID: "1", Name: n.Name}}}}
	configs := nodeConfigs{"1": n}
	s.configs.Store(&configs)
	start := make(chan struct{})
	results := make(chan []byte, 64)
	for i := 0; i < cap(results); i++ {
		go func() { <-start; results <- s.adminSnapshotBytes() }()
	}
	close(start)
	first := <-results
	if len(first) == 0 {
		t.Fatal("snapshot encoding unavailable")
	}
	for i := 1; i < cap(results); i++ {
		next := <-results
		if len(next) == 0 || &next[0] != &first[0] {
			t.Fatal("concurrent readers did not share one encoding")
		}
	}
}
