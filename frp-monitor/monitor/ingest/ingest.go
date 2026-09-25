// SPDX-License-Identifier: Apache-2.0

// Package ingest 实现节点监控 WSS 接收端点 /agent/v1/ws（根 README §5）。
//
// 流程：Bearer 认证（失败 401，不升级）→ WebSocket 升级（拒绝跨域）→
// 5 秒内必须收到 hello（schema 不支持回 CodeUnsupportedSchema 并关闭）→
// report 读循环。帧上限 256KiB，读超时 + pong 保活；每条消息独立
// json.Unmarshal + params.Validate()，消息级错误回 JSON-RPC 错误后继续，
// 仅会话级错误（旧会话）断开连接。agent 主动 ping 由 WS 层保活处理，
// ping.result 校验后确认但不存储（P2 才落库）。
package ingest

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
)

// Handler 为 /agent/v1/ws 的 HTTP 处理器。
type Handler struct {
	auth    *auth.NodeAuthenticator
	store   *store.Store
	version string

	upgrader websocket.Upgrader

	mu    sync.Mutex
	conns map[string]*clientConn // nodeID → 当前会话连接
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

	writeMu sync.Mutex
}

// writeJSON 串行化写一条 JSON 消息。
func (c *clientConn) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.ws.WriteJSON(v)
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
	c := &clientConn{ws: ws, nodeID: nodeID}
	defer ws.Close()

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
		// P1 不落库，仅校验后确认
		var p protocol.PingResultParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			c.respondError(id, protocol.CodeInvalidParams, "ping.result params 非法")
			return false
		}
		if err := p.Validate(); err != nil {
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
