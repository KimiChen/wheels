// Package monitor owns the independent monitoring listener and latest snapshots.
// It never imports FRP's client/server roots or participates in data forwarding.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/fatedier/frp/extension/frpmonitor/web"
	"github.com/gorilla/websocket"
)

const maxNodes = 1024
const heartbeat = 5 * time.Second
const readTimeout = 15 * time.Second

type credential struct {
	AgentID     string `json:"agent_id"`
	Name        string `json:"name"`
	TokenSHA256 string `json:"token_sha256"`
}
type node struct {
	credential          credential
	conn                *websocket.Conn
	sessionID           string
	sequence            uint64
	seen                bool
	lastSeen, metricsAt time.Time
	facts               *shared.Facts
	metrics             *shared.Metrics
	frp                 *shared.FRP
	probeEnabled        bool
}

type Service struct {
	cfg         shared.MonitorConfig
	ctx         context.Context
	cancel      context.CancelFunc
	server      *http.Server
	listener    net.Listener
	mu          sync.Mutex
	nodes       map[string]*node
	credentials []credential
	connections map[*websocket.Conn]struct{}
	wg          sync.WaitGroup
	closeOnce   sync.Once
	done        chan struct{}
	handshakes  chan struct{}
	streams     chan struct{}
	rateMu      sync.Mutex
	rateTokens  float64
	rateAt      time.Time
	public      atomic.Pointer[PublicSnapshot]
	store       *store.Store
	storeFailed bool
	tasks       atomic.Pointer[probeBook]
	taskError   atomic.Bool
	queries     chan struct{}
}

func Start(ctx context.Context, cfg shared.MonitorConfig) (*Service, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, errors.New("invalid monitor configuration")
	}
	creds, err := readCredentials(cfg.CredentialsFile)
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	s := &Service{cfg: cfg, ctx: child, cancel: cancel, nodes: map[string]*node{}, credentials: creds, connections: map[*websocket.Conn]struct{}{}, handshakes: make(chan struct{}, 32), streams: make(chan struct{}, 128), done: make(chan struct{}), rateTokens: 40, rateAt: time.Now()}
	for _, c := range creds {
		s.nodes[c.AgentID] = &node{credential: c}
	}
	s.queries = make(chan struct{}, 8)
	s.tasks.Store(&probeBook{Nodes: map[string][]configuredProbe{}})
	s.reloadTasks()
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ws", s.handleWS)
	mux.HandleFunc("/api/public/v1/nodes", s.handleNodes)
	mux.HandleFunc("/api/public/v1/nodes/", s.handleHistory)
	mux.HandleFunc("/events/public", s.handleEvents)
	mux.Handle("/", web.Handler())
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024, ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return child }}
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.BindAddr, fmt.Sprint(cfg.BindPort)))
	if err != nil {
		cancel()
		return nil, errors.New("monitor listener unavailable")
	}
	s.listener = ln
	// Load TLS material before returning success; ServeTLS errors must not look like a running service.
	if cfg.CertFile != "" {
		tlsCfg, err := serverTLS(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			ln.Close()
			cancel()
			return nil, errors.New("monitor TLS configuration unavailable")
		}
		s.server.TLSConfig = tlsCfg
	}
	if cfg.DatabaseFile != "" {
		s.store, err = store.Open(store.Config{Path: cfg.DatabaseFile, RetentionDays: cfg.RetentionDays, ReportInterval: time.Duration(cfg.ReportIntervalSeconds) * time.Second})
		s.storeFailed = err != nil
	}
	initial := s.snapshot(time.Now())
	s.public.Store(&initial)
	s.wg.Add(3)
	go s.taskLoop()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
				next := s.snapshot(time.Now())
				s.public.Store(&next)
			}
		}
	}()
	go func() {
		defer s.wg.Done()
		if s.server.TLSConfig != nil {
			_ = s.server.ServeTLS(ln, "", "")
		} else {
			_ = s.server.Serve(ln)
		}
		cancel()
	}()
	go func() { s.wg.Wait(); close(s.done) }()
	go func() { <-child.Done(); s.Close() }()
	return s, nil
}

func (s *Service) Address() string { return s.listener.Addr().String() }
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.server.Close()
		s.mu.Lock()
		for c := range s.connections {
			_ = c.Close()
		}
		s.mu.Unlock()
		if s.store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_ = s.store.Close(ctx)
		}
	})
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func readCredentials(path string) ([]credential, error) {
	lst, err := os.Lstat(path)
	if err != nil || !lst.Mode().IsRegular() {
		return nil, errors.New("monitor credentials require a regular 0600 file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("monitor credentials unavailable")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 1024*1024 || (runtime.GOOS != "windows" && st.Mode().Perm() != 0600) {
		return nil, errors.New("monitor credentials require a regular 0600 file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 || !utf8.Valid(data) {
		return nil, errors.New("invalid monitor credentials")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var creds []credential
	if d.Decode(&creds) != nil || d.Decode(new(any)) != io.EOF || len(creds) == 0 || len(creds) > maxNodes {
		return nil, errors.New("invalid monitor credentials")
	}
	ids, hashes := map[string]bool{}, map[string]bool{}
	for _, c := range creds {
		if !idPattern.MatchString(c.AgentID) || len(c.Name) > 128 || strings.TrimSpace(c.Name) == "" || strings.ContainsAny(c.Name, "\x00\r\n") || !digestPattern.MatchString(c.TokenSHA256) || ids[c.AgentID] || hashes[c.TokenSHA256] {
			return nil, errors.New("invalid monitor credentials")
		}
		ids[c.AgentID] = true
		hashes[c.TokenSHA256] = true
	}
	return creds, nil
}
func (s *Service) authenticate(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") || len(value) > 512 {
		return ""
	}
	token := strings.TrimPrefix(value, "Bearer ")
	if len(token) < 43 || strings.ContainsAny(token, " \r\n\t") {
		return ""
	}
	hash := sha256.Sum256([]byte(token))
	want := hex.EncodeToString(hash[:])
	id := ""
	// Constant-time digest comparisons; neither credential values nor client identifiers are logged.
	for _, c := range s.credentials {
		if subtle.ConstantTimeCompare([]byte(c.TokenSHA256), []byte(want)) == 1 {
			id = c.AgentID
		}
	}
	return id
}
func (s *Service) admit() bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := time.Now()
	s.rateTokens += now.Sub(s.rateAt).Seconds() * 20
	if s.rateTokens > 40 {
		s.rateTokens = 40
	}
	s.rateAt = now
	if s.rateTokens < 1 {
		return false
	}
	s.rateTokens--
	return true
}
func (s *Service) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.admit() {
		http.Error(w, "try later", http.StatusTooManyRequests)
		return
	}
	id := s.authenticate(r)
	if id == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	select {
	case s.handshakes <- struct{}{}:
	default:
		http.Error(w, "try later", http.StatusServiceUnavailable)
		return
	}
	releaseHandshake := sync.OnceFunc(func() { <-s.handshakes })
	defer releaseHandshake()
	// Browser origins are unnecessary for an agent and would enable credential misuse from pages.
	up := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, ReadBufferSize: 4096, WriteBufferSize: 4096, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	select {
	case <-s.ctx.Done():
		s.mu.Unlock()
		c.Close()
		return
	default:
	}
	s.connections[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.connections, c)
		n := s.nodes[id]
		if n.conn == c {
			n.conn = nil
		}
		s.mu.Unlock()
	}()
	c.SetReadLimit(shared.MaxFrameBytes)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, data, err := c.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		return
	}
	frame, err := shared.DecodeFrame(data)
	if err != nil || frame.Hello == nil || !allowedCapabilities(frame.Hello.Capabilities) {
		closeProtocol(c)
		return
	}
	releaseHandshake()
	hello := frame.Hello
	s.mu.Lock()
	n := s.nodes[id]
	old := n.conn
	n.conn = c
	n.sessionID = hello.SessionID
	n.sequence = 1
	n.seen = true
	n.lastSeen = time.Now()
	n.facts = &hello.Facts
	n.probeEnabled = hasCapability(hello.Capabilities, "ping.v1")
	n.frp = nil
	if hello.Extensions != nil {
		n.frp = hello.Extensions.FRP
	}
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	result := shared.HelloResult{Schema: shared.SchemaVersion, SessionID: hello.SessionID, Capabilities: append([]string(nil), hello.Capabilities...), ReportInterval: uint32(s.cfg.ReportIntervalSeconds)}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if c.WriteJSON(struct {
		JSONRPC string             `json:"jsonrpc"`
		ID      string             `json:"id"`
		Result  shared.HelloResult `json:"result"`
	}{"2.0", frame.ID, result}) != nil {
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(readTimeout))
	c.SetPongHandler(func(string) error {
		_ = c.SetReadDeadline(time.Now().Add(readTimeout))
		s.mu.Lock()
		if n.conn == c {
			n.lastSeen = time.Now()
		}
		s.mu.Unlock()
		return nil
	})
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer func() {
			if recover() != nil {
				c.Close()
			}
		}()
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		updates := time.NewTicker(time.Second)
		defer updates.Stop()
		var version, sequence uint64
		pushTasks := func() bool {
			if !hasCapability(hello.Capabilities, "ping.v1") {
				return true
			}
			book := s.tasks.Load()
			if book.Version == 0 || book.Version == version {
				return true
			}
			if sequence == ^uint64(0) {
				return false
			}
			sequence++
			if sendTasks(c, book, id, hello.SessionID, sequence) != nil {
				return false
			}
			version = book.Version
			return true
		}
		if !pushTasks() {
			c.Close()
			return
		}
		for {
			select {
			case <-done:
				return
			case <-s.ctx.Done():
				return
			case <-updates.C:
				if !pushTasks() {
					c.Close()
					return
				}
			case <-ticker.C:
				if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(3*time.Second)) != nil {
					c.Close()
					return
				}
			}
		}
	}()
	tokens, last := float64(8), time.Now()
	rate, burst := float64(2), float64(8)
	if hasCapability(hello.Capabilities, "ping.v1") {
		rate, burst, tokens = 40, 80, 80
	}
	for {
		kind, data, err = c.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			closeProtocol(c)
			return
		}
		now := time.Now()
		tokens += now.Sub(last).Seconds() * rate
		last = now
		if tokens > burst {
			tokens = burst
		}
		if tokens < 1 {
			closeProtocol(c)
			return
		}
		tokens--
		frame, err = shared.DecodeFrame(data)
		if err != nil || (frame.Report == nil && frame.PingResult == nil) {
			closeProtocol(c)
			return
		}
		if frame.PingResult != nil {
			if !hasCapability(hello.Capabilities, "ping.v1") || !s.acceptProbe(id, c, now, *frame.PingResult) {
				closeProtocol(c)
				return
			}
			continue
		}
		report := frame.Report
		s.mu.Lock()
		if n.conn != c || report.SessionID != n.sessionID || report.Sequence <= n.sequence {
			s.mu.Unlock()
			closeProtocol(c)
			return
		}
		n.sequence = report.Sequence
		n.lastSeen = now
		if report.Metrics != nil {
			n.metrics = report.Metrics
			n.metricsAt = now
			if s.store != nil {
				s.store.Accept(id, now, *report.Metrics)
			}
		}
		if report.Facts != nil {
			n.facts = report.Facts
		}
		n.frp = nil
		if report.Extensions != nil {
			n.frp = report.Extensions.FRP
		}
		s.mu.Unlock()
	}
}
func allowedCapabilities(c []string) bool {
	metrics := false
	for _, v := range c {
		switch v {
		case "metrics.v1":
			metrics = true
		case "frp.v1", "ping.v1":
		default:
			return false
		}
	}
	return metrics
}
func closeProtocol(c *websocket.Conn) {
	_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid monitor frame"), time.Now().Add(time.Second))
}

// PublicMetrics deliberately omits facts, boot ID, network interface and FRP associations.
type PublicMetrics struct {
	Scope      shared.Scope            `json:"scope"`
	CPU        shared.Field[float64]   `json:"cpu"`
	Load       shared.Field[[]float64] `json:"load"`
	MemTotal   shared.Field[string]    `json:"mem_total"`
	MemUsed    shared.Field[string]    `json:"mem_used"`
	SwapTotal  shared.Field[string]    `json:"swap_total"`
	SwapUsed   shared.Field[string]    `json:"swap_used"`
	DiskTotal  shared.Field[string]    `json:"disk_total"`
	DiskUsed   shared.Field[string]    `json:"disk_used"`
	NetRX      shared.Field[string]    `json:"net_rx"`
	NetTX      shared.Field[string]    `json:"net_tx"`
	NetRXTotal shared.Field[string]    `json:"net_rx_total"`
	NetTXTotal shared.Field[string]    `json:"net_tx_total"`
	Uptime     shared.Field[string]    `json:"uptime"`
	TCP        shared.Field[string]    `json:"tcp"`
	UDP        shared.Field[string]    `json:"udp"`
	Procs      shared.Field[string]    `json:"procs"`
}
type PublicFRP struct {
	ControlState string `json:"control_state"`
	ProxyTotal   int    `json:"proxy_total"`
	ProxyRunning int    `json:"proxy_running"`
}
type PublicNode struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Session         string         `json:"session"`
	Freshness       string         `json:"freshness"`
	LastSeen        *time.Time     `json:"last_seen"`
	MetricsAt       *time.Time     `json:"metrics_at"`
	IntervalSeconds int            `json:"interval_seconds"`
	FRP             PublicFRP      `json:"frp"`
	Metrics         *PublicMetrics `json:"metrics"`
}
type PublicSnapshot struct {
	Nodes       []PublicNode `json:"nodes"`
	GeneratedAt time.Time    `json:"generated_at"`
}

func (s *Service) snapshot(now time.Time) PublicSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := PublicSnapshot{Nodes: make([]PublicNode, 0, len(s.nodes)), GeneratedAt: now.UTC()}
	for _, n := range s.nodes {
		p := PublicNode{ID: n.credential.AgentID, Name: n.credential.Name, Session: "waiting", Freshness: "waiting", IntervalSeconds: s.cfg.ReportIntervalSeconds, FRP: PublicFRP{ControlState: "unknown"}}
		if n.seen {
			p.Session = "offline"
			if n.conn != nil {
				p.Session = "online"
			}
			v := n.lastSeen.UTC()
			p.LastSeen = &v
		}
		if n.metrics != nil {
			m, _ := n.metrics.Browser()
			p.Metrics = &PublicMetrics{m.Scope, m.CPU, m.Load, m.MemTotal, m.MemUsed, m.SwapTotal, m.SwapUsed, m.DiskTotal, m.DiskUsed, m.NetRX, m.NetTX, m.NetRXTotal, m.NetTXTotal, m.Uptime, m.TCP, m.UDP, m.Procs}
			v := n.metricsAt.UTC()
			p.MetricsAt = &v
			p.Freshness = "fresh"
			age := time.Duration(s.cfg.ReportIntervalSeconds*3) * time.Second
			if age < 10*time.Second {
				age = 10 * time.Second
			}
			if now.Sub(n.metricsAt) > age {
				p.Freshness = "stale"
			}
		}
		if n.frp != nil {
			p.FRP.ControlState = n.frp.ControlState
			p.FRP.ProxyTotal = len(n.frp.Proxies)
			for _, proxy := range n.frp.Proxies {
				if proxy.Enabled && proxy.Status == "running" {
					p.FRP.ProxyRunning++
				}
			}
		}
		out.Nodes = append(out.Nodes, p)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	return out
}
func publicHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
func (s *Service) handleNodes(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = json.NewEncoder(w).Encode(s.public.Load())
}
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		http.Error(w, "try later", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func() bool {
		data, err := json.Marshal(s.public.Load())
		if err != nil {
			return false
		}
		if controller.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
			return false
		}
		if _, err = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send() {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-tick.C:
			if !send() {
				return
			}
		}
	}
}
