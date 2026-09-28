// Package monitor owns the independent monitoring listener and latest snapshots.
// It never imports FRP's client/server roots or participates in data forwarding.
package monitor

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/fatedier/frp/extension/frpmonitor/web"
	"github.com/gorilla/websocket"
)

const maxNodes = 1024
const heartbeat = 5 * time.Second
const readTimeout = 15 * time.Second

type credential struct {
	AgentID     string             `json:"agent_id"`
	Name        string             `json:"name"`
	TokenSHA256 string             `json:"token_sha256"`
	FRPBinding  *shared.FRPBinding `json:"frp_binding,omitempty"`
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
	// cfg is immutable after Start returns. Reads happen in Start itself or
	// while holding s.mu; tests adjusting ReportIntervalSeconds also hold s.mu.
	cfg      shared.MonitorConfig
	ctx      context.Context
	cancel   context.CancelFunc
	server   *http.Server
	listener net.Listener
	// location is the accounting calendar timezone shared with the control
	// store, used by the today-traffic DTO for day boundaries.
	location        *time.Location
	mu              sync.Mutex
	nodes           map[string]*node
	connections     map[*websocket.Conn]credential
	wg              sync.WaitGroup
	closeOnce       sync.Once
	done            chan struct{}
	handshakes      chan struct{}
	streamsPublic   chan struct{}
	streamsAdmin    chan struct{}
	rateMu          sync.Mutex
	rateTokens      float64
	rateAt          time.Time
	public          atomic.Pointer[PublicSnapshot]
	publicJSON      atomic.Pointer[[]byte]
	publishMu       sync.Mutex
	adminPublishMu  sync.Mutex
	adminGeneration atomic.Uint64
	adminJSON       atomic.Pointer[adminEncodedSnapshot]
	control         *control.Store
	configs         atomic.Pointer[nodeConfigs]
	store           *store.Store
	storeFailed     bool
	tasks           atomic.Pointer[probeBook]
	taskError       atomic.Bool
	queries         chan struct{}
	configMu        sync.Mutex
	credentialError atomic.Bool
	admin           *adminState
	serverProvider  shared.ServerProvider
	serverSnapshot  atomic.Pointer[shared.ServerSnapshot]
}

func Start(ctx context.Context, cfg shared.MonitorConfig, providers ...shared.ServerProvider) (*Service, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, errors.New("invalid monitor configuration")
	}
	// Pass the accounting timezone explicitly so the control store and the
	// today-traffic DTO below can never disagree on day boundaries.
	db, err := control.Open(control.Config{Path: cfg.DatabaseFile, ReportInterval: time.Duration(cfg.ReportIntervalSeconds) * time.Second, Location: time.Local})
	if err != nil {
		return nil, errors.New("control database unavailable")
	}
	started := false
	defer func() {
		if !started {
			db.Close()
		}
	}()

	child, cancel := context.WithCancel(ctx)
	s := &Service{cfg: cfg, ctx: child, cancel: cancel, location: time.Local, nodes: map[string]*node{}, control: db, connections: map[*websocket.Conn]credential{}, handshakes: make(chan struct{}, 32), streamsPublic: make(chan struct{}, 128), streamsAdmin: make(chan struct{}, 64), done: make(chan struct{}), rateTokens: 40, rateAt: time.Now()}
	if len(providers) > 0 {
		s.serverProvider = providers[0]
	}
	s.admin, err = newAdmin(cfg)
	if err != nil {
		cancel()
		return nil, err
	}
	initialCtx, initialCancel := context.WithTimeout(child, 3*time.Second)
	err = s.refreshNodes(initialCtx)
	initialCancel()
	if err != nil {
		cancel()
		return nil, err
	}
	s.queries = make(chan struct{}, 8)
	s.tasks.Store(&probeBook{Nodes: map[string][]configuredProbe{}})
	s.reloadTasks()
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/ws", s.handleWS)
	mux.HandleFunc("/api/public/v1/nodes", s.handleNodes)
	mux.HandleFunc("/api/public/v1/nodes/", s.handleHistory)
	mux.HandleFunc("/events/public", s.handleEvents)
	mux.HandleFunc("/api/admin/v1/", s.handleAdmin)
	mux.HandleFunc("/events/admin", s.handleAdminEvents)
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
	if cfg.HistoryDataPath != "" {
		s.store, err = store.Open(store.Config{Path: cfg.HistoryDataPath, RetentionDays: cfg.RetentionDays, ReportInterval: time.Duration(cfg.ReportIntervalSeconds) * time.Second})
		s.storeFailed = err != nil
	}
	s.publishSnapshot(time.Now())
	s.publishAdminSnapshot()
	s.wg.Add(4)
	go s.serverLoop()
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
				s.publishSnapshot(time.Now())
				s.publishAdminSnapshot()
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
	started = true
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
		if s.control != nil {
			_ = s.control.Close()
		}
		if s.store != nil {
			// TSDB 排空与关闭在慢盘上可能超过预算。超时只结束等待，后台
			// worker 仍继续关闭；旧实例关闭完成前进程内不得重新打开历史。
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.store.Close(ctx)
		}
	})
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
}

// validNodeID matches control.validID: a positive decimal integer that
// round-trips through int64, so 19-digit values beyond int64 are rejected.
func validNodeID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

// nodeIDLess orders node IDs numerically. IDs come from the control store's
// autoincrement and always parse; a parse failure falls back to a total order
// (parseable first, then lexical) instead of silently sorting as zero.
func nodeIDLess(a, b string) bool {
	ai, aerr := strconv.ParseInt(a, 10, 64)
	bi, berr := strconv.ParseInt(b, 10, 64)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	if (aerr == nil) != (berr == nil) {
		return aerr == nil
	}
	return a < b
}

func (s *Service) authenticateCredential(r *http.Request) credential {
	if s.credentialError.Load() {
		return credential{}
	}
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") || len(value) > 512 {
		return credential{}
	}
	token := strings.TrimPrefix(value, "Bearer ")
	if len(token) < 43 || strings.ContainsAny(token, " \r\n\t") {
		return credential{}
	}
	want := tokenHash(token)
	match := credential{}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Constant-time digest comparisons; neither credential values nor client identifiers are logged.
	for _, n := range s.nodes {
		c := n.credential
		if subtle.ConstantTimeCompare([]byte(c.TokenSHA256), []byte(want)) == 1 {
			match = c
		}
	}
	return match
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
		methodNotAllowed(w, http.MethodGet)
		return
	}
	// Authentication is a cheap in-memory digest lookup and runs before the
	// handshake rate limit. A control store failure answers 503, distinct
	// from a bad credential's 401.
	if s.credentialError.Load() {
		http.Error(w, "control unavailable", http.StatusServiceUnavailable)
		return
	}
	grant := s.authenticateCredential(r)
	id := grant.AgentID
	if id == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.admit() {
		http.Error(w, "try later", http.StatusTooManyRequests)
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
	if n := s.nodes[id]; n == nil || n.credential.TokenSHA256 != grant.TokenSHA256 {
		s.mu.Unlock()
		c.Close()
		return
	}
	s.connections[c] = grant
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.connections, c)
		n := s.nodes[id]
		if n != nil && n.conn == c {
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
	if n == nil || n.credential.TokenSHA256 != grant.TokenSHA256 {
		s.mu.Unlock()
		c.Close()
		return
	}
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
	interval := s.cfg.ReportIntervalSeconds
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	result := shared.HelloResult{Schema: shared.SchemaVersion, SessionID: hello.SessionID, Capabilities: append([]string(nil), hello.Capabilities...), ReportInterval: uint32(interval)}
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
		if n != nil && n.conn == c {
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
		if s.nodes[id] != n || n.conn != c || report.SessionID != n.sessionID || report.Sequence <= n.sequence {
			s.mu.Unlock()
			closeProtocol(c)
			return
		}
		n.sequence = report.Sequence
		n.lastSeen = now
		if report.Metrics != nil {
			n.metrics = report.Metrics
			n.metricsAt = now
			// Lock order: s.mu is held while calling into both stores, so
			// control.Accept and store.Accept must remain non-blocking (bounded
			// queue, drop when full) and must never acquire s.mu; otherwise the
			// report loop deadlocks or stalls behind database backpressure.
			if s.control != nil {
				s.control.Accept(id, now, *report.Metrics)
			}
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

// PublicHardware exposes only the hardware and software summary used by public
// cards and node details. Do not embed Facts or BrowserFacts: they also contain
// private host identifiers, addresses and the exact kernel version.
type PublicHardware struct {
	OS           shared.Field[string] `json:"os"`
	Arch         shared.Field[string] `json:"arch"`
	Virt         shared.Field[string] `json:"virt"`
	CPUName      shared.Field[string] `json:"cpu_name"`
	CPUCores     shared.Field[uint32] `json:"cpu_cores"`
	AgentVersion shared.Field[string] `json:"agent_version"`
}

// PublicMetrics deliberately omits boot ID, network interface and FRP associations.
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
	Reconciliation string  `json:"reconciliation"`
	ServerOnline   *bool   `json:"server_online"`
	Registered     int     `json:"registered"`
	ControlState   string  `json:"control_state"`
	ProxyTotal     int     `json:"proxy_total"`
	ProxyRunning   int     `json:"proxy_running"`
	TodayRXBytes   *string `json:"today_rx_bytes"`
	TodayTXBytes   *string `json:"today_tx_bytes"`
	TrafficScope   string  `json:"traffic_scope"`
}
type PublicNode struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Session         string          `json:"session"`
	Freshness       string          `json:"freshness"`
	LastSeen        *time.Time      `json:"last_seen"`
	MetricsAt       *time.Time      `json:"metrics_at"`
	IntervalSeconds int             `json:"interval_seconds"`
	FRP             PublicFRP       `json:"frp"`
	Hardware        *PublicHardware `json:"hardware"`
	Metrics         *PublicMetrics  `json:"metrics"`
	PublicNote      string          `json:"public_note"`
	Billing         *nodeBilling    `json:"billing,omitempty"`
	TrafficToday    *nodeToday      `json:"traffic_today"`
	TrafficPlan     *nodePlan       `json:"traffic_plan,omitempty"`
	AccountingState string          `json:"accounting_state"`
}
type PublicSnapshot struct {
	Nodes       []PublicNode `json:"nodes"`
	GeneratedAt time.Time    `json:"generated_at"`
}

func (s *Service) snapshot(now time.Time) PublicSnapshot { return s.snapshotFor(now, false) }
func (s *Service) snapshotFor(now time.Time, private bool) PublicSnapshot {
	configs := s.configs.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := PublicSnapshot{Nodes: make([]PublicNode, 0, len(s.nodes)), GeneratedAt: now.UTC()}
	for _, n := range s.nodes {
		var config *control.Node
		if configs != nil {
			config = (*configs)[n.credential.AgentID]
		}
		if config == nil {
			continue
		}
		p := PublicNode{ID: n.credential.AgentID, Name: n.credential.Name, Session: "waiting", Freshness: "waiting", IntervalSeconds: s.cfg.ReportIntervalSeconds, FRP: PublicFRP{ControlState: "unknown"}}
		p.AccountingState = "ready"
		if s.control != nil && !s.control.Healthy() {
			p.AccountingState = "degraded"
		}
		p.PublicNote = config.PublicNote
		p.TrafficToday = todayDTO(config, now, s.location)
		if private || config.PublishBilling {
			p.Billing = billingDTO(config)
		}
		if private || config.PublishTrafficPlan {
			p.TrafficPlan = planDTO(config)
		}
		if p.AccountingState == "degraded" {
			p.TrafficToday.Partial = true
			if p.TrafficPlan != nil {
				p.TrafficPlan.Partial = true
			}
		}
		if n.facts != nil {
			p.Hardware = &PublicHardware{
				OS: n.facts.OS, Arch: n.facts.Arch, Virt: n.facts.Virt,
				CPUName: n.facts.CPUName, CPUCores: n.facts.CPUCores,
				AgentVersion: n.facts.AgentVersion,
			}
		}
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
	// Public reconciliation needs only identity decisions and counts. Avoid
	// constructing private per-proxy views on this high-frequency path.
	inputs := make([]ReconcileNode, 0, len(out.Nodes))
	for _, p := range out.Nodes {
		n := s.nodes[p.ID]
		var report *shared.FRP
		if n.frp != nil {
			copy := *n.frp
			copy.Proxies = nil
			report = &copy
		}
		inputs = append(inputs, ReconcileNode{ID: p.ID, Binding: n.credential.FRPBinding, Report: report, Fresh: n.conn != nil && p.Freshness == "fresh"})
	}
	server := s.currentServerSnapshot(now)
	reconciled := Reconcile(server, inputs)
	states := make(map[string]NodeReconciliation, len(reconciled.Nodes))
	registered := map[string]int{}
	for _, node := range reconciled.Nodes {
		states[node.ID] = node
	}
	for _, proxy := range reconciled.Proxies {
		if proxy.AgentID != nil && proxy.Online {
			registered[*proxy.AgentID]++
		}
	}
	for i := range out.Nodes {
		n := &out.Nodes[i]
		state := states[n.ID]
		n.FRP.Reconciliation = state.State
		n.FRP.ServerOnline = state.ServerOnline
		n.FRP.Registered = registered[n.ID]
		n.FRP.TrafficScope = reconciled.TrafficScope
		if state.State == "matched" && reconciled.TrafficScope == "server_local_day" {
			rx, tx := new(big.Int), new(big.Int)
			valid := true
			for _, proxy := range reconciled.Proxies {
				if proxy.AgentID == nil || *proxy.AgentID != n.ID {
					continue
				}
				if proxy.TodayRXBytes == nil || proxy.TodayTXBytes == nil {
					valid = false
					break
				}
				prx, ok := new(big.Int).SetString(*proxy.TodayRXBytes, 10)
				if !ok || prx.Sign() < 0 {
					valid = false
					break
				}
				ptx, ok := new(big.Int).SetString(*proxy.TodayTXBytes, 10)
				if !ok || ptx.Sign() < 0 {
					valid = false
					break
				}
				rx.Add(rx, prx)
				tx.Add(tx, ptx)
			}
			if valid {
				a, b := rx.String(), tx.String()
				n.FRP.TodayRXBytes = &a
				n.FRP.TodayTXBytes = &b
			}
		}
	}
	if !private {
		visible := make([]PublicNode, 0, len(out.Nodes))
		for _, p := range out.Nodes {
			if (*configs)[p.ID].IsPublic {
				visible = append(visible, p)
			}
		}
		out.Nodes = visible
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return nodeIDLess(out.Nodes[i].ID, out.Nodes[j].ID) })
	return out
}
func publicHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
func (s *Service) handleNodes(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	// Reuse the encoding produced at publish time instead of marshaling per request.
	if data := s.publicJSON.Load(); data != nil {
		_, _ = w.Write(*data)
		return
	}
	_ = json.NewEncoder(w).Encode(s.public.Load())
}
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	select {
	case s.streamsPublic <- struct{}{}:
		defer func() { <-s.streamsPublic }()
	default:
		http.Error(w, "try later", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func() bool {
		data := s.publicJSON.Load()
		if data == nil {
			return false
		}
		if controller.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", *data); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send() {
		return
	}
	// Public dashboards tolerate a two-second refresh; cap repeated full-snapshot
	// bandwidth independently of the one-second in-memory publication cadence.
	tick := time.NewTicker(2 * time.Second)
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

// Serialize computing and publishing so a pre-revocation snapshot cannot
// overwrite the immediate post-revocation snapshot in the public cache.
// The snapshot is encoded once here; the public API and every public SSE
// client reuse the cached bytes. A marshal failure (none of the DTOs can
// produce one) keeps the previous consistent snapshot/bytes pair.
func (s *Service) publishSnapshot(now time.Time) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	next := s.snapshot(now)
	data, err := json.Marshal(next)
	if err != nil {
		return
	}
	s.public.Store(&next)
	s.publicJSON.Store(&data)
}

func serverTLS(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}
