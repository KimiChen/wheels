package userstats

import (
	"fmt"
	"testing"

	"sing-box-plus/internal/testenv"
)

const (
	testUUID1 = "11111111-1111-4111-8111-111111111111"
	testUUID2 = "22222222-2222-4222-8222-222222222222"
	// 16 字节 uPSK/iPSK 的 Base64，对应 2022-blake3-aes-128-gcm。
	testPSKServer = "AQIDBAUGBwgJCgsMDQ4PEA=="
	testPSKUser1  = "EA8ODQwLCgkIBwYFBAMCAQ=="
	testPSKUser2  = "AAECAwQFBgcICQoLDA0ODw=="
)

func vlessServerConfig(port uint16, sock string) string {
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
    "inbounds": ["vless-in"]
  }]
}`, port, testUUID1, testUUID2, sock)
}

func vlessClientConfig(listenPort uint16, serverPort uint16, uuid string, target string, targetPort uint16) string {
	return fmt.Sprintf(`{
  "log": {"level": "error"},
  "inbounds": [{
    "type": "direct", "tag": "fwd",
    "listen": "127.0.0.1", "listen_port": %d,
    "override_address": %q, "override_port": %d
  }],
  "outbounds": [{
    "type": "vless", "tag": "proxy",
    "server": "127.0.0.1", "server_port": %d,
    "uuid": %q, "packet_encoding": "xudp"
  }]
}`, listenPort, target, targetPort, serverPort, uuid)
}

// TestVLESSByteOracleTCP 是 README §4.2 的口径断言：4 字节往返恒为 TCP 4/4，
// 且 VLESS header 与外层封装不计入。
func TestVLESSByteOracleTCP(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, vlessServerConfig(serverPort, sock))
	testenv.WaitPort(t, "127.0.0.1", serverPort)

	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	roundTripTCP(t, clientPort, []byte("ping"), 1)

	snapshot := fetchSnapshot(t, handle.sockPath)
	user := userOf(t, snapshot, "vless-in", "u1")
	if user.TCPUplinkBytes != 4 || user.TCPDownlinkBytes != 4 {
		t.Fatalf("四向计数不符：tcp_up=%d tcp_down=%d，期望 4/4", user.TCPUplinkBytes, user.TCPDownlinkBytes)
	}
	if user.UDPUplinkBytes != 0 || user.UDPDownlinkBytes != 0 {
		t.Fatalf("UDP 计数应为 0：%d/%d", user.UDPUplinkBytes, user.UDPDownlinkBytes)
	}
	idle := userOf(t, snapshot, "vless-in", "u2")
	if idle.TCPUplinkBytes != 0 || idle.TCPDownlinkBytes != 0 {
		t.Fatalf("未使用的身份不应有计数：%d/%d", idle.TCPUplinkBytes, idle.TCPDownlinkBytes)
	}
	if snapshot.NodeID != "node-test-01" || len(snapshot.RuntimeID) != 32 {
		t.Fatalf("信封字段异常：node_id=%s runtime_id=%s", snapshot.NodeID, snapshot.RuntimeID)
	}
	if snapshot.Sequence != 1 {
		t.Fatalf("首次快照的 sequence 应为 1，实际 %d", snapshot.Sequence)
	}
}

// TestVLESSByteOracleLarge 是大数据往返的 oracle：100×64 KiB 双向精确相等。
func TestVLESSByteOracleLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过大数据往返")
	}
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, vlessServerConfig(serverPort, sock))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	payload := make([]byte, 64*1024)
	for index := range payload {
		payload[index] = byte(index)
	}
	const repeat = 100
	roundTripTCP(t, clientPort, payload, repeat)

	const expected = uint64(repeat) * 64 * 1024
	snapshot := fetchSnapshot(t, handle.sockPath)
	user := userOf(t, snapshot, "vless-in", "u1")
	if user.TCPUplinkBytes != expected || user.TCPDownlinkBytes != expected {
		t.Fatalf("大数据 oracle 不符：up=%d down=%d，期望 %d/%d",
			user.TCPUplinkBytes, user.TCPDownlinkBytes, expected, expected)
	}
}

// TestVLESSUDPCounted 断言逻辑 UDP 归入 UDP 而非 TCP（XUDP 承载在 TCP 之上）。
func TestVLESSUDPCounted(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartUDPEcho(t)

	handle := startServer(t, vlessServerConfig(serverPort, sock))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	roundTripUDP(t, clientPort, []byte("ping"))

	snapshot := fetchSnapshot(t, handle.sockPath)
	user := userOf(t, snapshot, "vless-in", "u1")
	if user.UDPUplinkBytes != 4 || user.UDPDownlinkBytes != 4 {
		t.Fatalf("UDP 四向计数不符：udp_up=%d udp_down=%d，期望 4/4",
			user.UDPUplinkBytes, user.UDPDownlinkBytes)
	}
	if user.TCPUplinkBytes != 0 || user.TCPDownlinkBytes != 0 {
		t.Fatalf("逻辑 UDP 不得归入 TCP：tcp=%d/%d", user.TCPUplinkBytes, user.TCPDownlinkBytes)
	}
}

func ssServerConfig(port uint16, sock string) string {
	return fmt.Sprintf(`{
  "log": {"level": "error"},
  "inbounds": [{
    "type": "shadowsocks", "tag": "ss-in",
    "listen": "127.0.0.1", "listen_port": %d,
    "method": "2022-blake3-aes-128-gcm", "password": %q,
    "users": [{"name": "s1", "password": %q}, {"name": "s2", "password": %q}]
  }],
  "outbounds": [{"type": "direct", "tag": "out"}],
  "services": [{
    "type": "user_stats", "tag": "stats",
    "node_id": "node-test-01", "listen_path": %q,
    "inbounds": ["ss-in"]
  }]
}`, port, testPSKServer, testPSKUser1, testPSKUser2, sock)
}

func ssClientConfig(listenPort uint16, serverPort uint16, userPSK string, target string, targetPort uint16) string {
	return fmt.Sprintf(`{
  "log": {"level": "error"},
  "inbounds": [{
    "type": "direct", "tag": "fwd",
    "listen": "127.0.0.1", "listen_port": %d,
    "override_address": %q, "override_port": %d
  }],
  "outbounds": [{
    "type": "shadowsocks", "tag": "proxy",
    "server": "127.0.0.1", "server_port": %d,
    "method": "2022-blake3-aes-128-gcm", "password": "%s:%s"
  }]
}`, listenPort, target, targetPort, serverPort, testPSKServer, userPSK)
}

// TestShadowsocksByteOracle 断言 SS-2022 EIH 多用户的四向口径与 VLESS 相等：
// salt / EIH / AEAD 分块均不计入（README §4.2「VLESS 可作为 SS 的口径 oracle」）。
func TestShadowsocksByteOracle(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, ssServerConfig(serverPort, sock))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, ssClientConfig(clientPort, serverPort, testPSKUser1, echoHost, echoPort))

	roundTripTCP(t, clientPort, []byte("ping"), 1)

	snapshot := fetchSnapshot(t, handle.sockPath)
	user := userOf(t, snapshot, "ss-in", "s1")
	if user.TCPUplinkBytes != 4 || user.TCPDownlinkBytes != 4 {
		t.Fatalf("SS 四向计数不符：tcp_up=%d tcp_down=%d，期望 4/4",
			user.TCPUplinkBytes, user.TCPDownlinkBytes)
	}
	other := userOf(t, snapshot, "ss-in", "s2")
	if other.TCPUplinkBytes != 0 {
		t.Fatalf("EIH 身份分离失败：s2 不应有计数，实际 %d", other.TCPUplinkBytes)
	}
}
