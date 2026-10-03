// Package service samples locally and streams only the latest observation on an
// independent connection. Neither collection nor reconnection blocks FRP traffic.
package service

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/collect"
	"github.com/fatedier/frp/extension/frpmonitor/agent/probe"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/fatedier/frp/pkg/util/log"
	"github.com/gorilla/websocket"
)

// minStableSession is the lifetime a session must reach to prove a healthy
// endpoint; shorter sessions keep the reconnect backoff growing. It is a
// variable so tests can shrink it.
var minStableSession = 10 * time.Second

// warnEvery throttles a repeating warning to at most one log line per interval.
type warnEvery struct {
	mu   sync.Mutex
	last time.Time
}

func (w *warnEvery) allow(now time.Time, interval time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Sub(w.last) < interval {
		return false
	}
	w.last = now
	return true
}

type sampler interface {
	Facts() shared.Facts
	Metrics() shared.Metrics
}
type observation struct {
	facts      shared.Facts
	metrics    shared.Metrics
	frp        *shared.FRP
	detail     *shared.FRPDetail
	at         time.Time
	generation uint64
}
type Service struct {
	ctx             context.Context
	cancel          context.CancelFunc
	cfg             shared.AgentConfig
	token           string
	dialer          websocket.Dialer
	collector       sampler
	snapshot        func() shared.FRP
	detailProvider  shared.DetailProvider
	configProvider  shared.ConfigProvider
	configJobs      chan configJob
	configBusy      atomic.Bool
	configServiceID string
	detailResults   chan shared.FRPDetail
	detailPending   bool
	mu              sync.Mutex
	latest          observation
	conn            *websocket.Conn
	interval        atomic.Int64
	notify          chan struct{}
	intervalChanged chan struct{}
	wg              sync.WaitGroup
	closeOnce       sync.Once
	ready           chan struct{}
	done            chan struct{}
	readyOnce       sync.Once
	sampleWarn      warnEvery
	connectWarn     warnEvery
}

func Start(ctx context.Context, cfg shared.AgentConfig, snapshot func() shared.FRP, providers ...shared.DetailProvider) (*Service, error) {
	var detail shared.DetailProvider
	if len(providers) != 0 {
		detail = providers[0]
	}
	return StartWithProviders(ctx, cfg, snapshot, detail, nil)
}

// StartWithProviders preserves the original Start API. Management is opt-in
// in local configuration and still requires explicit capability negotiation.
func StartWithProviders(ctx context.Context, cfg shared.AgentConfig, snapshot func() shared.FRP, detail shared.DetailProvider, management shared.ConfigProvider) (*Service, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := cfg.Complete(); err != nil {
		return nil, errors.New("invalid telemetry configuration")
	}
	c, err := collect.New(collect.Config{Iface: cfg.Iface, Version: shared.Version})
	if err != nil {
		return nil, errors.New("telemetry collector unavailable")
	}
	return startWithProviders(ctx, cfg, snapshot, c, detail, management)
}
func start(ctx context.Context, cfg shared.AgentConfig, snapshot func() shared.FRP, c sampler, providers ...shared.DetailProvider) (*Service, error) {
	var detail shared.DetailProvider
	if len(providers) != 0 {
		detail = providers[0]
	}
	return startWithProviders(ctx, cfg, snapshot, c, detail, nil)
}
func startWithProviders(ctx context.Context, cfg shared.AgentConfig, snapshot func() shared.FRP, c sampler, detail shared.DetailProvider, management shared.ConfigProvider) (*Service, error) {
	token, err := readToken(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := readRegular(cfg.CAFile, 1024*1024, false)
		if err != nil || len(pem) > 1024*1024 {
			return nil, errors.New("telemetry CA unavailable")
		}
		// An explicit caFile pins trust to that CA alone: merging the system
		// roots would let any public CA impersonate the monitor.
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("telemetry CA invalid")
		}
		tlsCfg.RootCAs = pool
	}
	child, cancel := context.WithCancel(ctx)
	s := &Service{ctx: child, cancel: cancel, cfg: cfg, token: token, collector: c, snapshot: snapshot, notify: make(chan struct{}, 1), intervalChanged: make(chan struct{}, 1), ready: make(chan struct{}), done: make(chan struct{}), dialer: websocket.Dialer{HandshakeTimeout: 5 * time.Second, TLSClientConfig: tlsCfg, ReadBufferSize: 4096, WriteBufferSize: 4096}}
	s.detailProvider, s.configProvider = detail, management
	if s.configEnabled() {
		s.configJobs = make(chan configJob, 1)
		s.wg.Add(1)
		go s.guarded(s.configLoop)
	}
	s.interval.Store(int64(cfg.IntervalSeconds))
	s.wg.Add(2)
	go s.guarded(s.sampleLoop)
	go s.guarded(s.connectLoop)
	go func() { s.wg.Wait(); close(s.done) }()
	go func() { <-child.Done(); s.Close() }()
	return s, nil
}
func readRegular(path string, limit int64, private bool) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("invalid local telemetry file")
	}
	f, err := openRegular(path, st, limit, private)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("invalid local telemetry file")
	}
	return data, nil
}

// Reject a replaced path before reading, including replacements between Lstat
// and open. The platform opener also prevents a substituted Unix FIFO blocking.
func openRegular(path string, expected os.FileInfo, limit int64, private bool) (*os.File, error) {
	valid := func(st os.FileInfo) bool {
		return st.Mode().IsRegular() && st.Size() <= limit && (!private || runtime.GOOS == "windows" || st.Mode().Perm() == 0600)
	}
	// Windows fills FileInfo's file ID lazily; capture it before opening the
	// path again so the later SameFile check compares against this observation.
	if !valid(expected) || !os.SameFile(expected, expected) {
		return nil, errors.New("invalid local telemetry file")
	}
	f, err := openLocalFile(path)
	if err != nil {
		return nil, errors.New("local telemetry file unavailable")
	}
	actual, err := f.Stat()
	if err != nil || !valid(actual) || !os.SameFile(expected, actual) {
		f.Close()
		return nil, errors.New("invalid local telemetry file")
	}
	return f, nil
}
func readToken(path string) (string, error) {
	data, err := readRegular(path, 512, true)
	if err != nil {
		return "", errors.New("telemetry credential requires a regular 0600 file")
	}
	token := strings.TrimSpace(string(data))
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < 32 || len(raw) > 256 {
		return "", errors.New("telemetry credential must be base64url with at least 32 bytes")
	}
	return token, nil
}
func (s *Service) guarded(run func()) {
	defer s.wg.Done()
	defer func() {
		if recover() != nil {
			s.cancel()
		}
	}()
	run()
}
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		if s.conn != nil {
			s.conn.Close()
		}
		s.mu.Unlock()
	})
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
}
func (s *Service) sample() {
	value := observation{facts: s.collector.Facts(), metrics: s.collector.Metrics(), frp: s.frpSnapshot(), detail: s.frpDetailSnapshot(), at: time.Now().UTC()}
	if value.facts.Validate() != nil || value.metrics.Validate() != nil {
		if s.sampleWarn.allow(time.Now(), 5*time.Minute) {
			log.Warnf("frp-plus telemetry keeps dropping invalid local samples")
		}
		return
	}
	s.mu.Lock()
	value.generation = s.latest.generation + 1
	s.latest = value
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
	select {
	case s.notify <- struct{}{}:
	default:
	}
}
func (s *Service) frpSnapshot() (out *shared.FRP) {
	if s.snapshot == nil {
		return nil
	}
	// A faulty adapter must not turn a telemetry sample into a process-wide panic.
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	got := s.snapshot()
	if (&shared.Extensions{FRP: &got}).Validate() == nil {
		return &got
	}
	return nil
}
func extension(frp *shared.FRP) *shared.Extensions {
	if frp == nil {
		return nil
	}
	return &shared.Extensions{FRP: frp}
}

// Only the sampling loop owns detailPending and detailResults. A provider that
// gets stuck may leave one call outstanding, but never stalls host sampling or
// accumulates a new goroutine on every sample. Late results are discarded rather
// than being presented with the next sample's fresh timestamp.
func (s *Service) frpDetailSnapshot() *shared.FRPDetail {
	if s.detailProvider == nil {
		return nil
	}
	unavailable := shared.EmptyFRPDetail("unavailable")
	if s.detailResults == nil {
		s.detailResults = make(chan shared.FRPDetail, 1)
	}
	if s.detailPending {
		select {
		case <-s.detailResults:
			s.detailPending = false
		default:
			return &unavailable
		}
	}
	s.detailPending = true
	results, provider := s.detailResults, s.detailProvider
	go func() { results <- collectFRPDetail(provider) }()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case detail := <-results:
		s.detailPending = false
		return &detail
	case <-timer.C:
		return &unavailable
	case <-s.ctx.Done():
		return &unavailable
	}
}

func collectFRPDetail(provider shared.DetailProvider) (out shared.FRPDetail) {
	out = shared.EmptyFRPDetail("unavailable")
	defer func() {
		if recover() != nil {
			out = shared.EmptyFRPDetail("unavailable")
		}
	}()
	got := provider()
	if len(got.Proxies) > shared.MaxDetailProxies || len(got.Visitors) > shared.MaxDetailVisitors {
		return shared.EmptyFRPDetail("truncated")
	}
	if err := got.Validate(); err != nil {
		if errors.Is(err, shared.ErrFRPDetailTooLarge) {
			return shared.EmptyFRPDetail("truncated")
		}
		return out
	}
	data, err := json.Marshal(got)
	if err != nil {
		return out
	}
	// Detach all slices and pointers from provider-owned state before publishing.
	if json.Unmarshal(data, &out) != nil {
		return shared.EmptyFRPDetail("unavailable")
	}
	return out
}
func (s *Service) sampleLoop() {
	s.sample()
	timer := time.NewTimer(time.Duration(s.interval.Load()) * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			s.sample()
			timer.Reset(time.Duration(s.interval.Load()) * time.Second)
		case <-s.intervalChanged:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Duration(s.interval.Load()) * time.Second)
		}
	}
}
func (s *Service) current() observation { s.mu.Lock(); defer s.mu.Unlock(); return s.latest }
func (s *Service) connectLoop() {
	select {
	case <-s.ctx.Done():
		return
	case <-s.ready:
	}
	delay := time.Second
	for {
		if s.ctx.Err() != nil {
			return
		}
		stable, unauthorized := s.connect()
		if s.ctx.Err() != nil {
			return
		}
		if stable {
			delay = time.Second
		} else {
			if s.connectWarn.allow(time.Now(), 5*time.Minute) {
				if unauthorized {
					log.Warnf("frp-plus telemetry was rejected by the monitor; check the token")
				} else {
					log.Warnf("frp-plus telemetry cannot hold a monitor session; reconnecting with backoff")
				}
			}
			if delay < 30*time.Second {
				delay *= 2
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			}
		}
		wait := jitter(delay)
		if unauthorized {
			wait = jitter(60 * time.Second)
		}
		timer := time.NewTimer(wait)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func jitter(base time.Duration) time.Duration {
	var raw [1]byte
	_, _ = rand.Read(raw[:])
	return base*3/4 + time.Duration(raw[0])*base/512
}

// connect returns stable=true only when the session survived minStableSession.
// A server that drops sessions right after accepting hello must not reset the
// reconnect backoff.
func (s *Service) connect() (bool, bool) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.token)
	c, response, err := s.dialer.DialContext(s.ctx, s.cfg.Endpoint, header)
	if err != nil {
		if response != nil {
			if response.Body != nil {
				response.Body.Close()
			}
			return false, response.StatusCode == http.StatusUnauthorized
		}
		return false, false
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		c.Close()
		return false, false
	}
	s.conn = c
	s.mu.Unlock()
	defer func() {
		c.Close()
		s.mu.Lock()
		if s.conn == c {
			s.conn = nil
		}
		s.mu.Unlock()
	}()
	c.SetReadLimit(shared.MaxFrameBytes)
	var nonce [24]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return false, false
	}
	session := hex.EncodeToString(nonce[:])
	current := s.current()
	detailOffered := s.detailProvider != nil && response != nil && advertisedCapability(response.Header, shared.FRPDetailCapability)
	configOffered := s.configEnabled() && response != nil && advertisedCapability(response.Header, shared.ConfigManageCapability)
	hello := shared.Hello{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: 1, CollectedAt: current.at.Format(time.RFC3339Nano)}, Capabilities: []string{"metrics.v1", "frp.v1"}, Facts: current.facts, Extensions: extension(current.frp)}
	if s.cfg.ProbeEnabled {
		hello.Capabilities = append(hello.Capabilities, "ping.v1")
	}
	if detailOffered {
		hello.Capabilities = append(hello.Capabilities, shared.FRPDetailCapability)
	}
	if configOffered {
		hello.Capabilities = append(hello.Capabilities, shared.ConfigManageCapability)
	}
	if hello.Validate() != nil {
		return false, false
	}
	if writeFrame(c, "hello", "hello", hello) != nil {
		return false, false
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, data, err := c.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		return false, false
	}
	var answer struct {
		JSONRPC string             `json:"jsonrpc"`
		ID      string             `json:"id"`
		Result  shared.HelloResult `json:"result"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&answer) != nil || decoder.Decode(new(any)) != io.EOF || answer.JSONRPC != "2.0" || answer.ID != "hello" || answer.Result.Validate() != nil || answer.Result.SessionID != session || !capabilities(answer.Result.Capabilities, s.cfg.ProbeEnabled, detailOffered, configOffered) {
		return false, false
	}
	negotiated := int64(answer.Result.ReportInterval)
	if s.interval.Swap(negotiated) != negotiated {
		select {
		case s.intervalChanged <- struct{}{}:
		default:
		}
	}
	established := time.Now()
	stable := func() bool { return time.Since(established) >= minStableSession }
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(15 * time.Second)) })
	var probes *probe.Engine
	var results <-chan probe.Result
	detailAccepted, configAccepted := false, false
	connectionCtx, connectionCancel := context.WithCancel(s.ctx)
	defer connectionCancel()
	configResults := make(chan shared.ConfigResult, 4)
	for _, capability := range answer.Result.Capabilities {
		if capability == shared.ConfigManageCapability {
			configAccepted = true
		}
		if capability == shared.FRPDetailCapability {
			detailAccepted = true
		}
		if capability == "ping.v1" {
			probes = probe.New(s.ctx, probe.Config{AllowPrivate: s.cfg.ProbeAllowPrivate})
			results = probes.Results()
			defer probes.Close()
		}
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer connectionCancel()
		defer c.Close()
		var taskSequence uint64
		burst, rate := float64(4), float64(1)
		if configAccepted {
			burst, rate = 8, 2
		}
		taskBudget, replenished := burst, time.Now()
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage {
				return
			}
			now := time.Now()
			taskBudget += now.Sub(replenished).Seconds() * rate
			if taskBudget > burst {
				taskBudget = burst
			}
			replenished = now
			if taskBudget < 1 {
				return
			}
			taskBudget--
			frame, err := shared.DecodeFrame(data)
			if err != nil {
				return
			}
			switch {
			case frame.ConfigCommand != nil && configAccepted:
				command := *frame.ConfigCommand
				if command.SessionID != session || command.Sequence <= taskSequence {
					return
				}
				taskSequence = command.Sequence
				s.dispatchConfig(configJob{ctx: connectionCtx, command: command, results: configResults})
			case frame.PingTasks != nil && probes != nil:
				if frame.PingTasks.SessionID != session || frame.PingTasks.Sequence <= taskSequence || probes.Replace(*frame.PingTasks) != nil {
					return
				}
				taskSequence = frame.PingTasks.Sequence
			default:
				return
			}
			_ = c.SetReadDeadline(now.Add(15 * time.Second))
		}
	}()
	defer func() { c.Close(); <-readDone }()
	sequence, lastGeneration := uint64(1), uint64(0)
	send := func() bool {
		latest := s.current()
		if latest.generation == lastGeneration {
			return true
		}
		if !nextSequence(&sequence) {
			return false
		}
		report := shared.Report{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: sequence, CollectedAt: latest.at.Format(time.RFC3339Nano)}, Facts: &latest.facts, Metrics: &latest.metrics, Extensions: extension(latest.frp)}
		if report.Validate() != nil {
			return false
		}
		if err := writeFrame(c, "report", "", report); err != nil {
			// FRP extensions are the only field that can push a report past the
			// frame limit; drop them and retry once before giving up.
			if !errors.Is(err, errFrameTooLarge) || report.Extensions == nil {
				return false
			}
			report.Extensions = nil
			if writeFrame(c, "report", "", report) != nil {
				return false
			}
		}
		if detailAccepted && latest.detail != nil {
			if !nextSequence(&sequence) {
				return false
			}
			detail := shared.FRPDetailReport{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: sequence, CollectedAt: latest.at.Format(time.RFC3339Nano)}, Detail: *latest.detail}
			if err := detail.Validate(); err != nil {
				detail.Detail = shared.EmptyFRPDetail("unavailable")
				if errors.Is(err, shared.ErrFRPDetailTooLarge) {
					detail.Detail = shared.EmptyFRPDetail("truncated")
				}
			}
			if err := writeFrame(c, "frp.detail", "", detail); err != nil {
				if !errors.Is(err, errFrameTooLarge) {
					return false
				}
				detail.Detail = shared.EmptyFRPDetail("truncated")
				if writeFrame(c, "frp.detail", "", detail) != nil {
					return false
				}
			}
		}
		lastGeneration = latest.generation
		return true
	}
	if !send() {
		return stable(), false
	}
	ping := time.NewTicker(5 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return stable(), false
		case <-readDone:
			return stable(), false
		case <-ping.C:
			if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(3*time.Second)) != nil {
				return stable(), false
			}
		case <-s.notify:
			if !send() {
				return stable(), false
			}
		case result := <-configResults:
			if !configAccepted || result.SessionID != session {
				continue
			}
			if !nextSequence(&sequence) {
				return stable(), false
			}
			result.Meta = shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: sequence, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			if result.Validate() != nil {
				continue
			}
			if writeFrame(c, "config.result", "", result) != nil {
				return stable(), false
			}
		case result := <-results:
			if !probes.Current(result.TaskVersion, result.TaskID) {
				continue
			}
			if !nextSequence(&sequence) {
				return stable(), false
			}
			payload := shared.PingResult{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: sequence, CollectedAt: result.CollectedAt.Format(time.RFC3339Nano)}, TaskVersion: result.TaskVersion, TaskID: result.TaskID, LatencyMS: result.LatencyMS}
			if payload.Validate() != nil || writeFrame(c, "ping.result", "", payload) != nil {
				return stable(), false
			}
		}
	}
}
func capabilities(c []string, probeEnabled, detailOffered bool, configOptions ...bool) bool {
	configOffered := len(configOptions) != 0 && configOptions[0]
	metrics := false
	for _, v := range c {
		if v == "metrics.v1" {
			metrics = true
		} else if v != "frp.v1" && (v != "ping.v1" || !probeEnabled) && (v != shared.FRPDetailCapability || !detailOffered) && (v != shared.ConfigManageCapability || !configOffered) {
			return false
		}
	}
	return metrics
}

func nextSequence(sequence *uint64) bool {
	if *sequence == ^uint64(0) {
		return false
	}
	*sequence++
	return true
}

func advertisedCapability(header http.Header, capability string) bool {
	values := header.Values(shared.CapabilitiesHeader)
	size := 0
	for _, value := range values {
		size += len(value)
		if size > 4096 {
			return false
		}
	}
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.TrimSpace(token) == capability {
				return true
			}
		}
	}
	return false
}

// errFrameTooLarge marks a frame that exceeds shared.MaxFrameBytes before any
// byte was written, so callers may safely retry with a smaller payload.
var errFrameTooLarge = errors.New("telemetry frame exceeds the protocol byte limit")

func writeFrame(c *websocket.Conn, method, id string, params any) error {
	data, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id,omitempty"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return errors.New("invalid telemetry frame")
	}
	if len(data) > shared.MaxFrameBytes {
		return errFrameTooLarge
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.WriteMessage(websocket.TextMessage, data)
}
