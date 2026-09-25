// SPDX-License-Identifier: Apache-2.0

// Package api 提供 monitor 的公开/管理 JSON API、SSE 实时推送与静态资源
// 服务（根 README §7）。
//
// 字段裁剪在服务端完成：公开 DTO 绝不含 hostname/ipv4/ipv6/kernel/virt/
// cpu_name/boot_id/iface/local_addr/session_id。大整数（字节、计数器）
// 在 DTO 中一律十进制字符串，浮点速率保留 number；质量 unknown 或缺失
// 的字段输出 null。公开与管理路由同 Listener。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

// sseMergeWindow 为 SSE 状态变更的合并窗口（1 秒）。
const sseMergeWindow = time.Second

// sseHeartbeat 为 SSE 注释行心跳周期。
const sseHeartbeat = 25 * time.Second

// securityHeaders 为通用安全响应头：CSP 仅允许 self 的 script/style，
// SSE connect-src self。页面 `<head>` 有一段套件防主题闪烁的内联脚本，
// 按内容 sha256 白名单放行；页面改动该脚本时必须同步更新 hash
// （csp_test.go 会从嵌入资源重算并兜底）。
var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; " +
		"script-src 'self' 'sha256-5K7v5Q62UeN/ZKYsbF1pyjTR93OU70PKOEm1B0/x/fc=' 'sha256-CRj9BD/TqLC4vRgwp9B3cpGGr3oRGNGMM2H1X2oxo2c='; " +
		"style-src 'self'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'",
	"X-Content-Type-Options": "nosniff",
	"X-Frame-Options":        "DENY",
}

// Handler 为 API/SSE/静态资源处理器。
type Handler struct {
	store  *store.Store
	admin  *auth.Admin
	static fs.FS // 页面静态资源；nil 时页面路由降级为 503（不 panic）

	now func() time.Time // 测试可注入

	mux        *http.ServeMux
	fileServer http.Handler
}

// NewHandler 组装路由。static 为 web 包提供的静态资源 fs（index.html 位于根），
// 传 nil 时 API/SSE 仍可用，页面路由返回 503。
func NewHandler(st *store.Store, admin *auth.Admin, static fs.FS) *Handler {
	h := &Handler{store: st, admin: admin, static: static, now: time.Now}
	if static != nil {
		h.fileServer = http.FileServer(http.FS(static))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/public/v1/overview", h.handleOverview)
	mux.HandleFunc("GET /api/public/v1/nodes", h.handlePublicNodes)
	mux.HandleFunc("GET /api/public/v1/nodes/{id}", h.handlePublicNode)
	mux.HandleFunc("POST /api/admin/v1/login", h.handleLogin)
	mux.HandleFunc("POST /api/admin/v1/logout", h.handleLogout)
	mux.HandleFunc("GET /api/admin/v1/session", h.handleAdminSession)
	mux.HandleFunc("GET /api/admin/v1/nodes", h.handleAdminNodes)
	mux.HandleFunc("GET /api/admin/v1/nodes/{id}", h.handleAdminNode)
	mux.HandleFunc("GET /events/public", h.handlePublicEvents)
	mux.HandleFunc("GET /events/admin", h.handleAdminEvents)
	// 路由形态的 README 约定为 /admin/，静态资源实际文件为 admin.html。
	mux.HandleFunc("GET /admin", h.handleAdminRedirect)
	mux.HandleFunc("GET /admin/", h.handleAdminRedirect)
	mux.HandleFunc("/", h.handleStatic)
	h.mux = mux
	return h
}

// ServeHTTP 先写通用安全响应头再分发。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for k, v := range securityHeaders {
		w.Header().Set(k, v)
	}
	h.mux.ServeHTTP(w, r)
}

// ---------- DTO ----------

// nodeCommon 为公开与管理 DTO 共享的字段（均为裁剪后的安全字段）。
// 大整数字段为十进制字符串；指针为 nil 时输出 null。
type nodeCommon struct {
	ID                  string      `json:"id"`
	Online              bool        `json:"online"`
	MetricsStale        bool        `json:"metrics_stale"`
	ReportInterval      int         `json:"report_interval"`
	CollectedAt         *int64      `json:"collected_at"`
	LastSeen            *int64      `json:"last_seen"`
	CPU                 *float64    `json:"cpu"`
	Load                *[3]float64 `json:"load"`
	MemUsed             *string     `json:"mem_used"`
	MemTotal            *string     `json:"mem_total"`
	SwapUsed            *string     `json:"swap_used"`
	SwapTotal           *string     `json:"swap_total"`
	DiskUsed            *string     `json:"disk_used"`
	DiskTotal           *string     `json:"disk_total"`
	NetRX               *float64    `json:"net_rx"`
	NetTX               *float64    `json:"net_tx"`
	Uptime              *int64      `json:"uptime"`
	TCP                 *int64      `json:"tcp"`
	UDP                 *int64      `json:"udp"`
	Procs               *int64      `json:"procs"`
	FRPControlConnected *bool       `json:"frp_control_connected"`
}

// publicProxy 为公开 Proxy DTO：无本地目标。
type publicProxy struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
}

// adminProxy 为管理 Proxy DTO：含本地目标。
type adminProxy struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	LocalAddr string `json:"local_addr"`
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status"`
}

// publicNodeDTO 为公开节点 DTO（服务端裁剪结果）。
type publicNodeDTO struct {
	nodeCommon
	// Proxies 在从未收到 FRP 扩展时为 null，否则为数组（可为空）。
	Proxies []publicProxy `json:"proxies"`
}

// adminFactsDTO 为管理端资产信息；均输出 string|null（cpu_cores 为十进制
// 字符串，避免与「大整数为字符串」的 DTO 约定不一致）。Facts 从未上报时
// 整个 facts 字段为 null。
type adminFactsDTO struct {
	Hostname     *string `json:"hostname"`
	OS           *string `json:"os"`
	Kernel       *string `json:"kernel"`
	Arch         *string `json:"arch"`
	Virt         *string `json:"virt"`
	CPUName      *string `json:"cpu_name"`
	CPUCores     *string `json:"cpu_cores"`
	AgentVersion *string `json:"agent_version"`
	IPv4         *string `json:"ipv4"`
	IPv6         *string `json:"ipv6"`
}

// frpClientDTO 为服务端注册表对账结果条目。
type frpClientDTO struct {
	User             string `json:"user"`
	ClientID         string `json:"client_id"`
	RunID            string `json:"run_id"`
	Online           bool   `json:"online"`
	Version          string `json:"version"`
	FirstConnectedAt int64  `json:"first_connected_at"`
	LastConnectedAt  int64  `json:"last_connected_at"`
}

// adminNodeDTO 为管理节点 DTO：公开全部字段 + 资产 + 计数器范围 +
// 本地目标 + FRP 对账结果。
type adminNodeDTO struct {
	nodeCommon
	Proxies []adminProxy `json:"proxies"`

	Facts      *adminFactsDTO `json:"facts"`
	BootID     *string        `json:"boot_id"`
	Iface      *string        `json:"iface"`
	NetRXTotal *string        `json:"net_rx_total"`
	NetTXTotal *string        `json:"net_tx_total"`

	// FRPClientID 为对账绑定结果；冲突或未匹配为 null（不自动绑定）。
	FRPClientID *string        `json:"frp_client_id"`
	FRPClients  []frpClientDTO `json:"frp_clients"`
	ConnectedAt *int64         `json:"connected_at"`
}

// overviewDTO 为总览聚合。
type overviewDTO struct {
	Now              int64   `json:"now"`
	NodesTotal       int     `json:"nodes_total"`
	NodesOnline      int     `json:"nodes_online"`
	NodesOffline     int     `json:"nodes_offline"`
	FRPClientsOnline int     `json:"frp_clients_online"`
	NetRXBps         float64 `json:"net_rx_bps"`
	NetTXBps         float64 `json:"net_tx_bps"`
}

// ---------- DTO 构造 ----------

// qualityKnown 判断某数据组质量是否有效：Quality 缺失或标记为 ok/空
// 即有效；unknown 即无效（展示 null）。
func qualityKnown(q *metrics.Quality, group string) bool {
	if q == nil {
		return true
	}
	var v string
	switch group {
	case "cpu":
		v = q.CPU
	case "mem":
		v = q.Mem
	case "swap":
		v = q.Swap
	case "disk":
		v = q.Disk
	case "net_rate":
		v = q.NetRate
	case "net_total":
		v = q.NetTotal
	case "sys":
		v = q.Sys
	}
	return v != metrics.QualityUnknown
}

func u64str(v uint64) *string {
	s := strconv.FormatUint(v, 10)
	return &s
}

func strptr(s string) *string { return &s }

func i64(v uint64) *int64 {
	x := int64(v)
	return &x
}

// commonFor 填充共享字段。质量 unknown 或指标缺失的字段保持 null。
func (h *Handler) commonFor(n store.NodeState, now time.Time) nodeCommon {
	c := nodeCommon{
		ID:             n.NodeID,
		Online:         n.Online,
		MetricsStale:   n.MetricsStale(now),
		ReportInterval: n.ReportInterval,
	}
	if n.LastReportAt > 0 {
		ls := n.LastReportAt
		c.LastSeen = &ls
	} else if n.ConnectedAt > 0 {
		ca := n.ConnectedAt
		c.LastSeen = &ca
	}

	if m := n.Metrics; m != nil {
		ca := m.CollectedAt
		c.CollectedAt = &ca
		q := m.Quality
		if qualityKnown(q, "cpu") {
			cpu := m.CPU
			c.CPU = &cpu
		}
		if qualityKnown(q, "sys") {
			load := m.Load
			c.Load = &load
			c.Uptime = i64(m.Uptime)
			c.TCP = i64(m.TCP)
			c.UDP = i64(m.UDP)
			c.Procs = i64(m.Procs)
		}
		if qualityKnown(q, "mem") {
			c.MemUsed = u64str(m.MemUsed)
			c.MemTotal = u64str(m.MemTotal)
		}
		if qualityKnown(q, "swap") {
			c.SwapUsed = u64str(m.SwapUsed)
			c.SwapTotal = u64str(m.SwapTotal)
		}
		if qualityKnown(q, "disk") {
			c.DiskUsed = u64str(m.DiskUsed)
			c.DiskTotal = u64str(m.DiskTotal)
		}
		if qualityKnown(q, "net_rate") {
			rx := m.NetRX
			tx := m.NetTX
			c.NetRX = &rx
			c.NetTX = &tx
		}
	}
	if n.FRP != nil {
		cc := n.FRP.ControlConnected
		c.FRPControlConnected = &cc
	}
	return c
}

func (h *Handler) publicNodeFor(n store.NodeState, now time.Time) publicNodeDTO {
	d := publicNodeDTO{nodeCommon: h.commonFor(n, now)}
	if n.FRP != nil {
		d.Proxies = make([]publicProxy, 0, len(n.FRP.Proxies))
		for _, p := range n.FRP.Proxies {
			d.Proxies = append(d.Proxies, publicProxy{
				Name: p.Name, Type: p.Type, Enabled: p.Enabled, Status: p.Status,
			})
		}
	}
	return d
}

func (h *Handler) adminNodeFor(n store.NodeState, all []store.NodeState,
	clients []store.FRPClient, now time.Time) adminNodeDTO {

	d := adminNodeDTO{nodeCommon: h.commonFor(n, now), FRPClients: []frpClientDTO{}}
	if n.FRP != nil {
		d.Proxies = make([]adminProxy, 0, len(n.FRP.Proxies))
		for _, p := range n.FRP.Proxies {
			d.Proxies = append(d.Proxies, adminProxy{
				Name: p.Name, Type: p.Type, LocalAddr: p.LocalAddr,
				Enabled: p.Enabled, Status: p.Status,
			})
		}
	}
	if f := n.Facts; f != nil {
		d.Facts = &adminFactsDTO{
			Hostname:     strptr(f.Hostname),
			OS:           strptr(f.OS),
			Kernel:       strptr(f.Kernel),
			Arch:         strptr(f.Arch),
			Virt:         strptr(f.Virt),
			CPUName:      strptr(f.CPUName),
			CPUCores:     strptr(strconv.Itoa(f.CPUCores)),
			AgentVersion: strptr(f.AgentVersion),
			IPv4:         strptr(f.IPv4),
			IPv6:         strptr(f.IPv6),
		}
	}
	if m := n.Metrics; m != nil {
		d.BootID = strptr(m.BootID)
		d.Iface = strptr(m.Iface)
		if qualityKnown(m.Quality, "net_total") {
			d.NetRXTotal = u64str(m.NetRXTotal)
			d.NetTXTotal = u64str(m.NetTXTotal)
		}
	}
	if n.ConnectedAt > 0 {
		ca := n.ConnectedAt
		d.ConnectedAt = &ca
	}

	// FRP 对账：只读；冲突不绑定但仍返回匹配条目。
	matched, bound := store.Reconcile(n, all, clients)
	if bound && n.FRP != nil {
		d.FRPClientID = strptr(n.FRP.ClientID)
	}
	for _, c := range matched {
		d.FRPClients = append(d.FRPClients, frpClientDTO{
			User: c.User, ClientID: c.ClientID, RunID: c.RunID, Online: c.Online,
			Version:          c.Version,
			FirstConnectedAt: c.FirstConnectedAt, LastConnectedAt: c.LastConnectedAt,
		})
	}
	return d
}

func (h *Handler) overviewFor(now time.Time) overviewDTO {
	o := overviewDTO{Now: now.Unix()}
	for _, n := range h.store.Snapshot() {
		o.NodesTotal++
		if n.Online {
			o.NodesOnline++
			// net 为在线节点有效速率之和
			if m := n.Metrics; m != nil && qualityKnown(m.Quality, "net_rate") {
				o.NetRXBps += m.NetRX
				o.NetTXBps += m.NetTX
			}
		} else {
			o.NodesOffline++
		}
	}
	for _, c := range h.store.FRPClients() {
		if c.Online {
			o.FRPClientsOnline++
		}
	}
	return o
}

// ---------- 响应辅助 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"internal"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// ---------- 公开 API ----------

func (h *Handler) handleOverview(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.overviewFor(h.now()))
}

func (h *Handler) handlePublicNodes(w http.ResponseWriter, _ *http.Request) {
	now := h.now()
	snap := h.store.Snapshot()
	nodes := make([]publicNodeDTO, 0, len(snap))
	for _, n := range snap {
		nodes = append(nodes, h.publicNodeFor(n, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":   now.Unix(),
		"nodes": nodes,
	})
}

func (h *Handler) handlePublicNode(w http.ResponseWriter, r *http.Request) {
	n, ok := h.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":  h.now().Unix(),
		"node": h.publicNodeFor(n, h.now()),
	})
}

// ---------- 管理 API ----------

// requireAdmin 校验管理会话 Cookie，失败写 401 并返回 false。
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !h.admin.SessionValid(r, h.now()) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !auth.OriginAllowed(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !h.admin.Authenticate(body.Password) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	token, exp := h.admin.IssueToken(h.now())
	h.admin.SetCookie(w, r, token, exp)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !auth.OriginAllowed(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	h.admin.ClearCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	if !h.admin.SessionValid(r, h.now()) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) handleAdminNodes(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	now := h.now()
	snap := h.store.Snapshot()
	clients := h.store.FRPClients()
	nodes := make([]adminNodeDTO, 0, len(snap))
	for _, n := range snap {
		nodes = append(nodes, h.adminNodeFor(n, snap, clients, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":   now.Unix(),
		"nodes": nodes,
	})
}

func (h *Handler) handleAdminNode(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	n, ok := h.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":  h.now().Unix(),
		"node": h.adminNodeFor(n, h.store.Snapshot(), h.store.FRPClients(), h.now()),
	})
}

// ---------- SSE ----------

func (h *Handler) handlePublicEvents(w http.ResponseWriter, r *http.Request) {
	h.serveEvents(w, r, false)
}

func (h *Handler) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	h.serveEvents(w, r, true)
}

// serveEvents 推送 SSE：连接即发 snapshot 事件（overview + 全量节点 DTO），
// 之后状态变化按 1 秒窗口合并发 node 事件（单节点 DTO），每 25 秒一行
// `: ping` 心跳。
func (h *Handler) serveEvents(w http.ResponseWriter, r *http.Request, admin bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if !h.sendSnapshot(w, flusher, admin) {
		return
	}

	events, cancel := h.store.Subscribe()
	defer cancel()

	pending := make(map[string]struct{})
	refreshAll := false
	merge := time.NewTicker(sseMergeWindow)
	defer merge.Stop()
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			if ev.NodeID == "" {
				refreshAll = true
			} else {
				pending[ev.NodeID] = struct{}{}
			}
		case <-merge.C:
			if refreshAll {
				for _, n := range h.store.Snapshot() {
					pending[n.NodeID] = struct{}{}
				}
				refreshAll = false
			}
			if len(pending) == 0 {
				continue
			}
			ids := make([]string, 0, len(pending))
			for id := range pending {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			pending = make(map[string]struct{})
			now := h.now()
			var all []store.NodeState
			var clients []store.FRPClient
			if admin {
				all = h.store.Snapshot()
				clients = h.store.FRPClients()
			}
			for _, id := range ids {
				n, ok := h.store.Get(id)
				if !ok {
					continue
				}
				if !h.sendNode(w, n, all, clients, now, admin) {
					return
				}
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// sendSnapshot 发送 snapshot 事件。
func (h *Handler) sendSnapshot(w http.ResponseWriter, flusher http.Flusher, admin bool) bool {
	now := h.now()
	snap := h.store.Snapshot()
	var nodes any
	if admin {
		clients := h.store.FRPClients()
		list := make([]adminNodeDTO, 0, len(snap))
		for _, n := range snap {
			list = append(list, h.adminNodeFor(n, snap, clients, now))
		}
		nodes = list
	} else {
		list := make([]publicNodeDTO, 0, len(snap))
		for _, n := range snap {
			list = append(list, h.publicNodeFor(n, now))
		}
		nodes = list
	}
	return writeSSE(w, flusher, "snapshot", map[string]any{
		"now":      now.Unix(),
		"overview": h.overviewFor(now),
		"nodes":    nodes,
	})
}

// sendNode 发送单个节点的 node 事件。
func (h *Handler) sendNode(w http.ResponseWriter, n store.NodeState, all []store.NodeState,
	clients []store.FRPClient, now time.Time, admin bool) bool {

	if admin {
		return writeSSE(w, nil, "node", h.adminNodeFor(n, all, clients, now))
	}
	return writeSSE(w, nil, "node", h.publicNodeFor(n, now))
}

// writeSSE 写一条 SSE 事件；flusher 非 nil 时立即冲刷。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, v any) bool {
	data, err := json.Marshal(v)
	if err != nil {
		return true // 序列化失败跳过本条，不断开
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return false
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true
}

// ---------- 静态资源 ----------

// handleStatic 提供 web 包静态资源；static 为空 fs 时降级 503，不 panic。
func (h *Handler) handleStatic(w http.ResponseWriter, r *http.Request) {
	if h.fileServer == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "static assets unavailable\n")
		return
	}
	h.fileServer.ServeHTTP(w, r)
}

// handleAdminRedirect 把 /admin 与 /admin/ 重定向到 admin.html。
func (h *Handler) handleAdminRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin.html", http.StatusFound)
}
