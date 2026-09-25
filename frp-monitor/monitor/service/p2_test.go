// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// pickPort 找一个可用端口并启动一次监控服务。
func startOnFreePort(t *testing.T, ctx context.Context, cfg Config) int {
	t.Helper()
	for port := 17500; port < 17550; port++ {
		cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
		if err := Start(ctx, cfg, nil); err == nil {
			return port
		}
	}
	t.Fatal("无可用端口")
	return 0
}

func adminLogin(t *testing.T, baseURL, password string) string {
	t.Helper()
	body := fmt.Sprintf(`{"password":%q}`, password)
	resp, err := http.Post(baseURL+"/api/admin/v1/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录应 200，得到 %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "fm_admin" {
			return c.Value
		}
	}
	t.Fatal("应下发 fm_admin Cookie")
	return ""
}

func getJSON(t *testing.T, url, cookie string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", "fm_admin="+cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 应 200，得到 %d：%s", url, resp.StatusCode, b)
	}
	return string(b)
}

func waitPortFree(t *testing.T, cfg Config, port int) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		ctx2, cancel2 := context.WithCancel(context.Background())
		cfg2 := cfg
		cfg2.Addr = fmt.Sprintf("127.0.0.1:%d", port)
		cfg2.DataDir = "" // 探测用，不再打开库
		err := Start(ctx2, cfg2, nil)
		cancel2()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("6 秒内未释放端口：%v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestPersistenceAcrossRestart 验证：写库 → 重建 service → 节点离线但
// facts 在、探测任务与流量累计在、历史可用；last_seen 不复活在线。
func TestPersistenceAcrossRestart(t *testing.T) {
	creds, token := writeCreds(t)
	dataDir := t.TempDir()
	sum := sha256.Sum256([]byte("s3cret"))
	cfg := Config{
		Enable: true, CredentialsFile: creds, DataDir: dataDir,
		AdminPasswordHash: hex.EncodeToString(sum[:]),
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	port := startOnFreePort(t, ctx1, cfg)
	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	// agent：hello + 一个带 facts/metrics/计数器的 report
	c, _, err := websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://127.0.0.1:%d/agent/v1/ws", port),
		http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	mkReq := func(id uint64, method string, params any) []byte {
		req, err := protocol.NewRequest(&id, method, params)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(req)
		return b
	}
	if err := c.WriteMessage(websocket.TextMessage, mkReq(1, protocol.MethodHello, &protocol.HelloParams{
		SchemaVersion: protocol.SchemaVersion, AgentVersion: "0.1.0-dev", FRPVersion: "0.71.0",
		SessionID: "sess-1", ReportInterval: 1, SentAt: time.Now().Unix(),
		Capabilities: []string{"ping"},
	})); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatalf("hello 应答失败：%v", err)
	}
	now := time.Now()
	report := &protocol.ReportParams{
		SessionID: "sess-1", Sequence: 1, SentAt: now.Unix(),
		Facts: &metrics.Facts{
			Hostname: "node-host-1", OS: "linux", Kernel: "6.1", Arch: "amd64",
			CPUCores: 4, AgentVersion: "0.1.0-dev",
		},
		Metrics: &metrics.Metrics{
			CollectedAt: now.Unix(), CPU: 12.5, MemUsed: 123456789, MemTotal: 1 << 30,
			NetRXTotal: 1000, NetTXTotal: 2000, BootID: "boot-a", Iface: "eth0",
		},
	}
	if err := c.WriteMessage(websocket.TextMessage, mkReq(2, protocol.MethodReport, report)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatalf("report 应答失败：%v", err)
	}
	// 第二个 report：流量差分（+300/+500）
	report.Sequence = 2
	report.Facts = nil
	report.Metrics.NetRXTotal = 1300
	report.Metrics.NetTXTotal = 2500
	if err := c.WriteMessage(websocket.TextMessage, mkReq(3, protocol.MethodReport, report)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatalf("第二个 report 应答失败：%v", err)
	}
	c.Close()

	// 在线时 PUT 探测任务
	cookie := adminLogin(t, baseURL, "s3cret")
	req, _ := http.NewRequest(http.MethodPut, baseURL+"/api/admin/v1/nodes/node-1/probe-tasks",
		strings.NewReader(`{"tasks":[{"id":"t1","target":"example.com:443","interval":30}]}`))
	req.Header.Set("Cookie", "fm_admin="+cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"version":1`) {
		t.Fatalf("PUT probe-tasks 应 200：%d %s", resp.StatusCode, b)
	}

	// 优雅停机
	cancel1()
	waitPortFree(t, cfg, port)

	// 重启：同 DataDir
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := Start(ctx2, cfg, nil); err != nil {
		t.Fatalf("重启失败：%v", err)
	}

	cookie = adminLogin(t, baseURL, "s3cret")

	// 节点离线但 facts 在；last_seen 不复活在线
	body := getJSON(t, baseURL+"/api/admin/v1/nodes/node-1", cookie)
	if !strings.Contains(body, `"online":false`) {
		t.Fatalf("重启后节点应离线：%s", body)
	}
	if !strings.Contains(body, `"hostname":"node-host-1"`) {
		t.Fatalf("重启后 facts 应保留：%s", body)
	}
	if !strings.Contains(body, `"last_seen":`) {
		t.Fatalf("重启后 last_seen 应保留：%s", body)
	}

	// 探测任务恢复（版本不回退）
	body = getJSON(t, baseURL+"/api/admin/v1/nodes/node-1/probe-tasks", cookie)
	if !strings.Contains(body, `"version":1`) || !strings.Contains(body, `"target":"example.com:443"`) {
		t.Fatalf("重启后任务应恢复：%s", body)
	}

	// 流量累计恢复
	if !strings.Contains(body, "example.com:443") {
		t.Fatal(body)
	}
	body = getJSON(t, baseURL+"/api/admin/v1/nodes/node-1", cookie)
	if !strings.Contains(body, `"total_rx_bytes":"300"`) || !strings.Contains(body, `"total_tx_bytes":"500"`) {
		t.Fatalf("重启后累计流量应恢复：%s", body)
	}

	// 历史可用
	body = getJSON(t, baseURL+"/api/admin/v1/nodes/node-1/metrics?range=1h", cookie)
	if !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("重启后历史应可用：%s", body)
	}
}
