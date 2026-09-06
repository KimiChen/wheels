package userstats

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"sing-box-plus/internal/testenv"
)

func contractServer(t *testing.T) *serverHandle {
	t.Helper()
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	quotaSock := sockPath(dir, "quota.sock")
	port := testenv.FreePort(t)
	return startServer(t, quotaServerConfig(port, sock, quotaSock))
}

// rawUnix 发送一段原始字节，返回响应文本。用于构造 net/http 不允许我们构造的畸形请求。
func rawUnix(t *testing.T, sockPath string, payload string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", sockPath, 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = conn.Write([]byte(payload)); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	buffer := make([]byte, 8192)
	n, _ := conn.Read(buffer)
	return string(buffer[:n])
}

// TestSnapshotRoutes 覆盖 §4.5 的固定两条路由与错误码表。
func TestSnapshotRoutes(t *testing.T) {
	handle := contractServer(t)

	// 路径版本号与 schema_version 同步推进：不提供 /v1/snapshot，误配的采集器立即失败。
	status, body := httpUnix(t, handle.sockPath, "GET", "/v1/snapshot", nil)
	if status != 404 {
		t.Fatalf("/v1/snapshot 应返回 404，实际 %d", status)
	}
	assertSchemaVersion(t, body)

	status, body = httpUnix(t, handle.sockPath, "POST", "/v2/snapshot", nil)
	if status != 405 {
		t.Fatalf("非 GET 应返回 405，实际 %d", status)
	}
	assertSchemaVersion(t, body)

	status, _ = httpUnix(t, handle.sockPath, "GET", "/nope", nil)
	if status != 404 {
		t.Fatalf("未知路径应返回 404，实际 %d", status)
	}

	// 禁 query。
	status, _ = httpUnix(t, handle.sockPath, "GET", "/v2/snapshot?a=1", nil)
	if status != 400 {
		t.Fatalf("带 query 应返回 400，实际 %d", status)
	}

	// 版本不支持。
	response := rawUnix(t, handle.sockPath, "GET /v2/snapshot HTTP/1.0\r\nHost: x\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 505") {
		t.Fatalf("HTTP/1.0 应返回 505，实际：%q", firstLine(response))
	}

	// 只读 socket 上禁请求体。
	response = rawUnix(t, handle.sockPath, "GET /v2/snapshot HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\n\r\nabc")
	if !strings.HasPrefix(response, "HTTP/1.1 400") {
		t.Fatalf("只读 socket 带 body 应返回 400，实际：%q", firstLine(response))
	}

	// 超长请求行。
	response = rawUnix(t, handle.sockPath, "GET /"+strings.Repeat("a", 9000)+" HTTP/1.1\r\nHost: x\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 413") {
		t.Fatalf("超长请求行应返回 413，实际：%q", firstLine(response))
	}
}

// TestHealthzDoesNotAdvanceSequence 断言 /healthz 不推进 sequence，而 /v2/snapshot 严格递增。
func TestHealthzDoesNotAdvanceSequence(t *testing.T) {
	handle := contractServer(t)

	first := fetchSnapshot(t, handle.sockPath)
	status, body := httpUnix(t, handle.sockPath, "GET", "/healthz", nil)
	if status != 200 {
		t.Fatalf("/healthz 应返回 200，实际 %d：%s", status, body)
	}
	assertSchemaVersion(t, body)
	second := fetchSnapshot(t, handle.sockPath)
	if second.Sequence != first.Sequence+1 {
		t.Fatalf("sequence 应严格递增且不被 /healthz 推进：%d -> %d", first.Sequence, second.Sequence)
	}
	if second.StartedAtUnixMs != first.StartedAtUnixMs || second.RuntimeID != first.RuntimeID {
		t.Fatal("同一 runtime 内 started_at_unix_ms 与 runtime_id 必须恒定")
	}
}

// TestSnapshotShape 逐字段断言 §4.5 的形状：health 是三键闭集、listen 可被 ParseAddr 解析、
// listen_port 在 1..65535、排序按 ASCII 原始字节。
func TestSnapshotShape(t *testing.T) {
	handle := contractServer(t)
	_, body := httpUnix(t, handle.sockPath, "GET", "/v2/snapshot", nil)

	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("快照不是合法 JSON：%v", err)
	}
	expectedKeys := map[string]struct{}{
		"schema_version": {}, "node_id": {}, "runtime_id": {}, "started_at_unix_ms": {},
		"sequence": {}, "health": {}, "inbounds": {},
	}
	for key := range generic {
		if _, ok := expectedKeys[key]; !ok {
			t.Fatalf("快照出现额外顶层键：%s", key)
		}
	}
	if len(generic) != len(expectedKeys) {
		t.Fatalf("快照顶层键数量不符：%d != %d", len(generic), len(expectedKeys))
	}
	health, ok := generic["health"].(map[string]any)
	if !ok || len(health) != 3 {
		t.Fatalf("health 必须是恰好三键的闭集：%v", generic["health"])
	}
	for _, key := range []string{"counter_overflow", "sequence_overflow", "identity_limit_reached"} {
		if _, present := health[key]; !present {
			t.Fatalf("health 缺少 %s", key)
		}
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Fatal("JSON body 必须以 LF 结尾")
	}

	var snapshot Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("解析快照失败：%v", err)
	}
	for _, inbound := range snapshot.Inbounds {
		if _, err := netip.ParseAddr(inbound.Listen); err != nil {
			t.Fatalf("listen 必须是纯 host（不含 :port）：%q", inbound.Listen)
		}
		if inbound.ListenPort == 0 {
			t.Fatal("listen_port 必须在 1..65535")
		}
		if inbound.Generation != 1 {
			t.Fatalf("generation 固定输出 1，实际 %d", inbound.Generation)
		}
		names := make([]string, 0, len(inbound.Users))
		for _, user := range inbound.Users {
			names = append(names, user.Name)
		}
		for index := 1; index < len(names); index++ {
			if names[index-1] >= names[index] {
				t.Fatalf("users 未按 ASCII 原始字节升序：%v", names)
			}
		}
	}
	tags := make([]string, 0, len(snapshot.Inbounds))
	for _, inbound := range snapshot.Inbounds {
		tags = append(tags, inbound.Tag)
	}
	for index := 1; index < len(tags); index++ {
		if tags[index-1] >= tags[index] {
			t.Fatalf("inbounds 未按 ASCII 原始字节升序：%v", tags)
		}
	}
}

// TestQuotaEndpointFailClosed 覆盖 §4.9 控制端点的失败关闭矩阵。
func TestQuotaEndpointFailClosed(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	quotaSock := sockPath(dir, "quota.sock")
	port := testenv.FreePort(t)
	handle := startServer(t, quotaServerConfig(port, sock, quotaSock))
	nodeID := handle.registry.NodeID()
	runtimeID := handle.registry.RuntimeID()

	// 方法与路径。
	if status, _ := httpUnix(t, quotaSock, "GET", "/v2/quota", nil); status != 405 {
		t.Fatalf("非 PUT 应返回 405，实际 %d", status)
	}
	if status, _ := httpUnix(t, quotaSock, "PUT", "/v2/quotas", []byte("{}")); status != 404 {
		t.Fatalf("未知路径应返回 404，实际 %d", status)
	}

	// runtime_id 不符 → 409（这条同时是「重启即解封」的自动纠正信号）。
	if status, _ := putQuota(t, quotaSock, nodeID, "ffffffffffffffffffffffffffffffff", 1, nil); status != 409 {
		t.Fatalf("runtime_id 不符应返回 409，实际 %d", status)
	}
	if status, _ := putQuota(t, quotaSock, "other-node", runtimeID, 1, nil); status != 409 {
		t.Fatalf("node_id 不符应返回 409，实际 %d", status)
	}

	// epoch 为 0 → 400。
	if status, _ := putQuota(t, quotaSock, nodeID, runtimeID, 0, nil); status != 400 {
		t.Fatalf("epoch=0 应返回 400，实际 %d", status)
	}

	// 未知 lineage → 整份拒绝 400，并列出未知项。
	status, body := putQuota(t, quotaSock, nodeID, runtimeID, 1,
		[]QuotaEntry{{InboundTag: "vless-in", Name: "nobody", RemainingBytes: 1}})
	if status != 400 {
		t.Fatalf("未知 lineage 应整份拒绝 400，实际 %d", status)
	}
	if !strings.Contains(string(body), "vless-in/nobody") {
		t.Fatalf("响应应列出未知项：%s", body)
	}

	// 正常接受一份。
	if status, _ = putQuota(t, quotaSock, nodeID, runtimeID, 5,
		[]QuotaEntry{{InboundTag: "vless-in", Name: "u1", RemainingBytes: 100}}); status != 200 {
		t.Fatalf("合法请求应返回 200，实际 %d", status)
	}
	// epoch 持平或回退 → 409。
	if status, _ = putQuota(t, quotaSock, nodeID, runtimeID, 5, nil); status != 409 {
		t.Fatalf("epoch 持平应返回 409，实际 %d", status)
	}
	if status, _ = putQuota(t, quotaSock, nodeID, runtimeID, 4, nil); status != 409 {
		t.Fatalf("epoch 回退应返回 409，实际 %d", status)
	}

	// 未知字段 → 400（控制面与配置面同一纪律）。
	unknown := fmt.Sprintf(`{"schema_version":2,"node_id":%q,"runtime_id":%q,"epoch":9,"entries":[],"extra":1}`, nodeID, runtimeID)
	if status, _ = httpUnix(t, quotaSock, "PUT", "/v2/quota", []byte(unknown)); status != 400 {
		t.Fatalf("未知字段应返回 400，实际 %d", status)
	}

	// 超大请求体 → 413。
	oversized := make([]byte, 5*1024*1024)
	for index := range oversized {
		oversized[index] = ' '
	}
	if status, _ = httpUnix(t, quotaSock, "PUT", "/v2/quota", oversized); status != 413 {
		t.Fatalf("超大请求体应返回 413，实际 %d", status)
	}
}

func assertSchemaVersion(t *testing.T, body []byte) {
	t.Helper()
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%s）", err, body)
	}
	value, ok := generic["schema_version"].(float64)
	if !ok || int(value) != SchemaVersion {
		t.Fatalf("全套响应必须共用同一个 schema 版本常量，实际：%s", body)
	}
}

func firstLine(input string) string {
	if index := strings.Index(input, "\r\n"); index >= 0 {
		return input[:index]
	}
	return input
}
