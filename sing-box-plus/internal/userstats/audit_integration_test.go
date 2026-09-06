package userstats

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"sing-box-plus/internal/testenv"
)

// waitSessionsDrained 轮询快照的 tcp_sessions / udp_sessions 直至归零。
//
// 这正是 §5.3 计划重启流程的第 2 步。审计记录在连接关闭时才投递，
// 用固定 sleep 等待会得到一个偶尔通过的门禁，而门禁的意义正是「必须会失败」。
func waitSessionsDrained(t *testing.T, sock string, tag string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := fetchSnapshot(t, sock)
		for _, inbound := range snapshot.Inbounds {
			if inbound.Tag != tag {
				continue
			}
			if inbound.TCPSessions == 0 && inbound.UDPSessions == 0 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待会话排空超时")
}

func auditServerConfig(port uint16, sock string, logPath string) string {
	return fmt.Sprintf(`{
  "log": {"level": "error"},
  "inbounds": [{
    "type": "vless", "tag": "vless-in",
    "listen": "127.0.0.1", "listen_port": %d,
    "users": [{"name": "u1", "uuid": "%s"}, {"name": "u2", "uuid": "%s"}]
  }],
  "outbounds": [{"type": "direct", "tag": "out"}],
  "services": [{
    "type": "user_stats", "tag": "stats",
    "node_id": "node-test-01", "listen_path": %q,
    "inbounds": ["vless-in"],
    "access_log": {"path": %q, "flush_interval_ms": 20}
  }]
}`, port, testUUID1, testUUID2, sock, logPath)
}

// TestAuditMinimalGate 是 §4.8「验收先行」的最小门禁：
// 一次真实往返 → **停止进程后** JSONL 恰好一条，且 user/host/port/up/down 正确。
//
// 必须在停止之后断言：审计不做 fsync，运行中读文件会读到还在缓冲区里的记录，
// 那样的断言会在真实故障下依然通过。
func TestAuditMinimalGate(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	logPath := filepath.Join(dir, "access.jsonl")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, auditServerConfig(serverPort, sock, logPath))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	roundTripTCP(t, clientPort, []byte("ping"), 1)
	waitSessionsDrained(t, handle.sockPath, "vless-in")
	handle.instance.Close()

	records := readLines(t, logPath)
	var access []map[string]any
	for _, record := range records {
		if record["ev"] == nil {
			access = append(access, record)
		}
	}
	if len(access) != 1 {
		t.Fatalf("一次往返应恰好一条记录，实际 %d 条：%v", len(access), records)
	}
	record := access[0]
	if record["user"] != "u1" || record["in"] != "vless-in" || record["net"] != "tcp" {
		t.Fatalf("身份字段不符：%v", record)
	}
	if record["host"] != echoHost {
		t.Fatalf("host 应为目标地址 %q，实际 %v", echoHost, record["host"])
	}
	if record["host_src"] != "ip" {
		t.Fatalf("未启用 sniff 时 host_src 应为 ip，实际 %v", record["host_src"])
	}
	if int(record["port"].(float64)) != int(echoPort) {
		t.Fatalf("port 不符：%v != %d", record["port"], echoPort)
	}
	if int(record["up"].(float64)) != 4 || int(record["down"].(float64)) != 4 {
		t.Fatalf("up/down 应原样落盘为 4/4，实际 %v/%v", record["up"], record["down"])
	}
	if record["node"] != "node-test-01" || record["run"] != handle.registry.RuntimeID() {
		t.Fatalf("node/run 应与快照同源：%v", record)
	}
}

// TestAuditNoRecordForHandshakeOnly 是反例：只完成握手、没有成功负载，不得产生记录。
func TestAuditNoRecordForHandshakeOnly(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	logPath := filepath.Join(dir, "access.jsonl")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, auditServerConfig(serverPort, sock, logPath))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	// 只建连不发数据。
	conn := dialForward(t, clientPort)
	conn.Close()
	waitSessionsDrained(t, handle.sockPath, "vless-in")
	handle.instance.Close()

	for _, record := range readLines(t, logPath) {
		if record["ev"] == nil {
			t.Fatalf("只完成握手不应产生记录：%v", record)
		}
	}
}
