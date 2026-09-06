package userstats

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"

	"sing-box-plus/internal/minreg"
	"sing-box-plus/internal/testenv"
)

// serverContext 复刻自有 main 的 registry 构造（README §4.7 的必须骨架第 1 步）。
func serverContext(ctx context.Context) context.Context {
	serviceRegistry := boxService.NewRegistry()
	RegisterService(serviceRegistry)
	return box.Context(ctx, minreg.InboundRegistry(), minreg.OutboundRegistry(),
		endpoint.NewRegistry(), minreg.DNSTransportRegistry(), serviceRegistry,
		certificate.NewRegistry())
}

type serverHandle struct {
	registry *Registry
	config   *Config
	instance *box.Box
	sockPath string
}

// startServer 按 main 的顺序拉起被测服务端：
// 解码 → 校验 → 建进程级 registry → 对账 → box.New → AppendTracker → Start。
func startServer(t testing.TB, configJSON string) *serverHandle {
	t.Helper()
	ctx := serverContext(context.Background())
	options, err := singjson.UnmarshalExtendedContext[option.Options](ctx, []byte(configJSON))
	if err != nil {
		t.Fatalf("解析服务端配置失败：%v", err)
	}
	config, err := Validate(options)
	if err != nil {
		t.Fatalf("配置校验失败：%v", err)
	}
	if config == nil {
		t.Fatal("配置校验返回空：测试配置必须包含 user_stats")
	}
	registry, err := NewRegistry(config.NodeID, config.MaxIdentities)
	if err != nil {
		t.Fatalf("创建 registry 失败：%v", err)
	}
	if err = registry.Reconcile(config.Inbounds, true); err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	ctx = service.ContextWith(ctx, registry)
	instanceCtx, cancel := context.WithCancel(ctx)
	instance, err := box.New(box.Options{Context: instanceCtx, Options: options})
	if err != nil {
		cancel()
		t.Fatalf("创建服务端失败：%v", err)
	}
	instance.Router().AppendTracker(NewTracker(registry, log.NewNOPFactory().Logger()))
	if err = instance.Start(); err != nil {
		cancel()
		t.Fatalf("启动服务端失败：%v", err)
	}
	t.Cleanup(func() {
		instance.Close()
		cancel()
	})
	return &serverHandle{registry: registry, config: config, instance: instance, sockPath: config.ListenPath}
}

// shortTempDir 返回一个足够短的临时目录。
//
// macOS 的 TMPDIR 在 /var/folders 下很长，而 UDS 路径有 104 字节硬上限，
// 用默认 TempDir 会让测试在 bind 时随机失败。
func shortTempDir(t testing.TB) string {
	t.Helper()
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "sbp")
	if err != nil {
		t.Fatalf("创建临时目录失败：%v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// httpUnix 向本机 UDS 发一个 HTTP/1.1 请求并返回状态码与响应体。
func httpUnix(t testing.TB, sockPath string, method string, target string, body []byte) (int, []byte) {
	t.Helper()
	conn, err := net.DialTimeout("unix", sockPath, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 %s 失败：%v", sockPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	request := method + " " + target + " HTTP/1.1\r\nHost: localhost\r\n"
	if body != nil {
		request += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	request += "\r\n"
	if _, err = conn.Write([]byte(request)); err != nil {
		t.Fatalf("写请求失败：%v", err)
	}
	if len(body) > 0 {
		// 写失败不是测试失败：服务端对超限请求会先回 413 再关连接，
		// 此时剩余请求体必然写不完，正确的做法是继续读它已经写回的响应。
		_, _ = conn.Write(body)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("读响应失败：%v", err)
	}
	return parseHTTPResponse(t, raw)
}

func parseHTTPResponse(t testing.TB, raw []byte) (int, []byte) {
	t.Helper()
	text := string(raw)
	headerEnd := indexOf(text, "\r\n\r\n")
	if headerEnd < 0 {
		t.Fatalf("响应缺少头体分隔：%q", text)
	}
	statusLine := text[:indexOf(text, "\r\n")]
	fields := splitSpace(statusLine)
	if len(fields) < 2 {
		t.Fatalf("状态行异常：%q", statusLine)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("状态码异常：%q", statusLine)
	}
	return status, []byte(text[headerEnd+4:])
}

func indexOf(haystack string, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}

func splitSpace(input string) []string {
	var out []string
	start := -1
	for index := 0; index <= len(input); index++ {
		if index == len(input) || input[index] == ' ' {
			if start >= 0 {
				out = append(out, input[start:index])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = index
		}
	}
	return out
}

// fetchSnapshot 取一次快照并解析。
func fetchSnapshot(t testing.TB, sockPath string) *Snapshot {
	t.Helper()
	status, body := httpUnix(t, sockPath, "GET", "/v2/snapshot", nil)
	if status != 200 {
		t.Fatalf("快照返回 %d：%s", status, body)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("解析快照失败：%v，body=%s", err, body)
	}
	return &snapshot
}

func userOf(t testing.TB, snapshot *Snapshot, tag string, name string) SnapshotUser {
	t.Helper()
	for _, inbound := range snapshot.Inbounds {
		if inbound.Tag != tag {
			continue
		}
		for _, user := range inbound.Users {
			if user.Name == name {
				return user
			}
		}
	}
	t.Fatalf("快照中没有 %s/%s", tag, name)
	return SnapshotUser{}
}

// roundTripTCP 通过客户端转发端口做一次 TCP 往返。
func roundTripTCP(t testing.TB, port uint16, payload []byte, repeat int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(int(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("连接转发端口失败：%v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	echo := make([]byte, len(payload))
	for index := 0; index < repeat; index++ {
		if _, err = conn.Write(payload); err != nil {
			t.Fatalf("写入失败（第 %d 次）：%v", index, err)
		}
		if _, err = io.ReadFull(conn, echo); err != nil {
			t.Fatalf("读回失败（第 %d 次）：%v", index, err)
		}
	}
}

// roundTripUDP 通过客户端转发端口做一次 UDP 往返。
func roundTripUDP(t testing.TB, port uint16, payload []byte) {
	t.Helper()
	conn, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(int(port)))
	if err != nil {
		t.Fatalf("连接 UDP 转发端口失败：%v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = conn.Write(payload); err != nil {
		t.Fatalf("UDP 写入失败：%v", err)
	}
	buffer := make([]byte, 64*1024)
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("UDP 读回失败：%v", err)
	}
	if n != len(payload) {
		t.Fatalf("UDP 回声长度不符：%d != %d", n, len(payload))
	}
}

func sockPath(dir string, name string) string {
	return filepath.Join(dir, name)
}

var _ = testenv.FreePort
