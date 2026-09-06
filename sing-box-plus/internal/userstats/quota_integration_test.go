package userstats

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"sing-box-plus/internal/testenv"
)

func quotaServerConfig(port uint16, sock string, quotaSock string) string {
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
    "quota_control": {"listen_path": %q, "reconnect_throttle": "300ms"}
  }]
}`, port, testUUID1, testUUID2, sock, quotaSock)
}

func putQuota(t *testing.T, quotaSock string, nodeID string, runtimeID string, epoch uint64, entries []QuotaEntry) (int, []byte) {
	t.Helper()
	payload := map[string]any{
		"schema_version": SchemaVersion,
		"node_id":        nodeID,
		"runtime_id":     runtimeID,
		"epoch":          epoch,
		"entries":        entries,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编码配额请求失败：%v", err)
	}
	return httpUnix(t, quotaSock, "PUT", "/v2/quota", body)
}

// TestQuotaIsolation 是 §4.9 的隔离性主用例。
//
// 只给 u1 下发小于其在途增量的 remaining_bytes，断言：
// (a) u1 的在途连接在下一次 I/O 上断开、新连接被拒；
// (b) u2 的连接不中断、四向计数继续增长；
// (c) 快照中两者的 active 均不变（纪律 3：闸断状态不得写进 active）。
func TestQuotaIsolation(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	quotaSock := sockPath(dir, "quota.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, quotaServerConfig(serverPort, sock, quotaSock))
	testenv.WaitPort(t, "127.0.0.1", serverPort)

	port1 := testenv.FreePort(t)
	port2 := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(port1, serverPort, testUUID1, echoHost, echoPort))
	testenv.StartUpstreamClient(t, vlessClientConfig(port2, serverPort, testUUID2, echoHost, echoPort))

	// 两条长连接同时在跑。
	conn1 := dialForward(t, port1)
	defer conn1.Close()
	conn2 := dialForward(t, port2)
	defer conn2.Close()
	echoOnce(t, conn1, []byte("hello"))
	echoOnce(t, conn2, []byte("hello"))

	// 只给 u1 下发一个很小的剩余额度。
	status, body := putQuota(t, quotaSock, handle.registry.NodeID(), handle.registry.RuntimeID(), 1,
		[]QuotaEntry{{InboundTag: "vless-in", Name: "u1", RemainingBytes: 64}})
	if status != 200 {
		t.Fatalf("下发配额失败：%d %s", status, body)
	}

	// u1 的在途连接应在下一次 I/O 上断开。
	if err := pushUntilFailure(conn1, 4096, 20); err == nil {
		t.Fatal("u1 的在途连接应被闸断，实际仍在正常收发")
	}
	// u2 的在途连接不受影响。
	echoOnce(t, conn2, []byte("still-alive"))

	// u1 的新连接被拒（tracker 会 Close，客户端表现为连接建立后立刻被重置）。
	if err := tryRoundTrip(port1, []byte("ping")); err == nil {
		t.Fatal("u1 的新连接应被拒绝")
	}
	// u2 的新连接照常。
	if err := tryRoundTrip(port2, []byte("ping")); err != nil {
		t.Fatalf("u2 的新连接不应受影响：%v", err)
	}

	snapshot := fetchSnapshot(t, handle.sockPath)
	blocked := userOf(t, snapshot, "vless-in", "u1")
	healthy := userOf(t, snapshot, "vless-in", "u2")
	if !blocked.Active || !healthy.Active {
		t.Fatalf("闸断状态不得写进 active：u1.active=%v u2.active=%v", blocked.Active, healthy.Active)
	}
	if snapshot.Health.CounterOverflow || snapshot.Health.SequenceOverflow || snapshot.Health.IdentityLimitReached {
		t.Fatalf("health 三位不应因闸断而置位：%+v", snapshot.Health)
	}
	if healthy.TCPUplinkBytes == 0 {
		t.Fatal("u2 的计数应继续增长")
	}
}

// TestQuotaRejectedConnectionStillDials 固定住一条已知差额：
// 被闸断身份的新连接**仍会向目的地拨号**。
//
// tracker 位于「已选定 outbound、尚未交给 outbound handler」之处，而
// ConnectionManager.NewConnection 先拨号后 copy（route/conn.go:95-105），
// 因此 tracker 层的拒绝与节流都阻断不了拨号。这条断言存在的意义是：
// 一旦将来有人以为节流能省掉拨号，测试会立刻转红。
func TestQuotaRejectedConnectionStillDials(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	quotaSock := sockPath(dir, "quota.sock")
	serverPort := testenv.FreePort(t)

	var accepted atomic.Int64
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动计数目标失败：%v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer conn.Close()
				buffer := make([]byte, 4096)
				for {
					n, readErr := conn.Read(buffer)
					if n > 0 {
						_, _ = conn.Write(buffer[:n])
					}
					if readErr != nil {
						return
					}
				}
			}()
		}
	}()
	target := listener.Addr().(*net.TCPAddr)

	handle := startServer(t, quotaServerConfig(serverPort, sock, quotaSock))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	port1 := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(port1, serverPort, testUUID1,
		target.IP.String(), uint16(target.Port)))

	// 把 u1 的额度直接压到 0：任何新连接都会在 tracker 处被拒。
	status, body := putQuota(t, quotaSock, handle.registry.NodeID(), handle.registry.RuntimeID(), 1,
		[]QuotaEntry{{InboundTag: "vless-in", Name: "u1", RemainingBytes: 0}})
	if status != 200 {
		t.Fatalf("下发配额失败：%d %s", status, body)
	}
	before := accepted.Load()
	if err = tryRoundTrip(port1, []byte("ping")); err == nil {
		t.Fatal("额度为 0 的身份不应能完成往返")
	}
	deadline := time.Now().Add(2 * time.Second)
	for accepted.Load() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if accepted.Load() == before {
		t.Fatal("目的地未收到拨号：本条断言若要改判，必须同步改 README §4.9 与 §12 的已知差额")
	}
}

func dialForward(t *testing.T, port uint16) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(int(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("连接转发端口 %d 失败：%v", port, err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	return conn
}

func echoOnce(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("读回失败：%v", err)
	}
}

func pushUntilFailure(conn net.Conn, chunk int, rounds int) error {
	payload := make([]byte, chunk)
	echo := make([]byte, chunk)
	for index := 0; index < rounds; index++ {
		if _, err := conn.Write(payload); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, echo); err != nil {
			return err
		}
	}
	return nil
}

func tryRoundTrip(port uint16, payload []byte) error {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(int(port)), 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = conn.Write(payload); err != nil {
		return err
	}
	echo := make([]byte, len(payload))
	_, err = io.ReadFull(conn, echo)
	return err
}
