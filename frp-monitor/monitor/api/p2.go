// SPDX-License-Identifier: Apache-2.0

// P2 API：探测任务管理、探测/流量 DTO、历史趋势与流量日统计。
// DTO 契约为 web 子代理的固定接口；大整数一律十进制字符串。
package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// ---------- DTO ----------

// publicProbeDTO 为公开探测项：无 target（可能含敏感主机）。
type publicProbeDTO struct {
	ID          string   `json:"id"`
	LastLatency *int64   `json:"last_latency_ms"`
	FailRate    *float64 `json:"fail_rate"`
	Samples     int      `json:"samples"`
}

// adminProbeDTO 为管理探测项：额外含 target。
type adminProbeDTO struct {
	ID          string   `json:"id"`
	Target      string   `json:"target"`
	LastLatency *int64   `json:"last_latency_ms"`
	FailRate    *float64 `json:"fail_rate"`
	Samples     int      `json:"samples"`
}

// trafficDTO 为累计流量（日归属 UTC、按服务端接收时间；十进制字符串）。
type trafficDTO struct {
	TodayRX string `json:"today_rx_bytes"`
	TodayTX string `json:"today_tx_bytes"`
	TotalRX string `json:"total_rx_bytes"`
	TotalTX string `json:"total_tx_bytes"`
}

// metricsSeriesDTO 为历史趋势序列；断线缺口为 null，字节为十进制字符串。
type metricsSeriesDTO struct {
	CPU           []any `json:"cpu"`
	Load1         []any `json:"load1"`
	Load5         []any `json:"load5"`
	Load15        []any `json:"load15"`
	MemUsedBytes  []any `json:"mem_used_bytes"`
	SwapUsedBytes []any `json:"swap_used_bytes"`
	DiskUsedBytes []any `json:"disk_used_bytes"`
	NetRXBps      []any `json:"net_rx_bps"`
	NetTXBps      []any `json:"net_tx_bps"`
	TCP           []any `json:"tcp"`
	UDP           []any `json:"udp"`
	Procs         []any `json:"procs"`
}

// metricsHistoryDTO 为历史趋势响应。
type metricsHistoryDTO struct {
	Enabled bool             `json:"enabled"`
	Range   string           `json:"range"`
	T0      int64            `json:"t0"`
	Step    int64            `json:"step"`
	Series  metricsSeriesDTO `json:"series"`
}

// trafficDailyDTO 为日统计行。
type trafficDailyDTO struct {
	Day string `json:"day"`
	RX  string `json:"rx_bytes"`
	TX  string `json:"tx_bytes"`
}

// ---------- DTO 构造 ----------

// nullF 把 sql.NullFloat64 转为 *float64（NULL → nil）。
func nullF(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// originAllowedForWrite 校验管理写操作的 Origin（CSRF 防护），失败写 403。
func (h *Handler) originAllowedForWrite(w http.ResponseWriter, r *http.Request) bool {
	if !auth.OriginAllowed(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

func publicProbesFor(stats []store.ProbeStat) []publicProbeDTO {
	out := make([]publicProbeDTO, 0, len(stats))
	for _, ps := range stats {
		out = append(out, publicProbeDTO{
			ID: ps.ID, LastLatency: ps.LastLatency, FailRate: ps.FailRate, Samples: ps.Samples,
		})
	}
	return out
}

func adminProbesFor(stats []store.ProbeStat) []adminProbeDTO {
	out := make([]adminProbeDTO, 0, len(stats))
	for _, ps := range stats {
		out = append(out, adminProbeDTO{
			ID: ps.ID, Target: ps.Target, LastLatency: ps.LastLatency, FailRate: ps.FailRate, Samples: ps.Samples,
		})
	}
	return out
}

func trafficFor(st *store.Store, nodeID string, now time.Time) *trafficDTO {
	v, ok := st.Traffic(nodeID, now)
	if !ok {
		return nil
	}
	return &trafficDTO{
		TodayRX: strconv.FormatUint(v.TodayRX, 10),
		TodayTX: strconv.FormatUint(v.TodayTX, 10),
		TotalRX: strconv.FormatUint(v.TotalRX, 10),
		TotalTX: strconv.FormatUint(v.TotalTX, 10),
	}
}

// ---------- 探测任务管理（仅管理端） ----------

// probeTasksDTO 为 GET 响应；tasks 恒为数组（可为空）。
type probeTasksDTO struct {
	Version uint64              `json:"version"`
	Tasks   []protocol.PingTask `json:"tasks"`
}

func (h *Handler) handleAdminProbeTasksGet(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, ok := h.store.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	version, tasks := h.store.ProbeTasks(id)
	writeJSON(w, http.StatusOK, probeTasksDTO{Version: version, Tasks: tasks})
}

func (h *Handler) handleAdminProbeTasksPut(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.originAllowedForWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, ok := h.store.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		Tasks []protocol.PingTask `json:"tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	version, err := h.store.SetProbeTasks(id, body.Tasks)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

// ---------- 历史趋势（公开与管理同结构） ----------

// parseMetricsRange 解析 range 参数，返回步长与总跨度（秒）。
// 固定点数：1h→60（step 60s），6h→72、24h→288（step 300s），7d→336（step 1800s）。
func parseMetricsRange(r string) (step, span int64, ok bool) {
	switch r {
	case "1h":
		return 60, 3600, true
	case "6h":
		return 300, 6 * 3600, true
	case "24h":
		return 300, 24 * 3600, true
	case "7d":
		return 1800, 7 * 24 * 3600, true
	}
	return 0, 0, false
}

func (h *Handler) handleNodeMetrics(w http.ResponseWriter, r *http.Request, admin bool) {
	if admin && !h.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, ok := h.store.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	step, span, ok := parseMetricsRange(r.URL.Query().Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !h.store.HistoryEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	now := h.now().Unix()
	n := int(span / step)
	// t0 对齐到 step 边界，最后一个窗口覆盖当前时刻。
	end := now - now%step
	t0 := end - int64(n-1)*step
	rows, err := h.store.MetricsRange(id, t0, t0+span)
	if err != nil {
		log.Printf("api: 节点 %s 历史查询失败：%v", id, err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, buildMetricsHistory(r.URL.Query().Get("range"), t0, step, n, rows))
}

// metricsBucket 为一个 step 窗口的加权累加（权重 = 分钟样本数）。
type metricsBucket struct {
	cpu, load1, load5, load15 wAcc
	mem, swap, disk           wAcc
	netRX, netTX              wAcc
	tcp, udp, procs           wAcc
}

type wAcc struct {
	sum float64
	w   int64
}

func (a *wAcc) add(v *float64, w int64) {
	if v == nil {
		return
	}
	a.sum += *v * float64(w)
	a.w += w
}

func (a wAcc) value() any {
	if a.w == 0 {
		return nil
	}
	return a.sum / float64(a.w)
}

// bytesValue 为字节字段输出：十进制字符串（四舍五入到整数）。
func (a wAcc) bytesValue() any {
	if a.w == 0 {
		return nil
	}
	return strconv.FormatUint(uint64(a.sum/float64(a.w)+0.5), 10)
}

func buildMetricsHistory(rng string, t0, step int64, n int, rows []store.MetricRow) metricsHistoryDTO {
	buckets := make([]metricsBucket, n)
	for i := range rows {
		row := &rows[i]
		idx := int((row.TsMin - t0) / step)
		if idx < 0 || idx >= n {
			continue
		}
		b := &buckets[idx]
		w := row.Samples
		b.cpu.add(nullF(row.CPUAvg), w)
		b.load1.add(nullF(row.Load1Avg), w)
		b.load5.add(nullF(row.Load5Avg), w)
		b.load15.add(nullF(row.Load15Avg), w)
		b.mem.add(nullF(row.MemUsedAvg), w)
		b.swap.add(nullF(row.SwapUsedAvg), w)
		b.disk.add(nullF(row.DiskUsedAvg), w)
		b.netRX.add(nullF(row.NetRXAvg), w)
		b.netTX.add(nullF(row.NetTXAvg), w)
		b.tcp.add(nullF(row.TCPAvg), w)
		b.udp.add(nullF(row.UDPAvg), w)
		b.procs.add(nullF(row.ProcsAvg), w)
	}
	s := metricsSeriesDTO{
		CPU: make([]any, n), Load1: make([]any, n), Load5: make([]any, n), Load15: make([]any, n),
		MemUsedBytes: make([]any, n), SwapUsedBytes: make([]any, n), DiskUsedBytes: make([]any, n),
		NetRXBps: make([]any, n), NetTXBps: make([]any, n),
		TCP: make([]any, n), UDP: make([]any, n), Procs: make([]any, n),
	}
	for i := range buckets {
		b := &buckets[i]
		s.CPU[i] = b.cpu.value()
		s.Load1[i] = b.load1.value()
		s.Load5[i] = b.load5.value()
		s.Load15[i] = b.load15.value()
		s.MemUsedBytes[i] = b.mem.bytesValue()
		s.SwapUsedBytes[i] = b.swap.bytesValue()
		s.DiskUsedBytes[i] = b.disk.bytesValue()
		s.NetRXBps[i] = b.netRX.value()
		s.NetTXBps[i] = b.netTX.value()
		s.TCP[i] = b.tcp.value()
		s.UDP[i] = b.udp.value()
		s.Procs[i] = b.procs.value()
	}
	return metricsHistoryDTO{Enabled: true, Range: rng, T0: t0, Step: step, Series: s}
}

// ---------- 流量日统计（仅管理端） ----------

func (h *Handler) handleAdminTrafficDaily(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, ok := h.store.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	days := 7
	if q := r.URL.Query().Get("days"); q != "" {
		v, err := strconv.Atoi(q)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request")
			return
		}
		days = v
	}
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}
	rows := h.store.TrafficDaily(id, days, h.now())
	out := make([]trafficDailyDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, trafficDailyDTO{
			Day: row.Day,
			RX:  strconv.FormatUint(row.RX, 10),
			TX:  strconv.FormatUint(row.TX, 10),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": out})
}
