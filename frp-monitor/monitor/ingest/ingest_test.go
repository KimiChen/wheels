// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// setup 返回 httptest 服务端、store 与可用 token。
func setup(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.json")
	token := "node-token-1"
	sum := sha256.Sum256([]byte(token))
	body := fmt.Sprintf(`{"nodes":[{"id":"node-1","token_sha256":%q}]}`,
		hex.EncodeToString(sum[:]))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	nodeAuth, err := auth.NewNodeAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	st := store.New()
	srv := httptest.NewServer(NewHandler(nodeAuth, st, "frp-monitor/test"))
	t.Cleanup(srv.Close)
	return srv, st, token
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent/v1/ws"
}

func dial(t *testing.T, srv *httptest.Server, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	return websocket.DefaultDialer.Dial(wsURL(srv), header)
}

func rpcRequest(t *testing.T, id uint64, method string, params any) []byte {
	t.Helper()
	req, err := protocol.NewRequest(&id, method, params)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readResponse(t *testing.T, c *websocket.Conn) protocol.Response {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func helloParams(sessionID string, schema int) *protocol.HelloParams {
	return &protocol.HelloParams{
		SchemaVersion: schema, AgentVersion: "0.1.0-dev", FRPVersion: "0.71.0",
		SessionID: sessionID, ReportInterval: 1, SentAt: time.Now().Unix(),
	}
}

func TestHelloReportOfflineFlow(t *testing.T) {
	srv, st, token := setup(t)

	c, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	defer c.Close()

	// hello
	if err := c.WriteMessage(websocket.TextMessage, rpcRequest(t, 1, protocol.MethodHello, helloParams("sess-1", protocol.SchemaVersion))); err != nil {
		t.Fatal(err)
	}
	helloResp := readResponse(t, c)
	if helloResp.Error != nil {
		t.Fatalf("hello 应成功：%+v", helloResp.Error)
	}
	var hr protocol.HelloResult
	if err := json.Unmarshal(helloResp.Result, &hr); err != nil {
		t.Fatal(err)
	}
	if hr.SchemaVersion != protocol.SchemaVersion || hr.ServerVersion != "frp-monitor/test" {
		t.Fatalf("hello 结果不符：%+v", hr)
	}

	// 会话已建立
	n, ok := st.Get("node-1")
	if !ok || !n.Online || n.SessionID != "sess-1" {
		t.Fatalf("hello 后节点应在线：%+v ok=%v", n, ok)
	}

	// report
	report := &protocol.ReportParams{
		SessionID: "sess-1", Sequence: 1, SentAt: time.Now().Unix(),
		Facts: &metrics.Facts{
			Hostname: "h1", OS: "linux", Kernel: "6.1", Arch: "amd64", CPUCores: 4,
			AgentVersion: "0.1.0-dev",
		},
		Metrics: &metrics.Metrics{CollectedAt: time.Now().Unix(), CPU: 3.5, NetRX: 1, NetTX: 2},
	}
	if err := c.WriteMessage(websocket.TextMessage, rpcRequest(t, 2, protocol.MethodReport, report)); err != nil {
		t.Fatal(err)
	}
	reportResp := readResponse(t, c)
	if reportResp.Error != nil {
		t.Fatalf("report 应成功：%+v", reportResp.Error)
	}

	n, _ = st.Get("node-1")
	if n.Facts == nil || n.Facts.Hostname != "h1" {
		t.Fatalf("Facts 未入库：%+v", n.Facts)
	}
	if n.Metrics == nil || n.Metrics.CPU != 3.5 {
		t.Fatalf("Metrics 未入库：%+v", n.Metrics)
	}
	if n.MetricsReceivedAt == 0 || n.LastReportAt == 0 {
		t.Fatal("接收时间戳应已记录")
	}
	if n.MetricsStale(time.Now()) {
		t.Fatal("刚收到指标不应过期")
	}

	// 关闭连接 → 离线
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, _ = st.Get("node-1")
		if !n.Online {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("连接关闭后节点应离线")
}

func TestAuthFailure401(t *testing.T) {
	srv, _, _ := setup(t)

	// 无 Authorization
	_, resp, err := dial(t, srv, "")
	if err == nil {
		t.Fatal("无凭据应失败")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，得到 %d", resp.StatusCode)
	}

	// 错误 token
	_, resp, err = dial(t, srv, "wrong-token")
	if err == nil {
		t.Fatal("错误凭据应失败")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误凭据应 401，得到 %d", resp.StatusCode)
	}
}

func TestUnsupportedSchema(t *testing.T) {
	srv, _, token := setup(t)
	c, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 1, protocol.MethodHello, helloParams("sess-9", protocol.SchemaVersion+1))); err != nil {
		t.Fatal(err)
	}
	resp := readResponse(t, c)
	if resp.Error == nil || resp.Error.Code != protocol.CodeUnsupportedSchema {
		t.Fatalf("schema 不支持应回 CodeUnsupportedSchema：%+v", resp.Error)
	}
}

func TestStaleSessionAndOutOfOrder(t *testing.T) {
	srv, st, token := setup(t)
	c, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 1, protocol.MethodHello, helloParams("sess-1", protocol.SchemaVersion))); err != nil {
		t.Fatal(err)
	}
	if resp := readResponse(t, c); resp.Error != nil {
		t.Fatalf("hello 失败：%+v", resp.Error)
	}

	mkReport := func(sessionID string, seq uint64) *protocol.ReportParams {
		return &protocol.ReportParams{
			SessionID: sessionID, Sequence: seq, SentAt: time.Now().Unix(),
			Metrics: &metrics.Metrics{CollectedAt: time.Now().Unix(), CPU: 1},
		}
	}

	// 非当前会话 → CodeStaleSession
	if err := c.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 2, protocol.MethodReport, mkReport("sess-old", 1))); err != nil {
		t.Fatal(err)
	}
	resp := readResponse(t, c)
	if resp.Error == nil || resp.Error.Code != protocol.CodeStaleSession {
		t.Fatalf("旧会话应回 CodeStaleSession：%+v", resp.Error)
	}

	// 乱序 → CodeStaleSession（同会话错误响应，不断开）
	if err := c.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 3, protocol.MethodReport, mkReport("sess-1", 5))); err != nil {
		t.Fatal(err)
	}
	if resp := readResponse(t, c); resp.Error != nil {
		t.Fatalf("seq=5 应成功：%+v", resp.Error)
	}
	if err := c.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 4, protocol.MethodReport, mkReport("sess-1", 3))); err != nil {
		t.Fatal(err)
	}
	resp = readResponse(t, c)
	if resp.Error == nil || resp.Error.Code != protocol.CodeStaleSession {
		t.Fatalf("乱序应回 CodeStaleSession：%+v", resp.Error)
	}
	n, _ := st.Get("node-1")
	if n.LastSequence != 5 {
		t.Fatalf("乱序不得推进序号：%d", n.LastSequence)
	}

	// 未知方法 → CodeMethodNotFound
	if err := c.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 5, "no.such.method", nil)); err != nil {
		t.Fatal(err)
	}
	resp = readResponse(t, c)
	if resp.Error == nil || resp.Error.Code != protocol.CodeMethodNotFound {
		t.Fatalf("未知方法应回 CodeMethodNotFound：%+v", resp.Error)
	}

	// 非法 JSON → CodeParseError
	if err := c.WriteMessage(websocket.TextMessage, []byte("{oops")); err != nil {
		t.Fatal(err)
	}
	resp = readResponse(t, c)
	if resp.Error == nil || resp.Error.Code != protocol.CodeParseError {
		t.Fatalf("非法 JSON 应回 CodeParseError：%+v", resp.Error)
	}

	// 非法 params → CodeInvalidParams
	bad := &protocol.ReportParams{SessionID: "sess-1", Sequence: 99, SentAt: time.Now().Unix()}
	if err := c.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 6, protocol.MethodReport, bad)); err != nil {
		t.Fatal(err)
	}
	resp = readResponse(t, c)
	if resp.Error == nil || resp.Error.Code != protocol.CodeInvalidParams {
		t.Fatalf("空载荷应回 CodeInvalidParams：%+v", resp.Error)
	}
}

// TestSessionReplacementOverWS 验证新 hello 取代旧会话：旧连接被服务端关闭，
// 且旧连接的迟到退出不影响新会话。
func TestSessionReplacementOverWS(t *testing.T) {
	srv, st, token := setup(t)

	c1, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	if err := c1.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 1, protocol.MethodHello, helloParams("sess-1", protocol.SchemaVersion))); err != nil {
		t.Fatal(err)
	}
	if resp := readResponse(t, c1); resp.Error != nil {
		t.Fatalf("hello 失败：%+v", resp.Error)
	}

	c2, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.WriteMessage(websocket.TextMessage,
		rpcRequest(t, 1, protocol.MethodHello, helloParams("sess-2", protocol.SchemaVersion))); err != nil {
		t.Fatal(err)
	}
	if resp := readResponse(t, c2); resp.Error != nil {
		t.Fatalf("第二次 hello 失败：%+v", resp.Error)
	}

	// 旧连接应被服务端关闭
	c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := c1.ReadMessage(); err == nil {
		t.Fatal("旧连接应被关闭")
	}

	// 节点仍在线且为 sess-2
	n, _ := st.Get("node-1")
	if !n.Online || n.SessionID != "sess-2" {
		t.Fatalf("会话取代后应为 sess-2 在线：%+v", n)
	}
}

func TestNoHelloTimeout(t *testing.T) {
	srv, _, token := setup(t)
	c, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 不发 hello，服务端应在 5 秒超时后关闭；这里放宽到 8 秒等待关闭帧
	c.SetReadDeadline(time.Now().Add(8 * time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("hello 超时后连接应被关闭")
	}
}
