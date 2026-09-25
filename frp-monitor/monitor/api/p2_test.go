// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// newTestHandlerWithDB 创建带 SQLite 持久化的 handler（DataDir 模式）。
func newTestHandlerWithDB(t *testing.T, password string) (*Handler, *store.Store) {
	t.Helper()
	db, err := store.OpenDB(t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	st := store.New()
	st.AttachDB(db)
	t.Cleanup(st.Close)
	sum := sha256.Sum256([]byte(password))
	admin, err := auth.NewAdmin(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(st, admin, nil), st
}

// seedProbeAndTraffic 写入一个含探测统计与流量累计的节点。
func seedProbeAndTraffic(t *testing.T, st *store.Store, now time.Time) {
	t.Helper()
	st.StartSession("node-1", "sess-1", 1, now)
	if _, err := st.SetProbeTasks("node-1", []protocol.PingTask{
		{ID: "t1", Target: "example.com:443", Interval: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordPingResults("node-1", "sess-1", 1,
		[]protocol.PingResult{{TaskID: "t1", LatencyMS: 23}}, now); err != nil {
		t.Fatal(err)
	}
	mk := func(seq uint64, rx, tx uint64) *metrics.Metrics {
		return &metrics.Metrics{
			CollectedAt: now.Unix(), CPU: 1,
			NetRXTotal: rx, NetTXTotal: tx, BootID: "boot-a", Iface: "eth0",
		}
	}
	if err := st.Report("node-1", "sess-1", 1, nil, mk(1, 1000, 2000), nil, now); err != nil {
		t.Fatal(err)
	}
	if err := st.Report("node-1", "sess-1", 2, nil, mk(2, 1300, 2500), nil, now); err != nil {
		t.Fatal(err)
	}
}

func TestNodeDTOProbesAndTraffic(t *testing.T) {
	h, st := newTestHandler(t, "s3cret")
	now := time.Unix(1790380800, 0) // 2026-09-26 UTC
	h.now = func() time.Time { return now }
	seedProbeAndTraffic(t, st, now)

	// 公开 DTO：probes 无 target，traffic 为十进制字符串
	_, body := doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-1", nil, "")
	if !strings.Contains(body, `"probes":[{"id":"t1","last_latency_ms":23,"fail_rate":0,"samples":1}]`) {
		t.Fatalf("公开 probes 形态不符：%s", body)
	}
	if strings.Contains(body, "example.com") || strings.Contains(body, "target") {
		t.Fatalf("公开 DTO 不得含 target：%s", body)
	}
	if !strings.Contains(body, `"traffic":{"today_rx_bytes":"300","today_tx_bytes":"500","total_rx_bytes":"300","total_tx_bytes":"500"}`) {
		t.Fatalf("traffic DTO 不符：%s", body)
	}

	// 管理 DTO：probes 含 target
	cookie := loginCookie(t, h, "s3cret", "example.com")
	_, body = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1",
		map[string]string{"Cookie": cookie.String()}, "")
	if !strings.Contains(body, `"id":"t1","target":"example.com:443"`) {
		t.Fatalf("管理 probes 应含 target：%s", body)
	}

	// 无任务/无流量的节点：probes 为空数组、traffic 为 null
	st.StartSession("node-2", "sess-2", 1, now)
	_, body = doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-2", nil, "")
	if !strings.Contains(body, `"probes":[]`) {
		t.Fatalf("无任务应为空数组：%s", body)
	}
	if !strings.Contains(body, `"traffic":null`) {
		t.Fatalf("无流量应为 null：%s", body)
	}
}

func TestProbeTasksAPI(t *testing.T) {
	h, st := newTestHandler(t, "s3cret")
	st.StartSession("node-1", "sess-1", 1, time.Now())
	cookie := loginCookie(t, h, "s3cret", "example.com")
	authHeader := map[string]string{"Cookie": cookie.String()}

	// 未认证 → 401
	rec, _ := doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/probe-tasks", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，得到 %d", rec.Code)
	}

	// 未知节点 → 404
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/ghost/probe-tasks", authHeader, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知节点应 404，得到 %d", rec.Code)
	}
	rec, _ = doJSON(t, h, http.MethodPut, "/api/admin/v1/nodes/ghost/probe-tasks", authHeader, `{"tasks":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知节点 PUT 应 404，得到 %d", rec.Code)
	}

	// 初始为空
	rec, body := doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/probe-tasks", authHeader, "")
	if rec.Code != http.StatusOK || !strings.Contains(body, `"version":0,"tasks":[]`) {
		t.Fatalf("初始应为空：%d %s", rec.Code, body)
	}

	// 跨源写 → 403
	rec, _ = doJSON(t, h, http.MethodPut, "http://example.com/api/admin/v1/nodes/node-1/probe-tasks",
		map[string]string{"Cookie": cookie.String(), "Origin": "http://evil.com"}, `{"tasks":[]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨源 PUT 应 403，得到 %d", rec.Code)
	}

	// 非法任务 → 400
	rec, _ = doJSON(t, h, http.MethodPut, "/api/admin/v1/nodes/node-1/probe-tasks", authHeader,
		`{"tasks":[{"id":"t1","target":"example.com:443","interval":1}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("interval 越界应 400，得到 %d", rec.Code)
	}

	// 正常 PUT → 200 {"ok":true,"version":N}
	rec, body = doJSON(t, h, http.MethodPut, "/api/admin/v1/nodes/node-1/probe-tasks", authHeader,
		`{"tasks":[{"id":"t1","target":"example.com:443","interval":30}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(body, `"ok":true,"version":1`) {
		t.Fatalf("PUT 应 200 且版本为 1：%d %s", rec.Code, body)
	}

	// GET 回读
	rec, body = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/probe-tasks", authHeader, "")
	if rec.Code != http.StatusOK ||
		!strings.Contains(body, `"version":1,"tasks":[{"id":"t1","target":"example.com:443","interval":30}]`) {
		t.Fatalf("GET 回读不符：%d %s", rec.Code, body)
	}
}

func TestMetricsHistoryDisabled(t *testing.T) {
	h, st := newTestHandler(t, "")
	st.StartSession("node-1", "sess-1", 1, time.Now())

	rec, body := doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-1/metrics?range=1h", nil, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(body) != `{"enabled":false}` {
		t.Fatalf("无 DB 应返回 enabled:false：%d %s", rec.Code, body)
	}

	// 未知 range → 400（先校验参数再判断 enabled，便于调用方发现拼写错误）
	rec, _ = doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-1/metrics?range=30d", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知 range 应 400，得到 %d", rec.Code)
	}

	// 未知节点 → 404
	rec, _ = doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/ghost/metrics?range=1h", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知节点应 404，得到 %d", rec.Code)
	}
}

func TestMetricsHistory(t *testing.T) {
	h, st := newTestHandlerWithDB(t, "s3cret")
	// 固定 now 在 step 边界上，t0 对齐后最后一个窗口覆盖 now。
	now := time.Unix(1790380800, 0) // 60 整除
	h.now = func() time.Time { return now }

	// range=1h：step=60、60 点，t0 = now - 59*60 = 1790377260。
	// 第 5 个桶覆盖 [1790377560, 1790377620)。
	minute := int64(1790377560)
	st.StartSession("node-1", "sess-1", 1, time.Unix(minute, 0))
	mk := func(seq uint64, sec int64, cpu float64, mem uint64) *metrics.Metrics {
		return &metrics.Metrics{
			CollectedAt: minute + sec, CPU: cpu,
			Load: [3]float64{1, 2, 3}, MemUsed: mem,
			NetRX: 100, NetTX: 200, TCP: 10, UDP: 5, Procs: 3,
		}
	}
	if err := st.Report("node-1", "sess-1", 1, nil, mk(1, 10, 10, 100), nil, time.Unix(minute+10, 0)); err != nil {
		t.Fatal(err)
	}
	if err := st.Report("node-1", "sess-1", 2, nil, mk(2, 20, 30, 200), nil, time.Unix(minute+20, 0)); err != nil {
		t.Fatal(err)
	}
	st.FlushMetrics()

	rec, body := doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-1/metrics?range=1h", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d：%s", rec.Code, body)
	}
	var resp struct {
		Enabled bool   `json:"enabled"`
		Range   string `json:"range"`
		T0      int64  `json:"t0"`
		Step    int64  `json:"step"`
		Series  struct {
			CPU          []any `json:"cpu"`
			MemUsedBytes []any `json:"mem_used_bytes"`
			NetRXBps     []any `json:"net_rx_bps"`
			TCP          []any `json:"tcp"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enabled || resp.Range != "1h" || resp.Step != 60 {
		t.Fatalf("响应头不符：%+v", resp)
	}
	if resp.T0 != 1790377260 {
		t.Fatalf("t0 应为 1790377260：%d", resp.T0)
	}
	if len(resp.Series.CPU) != 60 || len(resp.Series.MemUsedBytes) != 60 {
		t.Fatalf("1h 应为 60 点：%d/%d", len(resp.Series.CPU), len(resp.Series.MemUsedBytes))
	}
	if resp.Series.CPU[5] != 20.0 {
		t.Fatalf("第 5 桶 cpu 均值应为 20：%v", resp.Series.CPU[5])
	}
	if resp.Series.MemUsedBytes[5] != "150" {
		t.Fatalf("第 5 桶 mem_used_bytes 应为 \"150\"：%v", resp.Series.MemUsedBytes[5])
	}
	if resp.Series.NetRXBps[5] != 100.0 || resp.Series.TCP[5] != 10.0 {
		t.Fatalf("第 5 桶速率/连接数不符：%v %v", resp.Series.NetRXBps[5], resp.Series.TCP[5])
	}
	// 断线缺口为 null
	if resp.Series.CPU[4] != nil || resp.Series.CPU[6] != nil || resp.Series.MemUsedBytes[59] != nil {
		t.Fatalf("缺口应为 null：%v %v %v", resp.Series.CPU[4], resp.Series.CPU[6], resp.Series.MemUsedBytes[59])
	}

	// 固定点数：6h→72、24h→288、7d→336
	for rng, want := range map[string]int{"6h": 72, "24h": 288, "7d": 336} {
		rec, body = doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-1/metrics?range="+rng, nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应 200：%d", rng, rec.Code)
		}
		var r2 struct {
			Step   int64 `json:"step"`
			Series struct {
				CPU []any `json:"cpu"`
			} `json:"series"`
		}
		if err := json.Unmarshal([]byte(body), &r2); err != nil {
			t.Fatal(err)
		}
		if len(r2.Series.CPU) != want {
			t.Fatalf("%s 应为 %d 点，得到 %d", rng, want, len(r2.Series.CPU))
		}
	}

	// admin 侧同结构（鉴权后）
	cookie := loginCookie(t, h, "s3cret", "example.com")
	rec, body = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/metrics?range=1h",
		map[string]string{"Cookie": cookie.String()}, "")
	if rec.Code != http.StatusOK || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("admin 侧应同结构：%d %s", rec.Code, body)
	}
	// admin 侧未认证 → 401
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/metrics?range=1h", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin 侧未认证应 401，得到 %d", rec.Code)
	}
}

func TestTrafficDailyAPI(t *testing.T) {
	h, st := newTestHandler(t, "s3cret")
	now := time.Unix(1790380800, 0) // 2026-09-26 UTC
	h.now = func() time.Time { return now }
	seedProbeAndTraffic(t, st, now)
	cookie := loginCookie(t, h, "s3cret", "example.com")
	authHeader := map[string]string{"Cookie": cookie.String()}

	rec, body := doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/traffic/daily?days=7", authHeader, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", rec.Code)
	}
	if !strings.Contains(body, `"days":[{"day":"2026-09-26","rx_bytes":"300","tx_bytes":"500"}]`) {
		t.Fatalf("日统计不符：%s", body)
	}

	// 未认证 → 401；未知节点 → 404；非法 days → 400
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/traffic/daily", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，得到 %d", rec.Code)
	}
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/ghost/traffic/daily", authHeader, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知节点应 404，得到 %d", rec.Code)
	}
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/traffic/daily?days=abc", authHeader, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 days 应 400，得到 %d", rec.Code)
	}

	// 公开侧无此端点 → 走静态资源（503，static 为 nil）
	rec, _ = doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-1/traffic/daily", nil, "")
	if rec.Code == http.StatusOK {
		t.Fatal("公开侧不应提供 traffic/daily")
	}
}
