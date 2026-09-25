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

	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// dialWS 用给定 token 拨号监控 WS 并完成 hello；返回连接或拨号错误。
func dialWS(t *testing.T, port int, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://127.0.0.1:%d/agent/v1/ws", port),
		http.Header{"Authorization": {"Bearer " + token}})
}

func wsHello(t *testing.T, c *websocket.Conn, sessionID string) {
	t.Helper()
	req, err := protocol.NewRequest(nil, protocol.MethodHello, &protocol.HelloParams{
		SchemaVersion: protocol.SchemaVersion, AgentVersion: "0.1.0-dev", FRPVersion: "0.71.0",
		SessionID: sessionID, ReportInterval: 1, SentAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req)
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("hello 应答读取失败：%v", err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("hello 应成功：%+v", resp.Error)
	}
}

// TestCredentialLifecycleOverWS 验证凭据管理全流：创建返回 token →
// 该 token 可认证 WS（ingest 路径）→ 删除后认证 401。
func TestCredentialLifecycleOverWS(t *testing.T) {
	creds, _ := writeCreds(t)
	sum := sha256.Sum256([]byte("s3cret"))
	cfg := Config{
		Enable: true, CredentialsFile: creds,
		AdminPasswordHash: hex.EncodeToString(sum[:]),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := startOnFreePort(t, ctx, cfg)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cookie := adminLogin(t, baseURL, "s3cret")

	// 创建凭据
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/admin/v1/credentials",
		strings.NewReader(`{"id":"node-x","comment":"WS 全流"}`))
	req.Header.Set("Cookie", "fm_admin="+cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("创建凭据应 200，得到 %d：%s", resp.StatusCode, b)
	}
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &created); err != nil || created.ID != "node-x" || created.Token == "" {
		t.Fatalf("创建响应不符：%s", b)
	}

	// 新 token 可认证 WS 并完成 hello
	c, _, err := dialWS(t, port, created.Token)
	if err != nil {
		t.Fatalf("新凭据应可拨号 WS：%v", err)
	}
	wsHello(t, c, "sess-x")
	body := getJSON(t, baseURL+"/api/admin/v1/nodes/node-x", cookie)
	if !strings.Contains(body, `"online":true`) {
		t.Fatalf("node-x hello 后应在线：%s", body)
	}
	c.Close()

	// 删除凭据
	req, _ = http.NewRequest(http.MethodDelete, baseURL+"/api/admin/v1/credentials/node-x", nil)
	req.Header.Set("Cookie", "fm_admin="+cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除凭据应 200，得到 %d", resp.StatusCode)
	}

	// 删除后同 token 认证 401
	_, resp, err = dialWS(t, port, created.Token)
	if err == nil {
		t.Fatal("删除后旧 token 不应再拨号成功")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("删除后应 401，得到 %d", resp.StatusCode)
	}
}

// TestPollFeedsTunnels 验证轮询同时拉取注册表与 proxy 统计：
// 未匹配节点的隧道出现在公开隧道列表且 node_id 为 null。
func TestPollFeedsTunnels(t *testing.T) {
	creds, _ := writeCreds(t)
	src := &fakeRegistry{
		clients: []FRPClientInfo{{User: "u1", RawClientID: "c1", RunID: "r1", Online: true}},
		proxies: []FRPProxyStat{{Name: "ssh", Type: "tcp", User: "u1", ClientID: "c1",
			Online: true, CurConns: 2, TodayTrafficIn: 10, TodayTrafficOut: 20}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := startOnFreePortWith(t, ctx, Config{Enable: true, CredentialsFile: creds}, src)
	// 首轮轮询在 2s 后，等待隧道出现
	deadline := time.Now().Add(6 * time.Second)
	for {
		body := getJSON(t, fmt.Sprintf("http://127.0.0.1:%d/api/public/v1/tunnels", port), "")
		if strings.Contains(body, `"name":"ssh"`) {
			if !strings.Contains(body, `"node_id":null`) || !strings.Contains(body, `"today_rx_bytes":"10"`) {
				t.Fatalf("未匹配隧道应 node_id=null 且带流量：%s", body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("6 秒内隧道未出现在公开列表：%s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestNilRegistrySourceSafe 验证 src 为 nil 时隧道端点正常返回空列表。
func TestNilRegistrySourceSafe(t *testing.T) {
	creds, _ := writeCreds(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := startOnFreePort(t, ctx, Config{Enable: true, CredentialsFile: creds})
	body := getJSON(t, fmt.Sprintf("http://127.0.0.1:%d/api/public/v1/tunnels", port), "")
	if !strings.Contains(body, `"tunnels":[]`) {
		t.Fatalf("无注册表源时隧道应为空数组：%s", body)
	}
	body = getJSON(t, fmt.Sprintf("http://127.0.0.1:%d/api/public/v1/overview", port), "")
	if !strings.Contains(body, `"tunnels_total":0`) || !strings.Contains(body, `"tunnels_online":0`) {
		t.Fatalf("无注册表源时隧道计数应为 0：%s", body)
	}
}
