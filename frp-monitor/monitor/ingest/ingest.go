// SPDX-License-Identifier: Apache-2.0

// Package ingest 实现节点监控 WSS 接收端点 /agent/v1/ws（根 README §5）。
//
// 流程：Bearer 认证（失败 401，不升级）→ WebSocket 升级（拒绝跨域）→
// 5 秒内必须收到 hello（schema 不支持回 CodeUnsupportedSchema 并关闭）→
// report 读循环。帧上限 256KiB，读超时 + pong 保活；每条消息独立
// json.Unmarshal + params.Validate()，消息级错误回 JSON-RPC 错误后继续，
// 仅会话级错误（旧会话）断开连接。agent 主动 ping 由 WS 层保活处理。
// P2 起：hello 声明 "ping" 能力的连接会收到 ping.tasks 任务下发（等 ack
// 5s，PUT 变更时版本 +1 全量重发）；ping.result 校验会话/序号后落库。
package ingest

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

const (
	// maxFrameBytes 为单帧上限 256KiB。
	maxFrameBytes = 256 << 10
	// helloTimeout 为升级后等待 hello 的时限。
	helloTimeout = 5 * time.Second
	// pongWait 为读超时；收到 pong 后续期。
	pongWait = 90 * time.Second
	// pingPeriod 为 WS 层 ping 发送周期（须小于 pongWait）。
	pingPeriod = 30 * time.Second
	// writeTimeout 为单次写超时。
	writeTimeout = 10 * time.Second
	// pingAckTimeout 为 ping.tasks 下发后等待 ack 的时限。
	pingAckTimeout = 5 * time.Second
	// pingCapability 为支持 TCP 探测的 hello 能力名。
	pingCapability = "ping"
)

// Handler 为 /agent/v1/ws 的 HTTP 处理器。
type Handler struct {
	auth    *auth.NodeAuthenticator
	store   *store.Store
	version string

	upgrader websocket.Upgrader

	mu    sync.Mutex
	conns map[string]*clientConn // nodeID → 当前会话连接

	nextReqID uint64 // 服务端请求 ID（ping.tasks 下发用），原子递增
}

// NewHandler 创建接收端点。serverVersion 填入 hello 应答的 server_version。
func NewHandler(a *auth.NodeAuthenticator, st *store.Store, serverVersion string) *Handler {
	h := &Handler{
		auth:    a,
		store:   st,
		version: serverVersion,
		conns:   make(map[string]*clientConn),
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// 拒绝跨域升级；无 Origin（非浏览器客户端）放行。
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			u, err := url.Parse(origin)
			if err != nil {
				return false
			}
			return u.Host == r.Host
		},
	}
	return h
}

// clientConn 为一条已升级的节点连接。写操作串行化。
type clientConn struct {
	ws        *websocket.Conn
	nodeID    string
	sessionID string // hello 完成后设置

	// pingCapable 为 hello 声明的 "ping" 能力。
	pingCapable bool
	// done 在连接服务函数退出时关闭，用于取消进行中的下发等待。
	done chan struct{}

	writeMu sync.Mutex

	// pending 为等待 ack 的服务端请求：请求 ID → 结果通道（缓冲 1）。
	pendingMu sync.Mutex
	pending   map[uint64]chan bool
}

// writeJSON 串行化写一条 JSON 消息。
func (c *clientConn) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.ws.WriteJSON(v)
}

// registerPending 登记一个等待 ack 的服务端请求。
func (c *clientConn) registerPending(id uint64) chan bool {
	ch := make(chan bool, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	return ch
}

// clearPending 摘除未等到应答的请求。
func (c *clientConn) clearPending(id uint64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// deliverPending 把 agent 的应答投递给等待者；ok=false 表示错误应答。
func (c *clientConn) deliverPending(id uint64, ok bool) {
	c.pendingMu.Lock()
	ch, found := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if found {
		ch <- ok
	}
}

// respond 回 JSON-RPC 正常结果。
func (c *clientConn) respond(id uint64, result json.RawMessage) {
	resp := &protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Result: result}
	if err := c.writeJSON(resp); err != nil {
		log.Printf("frpmonitor ingest: 节点 %s 响应写入失败：%v", c.nodeID, err)
	}
}

// respondError 回 JSON-RPC 错误。
func (c *clientConn) respondError(id uint64, code int, message string) {
	resp := &protocol.Response{
		JSONRPC: protocol.JSONRPCVersion,
		ID:      id,
		Error:   &protocol.Error{Code: code, Message: message},
	}
	if err := c.writeJSON(resp); err != nil {
		log.Printf("frpmonitor ingest: 节点 %s 错误响应写入失败：%v", c.nodeID, err)
	}
}

// ServeHTTP 处理 GET /agent/v1/ws。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		writeAuthError(w)
		return
	}
	nodeID, ok := h.auth.Authenticate(token)
	if !ok {
		writeAuthError(w)
		return
	}
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade 内部已写错误响应
		return
	}
	h.serve(ws, nodeID)
}

func writeAuthError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

// bearerToken 提取 Authorization: Bearer <token>。
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

// serve 驱动一条连接的生命周期（同步，占用当前 goroutine）。
func (h *Handler) serve(ws *websocket.Conn, nodeID string) {
	c := &clientConn{ws: ws, nodeID: nodeID, done: make(chan struct{}), pending: make(map[uint64]chan bool)}
	defer ws.Close()
	defer close(c.done)

	ws.SetReadLimit(maxFrameBytes)

	// hello 协商：5 秒内必须到达且为会话首条消息
	ws.SetReadDeadline(time.Now().Add(helloTimeout))
	_, data, err := ws.ReadMessage()
	if err != nil {
		return
	}
	var req protocol.Request
	if err := json.Unmarshal(data, &req); err != nil {
		c.respondError(0, protocol.CodeParseError, "非法 JSON")
		return
	}
	if req.JSONRPC != protocol.JSONRPCVersion || req.Method != protocol.MethodHello {
		c.respondError(requestID(&req), protocol.CodeInvalidRequest, "首条消息必须是 hello")
		return
	}
	var hello protocol.HelloParams
	if err := json.Unmarshal(req.Params, &hello); err != nil {
		c.respondError(requestID(&req), protocol.CodeInvalidParams, "hello params 非法")
		return
	}
	if err := hello.Validate(); err != nil {
		c.respondError(requestID(&req), protocol.CodeInvalidParams, err.Error())
		return
	}
	if hello.SchemaVersion != protocol.SchemaVersion {
		c.respondError(requestID(&req), protocol.CodeUnsupportedSchema, "不支持的 schema 版本")
		return
	}

	// 建立新会话并取代旧会话；本端点负责关闭被取代的旧连接。
	replaced := h.store.StartSession(nodeID, hello.SessionID, hello.ReportInterval, time.Now())
	h.mu.Lock()
	old := h.conns[nodeID]
	h.conns[nodeID] = c
	h.mu.Unlock()
	if old != nil && old.sessionID == replaced {
		_ = old.ws.Close()
	}
	c.sessionID = hello.SessionID
	defer func() {
		// 仅当本连接仍是当前会话时才摘除注册并标记离线；
		// 旧连接的迟到退出不得覆盖新会话。
		h.mu.Lock()
		if h.conns[nodeID] == c {
			delete(h.conns, nodeID)
		}
		h.mu.Unlock()
		h.store.EndSession(nodeID, hello.SessionID)
	}()

	result, _ := json.Marshal(&protocol.HelloResult{
		SchemaVersion: protocol.SchemaVersion,
		ServerVersion: h.version,
		Capabilities:  []string{},
		ServerTime:    time.Now().Unix(),
	})
	c.respond(requestID(&req), result)

	// 声明 "ping" 能力：hello 成功后下发当前任务列表（离线期间的变更
	// 在此补发）；之后 PUT 变更经 SetProbeTasks 的 hook 全量重发。
	if hasCapability(hello.Capabilities, pingCapability) {
		c.pingCapable = true
		go h.dispatchPingTasks(c)
	}

	// 读循环 + WS 层 ping/pong 保活
	ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	done := make(chan struct{})
	defer close(done)
	go h.pingLoop(c, done)

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if isResponseFrame(data) {
			h.handleResponse(c, data)
			continue
		}
		if h.handleMessage(c, data) {
			return
		}
	}
}

// pingLoop 周期发送 WS ping，对端 pong 由读循环的 handler 续期。
func (h *Handler) pingLoop(c *clientConn, done <-chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			c.writeMu.Lock()
			c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// handleMessage 处理一条帧。返回 true 表示应断开连接（会话级错误）。
func (h *Handler) handleMessage(c *clientConn, data []byte) (closeConn bool) {
	var req protocol.Request
	if err := json.Unmarshal(data, &req); err != nil {
		c.respondError(0, protocol.CodeParseError, "非法 JSON")
		return false
	}
	id := requestID(&req)
	if req.JSONRPC != protocol.JSONRPCVersion {
		c.respondError(id, protocol.CodeInvalidRequest, "jsonrpc 必须为 2.0")
		return false
	}

	switch req.Method {
	case protocol.MethodReport:
		var p protocol.ReportParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			c.respondError(id, protocol.CodeInvalidParams, "report params 非法")
			return false
		}
		if err := p.Validate(); err != nil {
			c.respondError(id, protocol.CodeInvalidParams, err.Error())
			return false
		}
		err := h.store.Report(c.nodeID, p.SessionID, p.Sequence, p.Facts, p.Metrics,
			frpExtension(p.Extensions), time.Now())
		switch {
		case errors.Is(err, store.ErrStaleSession):
			c.respondError(id, protocol.CodeStaleSession, err.Error())
			// 本连接自己的会话已被取代时才断开（旧连接退场）；
			// 当前会话内发错 session_id 仅回错误，不惩罚连接。
			return p.SessionID == c.sessionID
		case errors.Is(err, store.ErrOutOfOrder):
			c.respondError(id, protocol.CodeStaleSession, err.Error())
			return false
		case err != nil:
			c.respondError(id, protocol.CodeInvalidParams, err.Error())
			return false
		}
		if req.ID != nil {
			c.respond(id, json.RawMessage(`{}`))
		}
		return false

	case protocol.MethodPingResult:
		var p protocol.PingResultParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			c.respondError(id, protocol.CodeInvalidParams, "ping.result params 非法")
			return false
		}
		if err := p.Validate(); err != nil {
			c.respondError(id, protocol.CodeInvalidParams, err.Error())
			return false
		}
		err := h.store.RecordPingResults(c.nodeID, p.SessionID, p.Sequence, p.Results, time.Now())
		switch {
		case errors.Is(err, store.ErrStaleSession):
			c.respondError(id, protocol.CodeStaleSession, err.Error())
			// 与 report 一致：本连接自己的会话已被取代时才断开。
			return p.SessionID == c.sessionID
		case errors.Is(err, store.ErrOutOfOrder):
			c.respondError(id, protocol.CodeStaleSession, err.Error())
			return false
		case err != nil:
			c.respondError(id, protocol.CodeInvalidParams, err.Error())
			return false
		}
		if req.ID != nil {
			c.respond(id, json.RawMessage(`{}`))
		}
		return false

	default:
		if req.ID != nil {
			c.respondError(id, protocol.CodeMethodNotFound, "未知方法")
		}
		return false
	}
}

func requestID(req *protocol.Request) uint64 {
	if req.ID != nil {
		return *req.ID
	}
	return 0
}

func frpExtension(ext *protocol.Extensions) *protocol.FRPExtension {
	if ext == nil {
		return nil
	}
	return ext.FRP
}

// ---------- ping.tasks 下发闭环 ----------

// hasCapability 判断 hello 能力列表是否包含指定能力。
func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// isResponseFrame 判断一帧是否为 JSON-RPC 应答（无 method 字段）；
// 无法解析时按请求处理（由 handleMessage 回报 parse error）。
func isResponseFrame(data []byte) bool {
	var head struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return false
	}
	return head.Method == ""
}

// handleResponse 处理 agent 对服务端请求的应答（ping.tasks 的 ack）。
func (h *Handler) handleResponse(c *clientConn, data []byte) {
	var resp protocol.Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return
	}
	c.deliverPending(resp.ID, resp.Error == nil)
}

// NotifyProbeTasksChanged 为 store.SetProbeTasks 的变更回调：节点在线且
// 声明了 "ping" 能力时全量重发当前任务列表。
func (h *Handler) NotifyProbeTasksChanged(nodeID string) {
	h.mu.Lock()
	c := h.conns[nodeID]
	h.mu.Unlock()
	if c == nil || !c.pingCapable {
		return
	}
	go h.dispatchPingTasks(c)
}

// dispatchPingTasks 下发当前任务列表并等待 ack（5s）。版本为 0（从未配置）
// 时不下发。失败/超时只记日志；goroutine 局部 recover。
func (h *Handler) dispatchPingTasks(c *clientConn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("frpmonitor ingest: 节点 %s 任务下发 panic（已降级）：%v", c.nodeID, r)
		}
	}()
	version, tasks := h.store.ProbeTasks(c.nodeID)
	if version == 0 {
		return
	}
	id := atomic.AddUint64(&h.nextReqID, 1)
	req, err := protocol.NewRequest(&id, protocol.MethodPingTasks,
		&protocol.PingTasksParams{Version: version, Tasks: tasks})
	if err != nil {
		return
	}
	ack := c.registerPending(id)
	if err := c.writeJSON(req); err != nil {
		c.clearPending(id)
		log.Printf("frpmonitor ingest: 节点 %s 任务下发写入失败：%v", c.nodeID, err)
		return
	}
	h.store.MarkProbeTasksSent(c.nodeID, version)
	select {
	case ok := <-ack:
		if ok {
			h.store.MarkProbeTasksAcked(c.nodeID, version)
		} else {
			log.Printf("frpmonitor ingest: 节点 %s 拒绝任务版本 %d", c.nodeID, version)
		}
	case <-time.After(pingAckTimeout):
		c.clearPending(id)
		log.Printf("frpmonitor ingest: 节点 %s 任务版本 %d 等 ack 超时", c.nodeID, version)
	case <-c.done:
	}
}
