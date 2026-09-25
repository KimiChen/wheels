// SPDX-License-Identifier: Apache-2.0

// P3 API：隧道对账展示、FRP 状态事件、节点凭据管理与数据库备份。
// DTO 契约为 web 子代理的固定接口；大整数一律十进制字符串。
// 公开隧道 DTO 绝不含 user/client_id/local_addr；local_addr 只出现在
// 管理视图（agent 上报 proxies 按 name 匹配填充）。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
)

// ---------- 隧道 DTO ----------

// tunnelCore 为公开与管理共享的隧道字段。rx/tx 为服务端视角
// （rx=frps 从隧道收到字节，tx=frps 向隧道发出字节），十进制字符串。
type tunnelCore struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Online   bool   `json:"online"`
	CurConns int64  `json:"cur_conns"`
	TodayRX  string `json:"today_rx_bytes"`
	TodayTX  string `json:"today_tx_bytes"`
}

// publicTunnelDTO 为公开隧道列表项：node_id 为对账到的节点，未匹配为 null。
type publicTunnelDTO struct {
	NodeID *string `json:"node_id"`
	tunnelCore
}

// adminTunnelCore 为管理节点 DTO 内的隧道项：额外含 user/client_id/local_addr。
type adminTunnelCore struct {
	tunnelCore
	User      string  `json:"user"`
	ClientID  string  `json:"client_id"`
	LocalAddr *string `json:"local_addr"`
}

// adminTunnelDTO 为管理隧道列表项（SSE tunnels 事件用）。
type adminTunnelDTO struct {
	NodeID *string `json:"node_id"`
	adminTunnelCore
}

func tunnelCoreFor(t store.Tunnel) tunnelCore {
	return tunnelCore{
		Name: t.Proxy.Name, Type: t.Proxy.Type, Online: t.Proxy.Online,
		CurConns: t.Proxy.CurConns,
		TodayRX:  strconv.FormatInt(t.Proxy.TodayTrafficIn, 10),
		TodayTX:  strconv.FormatInt(t.Proxy.TodayTrafficOut, 10),
	}
}

func nodeIDPtr(nodeID string) *string {
	if nodeID == "" {
		return nil
	}
	return &nodeID
}

// tunnels 返回当前隧道对账结果（proxy stats × 节点 FRP 绑定键）。
func (h *Handler) tunnels() []store.Tunnel {
	return store.ReconcileTunnels(h.store.Snapshot(), h.store.FRPProxies())
}

// nodeTunnelsFor 过滤出某节点的公开隧道项（空数组兜底）。
func nodeTunnelsFor(tunnels []store.Tunnel, nodeID string) []tunnelCore {
	out := make([]tunnelCore, 0)
	for _, t := range tunnels {
		if t.NodeID == nodeID {
			out = append(out, tunnelCoreFor(t))
		}
	}
	return out
}

// adminNodeTunnelsFor 过滤出某节点的管理隧道项（空数组兜底）。
func adminNodeTunnelsFor(tunnels []store.Tunnel, nodeID string) []adminTunnelCore {
	out := make([]adminTunnelCore, 0)
	for _, t := range tunnels {
		if t.NodeID != nodeID {
			continue
		}
		out = append(out, adminTunnelCore{
			tunnelCore: tunnelCoreFor(t),
			User:       t.Proxy.User, ClientID: t.Proxy.ClientID, LocalAddr: t.LocalAddr,
		})
	}
	return out
}

func (h *Handler) handlePublicTunnels(w http.ResponseWriter, _ *http.Request) {
	tunnels := h.tunnels()
	out := make([]publicTunnelDTO, 0, len(tunnels))
	for _, t := range tunnels {
		out = append(out, publicTunnelDTO{
			NodeID:     nodeIDPtr(t.NodeID),
			tunnelCore: tunnelCoreFor(t),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":     h.now().Unix(),
		"tunnels": out,
	})
}

// sendTunnels 发送 tunnels 事件（公开/管理各自裁剪版）。
func (h *Handler) sendTunnels(w http.ResponseWriter, admin bool) bool {
	tunnels := h.tunnels()
	if admin {
		out := make([]adminTunnelDTO, 0, len(tunnels))
		for _, t := range tunnels {
			out = append(out, adminTunnelDTO{
				NodeID: nodeIDPtr(t.NodeID),
				adminTunnelCore: adminTunnelCore{
					tunnelCore: tunnelCoreFor(t),
					User:       t.Proxy.User, ClientID: t.Proxy.ClientID, LocalAddr: t.LocalAddr,
				},
			})
		}
		return writeSSE(w, nil, "tunnels", map[string]any{"tunnels": out})
	}
	out := make([]publicTunnelDTO, 0, len(tunnels))
	for _, t := range tunnels {
		out = append(out, publicTunnelDTO{NodeID: nodeIDPtr(t.NodeID), tunnelCore: tunnelCoreFor(t)})
	}
	return writeSSE(w, nil, "tunnels", map[string]any{"tunnels": out})
}

// ---------- FRP 状态事件（仅管理端） ----------

// frpEventDTO 为一条 FRP 状态事件。
type frpEventDTO struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

func (h *Handler) handleAdminNodeEvents(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, ok := h.store.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		v, err := strconv.Atoi(q)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request")
			return
		}
		limit = v
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	evs := h.store.FRPEvents(id, limit)
	out := make([]frpEventDTO, 0, len(evs))
	for _, ev := range evs {
		out = append(out, frpEventDTO{TS: ev.TS, Kind: ev.Kind, Name: ev.Name, Detail: ev.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// ---------- 节点凭据管理（仅管理端；写操作 Origin 校验） ----------

// credentialDTO 为凭据列表项（不含摘要）。
type credentialDTO struct {
	ID        string `json:"id"`
	Comment   string `json:"comment"`
	CreatedAt int64  `json:"created_at"`
}

func (h *Handler) handleAdminCredentialsList(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.nodeAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	infos := h.nodeAuth.List()
	out := make([]credentialDTO, 0, len(infos))
	for _, info := range infos {
		out = append(out, credentialDTO{ID: info.ID, Comment: info.Comment, CreatedAt: info.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

func (h *Handler) handleAdminCredentialsCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.originAllowedForWrite(w, r) {
		return
	}
	if h.nodeAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		ID      string `json:"id"`
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	token, err := h.nodeAuth.Add(body.ID, body.Comment)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentialID):
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	case errors.Is(err, auth.ErrCredentialConflict):
		writeError(w, http.StatusConflict, "conflict")
		return
	case err != nil:
		log.Printf("api: 凭据创建失败：%v", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	// 明文 token 仅在本响应返回一次；服务端只落盘摘要。
	writeJSON(w, http.StatusOK, map[string]string{"id": body.ID, "token": token})
}

func (h *Handler) handleAdminCredentialsDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.originAllowedForWrite(w, r) {
		return
	}
	if h.nodeAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	found, err := h.nodeAuth.Delete(r.PathValue("id"))
	if err != nil {
		log.Printf("api: 凭据删除失败：%v", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- 数据库备份（仅管理端） ----------

func (h *Handler) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	tmp, err := os.CreateTemp("", "frp-monitor-backup-*.db")
	if err != nil {
		log.Printf("api: 备份临时文件创建失败：%v", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	// VACUUM INTO 要求目标文件不存在；发送后删除临时文件。
	_ = os.Remove(tmpPath)
	defer os.Remove(tmpPath)

	if err := h.store.BackupDB(tmpPath); err != nil {
		if errors.Is(err, store.ErrNoPersistence) {
			writeError(w, http.StatusConflict, "no_data_dir")
			return
		}
		log.Printf("api: 备份失败：%v", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	name := fmt.Sprintf("frp-monitor-backup-%s.db", h.now().UTC().Format("20060102T150405Z"))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	http.ServeFile(w, r, tmpPath)
}
