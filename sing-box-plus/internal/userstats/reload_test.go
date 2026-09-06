package userstats

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/bufio"
	singjson "github.com/sagernet/sing/common/json"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"

	"sing-box-plus/internal/testenv"
)

// restartBox 用**同一个**进程级 registry 重建 Box，模拟 SIGHUP 重载。
func restartBox(t *testing.T, registry *Registry, configJSON string) func() {
	t.Helper()
	ctx := serverContext(context.Background())
	options, err := singjson.UnmarshalExtendedContext[option.Options](ctx, []byte(configJSON))
	if err != nil {
		t.Fatalf("解析配置失败：%v", err)
	}
	config, err := Validate(options)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if err = registry.Reconcile(config.Inbounds, false); err != nil {
		t.Fatalf("重载对账失败：%v", err)
	}
	ctx = service.ContextWith(ctx, registry)
	instanceCtx, cancel := context.WithCancel(ctx)
	instance, err := box.New(box.Options{Context: instanceCtx, Options: options})
	if err != nil {
		cancel()
		t.Fatalf("重建 Box 失败：%v", err)
	}
	instance.Router().AppendTracker(NewTracker(registry, log.NewNOPFactory().Logger()))
	if err = instance.Start(); err != nil {
		cancel()
		t.Fatalf("重启 Box 失败：%v", err)
	}
	return func() {
		instance.Close()
		cancel()
	}
}

// TestReloadEnvelopeInvariance 是 §4.4 的信封不变性断言。
//
// 重载前后 runtime_id 与 started_at_unix_ms 逐字节相等、sequence 严格递增且不回退、
// 四向累计值不清零——三者中任意一个挂到 services[] 实例上都会在这里转红。
func TestReloadEnvelopeInvariance(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)
	config := vlessServerConfig(serverPort, sock)

	handle := startServer(t, config)
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))
	roundTripTCP(t, clientPort, []byte("ping"), 1)

	before := fetchSnapshot(t, handle.sockPath)
	beforeUser := userOf(t, before, "vless-in", "u1")
	if beforeUser.TCPDownlinkBytes != 4 {
		t.Fatalf("重载前计数不符：%d", beforeUser.TCPDownlinkBytes)
	}

	// 关闭旧 Box、用同一个 registry 重建（SIGHUP 的语义）。
	handle.instance.Close()
	stop := restartBox(t, handle.registry, config)
	defer stop()
	testenv.WaitPort(t, "127.0.0.1", serverPort)
	roundTripTCP(t, clientPort, []byte("pong"), 1)

	after := fetchSnapshot(t, sock)
	if after.RuntimeID != before.RuntimeID {
		t.Fatalf("重载不得改变 runtime_id：%s -> %s", before.RuntimeID, after.RuntimeID)
	}
	if after.StartedAtUnixMs != before.StartedAtUnixMs {
		t.Fatalf("重载不得改变 started_at_unix_ms：%d -> %d", before.StartedAtUnixMs, after.StartedAtUnixMs)
	}
	if after.Sequence <= before.Sequence {
		t.Fatalf("sequence 必须严格递增：%d -> %d", before.Sequence, after.Sequence)
	}
	afterUser := userOf(t, after, "vless-in", "u1")
	if afterUser.TCPUplinkBytes != 8 || afterUser.TCPDownlinkBytes != 8 {
		t.Fatalf("重载后计数应自然延续为 8/8，实际 %d/%d",
			afterUser.TCPUplinkBytes, afterUser.TCPDownlinkBytes)
	}
}

// countingTracker 是一个只做计数的旁路 tracker，用来模拟上游账本叠加的场景。
//
// 计数器用 atomic：CountFunc 会在上下行两个 copy goroutine 里并发调用。
type countingTracker struct {
	uplink   *atomic.Int64
	downlink *atomic.Int64
}

func (t *countingTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	return bufio.NewCounterConn(conn,
		[]N.CountFunc{func(n int64) { t.uplink.Add(n) }},
		[]N.CountFunc{func(n int64) { t.downlink.Add(n) }})
}

func (t *countingTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	return conn
}

func (t *countingTracker) RoutedFlow(ctx context.Context, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) tun.FlowTracker {
	return nil
}

// TestCounterUnwrapWithStackedTrackers 是 §4.1 的口径断言。
//
// 本项目的 tracker 恒定是包装链最外层。sing 的 bufio.Copy 只有能从连接链顶端解包出 counter
// 时才在「目标写成功后」计数，否则回落为「源读成功」计数；因此包装必须实现 Upstream() 与
// Reader/WriterReplaceable()。此处叠一个内层 counter tracker，断言两层计数都精确。
func TestCounterUnwrapWithStackedTrackers(t *testing.T) {
	dir := shortTempDir(t)
	sock := sockPath(dir, "stats.sock")
	serverPort := testenv.FreePort(t)
	echoHost, echoPort := testenv.StartTCPEcho(t)

	ctx := serverContext(context.Background())
	options, err := singjson.UnmarshalExtendedContext[option.Options](ctx, []byte(vlessServerConfig(serverPort, sock)))
	if err != nil {
		t.Fatalf("解析配置失败：%v", err)
	}
	config, err := Validate(options)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
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
	defer cancel()
	instance, err := box.New(box.Options{Context: instanceCtx, Options: options})
	if err != nil {
		t.Fatalf("创建 Box 失败：%v", err)
	}
	defer instance.Close()

	var innerUp, innerDown atomic.Int64
	// 内层：先追加的 tracker 更靠内。
	instance.Router().AppendTracker(&countingTracker{uplink: &innerUp, downlink: &innerDown})
	// 外层：本项目的 tracker。
	instance.Router().AppendTracker(NewTracker(registry, log.NewNOPFactory().Logger()))
	if err = instance.Start(); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	testenv.WaitPort(t, "127.0.0.1", serverPort)

	clientPort := testenv.FreePort(t)
	testenv.StartUpstreamClient(t, vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort))
	roundTripTCP(t, clientPort, []byte("ping"), 1)

	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatalf("取快照失败：%v", err)
	}
	user := findUser(t, snapshot, "vless-in", "u1")
	if user.TCPUplinkBytes != 4 || user.TCPDownlinkBytes != 4 {
		t.Fatalf("叠加 tracker 后本项目计数应仍为 4/4，实际 %d/%d",
			user.TCPUplinkBytes, user.TCPDownlinkBytes)
	}
	if innerUp.Load() != 4 || innerDown.Load() != 4 {
		t.Fatalf("内层 counter 未被 unwrap 收集：%d/%d", innerUp.Load(), innerDown.Load())
	}
}
