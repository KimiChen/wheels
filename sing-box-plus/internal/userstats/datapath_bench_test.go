package userstats

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
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

// 数据面三组对照（README §8）。
//
//	A 未启用     —— 配置里没有 user_stats，也不注入 tracker
//	B 已配置     —— 完整统计链路
//	C 配额闸断   —— 在 B 之上打开 quota_control 并下发一份全量表
//
// A 与 B 的差是「用起来的成本」；B 与 C 的差是回调里那两次原子操作的代价。
// 「编译进去但没配置」等价于 A：未配置时不创建 registry、exporter 或任何附加包装，
// 数据面代码路径与 A 完全一致，因此不单列一组。

func benchDataPath(b *testing.B, quota bool, tracked bool) {
	dir := shortTempDir(b)
	sock := sockPath(dir, "s.sock")
	quotaSock := sockPath(dir, "q.sock")
	serverPort := testenv.FreePort(b)
	echoHost, echoPort := startEchoForBench(b)

	var configJSON string
	switch {
	case !tracked:
		configJSON = fmt.Sprintf(`{
  "log": {"disabled": true},
  "inbounds": [{"type":"vless","tag":"vless-in","listen":"127.0.0.1","listen_port":%d,
    "users":[{"name":"u1","uuid":"%s"}]}],
  "outbounds": [{"type":"direct","tag":"out"}]
}`, serverPort, testUUID1)
	case quota:
		configJSON = silenceLog(quotaServerConfig(serverPort, sock, quotaSock))
	default:
		configJSON = silenceLog(vlessServerConfig(serverPort, sock))
	}

	ctx := serverContext(context.Background())
	if !tracked {
		// A 组连 service registry 都不需要注册 user_stats：走的就是上游快路径。
		ctx = box.Context(context.Background(), minreg.InboundRegistry(), minreg.OutboundRegistry(),
			endpoint.NewRegistry(), minreg.DNSTransportRegistry(), boxService.NewRegistry(),
			certificate.NewRegistry())
	}
	options, err := singjson.UnmarshalExtendedContext[option.Options](ctx, []byte(configJSON))
	if err != nil {
		b.Fatalf("解析配置失败：%v", err)
	}
	var registry *Registry
	if tracked {
		config, validateErr := Validate(options)
		if validateErr != nil {
			b.Fatalf("校验失败：%v", validateErr)
		}
		registry, err = NewRegistry(config.NodeID, config.MaxIdentities)
		if err != nil {
			b.Fatalf("创建 registry 失败：%v", err)
		}
		if err = registry.Reconcile(config.Inbounds, true); err != nil {
			b.Fatalf("对账失败：%v", err)
		}
		ctx = service.ContextWith(ctx, registry)
	}
	instanceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	instance, err := box.New(box.Options{Context: instanceCtx, Options: options})
	if err != nil {
		b.Fatalf("创建 Box 失败：%v", err)
	}
	defer instance.Close()
	if registry != nil {
		instance.Router().AppendTracker(NewTracker(registry, log.NewNOPFactory().Logger()))
	}
	if err = instance.Start(); err != nil {
		b.Fatalf("启动失败：%v", err)
	}
	testenv.WaitPort(b, "127.0.0.1", serverPort)

	clientPort := testenv.FreePort(b)
	testenv.StartUpstreamClient(b, silenceLog(vlessClientConfig(clientPort, serverPort, testUUID1, echoHost, echoPort)))

	if quota {
		// 额度给得足够大：衡量的是扣减本身，不是闸断后的短路。
		applied, unknown, applyErr := registry.ApplyQuotaTable(1, []QuotaEntry{
			{InboundTag: "vless-in", Name: "u1", RemainingBytes: 1 << 62},
		})
		if applyErr != nil || applied != 1 {
			b.Fatalf("下发配额失败：%v %v", applyErr, unknown)
		}
	}

	const chunk = 64 * 1024
	payload := make([]byte, chunk)
	echo := make([]byte, chunk)
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(int(clientPort)), 5*time.Second)
	if err != nil {
		b.Fatalf("连接失败：%v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))

	b.SetBytes(chunk)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err = conn.Write(payload); err != nil {
			b.Fatalf("写入失败：%v", err)
		}
		if _, err = io.ReadFull(conn, echo); err != nil {
			b.Fatalf("读回失败：%v", err)
		}
	}
}

func BenchmarkDataPathA_NoStats(b *testing.B)    { benchDataPath(b, false, false) }
func BenchmarkDataPathB_Stats(b *testing.B)      { benchDataPath(b, false, true) }
func BenchmarkDataPathC_StatsQuota(b *testing.B) { benchDataPath(b, true, true) }

func startEchoForBench(b *testing.B) (string, uint16) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("启动回声服务失败：%v", err)
	}
	b.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				buffer := make([]byte, 64*1024)
				for {
					n, readErr := conn.Read(buffer)
					if n > 0 {
						if _, writeErr := conn.Write(buffer[:n]); writeErr != nil {
							return
						}
					}
					if readErr != nil {
						return
					}
				}
			}()
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), uint16(addr.Port)
}

// silenceLog 关掉数据面日志。
//
// 基准的输出必须能被 benchstat 之类的工具直接吃掉；连接关闭时的 EOF 日志会把 ns/op 行冲散。
func silenceLog(configJSON string) string {
	return strings.Replace(configJSON, `"log": {"level": "error"}`, `"log": {"disabled": true}`, 1)
}
