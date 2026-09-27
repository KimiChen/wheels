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
	"github.com/gorilla/websocket"
)

type sampler interface {
	Facts() shared.Facts
	Metrics() shared.Metrics
}
type observation struct {
	facts      shared.Facts
	metrics    shared.Metrics
	frp        *shared.FRP
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
}

func Start(ctx context.Context, cfg shared.AgentConfig, snapshot func() shared.FRP) (*Service, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, errors.New("invalid telemetry configuration")
	}
	c, err := collect.New(collect.Config{Iface: cfg.Iface, Version: shared.Version})
	if err != nil {
		return nil, errors.New("telemetry collector unavailable")
	}
	return start(ctx, cfg, snapshot, c)
}
func start(ctx context.Context, cfg shared.AgentConfig, snapshot func() shared.FRP, c sampler) (*Service, error) {
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
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("telemetry CA invalid")
		}
		tlsCfg.RootCAs = pool
	}
	child, cancel := context.WithCancel(ctx)
	s := &Service{ctx: child, cancel: cancel, cfg: cfg, token: token, collector: c, snapshot: snapshot, notify: make(chan struct{}, 1), intervalChanged: make(chan struct{}, 1), ready: make(chan struct{}), done: make(chan struct{}), dialer: websocket.Dialer{HandshakeTimeout: 5 * time.Second, TLSClientConfig: tlsCfg, ReadBufferSize: 4096, WriteBufferSize: 4096}}
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
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit || (private && runtime.GOOS != "windows" && st.Mode().Perm() != 0600) {
		return nil, errors.New("invalid local telemetry file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("local telemetry file unavailable")
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || actual.Size() > limit || (private && runtime.GOOS != "windows" && actual.Mode().Perm() != 0600) {
		return nil, errors.New("invalid local telemetry file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("invalid local telemetry file")
	}
	return data, nil
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
	value := observation{facts: s.collector.Facts(), metrics: s.collector.Metrics(), frp: s.frpSnapshot(), at: time.Now().UTC()}
	if value.facts.Validate() != nil || value.metrics.Validate() != nil {
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
		connected, unauthorized := s.connect()
		if s.ctx.Err() != nil {
			return
		}
		if connected {
			delay = time.Second
		} else if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
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
	hello := shared.Hello{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: 1, CollectedAt: current.at.Format(time.RFC3339Nano)}, Capabilities: []string{"metrics.v1", "frp.v1"}, Facts: current.facts, Extensions: extension(current.frp)}
	if s.cfg.ProbeEnabled {
		hello.Capabilities = append(hello.Capabilities, "ping.v1")
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
	if decoder.Decode(&answer) != nil || decoder.Decode(new(any)) != io.EOF || answer.JSONRPC != "2.0" || answer.ID != "hello" || answer.Result.Validate() != nil || answer.Result.SessionID != session || !capabilities(answer.Result.Capabilities, s.cfg.ProbeEnabled) {
		return false, false
	}
	negotiated := int64(answer.Result.ReportInterval)
	if s.interval.Swap(negotiated) != negotiated {
		select {
		case s.intervalChanged <- struct{}{}:
		default:
		}
	}
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(15 * time.Second)) })
	var probes *probe.Engine
	var results <-chan probe.Result
	for _, capability := range answer.Result.Capabilities {
		if capability == "ping.v1" {
			probes = probe.New(s.ctx, probe.Config{AllowPrivate: s.cfg.ProbeAllowPrivate})
			results = probes.Results()
			defer probes.Close()
		}
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer c.Close()
		var taskSequence uint64
		taskBudget, replenished := float64(4), time.Now()
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage || probes == nil {
				return
			}
			now := time.Now()
			taskBudget += now.Sub(replenished).Seconds()
			if taskBudget > 4 {
				taskBudget = 4
			}
			replenished = now
			if taskBudget < 1 {
				return
			}
			taskBudget--
			frame, err := shared.DecodeFrame(data)
			if err != nil || frame.PingTasks == nil || frame.PingTasks.SessionID != session || frame.PingTasks.Sequence <= taskSequence || probes.Replace(*frame.PingTasks) != nil {
				return
			}
			taskSequence = frame.PingTasks.Sequence
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
		sequence++
		report := shared.Report{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: sequence, CollectedAt: latest.at.Format(time.RFC3339Nano)}, Facts: &latest.facts, Metrics: &latest.metrics, Extensions: extension(latest.frp)}
		if report.Validate() != nil {
			return false
		}
		if writeFrame(c, "report", "", report) != nil {
			return false
		}
		lastGeneration = latest.generation
		return true
	}
	if !send() {
		return true, false
	}
	ping := time.NewTicker(5 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return true, false
		case <-readDone:
			return true, false
		case <-ping.C:
			if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(3*time.Second)) != nil {
				return true, false
			}
		case <-s.notify:
			if !send() {
				return true, false
			}
		case result := <-results:
			if !probes.Current(result.TaskVersion, result.TaskID) {
				continue
			}
			sequence++
			payload := shared.PingResult{Meta: shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: sequence, CollectedAt: result.CollectedAt.Format(time.RFC3339Nano)}, TaskVersion: result.TaskVersion, TaskID: result.TaskID, LatencyMS: result.LatencyMS}
			if payload.Validate() != nil || writeFrame(c, "ping.result", "", payload) != nil {
				return true, false
			}
		}
	}
}
func capabilities(c []string, probeEnabled bool) bool {
	metrics := false
	for _, v := range c {
		if v == "metrics.v1" {
			metrics = true
		} else if v != "frp.v1" && (v != "ping.v1" || !probeEnabled) {
			return false
		}
	}
	return metrics
}
func writeFrame(c *websocket.Conn, method, id string, params any) error {
	data, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id,omitempty"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err != nil || len(data) > shared.MaxFrameBytes {
		return errors.New("invalid telemetry frame")
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.WriteMessage(websocket.TextMessage, data)
}
