// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

func newTestHandler(t *testing.T, password string) (*Handler, *store.Store) {
	t.Helper()
	st := store.New()
	var admin *auth.Admin
	var err error
	if password != "" {
		sum := sha256.Sum256([]byte(password))
		admin, err = auth.NewAdmin(hex.EncodeToString(sum[:]))
	} else {
		admin, err = auth.NewAdmin("")
	}
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(st, admin, nil), st
}

// seedFullNode 写入一个含全部字段（含敏感字段）的节点。
func seedFullNode(t *testing.T, st *store.Store) time.Time {
	t.Helper()
	now := time.Unix(1790380800, 0)
	st.StartSession("node-1", "sess-secret-1", 1, now)
	facts := &metrics.Facts{
		Hostname: "secret-hostname", OS: "linux", Kernel: "6.1.0-secret",
		Arch: "amd64", Virt: "kvm", CPUName: "Secret CPU X", CPUCores: 8,
		AgentVersion: "0.1.0-dev", IPv4: "10.0.0.8", IPv6: "fd00::8",
		MemTotal: 1 << 30, SwapTotal: 1 << 29, DiskTotal: 1 << 40,
	}
	m := &metrics.Metrics{
		CollectedAt: 1790380799, CPU: 42.5, Load: [3]float64{0.1, 0.2, 0.3},
		MemUsed: 12345678901, SwapUsed: 2 << 20, DiskUsed: 987654321098,
		MemTotal: 1 << 30, SwapTotal: 1 << 29, DiskTotal: 1 << 40,
		NetRX: 1024.5, NetTX: 2048.25,
		NetRXTotal: 11234567890123456, NetTXTotal: 21234567890123456,
		BootID: "boot-secret-uuid", Iface: "eth0,eth1",
		Uptime: 86400, TCP: 100, UDP: 20, Procs: 250,
	}
	frp := &protocol.FRPExtension{
		ClientID: "client-secret-1", FRPVersion: "0.71.0", ControlConnected: true,
		Proxies: []protocol.ProxyInfo{
			{Name: "ssh", Type: "tcp", LocalAddr: "127.0.0.1:22", Enabled: true, Status: "running"},
		},
	}
	if err := st.Report("node-1", "sess-secret-1", 1, facts, m, frp, now); err != nil {
		t.Fatal(err)
	}
	st.UpdateFRPClients([]store.FRPClient{
		{User: "u1", ClientID: "client-secret-1", RunID: "run-1", Version: "0.71.0",
			Online: true, FirstConnectedAt: 1790380000, LastConnectedAt: 1790380700},
	})
	return now
}

func doJSON(t *testing.T, h http.Handler, method, target string,
	headers map[string]string, reqBody string) (*httptest.ResponseRecorder, string) {

	t.Helper()
	var r *http.Request
	if reqBody != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(reqBody))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, rec.Body.String()
}

// 公开 DTO 绝不含的字段名与敏感值。
var forbiddenSubstrings = []string{
	"hostname", "secret-hostname",
	"ipv4", "ipv6", "10.0.0.8", "fd00::8",
	"kernel", "6.1.0-secret", "virt", "cpu_name", "Secret CPU",
	"boot_id", "boot-secret-uuid", "iface", "eth0",
	"local_addr", "127.0.0.1:22",
	"session", "sess-secret-1",
	"client-secret-1", "run-1",
	"11234567890123456", // net_rx_total 不应出现在公开视图
}

func TestPublicNodeRedaction(t *testing.T) {
	h, st := newTestHandler(t, "")
	seedFullNode(t, st)

	for _, target := range []string{
		"/api/public/v1/nodes",
		"/api/public/v1/nodes/node-1",
	} {
		rec, body := doJSON(t, h, http.MethodGet, target, nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应 200，得到 %d：%s", target, rec.Code, body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Fatalf("%s Content-Type 应为 application/json; charset=utf-8，得到 %q", target, ct)
		}
		for _, bad := range forbiddenSubstrings {
			if strings.Contains(body, bad) {
				t.Fatalf("%s 公开响应不应含 %q：%s", target, bad, body)
			}
		}
		// 大整数为十进制字符串
		if !strings.Contains(body, `"mem_used":"12345678901"`) {
			t.Fatalf("%s mem_used 应为字符串：%s", target, body)
		}
		// 浮点速率保留 number
		if !strings.Contains(body, `"net_rx":1024.5`) {
			t.Fatalf("%s net_rx 应为 number：%s", target, body)
		}
		// 公开 proxies 无 local_addr 但有其余字段
		if !strings.Contains(body, `"proxies":[{"name":"ssh","type":"tcp","enabled":true,"status":"running"}]`) {
			t.Fatalf("%s 公开 proxies 形态不符：%s", target, body)
		}
	}
}

func TestPublicNodeNotFound(t *testing.T) {
	h, _ := newTestHandler(t, "")
	rec, body := doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/ghost", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，得到 %d", rec.Code)
	}
	if !strings.Contains(body, `{"error":"not_found"}`) {
		t.Fatalf("错误体不符：%s", body)
	}
}

func TestOverview(t *testing.T) {
	h, st := newTestHandler(t, "")
	now := seedFullNode(t, st)

	// 离线节点（有会话历史但已下线）
	st.StartSession("node-2", "sess-2", 1, now)
	st.EndSession("node-2", "sess-2")

	rec, body := doJSON(t, h, http.MethodGet, "/api/public/v1/overview", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", rec.Code)
	}
	var o overviewDTO
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatal(err)
	}
	if o.NodesTotal != 2 || o.NodesOnline != 1 || o.NodesOffline != 1 {
		t.Fatalf("节点计数不符：%+v", o)
	}
	if o.FRPClientsOnline != 1 {
		t.Fatalf("frp_clients_online 应为 1：%+v", o)
	}
	if o.NetRXBps != 1024.5 || o.NetTXBps != 2048.25 {
		t.Fatalf("net 应为在线节点有效速率之和：%+v", o)
	}
}

func TestQualityUnknownRendersNull(t *testing.T) {
	h, st := newTestHandler(t, "")
	now := time.Unix(1790380800, 0)
	st.StartSession("node-q", "sess-q", 1, now)
	m := &metrics.Metrics{
		CollectedAt: 1790380799, CPU: 10, NetRX: 5, NetTX: 6,
		MemUsed: 100, MemTotal: 200,
		Quality: &metrics.Quality{CPU: metrics.QualityUnknown, NetRate: metrics.QualityUnknown},
	}
	if err := st.Report("node-q", "sess-q", 1, nil, m, nil, now); err != nil {
		t.Fatal(err)
	}
	_, body := doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-q", nil, "")
	if !strings.Contains(body, `"cpu":null`) || !strings.Contains(body, `"net_rx":null`) {
		t.Fatalf("unknown 质量应输出 null：%s", body)
	}
	if !strings.Contains(body, `"mem_used":"100"`) {
		t.Fatalf("有效组应正常输出：%s", body)
	}
	// 无 FRP 扩展：frp_control_connected 与 proxies 为 null
	if !strings.Contains(body, `"frp_control_connected":null`) {
		t.Fatalf("无 FRP 扩展应为 null：%s", body)
	}
}

// loginCookie 登录并返回 Cookie。
func loginCookie(t *testing.T, h http.Handler, password, host string) *http.Cookie {
	t.Helper()
	rec, _ := doJSON(t, h, http.MethodPost, "http://"+host+"/api/admin/v1/login",
		nil, `{"password":`+jsonString(password)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录应 200，得到 %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.AdminCookieName {
		t.Fatalf("应下发 %s Cookie：%v", auth.AdminCookieName, cookies)
	}
	return cookies[0]
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestAdminAuthFlow(t *testing.T) {
	h, st := newTestHandler(t, "s3cret")
	seedFullNode(t, st)

	// 未认证 → 401
	rec, body := doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes", nil, "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(body, `{"error":"unauthorized"}`) {
		t.Fatalf("未认证应 401 unauthorized：%d %s", rec.Code, body)
	}
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/session", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session 未认证应 401，得到 %d", rec.Code)
	}

	// 错误密码 → 401
	rec, _ = doJSON(t, h, http.MethodPost, "/api/admin/v1/login", nil, `{"password":"wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，得到 %d", rec.Code)
	}

	// 正确密码 → 200 + Cookie
	cookie := loginCookie(t, h, "s3cret", "example.com")

	rec, body = doJSON(t, h, http.MethodGet, "/api/admin/v1/session",
		map[string]string{"Cookie": cookie.String()}, "")
	if rec.Code != http.StatusOK || !strings.Contains(body, `{"ok":true}`) {
		t.Fatalf("session 应 200 ok：%d %s", rec.Code, body)
	}

	// 管理节点详情：含 facts、local_addr、对账结果
	rec, body = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1",
		map[string]string{"Cookie": cookie.String()}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("管理详情应 200，得到 %d：%s", rec.Code, body)
	}
	for _, want := range []string{
		`"hostname":"secret-hostname"`, `"local_addr":"127.0.0.1:22"`,
		`"boot_id":"boot-secret-uuid"`, `"iface":"eth0,eth1"`,
		`"net_rx_total":"11234567890123456"`, `"frp_client_id":"client-secret-1"`,
		`"run_id":"run-1"`, `"cpu_cores":"8"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("管理详情应含 %s：%s", want, body)
		}
	}

	// 登出
	rec, _ = doJSON(t, h, http.MethodPost, "/api/admin/v1/logout", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("登出应 200，得到 %d", rec.Code)
	}
}

func TestAdminLoginOriginCheck(t *testing.T) {
	h, _ := newTestHandler(t, "s3cret")

	// 跨源写操作 → 403
	rec, _ := doJSON(t, h, http.MethodPost, "http://example.com/api/admin/v1/login",
		map[string]string{"Origin": "http://evil.com"}, `{"password":"s3cret"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨源 login 应 403，得到 %d", rec.Code)
	}
	rec, _ = doJSON(t, h, http.MethodPost, "http://example.com/api/admin/v1/logout",
		map[string]string{"Origin": "http://evil.com"}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨源 logout 应 403，得到 %d", rec.Code)
	}
	// 同源 → 放行
	rec, _ = doJSON(t, h, http.MethodPost, "http://example.com/api/admin/v1/login",
		map[string]string{"Origin": "http://example.com"}, `{"password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("同源 login 应 200，得到 %d", rec.Code)
	}
}

func TestAdminDisabled(t *testing.T) {
	h, _ := newTestHandler(t, "")
	rec, _ := doJSON(t, h, http.MethodPost, "/api/admin/v1/login", nil, `{"password":"x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("管理端禁用时应 401，得到 %d", rec.Code)
	}
}

func TestStaticUnavailableNoPanic(t *testing.T) {
	h, _ := newTestHandler(t, "")
	for _, target := range []string{"/", "/node.html", "/admin.html", "/assets/app.css", "/src/api.js"} {
		rec, _ := doJSON(t, h, http.MethodGet, target, nil, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s static 为空时应 503，得到 %d", target, rec.Code)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h, _ := newTestHandler(t, "")
	rec, _ := doJSON(t, h, http.MethodGet, "/api/public/v1/overview", nil, "")
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "connect-src 'self'") {
		t.Fatalf("CSP 不符：%q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff 缺失：%q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options 缺失：%q", got)
	}
}

func TestSSESnapshot(t *testing.T) {
	h, st := newTestHandler(t, "")
	seedFullNode(t, st)

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events/public")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type 不符：%q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	deadline := time.After(5 * time.Second)
	got := make(chan string, 1)
	go func() {
		var sb strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			sb.WriteString(line)
			if line == "\n" {
				got <- sb.String()
				return
			}
		}
	}()
	select {
	case event := <-got:
		if !strings.HasPrefix(event, "event: snapshot\n") {
			t.Fatalf("首事件应为 snapshot：%q", event)
		}
		if !strings.Contains(event, `"overview":`) || !strings.Contains(event, `"nodes":`) {
			t.Fatalf("snapshot 应含 overview 与 nodes：%q", event)
		}
		for _, bad := range forbiddenSubstrings {
			if strings.Contains(event, bad) {
				t.Fatalf("公开 SSE 不应含 %q：%q", bad, event)
			}
		}
	case <-deadline:
		t.Fatal("5 秒内未收到 snapshot 事件")
	}
}

func TestSSEAdminRequiresAuth(t *testing.T) {
	h, _ := newTestHandler(t, "s3cret")
	rec, body := doJSON(t, h, http.MethodGet, "/events/admin", nil, "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(body, `"unauthorized"`) {
		t.Fatalf("未认证 admin SSE 应 401：%d %s", rec.Code, body)
	}
}
