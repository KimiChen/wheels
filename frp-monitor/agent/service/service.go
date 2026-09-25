// SPDX-License-Identifier: Apache-2.0

// Package service 为 frp-monitor agent 的生命周期与调度入口。
//
// 由 frpc 生命周期钩子（build tag frpmonitor）调用；监控 goroutine 独立于
// FRP 转发路径运行，任何采集/上报失败只影响监控自身。
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	frpversion "github.com/fatedier/frp/pkg/util/version"

	"github.com/fatedier/frp/extension/frpmonitor/agent/collect"
	"github.com/fatedier/frp/extension/frpmonitor/agent/probe"
	"github.com/fatedier/frp/extension/frpmonitor/agent/transport"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
	"github.com/fatedier/frp/extension/frpmonitor/shared/version"
)

// Config 为 agent 监控配置（由配置胶水从 frpc 配置翻译而来）。
type Config struct {
	Enable                bool
	ServerURL             string // wss://…/agent/v1/ws；ws:// 仅回环
	Token                 string // 节点独立监控凭据
	ReportIntervalSeconds int    // 默认 1，范围 [1,3600]
	ProcRoot              string // 测试注入用；空为 /proc
	SysRoot               string // 测试注入用；空为 /sys
}

// FRPStatusSource 提供 frpc 只读状态快照，由 client 包内胶水实现。
type FRPStatusSource interface {
	Snapshot() protocol.FRPExtension
}

const (
	frpResendInterval = 60 * time.Second // FRP 扩展区周期兜底
	readIdleTimeout   = 90 * time.Second
	maxBackoff        = 30 * time.Second
	authBackoff       = 60 * time.Second
)

// Service 为 agent 监控运行时。ctx 取消即停止。
type Service struct {
	cfg       Config
	collector *collect.Collector
	src       FRPStatusSource
}

// Start 校验配置并启动后台 goroutine；监控关闭（Enable=false）时返回 nil。
// 启动失败不返回错误：监控问题不得影响 frpc 主流程，只记日志。
func Start(ctx context.Context, cfg Config, src FRPStatusSource) *Service {
	if !cfg.Enable {
		return nil
	}
	if cfg.ServerURL == "" || cfg.Token == "" {
		log.Printf("[frp-monitor] agent 已启用但缺少 serverURL/token，监控不启动")
		return nil
	}
	if cfg.ReportIntervalSeconds < 1 || cfg.ReportIntervalSeconds > 3600 {
		log.Printf("[frp-monitor] reportIntervalSeconds 越界 %d，按默认 1s", cfg.ReportIntervalSeconds)
		cfg.ReportIntervalSeconds = 1
	}
	procRoot, sysRoot := cfg.ProcRoot, cfg.SysRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	s := &Service{
		cfg:       cfg,
		collector: collect.New(procRoot, sysRoot, collect.IfaceFilter{}),
		src:       src,
	}
	go s.run(ctx)
	return s
}

// run 为「连接→会话→断线退避重连」主循环。
func (s *Service) run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.runSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if transport.IsAuth(err) {
			// 认证失败不高频重试：凭据问题只能等运维介入。
			log.Printf("[frp-monitor] %v，%v 后重试", err, authBackoff)
			if !sleepCtx(ctx, jitter(authBackoff)) {
				return
			}
			backoff = time.Second
			continue
		}
		log.Printf("[frp-monitor] 监控连接断开：%v，%v 后重连", err, backoff)
		if !sleepCtx(ctx, jitter(backoff)) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runSession 完成一次监控会话：连接→hello→周期上报，出错返回。
func (s *Service) runSession(ctx context.Context) error {
	conn, err := transport.Dial(ctx, s.cfg.ServerURL, s.cfg.Token)
	if err != nil {
		return err
	}
	defer conn.Close()

	prober := probe.NewManager()
	sess := &session{
		conn:    conn,
		id:      newSessionID(),
		prober:  prober,
		pending: make(map[uint64]chan *protocol.Response),
		readErr: make(chan error, 1),
	}
	go sess.readPump()

	hello := &protocol.HelloParams{
		SchemaVersion:  protocol.SchemaVersion,
		AgentVersion:   version.Version,
		FRPVersion:     frpversion.Base(),
		Capabilities:   []string{"metrics", "ping"},
		SessionID:      sess.id,
		ReportInterval: s.cfg.ReportIntervalSeconds,
		SentAt:         time.Now().Unix(),
	}
	if err := sess.call(ctx, protocol.MethodHello, hello); err != nil {
		return err
	}
	log.Printf("[frp-monitor] 已连接 %s（会话 %s）", s.cfg.ServerURL, sess.id)

	// TCP 探测随会话生命周期：每会话一个 Manager，会话结束取消本轮
	// 全部探测；结果经 ping.result 逐条上报（复用会话 sequence 与 call
	// 通道，call 已并发安全）。
	probeCtx, probeCancel := context.WithCancel(ctx)
	defer probeCancel()
	go prober.Run(probeCtx, func(r protocol.PingResult) {
		params := &protocol.PingResultParams{
			SessionID: sess.id,
			Sequence:  atomic.AddUint64(&sess.sequence, 1),
			SentAt:    time.Now().Unix(),
			Results:   []protocol.PingResult{r},
		}
		// 上报失败不致命：连接级错误会经 readErr 或主循环 call 结束会话。
		if err := sess.call(probeCtx, protocol.MethodPingResult, params); err != nil && probeCtx.Err() == nil {
			log.Printf("[frp-monitor] ping.result 上报失败：%v", err)
		}
	})

	var lastFactsHash, lastFRPHash string
	var lastFRPSent time.Time

	ticker := time.NewTicker(time.Duration(s.cfg.ReportIntervalSeconds) * time.Second)
	defer ticker.Stop()

	// 首报立即执行，不等第一个 tick；错过的 tick 不集中补跑（ticker 自然丢弃）。
	for {
		m := s.collector.Metrics(ctx)
		params := &protocol.ReportParams{
			SessionID: sess.id,
			Sequence:  atomic.AddUint64(&sess.sequence, 1),
			SentAt:    time.Now().Unix(),
			Metrics:   &m,
		}
		// Facts 每次连接先报，之后变化时重报。
		if facts, ferr := s.collector.Facts(ctx); ferr == nil {
			facts.AgentVersion = version.Version
			if h := hashJSON(facts); h != lastFactsHash {
				params.Facts = facts
				lastFactsHash = h
			}
		}
		// FRP 扩展区：变化上报，60 秒兜底。
		if s.src != nil {
			frpState := s.src.Snapshot()
			if h := hashJSON(&frpState); h != lastFRPHash || time.Since(lastFRPSent) >= frpResendInterval {
				params.Extensions = &protocol.Extensions{FRP: &frpState}
				lastFRPHash = h
				lastFRPSent = time.Now()
			}
		}
		if err := sess.call(ctx, protocol.MethodReport, params); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-sess.readErr:
			return err
		case <-ticker.C:
		}
	}
}

// session 维护一条连接上的请求/响应配对与读 pump。
type session struct {
	conn   *transport.Conn
	id     string
	prober *probe.Manager // 本会话的探测调度器；创建后不再变更

	sequence uint64 // 会话内递增的上报序号（report 与 ping.result 共用，atomic 递增）
	callID   uint64 // JSON-RPC 请求 id（atomic 递增；多 goroutine call 并发安全）

	pendingMu sync.Mutex
	pending   map[uint64]chan *protocol.Response
	readErr   chan error // 读 pump 出错即通知
}

// call 发送请求并等待服务端响应（hello/report 都需要服务端 ack 以发现旧会话拒绝）。
func (s *session) call(ctx context.Context, method string, params any) error {
	id := atomic.AddUint64(&s.callID, 1)
	req, err := protocol.NewRequest(&id, method, params)
	if err != nil {
		return err
	}
	ch := make(chan *protocol.Response, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}()

	if err := s.conn.Send(req); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-s.readErr:
		return err
	case resp := <-ch:
		if resp.Error != nil {
			return &rpcError{Code: resp.Error.Code, Message: resp.Error.Message}
		}
		return nil
	case <-time.After(transport.WriteTimeout + 5*time.Second):
		return errors.New("等待服务端响应超时")
	}
}

// readPump 为唯一读循环：分发响应与下行请求（ping.tasks 等）。
func (s *session) readPump() {
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(readIdleTimeout))
		data, err := s.conn.ReadMessage()
		if err != nil {
			s.notifyErr(err)
			return
		}
		var frame struct {
			ID     *uint64         `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *protocol.Error `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			continue // 畸形帧不致命
		}
		if frame.Method != "" {
			s.handleDownlink(frame.ID, frame.Method, frame.Params)
			continue
		}
		if frame.ID == nil {
			continue
		}
		s.pendingMu.Lock()
		ch, ok := s.pending[*frame.ID]
		s.pendingMu.Unlock()
		if ok {
			ch <- &protocol.Response{Result: frame.Result, Error: frame.Error}
		}
	}
}

// handleDownlink 分发 monitor 下行请求：ping.tasks 应用到探测管理器，
// 其余回 MethodNotFound。无 ID 的下行帧按通知处理，不回复。
func (s *session) handleDownlink(id *uint64, method string, params json.RawMessage) {
	var resp *protocol.Response
	if method == protocol.MethodPingTasks {
		resp = applyPingTasks(s.prober, id, params)
	} else if id != nil {
		resp = &protocol.Response{
			JSONRPC: protocol.JSONRPCVersion,
			ID:      *id,
			Error:   &protocol.Error{Code: protocol.CodeMethodNotFound, Message: "agent 未实现：" + method},
		}
	}
	// 写失败由 readErr / 主循环 call 发现，此处忽略。
	if resp != nil {
		_ = s.conn.Send(resp)
	}
}

// applyPingTasks 校验并整体替换探测任务列表，产出对 ping.tasks 的响应：
// 成功回 result null；参数或版本非法回 CodeInvalidParams。id 为 nil 时
// 仍应用变更但不产出响应。
func applyPingTasks(prober *probe.Manager, id *uint64, params json.RawMessage) *protocol.Response {
	var p protocol.PingTasksParams
	var rpcErr *protocol.Error
	if err := json.Unmarshal(params, &p); err != nil {
		rpcErr = &protocol.Error{Code: protocol.CodeInvalidParams, Message: "ping.tasks params 非法"}
	} else if err := prober.Apply(p); err != nil {
		rpcErr = &protocol.Error{Code: protocol.CodeInvalidParams, Message: err.Error()}
	}
	if id == nil {
		return nil
	}
	resp := &protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: *id}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = json.RawMessage("null")
	}
	return resp
}

func (s *session) notifyErr(err error) {
	select {
	case s.readErr <- err:
	default:
	}
}

// rpcError 为服务端返回的 JSON-RPC 错误。
type rpcError struct {
	Code    int
	Message string
}

func (e *rpcError) Error() string {
	return "monitor 拒绝（" + strconv.Itoa(e.Code) + "）：" + e.Message
}

func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strconv.FormatInt(time.Now().Unix(), 10) + "-" + hex.EncodeToString(b[:])
}

// hashJSON 用于 Facts/FRP 扩展区的变化检测。
func hashJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// jitter 对退避时长加 ±20% 抖动。
func jitter(d time.Duration) time.Duration {
	var b [8]byte
	_, _ = rand.Read(b[:])
	factor := 0.8 + 0.4*float64(int(b[0])<<8|int(b[1]))/65535.0
	return time.Duration(float64(d) * factor)
}

// sleepCtx 睡眠 d；ctx 取消返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
