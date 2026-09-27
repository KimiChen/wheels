package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

type fakeSampler struct {
	facts   shared.Facts
	metrics shared.Metrics
	n       atomic.Uint64
}

func (f *fakeSampler) Facts() shared.Facts { return f.facts }
func (f *fakeSampler) Metrics() shared.Metrics {
	m := f.metrics
	v := f.n.Add(1)
	m.Uptime = shared.Field[uint64]{Value: &v, Quality: shared.QualityOK}
	return m
}
func fixtures(t *testing.T) *fakeSampler {
	t.Helper()
	read := func(name string) *shared.Frame {
		data, err := os.ReadFile(filepath.Join("..", "..", "shared", "testdata", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		f, err := shared.DecodeFrame(data)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	return &fakeSampler{facts: read("hello").Hello.Facts, metrics: *read("report-first").Report.Metrics}
}
func testConfig(t *testing.T) shared.AgentConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return shared.AgentConfig{Enabled: true, TokenFile: path, ServerID: "example", IntervalSeconds: 1, AllowInsecureLoopback: true}
}
func TestClientTLSHandshakeReconnectLatestAndFacts(t *testing.T) {
	cfg := testConfig(t)
	samples := fixtures(t)
	reports := make(chan *shared.Report, 4)
	sessions := make(chan string, 4)
	var connections atomic.Int32
	var bad atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			bad.Store(true)
			w.WriteHeader(401)
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		f, err := shared.DecodeFrame(data)
		if err != nil || f.Hello == nil {
			bad.Store(true)
			return
		}
		sessions <- f.Hello.SessionID
		if err = c.WriteJSON(struct {
			JSONRPC string             `json:"jsonrpc"`
			ID      string             `json:"id"`
			Result  shared.HelloResult `json:"result"`
		}{"2.0", f.ID, shared.HelloResult{Schema: 1, SessionID: f.Hello.SessionID, Capabilities: []string{"metrics.v1", "frp.v1"}, ReportInterval: 1}}); err != nil {
			return
		}
		_, data, err = c.ReadMessage()
		if err != nil {
			return
		}
		f, err = shared.DecodeFrame(data)
		if err != nil || f.Report == nil || f.Report.Facts == nil {
			bad.Store(true)
			return
		}
		reports <- f.Report
		if connections.Add(1) == 1 {
			return
		}
		for {
			if _, _, err = c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	cfg.Endpoint = "wss" + strings.TrimPrefix(server.URL, "https") + "/agent/v1/ws"
	cfg.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := start(context.Background(), cfg, func() shared.FRP { panic("adapter failure") }, samples)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	take := func() *shared.Report {
		select {
		case r := <-reports:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("did not receive report")
			return nil
		}
	}
	first := take()
	for i := 0; i < 100; i++ {
		s.sample()
	}
	if len(s.notify) > 1 {
		t.Fatal("latest notification queue grew")
	}
	second := take()
	if first.SessionID == second.SessionID || first.Sequence != 2 || second.Sequence != 2 || second.Extensions != nil || *second.Metrics.Uptime.Value < 101 {
		t.Fatalf("reconnect did not send newest sample: %+v", second)
	}
	if bad.Load() {
		t.Fatal("invalid client handshake/report")
	}
}
func TestClientRejectsUntrustedTLS(t *testing.T) {
	cfg := testConfig(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS reached HTTP handler") }))
	defer server.Close()
	cfg.Endpoint = "wss" + strings.TrimPrefix(server.URL, "https") + "/agent/v1/ws"
	s, err := start(context.Background(), cfg, nil, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	c, _, dialErr := s.dialer.DialContext(context.Background(), cfg.Endpoint, nil)
	if c != nil {
		c.Close()
	}
	var unknown x509.UnknownAuthorityError
	if dialErr == nil || !errors.As(dialErr, &unknown) {
		t.Fatalf("expected unknown CA rejection, got %v", dialErr)
	}
	s.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		t.Fatal("untrusted TLS connected")
	}
}
func TestClientAndMonitorEndToEndTLS(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	tokenData, _ := os.ReadFile(cfg.TokenFile)
	token := strings.TrimSpace(string(tokenData))
	digest := sha256.Sum256([]byte(token))
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(realDir, 0700)
	database := filepath.Join(realDir, "control.sqlite")
	db, err := control.Open(control.Config{Path: database})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateNode(context.Background(), control.DefaultNodeConfig("Synthetic node"), hex.EncodeToString(digest[:]), nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portListener.Addr().(*net.TCPAddr).Port
	portListener.Close()
	certSource := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := certSource.TLS.Certificates[0]
	certSource.Close()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600)
	der, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600)
	mon, err := monitor.Start(context.Background(), shared.MonitorConfig{Enabled: true, BindAddr: "127.0.0.1", BindPort: port, ServerID: "example", DatabaseFile: database, CertFile: certFile, KeyFile: keyFile, ReportIntervalSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer mon.Close()
	cfg.Endpoint = "wss://" + mon.Address() + "/agent/v1/ws"
	cfg.CAFile = certFile
	agent, err := start(context.Background(), cfg, nil, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = agent.dialer.TLSClientConfig.Clone()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	defer transport.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get("https://" + mon.Address() + "/api/public/v1/nodes")
		if err != nil {
			t.Fatal(err)
		}
		var out monitor.PublicSnapshot
		err = json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Nodes) == 1 && out.Nodes[0].Metrics != nil && out.Nodes[0].Session == "online" && out.Nodes[0].Freshness == "fresh" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("TLS report not visible")
}
func TestSlowSamplingStillHeartbeats(t *testing.T) {
	cfg := testConfig(t)
	cfg.IntervalSeconds = 3600
	pong := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(8 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		f, err := shared.DecodeFrame(data)
		if err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": shared.HelloResult{Schema: 1, SessionID: f.Hello.SessionID, Capabilities: []string{"metrics.v1"}, ReportInterval: 3600}})
		c.SetPingHandler(func(string) error {
			select {
			case pong <- struct{}{}:
			default:
			}
			return c.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second))
		})
		for {
			if _, _, err = c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/ws"
	s, err := start(context.Background(), cfg, nil, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case <-pong:
	case <-time.After(7 * time.Second):
		t.Fatal("heartbeat coupled to slow metric interval")
	}
}
func TestTokenPermissionsAndCloseDuringOutage(t *testing.T) {
	cfg := testConfig(t)
	os.Chmod(cfg.TokenFile, 0644)
	if _, err := readToken(cfg.TokenFile); err == nil {
		t.Fatal("insecure token mode accepted")
	}
	os.Chmod(cfg.TokenFile, 0600)
	cfg.Endpoint = "ws://127.0.0.1:1/agent/v1/ws"
	s, err := start(context.Background(), cfg, nil, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Close()
	if time.Since(start) > time.Second {
		t.Fatal("outage blocked shutdown")
	}
}

type slowSampler struct {
	unblock <-chan struct{}
	base    *fakeSampler
}

func (s slowSampler) Facts() shared.Facts     { <-s.unblock; return s.base.Facts() }
func (s slowSampler) Metrics() shared.Metrics { return s.base.Metrics() }

type panicSampler struct{ base *fakeSampler }

func (s panicSampler) Facts() shared.Facts     { panic("synthetic collection failure") }
func (s panicSampler) Metrics() shared.Metrics { return s.base.Metrics() }
func TestSlowCollectorDoesNotBlockStartOrShutdown(t *testing.T) {
	cfg := testConfig(t)
	cfg.Endpoint = "ws://127.0.0.1:1/agent/v1/ws"
	unblock := make(chan struct{})
	begin := time.Now()
	s, err := start(context.Background(), cfg, nil, slowSampler{unblock, fixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(begin) > 200*time.Millisecond {
		t.Fatal("slow collector blocked Start")
	}
	begin = time.Now()
	s.Close()
	if elapsed := time.Since(begin); elapsed > 2500*time.Millisecond {
		t.Fatal("slow collector blocked Close", elapsed)
	}
	close(unblock)
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("collector did not unwind")
	}
}
func TestCollectorPanicIsLocal(t *testing.T) {
	cfg := testConfig(t)
	cfg.Endpoint = "ws://127.0.0.1:1/agent/v1/ws"
	s, err := start(context.Background(), cfg, nil, panicSampler{fixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case <-s.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("collector panic did not stop telemetry")
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("collector panic leaked worker")
	}
}

func selfSignedCA(t *testing.T, commonName string) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM
}
func TestPinnedCARejectsOtherIssuers(t *testing.T) {
	cfg := testConfig(t)
	// httptest serves the same built-in localhost certificate everywhere, so
	// both sides need freshly generated, distinct self-signed CAs.
	otherCert, _ := selfSignedCA(t, "unpinned")
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("another issuer reached the HTTP handler") }))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{otherCert}}
	other.StartTLS()
	defer other.Close()
	_, pinnedPEM := selfSignedCA(t, "pinned")
	cfg.Endpoint = "wss" + strings.TrimPrefix(other.URL, "https") + "/agent/v1/ws"
	cfg.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cfg.CAFile, pinnedPEM, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := start(context.Background(), cfg, nil, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, _, dialErr := s.dialer.DialContext(context.Background(), cfg.Endpoint, nil)
	if c != nil {
		c.Close()
	}
	var unknown x509.UnknownAuthorityError
	if dialErr == nil || !errors.As(dialErr, &unknown) {
		t.Fatalf("pinned CA accepted another issuer, got %v", dialErr)
	}
}
func TestShortSessionsKeepBackingOff(t *testing.T) {
	cfg := testConfig(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		f, err := shared.DecodeFrame(data)
		if err != nil || f.Hello == nil {
			return
		}
		attempts.Add(1)
		// Accept hello, then close immediately: a healthy monitor holds sessions.
		_ = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": shared.HelloResult{Schema: 1, SessionID: f.Hello.SessionID, Capabilities: []string{"metrics.v1"}, ReportInterval: 1}})
	}))
	defer server.Close()
	cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/ws"
	s, err := start(context.Background(), cfg, nil, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	time.Sleep(6 * time.Second)
	if got := attempts.Load(); got == 0 || got > 3 {
		t.Fatalf("short sessions reset the reconnect backoff: %d attempts in 6s", got)
	}
}
func TestStableSessionsResetBackoff(t *testing.T) {
	defer func(saved time.Duration) { minStableSession = saved }(minStableSession)
	minStableSession = 200 * time.Millisecond
	cfg := testConfig(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(4 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		f, err := shared.DecodeFrame(data)
		if err != nil || f.Hello == nil {
			return
		}
		attempts.Add(1)
		if err = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": shared.HelloResult{Schema: 1, SessionID: f.Hello.SessionID, Capabilities: []string{"metrics.v1"}, ReportInterval: 1}}); err != nil {
			return
		}
		// Hold the session past minStableSession before closing.
		time.Sleep(400 * time.Millisecond)
	}))
	defer server.Close()
	cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/ws"
	s, err := start(context.Background(), cfg, nil, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	time.Sleep(7 * time.Second)
	if got := attempts.Load(); got < 4 {
		t.Fatalf("stable sessions did not reset the reconnect backoff: %d attempts in 7s", got)
	}
}
func TestOversizeReportDropsExtensionsOnce(t *testing.T) {
	cfg := testConfig(t)
	cfg.IntervalSeconds = 3600
	reports := make(chan *shared.Report, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		f, err := shared.DecodeFrame(data)
		if err != nil || f.Hello == nil {
			return
		}
		if err = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": shared.HelloResult{Schema: 1, SessionID: f.Hello.SessionID, Capabilities: []string{"metrics.v1", "frp.v1"}, ReportInterval: 3600}}); err != nil {
			return
		}
		for {
			_, data, err = c.ReadMessage()
			if err != nil {
				return
			}
			f, err = shared.DecodeFrame(data)
			if err != nil || f.Report == nil {
				return
			}
			reports <- f.Report
		}
	}))
	defer server.Close()
	cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/ws"
	small := shared.FRP{Association: shared.FRPAssociation{ServerID: "example"}, Version: "test", ControlState: "connected",
		Proxies: []shared.Proxy{{Name: "ssh", Type: "tcp", Enabled: true, Status: "running"}}}
	big := small
	big.Proxies = make([]shared.Proxy, 0, shared.MaxProxies)
	for i := 0; i < shared.MaxProxies; i++ {
		target := strings.Repeat("t", 900)
		big.Proxies = append(big.Proxies, shared.Proxy{Name: fmt.Sprintf("proxy-%03d-%s", i, strings.Repeat("n", 200)), Type: "tcp", LocalTarget: &target, Enabled: true, Status: "running"})
	}
	var useBig atomic.Bool
	s, err := start(context.Background(), cfg, func() shared.FRP {
		if useBig.Load() {
			return big
		}
		return small
	}, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	take := func() *shared.Report {
		select {
		case r := <-reports:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("did not receive report")
			return nil
		}
	}
	first := take()
	if first.Extensions == nil || first.Extensions.FRP == nil {
		t.Fatal("small report lost its extensions")
	}
	useBig.Store(true)
	s.sample()
	second := take()
	if second.Extensions != nil || second.Facts == nil || second.Metrics == nil {
		t.Fatalf("oversize report was not degraded to facts+metrics: %+v", second)
	}
	if second.SessionID != first.SessionID || second.Sequence != first.Sequence+1 {
		t.Fatal("degraded retry broke the session")
	}
}
func TestNegotiatedIntervalReplacesPendingLongTimer(t *testing.T) {
	cfg := testConfig(t)
	cfg.IntervalSeconds = 3600
	reports := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(4 * time.Second))
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		f, err := shared.DecodeFrame(data)
		if err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": shared.HelloResult{Schema: 1, SessionID: f.Hello.SessionID, Capabilities: []string{"metrics.v1"}, ReportInterval: 1}})
		for {
			_, data, err = c.ReadMessage()
			if err != nil {
				return
			}
			f, err = shared.DecodeFrame(data)
			if err != nil || f.Report == nil {
				return
			}
			select {
			case reports <- struct{}{}:
			default:
			}
		}
	}))
	defer server.Close()
	cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/ws"
	s, err := start(context.Background(), cfg, nil, fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deadline := time.After(2500 * time.Millisecond)
	for i := 0; i < 2; i++ {
		select {
		case <-reports:
		case <-deadline:
			t.Fatal("negotiated interval left old timer pending")
		}
	}
}
