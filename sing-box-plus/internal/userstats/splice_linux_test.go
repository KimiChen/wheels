//go:build linux

package userstats

import (
	"testing"

	"sing-box-plus/internal/testenv"
)

// TestSpliceByteOracle 覆盖 Linux 真实 splice 路径。
//
// 裸 TCP（无 TLS / REALITY / Vision）的 VLESS 链路可以解包到 syscall.Conn，
// sing 的 copyDirect 会走 splice；splice 只调 CountFunc（common/bufio/splice_linux.go:88-92），
// 因此这条用例同时钉住两件事：口径不因 splice 退化，以及 §4.9 纪律 4——
// 把闸断逻辑从 CountFunc 挪进包装层的 Read/Write 后，本用例与配额闸断用例必然转红。
func TestSpliceByteOracle(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, vlessServerConfig(serverPort, sock))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	payload := make([]byte, 256*1024)
	for index := range payload {
		payload[index] = byte(index)
	}
	const repeat = 40
	roundTripTCP(t, clientPort, payload, repeat)

	const expected = uint64(repeat) * 256 * 1024
	snapshot := fetchSnapshot(t, handle.sockPath)
	user := userOf(t, snapshot, "vless-in", "u1")
	if user.TCPUplinkBytes != expected || user.TCPDownlinkBytes != expected {
		t.Fatalf("splice 路径口径不符：up=%d down=%d，期望 %d", user.TCPUplinkBytes, user.TCPDownlinkBytes, expected)
	}
}

// TestSpliceQuotaEnforcement 断言配额闸断在 splice 路径上同样生效。
func TestSpliceQuotaEnforcement(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	quotaSock := sockPath(dir, "quota.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, quotaServerConfig(serverPort, sock, quotaSock))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))

	conn := dialForward(t, clientPort)
	defer conn.Close()
	echoOnce(t, conn, []byte("hello"))

	status, body := putQuota(t, quotaSock, handle.registry.NodeID(), handle.registry.RuntimeID(), 1,
		[]QuotaEntry{{InboundTag: "vless-in", Name: "u1", RemainingBytes: 1024}})
	if status != 200 {
		t.Fatalf("下发配额失败：%d %s", status, body)
	}
	if err := pushUntilFailure(conn, 64*1024, 20); err == nil {
		t.Fatal("splice 路径上的在途连接同样必须被闸断")
	}
}
