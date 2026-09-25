// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
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

	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// newTestHandlerWithCreds 创建带节点凭据文件与管理端密码的 handler。
// 返回 handler、store、凭据文件路径与初始凭据 token（node-1）。
func newTestHandlerWithCreds(t *testing.T, password string) (*Handler, *store.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.json")
	token := "node-token-1"
	sum := sha256.Sum256([]byte(token))
	body := fmt.Sprintf(`{"nodes":[{"id":"node-1","token_sha256":%q,"comment":"初始节点"}]}`,
		hex.EncodeToString(sum[:]))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	nodeAuth, err := auth.NewNodeAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	adminSum := sha256.Sum256([]byte(password))
	admin, err := auth.NewAdmin(hex.EncodeToString(adminSum[:]))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New()
	t.Cleanup(st.Close)
	return NewHandler(st, admin, nodeAuth, nil), st, path, token
}

// seedTunnels 写入一个声明 (u1, c1) 绑定键的节点与两条 proxy 统计
// （一条匹配、一条无节点归属）。
func seedTunnels(t *testing.T, st *store.Store, now time.Time) {
	t.Helper()
	st.StartSession("node-1", "sess-1", 1, now)
	frp := &protocol.FRPExtension{
		User: "u1", ClientID: "c1", ControlConnected: true,
		Proxies: []protocol.ProxyInfo{
			{Name: "ssh", Type: "tcp", LocalAddr: "127.0.0.1:22", Enabled: true, Status: "running"},
		},
	}
	if err := st.Report("node-1", "sess-1", 1, nil, nil, frp, now); err != nil {
		t.Fatal(err)
	}
	st.UpdateFRPProxies([]store.FRPProxy{
		{Name: "ssh", Type: "tcp", User: "u1", ClientID: "c1", Online: true, CurConns: 3,
			TodayTrafficIn: 123, TodayTrafficOut: 456},
		{Name: "orphan", Type: "tcp", User: "u9", ClientID: "c9", Online: false},
	}, now)
}

func TestPublicTunnels(t *testing.T) {
	h, st := newTestHandler(t, "")
	now := time.Unix(1790380800, 0)
	seedTunnels(t, st, now)

	rec, body := doJSON(t, h, http.MethodGet, "/api/public/v1/tunnels", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d：%s", rec.Code, body)
	}
	if !strings.Contains(body, `"tunnels":[`) {
		t.Fatalf("应含 tunnels 数组：%s", body)
	}
	// 匹配隧道的完整公开形态
	if !strings.Contains(body, `{"node_id":"node-1","name":"ssh","type":"tcp","online":true,`+
		`"cur_conns":3,"today_rx_bytes":"123","today_tx_bytes":"456"}`) {
		t.Fatalf("匹配隧道形态不符：%s", body)
	}
	// 未匹配隧道 node_id 为 null
	if !strings.Contains(body, `{"node_id":null,"name":"orphan","type":"tcp","online":false,`+
		`"cur_conns":0,"today_rx_bytes":"0","today_tx_bytes":"0"}`) {
		t.Fatalf("未匹配隧道 node_id 应为 null：%s", body)
	}
	// 公开 DTO 绝不含 user/client_id/local_addr
	for _, bad := range []string{"local_addr", "127.0.0.1:22", `"user"`, `"client_id"`, `"u1"`, `"c1"`} {
		if strings.Contains(body, bad) {
			t.Fatalf("公开隧道不应含 %s：%s", bad, body)
		}
	}
}

func TestOverviewTunnels(t *testing.T) {
	h, st := newTestHandler(t, "")
	now := time.Unix(1790380800, 0)
	seedTunnels(t, st, now)

	_, body := doJSON(t, h, http.MethodGet, "/api/public/v1/overview", nil, "")
	if !strings.Contains(body, `"tunnels_total":2`) || !strings.Contains(body, `"tunnels_online":1`) {
		t.Fatalf("overview 隧道计数不符：%s", body)
	}
}

func TestNodeTunnelsDTO(t *testing.T) {
	h, st := newTestHandler(t, "s3cret")
	now := time.Unix(1790380800, 0)
	seedTunnels(t, st, now)

	// 公开节点 DTO：tunnels 裁剪版，无 local_addr
	_, body := doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-1", nil, "")
	if !strings.Contains(body, `"tunnels":[{"name":"ssh","type":"tcp","online":true,`+
		`"cur_conns":3,"today_rx_bytes":"123","today_tx_bytes":"456"}]`) {
		t.Fatalf("公开节点 tunnels 不符：%s", body)
	}
	if strings.Contains(body, "local_addr") || strings.Contains(body, "127.0.0.1:22") {
		t.Fatalf("公开节点 tunnels 不应含本地目标：%s", body)
	}

	// 无隧道节点：tunnels 为空数组
	st.StartSession("node-2", "sess-2", 1, now)
	_, body = doJSON(t, h, http.MethodGet, "/api/public/v1/nodes/node-2", nil, "")
	if !strings.Contains(body, `"tunnels":[]`) {
		t.Fatalf("无隧道节点应为空数组：%s", body)
	}

	// 管理节点 DTO：tunnels 项额外含 user/client_id/local_addr
	cookie := loginCookie(t, h, "s3cret", "example.com")
	_, body = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1",
		map[string]string{"Cookie": cookie.String()}, "")
	if !strings.Contains(body, `"tunnels":[{"name":"ssh","type":"tcp","online":true,"cur_conns":3,`+
		`"today_rx_bytes":"123","today_tx_bytes":"456","user":"u1","client_id":"c1",`+
		`"local_addr":"127.0.0.1:22"}]`) {
		t.Fatalf("管理节点 tunnels 不符：%s", body)
	}
}

func TestAdminNodeEvents(t *testing.T) {
	h, st := newTestHandler(t, "s3cret")
	t0 := time.Unix(1790380800, 0)
	seedTunnels(t, st, t0)
	// 两次翻转 → tunnel_offline + tunnel_online
	flip := func(online bool, at time.Time) {
		st.UpdateFRPProxies([]store.FRPProxy{
			{Name: "ssh", Type: "tcp", User: "u1", ClientID: "c1", Online: online},
		}, at)
	}
	flip(false, t0.Add(2*time.Second))
	flip(true, t0.Add(4*time.Second))

	cookie := loginCookie(t, h, "s3cret", "example.com")
	authHeader := map[string]string{"Cookie": cookie.String()}

	// 未认证 → 401
	rec, _ := doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/events", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，得到 %d", rec.Code)
	}
	// 未知节点 → 404
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/ghost/events", authHeader, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知节点应 404，得到 %d", rec.Code)
	}
	// 非法 limit → 400
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/events?limit=abc", authHeader, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 limit 应 400，得到 %d", rec.Code)
	}

	// 倒序：新事件在前
	rec, body := doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/events", authHeader, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d：%s", rec.Code, body)
	}
	var resp struct {
		Events []struct {
			TS   int64  `json:"ts"`
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 2 || resp.Events[0].Kind != "tunnel_online" || resp.Events[1].Kind != "tunnel_offline" {
		t.Fatalf("事件倒序不符：%s", body)
	}
	if resp.Events[0].Name != "ssh" || resp.Events[0].TS != t0.Add(4*time.Second).Unix() {
		t.Fatalf("事件内容不符：%s", body)
	}

	// limit=1 只留最新
	_, body = doJSON(t, h, http.MethodGet, "/api/admin/v1/nodes/node-1/events?limit=1", authHeader, "")
	if !strings.Contains(body, `"events":[{"ts":`) || strings.Contains(body, "tunnel_offline") {
		t.Fatalf("limit=1 应只返回 tunnel_online：%s", body)
	}
}

func TestSSETunnelsEvent(t *testing.T) {
	h, st := newTestHandler(t, "")
	now := time.Unix(1790380800, 0)
	seedTunnels(t, st, now)

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events/public")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	events := make(chan string, 16)
	go func() {
		reader := bufio.NewReader(resp.Body)
		var sb strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\n" {
				events <- sb.String()
				sb.Reset()
				continue
			}
			sb.WriteString(line)
		}
	}()

	// 先收 snapshot
	select {
	case ev := <-events:
		if !strings.HasPrefix(ev, "event: snapshot\n") {
			t.Fatalf("首事件应为 snapshot：%q", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内未收到 snapshot")
	}

	// 隧道快照变化 → 收到 tunnels 事件（合并窗口 1s）
	st.UpdateFRPProxies([]store.FRPProxy{
		{Name: "ssh", Type: "tcp", User: "u1", ClientID: "c1", Online: false, CurConns: 0},
	}, now.Add(10*time.Second))
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if !strings.HasPrefix(ev, "event: tunnels\n") {
				continue
			}
			if !strings.Contains(ev, `"name":"ssh"`) || !strings.Contains(ev, `"online":false`) {
				t.Fatalf("tunnels 事件内容不符：%q", ev)
			}
			for _, bad := range []string{"local_addr", "127.0.0.1:22", `"client_id"`} {
				if strings.Contains(ev, bad) {
					t.Fatalf("公开 tunnels 事件不应含 %s：%q", bad, ev)
				}
			}
			return
		case <-deadline:
			t.Fatal("5 秒内未收到 tunnels 事件")
		}
	}
}

func TestSSETunnelsNotRepeated(t *testing.T) {
	h, st := newTestHandler(t, "")
	now := time.Unix(1790380800, 0)
	seedTunnels(t, st, now)

	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/events/public")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	events := make(chan string, 16)
	go func() {
		reader := bufio.NewReader(resp.Body)
		var sb strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\n" {
				events <- sb.String()
				sb.Reset()
				continue
			}
			sb.WriteString(line)
		}
	}()
	select {
	case <-events: // snapshot
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内未收到 snapshot")
	}

	// 相同快照重复喂入 → 不增发 tunnels 事件
	st.UpdateFRPProxies([]store.FRPProxy{
		{Name: "ssh", Type: "tcp", User: "u1", ClientID: "c1", Online: true, CurConns: 3,
			TodayTrafficIn: 123, TodayTrafficOut: 456},
		{Name: "orphan", Type: "tcp", User: "u9", ClientID: "c9", Online: false},
	}, now.Add(time.Second))
	timeout := time.After(2500 * time.Millisecond)
	for {
		select {
		case ev := <-events:
			if strings.HasPrefix(ev, "event: tunnels\n") {
				t.Fatalf("快照未变化不应增发 tunnels 事件：%q", ev)
			}
		case <-timeout:
			return
		}
	}
}

func TestCredentialsAPI(t *testing.T) {
	h, _, credPath, _ := newTestHandlerWithCreds(t, "s3cret")
	cookie := loginCookie(t, h, "s3cret", "example.com")
	authHeader := map[string]string{"Cookie": cookie.String()}

	// 未认证 → 401
	rec, _ := doJSON(t, h, http.MethodGet, "/api/admin/v1/credentials", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，得到 %d", rec.Code)
	}

	// 列表：初始 1 条，不含摘要
	rec, body := doJSON(t, h, http.MethodGet, "/api/admin/v1/credentials", authHeader, "")
	if rec.Code != http.StatusOK || !strings.Contains(body, `"id":"node-1"`) ||
		!strings.Contains(body, `"comment":"初始节点"`) {
		t.Fatalf("凭据列表不符：%d %s", rec.Code, body)
	}
	if strings.Contains(body, "token_sha256") {
		t.Fatalf("列表不得含摘要：%s", body)
	}

	// 跨源写 → 403
	rec, _ = doJSON(t, h, http.MethodPost, "http://example.com/api/admin/v1/credentials",
		map[string]string{"Cookie": cookie.String(), "Origin": "http://evil.com"},
		`{"id":"node-x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨源 POST 应 403，得到 %d", rec.Code)
	}

	// 非法 id → 400
	for _, badID := range []string{"", "has space", strings.Repeat("x", 65)} {
		rec, _ = doJSON(t, h, http.MethodPost, "/api/admin/v1/credentials", authHeader,
			`{"id":`+jsonString(badID)+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("非法 id %q 应 400，得到 %d", badID, rec.Code)
		}
	}

	// 创建 → 200，明文 token 仅本次返回
	rec, body = doJSON(t, h, http.MethodPost, "/api/admin/v1/credentials", authHeader,
		`{"id":"node-x","comment":"第二节点"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应 200，得到 %d：%s", rec.Code, body)
	}
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "node-x" || len(created.Token) != 64 {
		t.Fatalf("创建响应不符：%s", body)
	}

	// 冲突 → 409
	rec, body = doJSON(t, h, http.MethodPost, "/api/admin/v1/credentials", authHeader, `{"id":"node-x"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(body, `{"error":"conflict"}`) {
		t.Fatalf("冲突应 409 conflict：%d %s", rec.Code, body)
	}

	// 落盘：0600、保留既有条目与新条目摘要
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("凭据文件应为 0600，得到 %o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Nodes []struct {
			ID          string `json:"id"`
			TokenSHA256 string `json:"token_sha256"`
			Comment     string `json:"comment"`
			CreatedAt   int64  `json:"created_at"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("凭据文件应可解析：%v", err)
	}
	if len(file.Nodes) != 2 || file.Nodes[0].ID != "node-1" || file.Nodes[1].ID != "node-x" {
		t.Fatalf("凭据文件应保留既有条目：%s", raw)
	}
	if file.Nodes[1].Comment != "第二节点" || file.Nodes[1].CreatedAt <= 0 {
		t.Fatalf("新条目 comment/created_at 不符：%s", raw)
	}
	newSum := sha256.Sum256([]byte(created.Token))
	if file.Nodes[1].TokenSHA256 != hex.EncodeToString(newSum[:]) {
		t.Fatalf("落盘摘要应为新 token 的 sha256：%s", raw)
	}

	// 列表出现 created_at 且仍无摘要
	_, body = doJSON(t, h, http.MethodGet, "/api/admin/v1/credentials", authHeader, "")
	if !strings.Contains(body, `"id":"node-x"`) || !strings.Contains(body, `"created_at":`) {
		t.Fatalf("列表应含新条目与 created_at：%s", body)
	}
	if strings.Contains(body, "token_sha256") {
		t.Fatalf("列表不得含摘要：%s", body)
	}

	// 删除 → ok；再删 → 404
	rec, body = doJSON(t, h, http.MethodDelete, "/api/admin/v1/credentials/node-x", authHeader, "")
	if rec.Code != http.StatusOK || !strings.Contains(body, `{"ok":true}`) {
		t.Fatalf("删除应 200 ok：%d %s", rec.Code, body)
	}
	rec, _ = doJSON(t, h, http.MethodDelete, "/api/admin/v1/credentials/node-x", authHeader, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删除不存在应 404，得到 %d", rec.Code)
	}
}

func TestAdminBackup(t *testing.T) {
	// 有库：200 + SQLite 文件头
	h, st := newTestHandlerWithDB(t, "s3cret")
	st.StartSession("node-1", "sess-1", 1, time.Unix(1790380800, 0))
	cookie := loginCookie(t, h, "s3cret", "example.com")

	rec, body := doJSON(t, h, http.MethodGet, "/api/admin/v1/backup",
		map[string]string{"Cookie": cookie.String()}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("备份应 200，得到 %d：%s", rec.Code, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type 不符：%q", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="frp-monitor-backup-`) || !strings.HasSuffix(cd, `.db"`) {
		t.Fatalf("Content-Disposition 不符：%q", cd)
	}
	if !strings.HasPrefix(body, "SQLite format 3") {
		t.Fatalf("备份内容应为 SQLite 文件：%q", body[:15])
	}

	// 未认证 → 401
	rec, _ = doJSON(t, h, http.MethodGet, "/api/admin/v1/backup", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，得到 %d", rec.Code)
	}

	// 无库：409 no_data_dir
	h2, _ := newTestHandler(t, "s3cret")
	cookie2 := loginCookie(t, h2, "s3cret", "example.com")
	rec, body = doJSON(t, h2, http.MethodGet, "/api/admin/v1/backup",
		map[string]string{"Cookie": cookie2.String()}, "")
	if rec.Code != http.StatusConflict || !strings.Contains(body, `{"error":"no_data_dir"}`) {
		t.Fatalf("无库应 409 no_data_dir：%d %s", rec.Code, body)
	}
}
