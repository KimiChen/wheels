package monitor

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

func testMonitor(t *testing.T) (*Service, string) {
	t.Helper()
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	cfg := testControlConfig(t, "Test node", token)
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, token
}
func fixture(t *testing.T, name string) *shared.Frame {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "shared", "testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := shared.DecodeFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func dial(t *testing.T, s *Service, token, session string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	c, _, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	h := fixture(t, "hello").Hello
	h.SessionID = session
	h.Capabilities = []string{"metrics.v1", "frp.v1"}
	send(t, c, "hello", "hello-1", h)
	c.SetReadDeadline(time.Now().Add(time.Second))
	var result struct {
		Result shared.HelloResult `json:"result"`
	}
	if err = c.ReadJSON(&result); err != nil || result.Result.SessionID != session {
		t.Fatalf("hello: %v %+v", err, result)
	}
	return c
}
func send(t *testing.T, c *websocket.Conn, method, id string, p any) {
	t.Helper()
	if err := c.WriteJSON(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id,omitempty"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, p}); err != nil {
		t.Fatal(err)
	}
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func TestAuthenticationAndCapabilityNegotiation(t *testing.T) {
	s, token := testMonitor(t)
	for _, auth := range []string{"", "Bearer invalid", "Bearer " + token + "x"} {
		h := http.Header{}
		h.Set("Authorization", auth)
		c, r, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", h)
		if c != nil {
			c.Close()
		}
		if err == nil || r == nil || r.StatusCode != 401 {
			t.Fatalf("expected 401, got %v %v", r, err)
		}
		r.Body.Close()
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	c, _, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", h)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hello := fixture(t, "hello").Hello
	hello.Capabilities = append(hello.Capabilities, "future-unsupported.v1")
	send(t, c, "hello", "probe", hello)
	c.SetReadDeadline(time.Now().Add(time.Second))
	var answer struct {
		Result shared.HelloResult `json:"result"`
	}
	if err = c.ReadJSON(&answer); err != nil {
		t.Fatal(err)
	}
	if hasCapability(answer.Result.Capabilities, "future-unsupported.v1") || !hasCapability(answer.Result.Capabilities, "metrics.v1") {
		t.Fatalf("capabilities must be a supported subset: %v", answer.Result.Capabilities)
	}
}
func TestSessionOwnershipSequenceAndRedaction(t *testing.T) {
	s, token := testMonitor(t)
	first := dial(t, s, token, "session-first")
	r := fixture(t, "report-first").Report
	r.SessionID = "session-first"
	send(t, first, "report", "", r)
	eventually(t, func() bool { return s.snapshot(time.Now()).Nodes[0].Metrics != nil })
	second := dial(t, s, token, "session-second")
	first.Close()
	time.Sleep(20 * time.Millisecond)
	n := s.snapshot(time.Now()).Nodes[0]
	if n.Session != "online" {
		t.Fatal("old session close removed new owner")
	}
	r.SessionID = "session-second"
	send(t, second, "report", "", r)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].sequence == 2 })
	data, _ := json.Marshal(s.snapshot(time.Now()))
	for _, secret := range []string{"hostname", "ipv4", "ipv6", "kernel", "boot_id", "iface", "raw_client_id", "local_target", "example-client", "example-node", "example-boot-id", "token"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("public leak: %s", secret)
		}
	}
	if !strings.Contains(string(data), `"18446744073709551615"`) {
		t.Fatal("public uint64 lost decimal precision")
	}
	send(t, second, "report", "", r)
	second.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := second.ReadMessage(); err == nil {
		t.Fatal("duplicate sequence accepted")
	}
	eventually(t, func() bool { return s.snapshot(time.Now()).Nodes[0].Session == "offline" })
}
func TestWrongSessionAndLateReportsRejected(t *testing.T) {
	s, token := testMonitor(t)
	c := dial(t, s, token, "current-session")
	r := fixture(t, "report-first").Report
	r.SessionID = "previous-session"
	send(t, c, "report", "", r)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("wrong session accepted")
	}
	if s.snapshot(time.Now()).Nodes[0].Metrics != nil {
		t.Fatal("wrong session changed metrics")
	}
}
func TestFreshnessIndependentFromSessionAndFRP(t *testing.T) {
	s, token := testMonitor(t)
	if got := s.snapshot(time.Now()).Nodes[0]; got.Session != "waiting" || got.Freshness != "waiting" {
		t.Fatal(got)
	}
	c := dial(t, s, token, "freshness-session")
	r := fixture(t, "report-first").Report
	r.SessionID = "freshness-session"
	r.Extensions = fixture(t, "hello").Hello.Extensions
	send(t, c, "report", "", r)
	eventually(t, func() bool { return s.snapshot(time.Now()).Nodes[0].Metrics != nil })
	now := time.Now()
	n := s.snapshot(now.Add(11 * time.Second)).Nodes[0]
	if n.Session != "online" || n.Freshness != "stale" || n.FRP.ControlState != "disconnected" {
		t.Fatal(n)
	}
	s.mu.Lock()
	s.cfg.ReportIntervalSeconds = 60
	s.mu.Unlock()
	if s.snapshot(now.Add(179 * time.Second)).Nodes[0].Freshness != "fresh" || s.snapshot(now.Add(181 * time.Second)).Nodes[0].Freshness != "stale" {
		t.Fatal("interval freshness threshold not respected")
	}
}
func TestPublicAPIAndImmediateSSE(t *testing.T) {
	s, _ := testMonitor(t)
	r, err := http.Get("http://" + s.Address() + "/api/public/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out PublicSnapshot
	if err = json.NewDecoder(r.Body).Decode(&out); err != nil || len(out.Nodes) != 1 || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot: %v %+v", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+"/events/public", nil)
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	reader := bufio.NewReader(r.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: snapshot\n" {
		t.Fatalf("SSE: %q %v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data: {") {
		t.Fatal("missing immediate snapshot", line, err)
	}
}
func testControlConfig(t *testing.T, name, token string) shared.MonitorConfig {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "control.sqlite")
	db, err := control.Open(control.Config{Path: path, ReportInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateNode(context.Background(), control.DefaultNodeConfig(name), tokenHash(token), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	// BindPort 0 is rejected by configuration validation; reserve an ephemeral
	// port for the test listener instead.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return shared.MonitorConfig{Enabled: true, BindAddr: "127.0.0.1", BindPort: port, ServerID: "example", ReportIntervalSeconds: 1, DatabaseFile: path}
}
func TestControlDatabaseRequiredAndPrivate(t *testing.T) {
	token, _ := randomToken()
	cfg := testControlConfig(t, "A", token)
	if err := os.Chmod(cfg.DatabaseFile, 0644); err != nil {
		t.Fatal(err)
	}
	if svc, err := Start(context.Background(), cfg); err == nil {
		svc.Close()
		t.Fatal("public database accepted")
	}
	cfg.DatabaseFile = ""
	if _, err := Start(context.Background(), cfg); err == nil {
		t.Fatal("missing control database accepted")
	}
}
func TestCloseTerminatesOpenSession(t *testing.T) {
	s, token := testMonitor(t)
	c := dial(t, s, token, "close-session")
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked")
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("expected closed connection", err)
	}
}

func TestMissingFRPExtensionDoesNotKeepOldConnectedState(t *testing.T) {
	s, token := testMonitor(t)
	c := dial(t, s, token, "frp-state-session")
	r := fixture(t, "report-first").Report
	r.SessionID = "frp-state-session"
	r.Extensions = fixture(t, "hello").Hello.Extensions
	r.Extensions.FRP.ControlState = "connected"
	send(t, c, "report", "", r)
	eventually(t, func() bool { return s.snapshot(time.Now()).Nodes[0].FRP.ControlState == "connected" })
	r.Sequence = 3
	r.Extensions = nil
	send(t, c, "report", "", r)
	eventually(t, func() bool { return s.snapshot(time.Now()).Nodes[0].FRP.ControlState == "unknown" })
}
func TestOversizedFrameAndSSELimit(t *testing.T) {
	s, token := testMonitor(t)
	c := dial(t, s, token, "limits-session")
	_ = c.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", shared.MaxFrameBytes+1)))
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("oversized frame accepted")
	}
	for i := 0; i < cap(s.streamsPublic); i++ {
		s.streamsPublic <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(s.streamsPublic); i++ {
			<-s.streamsPublic
		}
	}()
	res, err := http.Get("http://" + s.Address() + "/events/public")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatalf("unbounded SSE admission: %d", res.StatusCode)
	}
}

func TestWSAuthenticatesBeforeRateLimit(t *testing.T) {
	s, token := testMonitor(t)
	s.rateMu.Lock()
	s.rateTokens = -100
	s.rateAt = time.Now()
	s.rateMu.Unlock()
	dial := func(auth string) int {
		h := http.Header{}
		if auth != "" {
			h.Set("Authorization", "Bearer "+auth)
		}
		c, r, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", h)
		if c != nil {
			c.Close()
		}
		if err == nil {
			t.Fatal("rate-limited dial succeeded")
		}
		defer r.Body.Close()
		return r.StatusCode
	}
	// An exhausted limiter must not hide authentication outcomes.
	if got := dial(""); got != 401 {
		t.Fatalf("unauthenticated under rate limit: %d", got)
	}
	if got := dial(token); got != 429 {
		t.Fatalf("authenticated under rate limit: %d", got)
	}
}

func TestWSControlFailureReportsUnavailable(t *testing.T) {
	s, agent, _ := testAdmin(t)
	s.control.Close()
	s.configMu.Lock()
	s.reloadCredentials()
	s.configMu.Unlock()
	h := http.Header{"Authorization": []string{"Bearer " + agent}}
	c, r, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", h)
	if c != nil {
		c.Close()
	}
	if err == nil || r == nil || r.StatusCode != 503 {
		status := 0
		if r != nil {
			status = r.StatusCode
		}
		t.Fatalf("control failure answered %d, want 503: %v", status, err)
	}
	r.Body.Close()
}

func TestPublishedSnapshotSharesEncodedBytes(t *testing.T) {
	s, _ := testMonitor(t)
	s.publishSnapshot(time.Now())
	cached := s.publicJSON.Load()
	if cached == nil {
		t.Fatal("publish did not cache encoded bytes")
	}
	want, err := json.Marshal(s.public.Load())
	if err != nil || string(*cached) != string(want) {
		t.Fatal("cached bytes diverge from published snapshot")
	}
	before := *cached
	r, err := http.Get("http://" + s.Address() + "/api/public/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	// A once-per-second republish may land between the two reads; the response
	// must equal one of the cached generations, proving no per-request marshal.
	after := *s.publicJSON.Load()
	if string(data) != string(before) && string(data) != string(after) {
		t.Fatal("public API did not serve the cached encoding")
	}
}
