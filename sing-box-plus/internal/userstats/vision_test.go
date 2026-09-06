package userstats

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sing-box-plus/internal/testenv"
)

// selfSignedCert 生成一对自签证书，供 Vision 链路的对账使用。
//
// 用真实 TLS 而不是裸链路，是因为 Vision 只在 TLS 之上存在；而 REALITY 需要一个外部握手目标，
// 会把这条用例变成依赖网络的用例。TLS + xtls-rprx-vision 已足以覆盖 §4.2 要求的两点：
// padding 不计入，以及 buffered → direct 切换后口径不变。
func selfSignedCert(t testing.TB, host string) (certPath string, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{host},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败：%v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("编码密钥失败：%v", err)
	}
	dir := shortTempDir(t)
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	return certPath, keyPath
}

func writePEM(t testing.TB, path string, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatalf("写入 %s 失败：%v", path, err)
	}
}

func visionServerConfig(port uint16, sock string, certPath string, keyPath string) string {
	return fmt.Sprintf(`{
  "log": {"level": "error"},
  "inbounds": [{
    "type": "vless", "tag": "vless-in",
    "listen": "127.0.0.1", "listen_port": %d,
    "users": [{"name": "u1", "uuid": %q, "flow": "xtls-rprx-vision"}],
    "tls": {
      "enabled": true,
      "server_name": "vision.test.invalid",
      "certificate_path": %q,
      "key_path": %q
    }
  }],
  "outbounds": [{"type": "direct", "tag": "out"}],
  "services": [{
    "type": "user_stats", "tag": "stats",
    "node_id": "node-test-01", "listen_path": %q,
    "inbounds": ["vless-in"]
  }]
}`, port, testUUID1, certPath, keyPath, sock)
}

func visionClientConfig(listenPort uint16, serverPort uint16, certPath string, target string, targetPort uint16) string {
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
    "uuid": %q, "flow": "xtls-rprx-vision",
    "tls": {
      "enabled": true,
      "server_name": "vision.test.invalid",
      "certificate_path": %q
    }
  }]
}`, listenPort, target, targetPort, serverPort, testUUID1, certPath)
}

// TestVisionByteOracle 是 §4.2 要求的 Vision 对账。
//
// 两个断言合起来才有意义：
//   - 小往返恒为 4/4——Vision 的 padding 在 VisionConn.Read/Write 内剥离，counter 在其外层，
//     因此 padding 不得计入；
//   - 大数据往返精确相等——足以越过 buffered → direct 的切换点，切换后口径不得退化。
//     注意 Vision **不进入 splice**：VisionConn 不实现 Reader/WriterReplaceable、
//     也不暴露 syscall.Conn，sing 的 unwrap 停在该层，只走用户态直读。
func TestVisionByteOracle(t *testing.T) {
	if raceEnabled {
		// -race 会一并打开 checkptr，而上游 sing-vmess 的 Vision 实现用
		// `unsafe.Pointer(reflectPointer + field.Offset)` 直接读 crypto/tls.Conn 的私有
		// input / rawInput 字段（sing-vmess vless/vision.go:87）。这类 uintptr 运算被
		// checkptr 判为「指向无效分配」并 fatal，整个测试进程会当场死掉——
		// 它是上游的实现方式，零补丁形态下无法规避，也不是本项目的竞争。
		// 本用例因此只在非 -race 轮次执行；scripts/verify.sh 为此专门跑一轮不带 -race 的全量。
		t.Skip("上游 sing-vmess 的 Vision 实现与 checkptr 不兼容，见 docs/UPSTREAM_BASELINE.md")
	}
	certPath, keyPath := selfSignedCert(t, "vision.test.invalid")
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	handle := startServer(t, visionServerConfig(serverPort, sock, certPath, keyPath))
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, visionClientConfig(clientPort, serverPort, certPath, echoHost, echoPort))

	roundTripTCP(t, clientPort, []byte("ping"), 1)
	snapshot := fetchSnapshot(t, handle.sockPath)
	user := userOf(t, snapshot, "vless-in", "u1")
	if user.TCPUplinkBytes != 4 || user.TCPDownlinkBytes != 4 {
		t.Fatalf("Vision 小往返口径不符：%d/%d，期望 4/4（padding 不得计入）",
			user.TCPUplinkBytes, user.TCPDownlinkBytes)
	}

	payload := make([]byte, 64*1024)
	for index := range payload {
		payload[index] = byte(index)
	}
	const repeat = 100
	roundTripTCP(t, clientPort, payload, repeat)

	expected := uint64(4 + repeat*64*1024)
	snapshot = fetchSnapshot(t, handle.sockPath)
	user = userOf(t, snapshot, "vless-in", "u1")
	if user.TCPUplinkBytes != expected || user.TCPDownlinkBytes != expected {
		t.Fatalf("Vision 大数据往返口径不符：%d/%d，期望 %d（buffered → direct 切换后不得退化）",
			user.TCPUplinkBytes, user.TCPDownlinkBytes, expected)
	}
}
