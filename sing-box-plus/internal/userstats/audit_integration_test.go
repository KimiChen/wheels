package userstats

import (
	"fmt"
	"os"
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

func auditServerConfig(port uint16, sock string, logDir string) string {
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
    "access_log": {"directory": %q, "flush_interval_ms": 20}
  }]
}`, port, testUUID1, testUUID2, sock, logDir)
}

// TestAuditMinimalGate 是 §4.8「验收先行」的最小门禁：
// 一次真实往返 → **停止进程后** JSONL 恰好一条，且 user/host/port/up/down 正确。
//
// 必须在停止之后断言：审计不做 fsync，运行中读文件会读到还在缓冲区里的记录，
// 那样的断言会在真实故障下依然通过。
func TestAuditMinimalGate(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	logDir := filepath.Join(dir, "access")
	if err := os.Mkdir(logDir, 0o750); err != nil {
		t.Fatalf("创建审计目录失败：%v", err)
	}
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, auditServerConfig(serverPort, sock, logDir))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	roundTripTCP(t, clientPort, []byte("ping"), 1)
	waitSessionsDrained(t, handle.sockPath, "vless-in")
	handle.instance.Close()

	records := readLines(t, filepath.Join(logDir, auditFileName("u1")))
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
	logDir := filepath.Join(dir, "access")
	if err := os.Mkdir(logDir, 0o750); err != nil {
		t.Fatalf("创建审计目录失败：%v", err)
	}
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, auditServerConfig(serverPort, sock, logDir))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	// 只建连不发数据。
	conn := dialForward(t, clientPort)
	conn.Close()
	waitSessionsDrained(t, handle.sockPath, "vless-in")
	handle.instance.Close()

	// 逐用户分文件之后，从未产生过成功访问的身份**连文件都不该有**——
	// 惰性创建比创建一个空文件更诚实：目录里出现某人的文件本身就是一条信息。
	path := filepath.Join(logDir, auditFileName("u1"))
	if _, err := os.Stat(path); err == nil {
		for _, record := range readLines(t, path) {
			if record["ev"] == nil {
				t.Fatalf("只完成握手不应产生记录：%v", record)
			}
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("检查审计文件失败：%v", err)
	}
}

func excludeServerConfig(port uint16, sock string, logDir string) string {
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
    "access_log": {
      "directory": %q, "flush_interval_ms": 20,
      "exclude_ips": ["127.0.0.0/8"]
    }
  }]
}`, port, testUUID1, testUUID2, sock, logDir)
}

// TestExcludedTrafficStillBilled 是排除规则的**核心断言**，也是它唯一危险的地方：
// 被排除的连接不得留下审计记录，但必须照常计费。
//
// 两条路径在 connState 里是分开的——countUplink/countDownlink 无条件推进计数器与配额，
// 审计只是 if s.audit != nil 的一条旁路。这个用例钉住那条分界线：
// 如果有人图省事把排除判断塞进计数回调，计费就会跟着少，而这种少是静默的、
// 只有对账时才会发现。
func TestExcludedTrafficStillBilled(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	logDir := filepath.Join(dir, "access")
	if err := os.Mkdir(logDir, 0o750); err != nil {
		t.Fatalf("创建审计目录失败：%v", err)
	}
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, excludeServerConfig(serverPort, sock, logDir))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	payload := []byte("ping")
	roundTripTCP(t, clientPort, payload, 1)
	waitSessionsDrained(t, handle.sockPath, "vless-in")

	// 计费侧：echo 目标是 127.0.0.1，命中 exclude_ips，但字节必须照常进计数器。
	snapshot := fetchSnapshot(t, handle.sockPath)
	var billed uint64
	for _, inbound := range snapshot.Inbounds {
		for _, user := range inbound.Users {
			if user.Name == "u1" {
				billed = user.TCPUplinkBytes + user.TCPDownlinkBytes
			}
		}
	}
	if billed < uint64(len(payload)*2) {
		t.Fatalf("被排除的流量仍须计费，实际只记到 %d 字节", billed)
	}

	handle.instance.Close()

	// 审计侧：连文件都不该出现——排除在连接建立时判掉，那个身份这一轮没有任何可写的记录。
	if _, err := os.Stat(filepath.Join(logDir, auditFileName("u1"))); !os.IsNotExist(err) {
		records := readLines(t, filepath.Join(logDir, auditFileName("u1")))
		var access []map[string]any
		for _, record := range records {
			if record["ev"] == nil {
				access = append(access, record)
			}
		}
		if len(access) != 0 {
			t.Fatalf("命中排除规则的连接不得留下审计记录，实际 %d 条：%v", len(access), access)
		}
	}
}

func stampServerConfig(port uint16, sock string, logDir string) string {
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
    "access_log": {
      "directory": %q, "flush_interval_ms": 20,
      "exclude_hosts": ["github.com"],
      "exclude_ips": ["203.0.113.0/24"]
    }
  }]
}`, port, testUUID1, testUUID2, sock, logDir)
}

// TestFilterStampLandsWithFirstRealRecord 补齐 TestExcludedTrafficStillBilled 的另一半：
// 那个用例证明命中排除的连接不留记录，这个证明**没命中**的连接照常留记录，
// 并且文件里带着当时生效的规则。
//
// 两半都要有，否则「审计里什么都没有」既可能是规则生效，也可能是写入坏了——
// 生产上正是这个二义性最难判断（配了排除之后，空文件不再是异常信号）。
func TestFilterStampLandsWithFirstRealRecord(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	logDir := filepath.Join(dir, "access")
	if err := os.Mkdir(logDir, 0o750); err != nil {
		t.Fatalf("创建审计目录失败：%v", err)
	}
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, stampServerConfig(serverPort, sock, logDir))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	roundTripTCP(t, clientPort, []byte("ping"), 1)
	waitSessionsDrained(t, handle.sockPath, "vless-in")
	handle.instance.Close()

	records := readLines(t, filepath.Join(logDir, auditFileName("u1")))
	if len(records) == 0 {
		t.Fatal("未命中排除的连接必须留下记录")
	}
	if records[0]["ev"] != "filter" {
		t.Fatalf("首行应是规则留痕，实际：%v", records[0])
	}
	hosts, _ := records[0]["hosts"].([]any)
	ips, _ := records[0]["ips"].([]any)
	if len(hosts) != 1 || hosts[0] != "github.com" || len(ips) != 1 || ips[0] != "203.0.113.0/24" {
		t.Fatalf("留痕内容与配置不符：%v", records[0])
	}
	var access []map[string]any
	for _, record := range records {
		if record["ev"] == nil {
			access = append(access, record)
		}
	}
	if len(access) != 1 || access[0]["host"] != echoHost {
		t.Fatalf("应恰好一条访问记录且 host 为 %q，实际：%v", echoHost, access)
	}
}
