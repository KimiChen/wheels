package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

func testAdmin(t *testing.T) (*Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	agentToken, _ := randomToken()
	adminToken, _ := randomToken()
	creds := filepath.Join(dir, "agents.json")
	admin := filepath.Join(dir, "admin.json")
	probes := filepath.Join(dir, "probes.json")
	for path, value := range map[string]any{creds: []credential{{AgentID: "test-node-1", Name: "Public name", TokenSHA256: tokenHash(agentToken)}}, admin: adminCredential{tokenHash(adminToken)}, probes: probeFile{Version: 1, Nodes: probeDocument(&probeBook{Nodes: map[string][]configuredProbe{}}).Nodes}} {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Start(context.Background(), shared.MonitorConfig{Enabled: true, BindAddr: "127.0.0.1", ServerID: "example", CredentialsFile: creds, AdminCredentialsFile: admin, ProbeTasksFile: probes, ReportIntervalSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, agentToken, adminToken
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
func login(t *testing.T, s *Service, token string) (*http.Cookie, adminSession) {
	t.Helper()
	resp := adminRequest(t, s, "POST", "/api/admin/v1/login", `{"token":"`+token+`"}`, nil, "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	var session adminSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing cookie")
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Secure || cookies[0].MaxAge != 28800 {
		t.Fatal("unsafe local session cookie")
	}
	return cookies[0], session
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
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/login", `{"token":"`+admin+`"}`, nil, "", http.Header{"Origin": []string{"https://evil.invalid"}}), 403)
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
	req := httptest.NewRequest("POST", "https://monitor.invalid/api/admin/v1/login", strings.NewReader(`{"token":"`+admin+`"}`))
	req.Header.Set("Content-Type", "application/json")
	record := httptest.NewRecorder()
	s.handleAdmin(record, req)
	if record.Code != 200 || !record.Result().Cookies()[0].Secure {
		t.Fatal("TLS cookie is not secure")
	}
	for i := 0; i < 3; i++ {
		expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/login", `{"token":"invalid"}`, nil, "", nil), 401)
	}
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/login", `{"token":"`+admin+`"}`, nil, "", nil), 429)
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
	response := adminRequest(t, s, "POST", "/api/admin/v1/nodes/test-node-1/rotate", "", cookie, session.CSRF, nil)
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
	expectStatus(t, adminRequest(t, s, "DELETE", "/api/admin/v1/nodes/test-node-1", "", cookie, session.CSRF, nil), 204)
	creds, err := readCredentials(s.cfg.CredentialsFile)
	if err != nil || len(creds) != 0 {
		t.Fatalf("last credential revoke: %v", err)
	}
	expectStatus(t, adminRequest(t, s, "GET", "/api/public/v1/nodes/test-node-1/history", "", nil, "", nil), 404)
	if len(s.public.Load().Nodes) != 0 {
		t.Fatal("revoked node remains publicly cached")
	}
	response = adminRequest(t, s, "POST", "/api/admin/v1/nodes", `{"name":"Replacement","frp_binding":{"server_id":"example","user":"public-user","raw_client_id":"stable-client"}}`, cookie, session.CSRF, nil)
	expectStatus(t, response, 201)
	var created map[string]string
	_ = json.NewDecoder(response.Body).Decode(&created)
	if len(created["id"]) != 43 || len(created["token"]) != 43 {
		t.Fatal("invalid new credential")
	}
	creds, err = readCredentials(s.cfg.CredentialsFile)
	if err != nil || len(creds) != 1 || creds[0].FRPBinding == nil {
		t.Fatal("binding not persisted")
	}
	bytes, _ := os.ReadFile(s.cfg.CredentialsFile)
	if strings.Contains(string(bytes), created["token"]) {
		t.Fatal("plaintext token persisted")
	}
	expectStatus(t, adminRequest(t, s, "DELETE", "/api/admin/v1/nodes/"+created["id"]+"/binding", "", cookie, session.CSRF, nil), 204)
}
func TestCredentialsReloadKeepsValidConfigAndRevokesSessions(t *testing.T) {
	s, agent, admin := testAdmin(t)
	cookie, _ := login(t, s, admin)
	c := dial(t, s, agent, "reload")
	if err := os.WriteFile(s.cfg.CredentialsFile, []byte(`[{"invalid":true}]`), 0600); err != nil {
		t.Fatal(err)
	}
	s.configMu.Lock()
	s.reloadCredentials()
	s.configMu.Unlock()
	if !s.credentialError.Load() || s.authenticate(&http.Request{Header: http.Header{"Authorization": []string{"Bearer " + agent}}}) == "" {
		t.Fatal("invalid reload discarded valid credentials")
	}
	if err := os.WriteFile(s.cfg.CredentialsFile, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	s.configMu.Lock()
	s.reloadCredentials()
	s.configMu.Unlock()
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("revocation did not terminate socket")
	}
	if s.credentialError.Load() {
		t.Fatal("reload error not cleared")
	}
	replacement, _ := randomToken()
	data, _ := json.Marshal(adminCredential{tokenHash(replacement)})
	if err := os.WriteFile(s.cfg.AdminCredentialsFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	s.reloadAdmin()
	expectStatus(t, adminRequest(t, s, "GET", "/api/admin/v1/session", "", cookie, "", nil), 401)
	login(t, s, replacement)
}
func TestStrictPrivateJSONAndProbeWrites(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	for _, body := range []string{`{"name":"A","name":"B"}`, `{"Name":"A"}`, `{"name":null}`, `{"name":"A","unknown":1}`} {
		expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/nodes", body, cookie, session.CSRF, nil), 400)
	}
	body := `{"version":2,"nodes":[{"agent_id":"test-node-1","tasks":[{"id":"reach","name":"Public","target":"example.invalid:443","interval":30}]}]}`
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
	if s.acceptProbe("test-node-1", nil, time.Now(), shared.PingResult{}) {
		t.Fatal("late result from revoked node accepted")
	}
}
func TestPrivateFilesRejectAliasesAndAdminReloadDegraded(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, _ := login(t, s, admin)
	for _, data := range []string{`null`, `[{"Agent_ID":"test-node-1","name":"A","token_sha256":"` + strings.Repeat("a", 64) + `"}]`, `[{"agent_id":"test-node-1","name":"A","name":"B","token_sha256":"` + strings.Repeat("a", 64) + `"}]`} {
		if err := os.WriteFile(s.cfg.CredentialsFile, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readCredentials(s.cfg.CredentialsFile); err == nil {
			t.Fatal("ambiguous credential accepted")
		}
	}
	if err := os.WriteFile(s.cfg.AdminCredentialsFile, []byte(`{"token_sha256":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	s.reloadAdmin()
	expectStatus(t, adminRequest(t, s, "GET", "/api/admin/v1/session", "", cookie, "", nil), 200)
	if s.adminSnapshot().AdminCredentialsState != "degraded" {
		t.Fatal("admin reload error hidden")
	}
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
	n := s.nodes["test-node-1"]
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
	for _, private := range []string{raw, "secret-host", "192.0.2.44", "secret-tunnel", "today_rx_bytes"} {
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
