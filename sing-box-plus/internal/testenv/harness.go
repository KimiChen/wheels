// Package testenv 为集成测试提供最小的「客户端 Box + 服务端 Box + 回声目标」拓扑。
//
// 单独成包而不是放在 _test.go 里，是因为客户端必须使用**上游** include registry
// （它要 vless/shadowsocks outbound 与 direct inbound），而被测服务端必须使用本项目的
// 最小 registry。两套 registry 混在同一个测试文件里极易写错，分开更难搞混。
package testenv

import (
	"context"
	"net"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/block"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing/common/json"
)

// ClientContext 构造测试客户端用的 registry。
//
// 刻意**不用** include.Context()：它会把全部协议连同 anytls / shadowtls / snell 等模块拖进
// 本项目的 go.sum，既放大依赖面，也让「非白名单类型在解码期被拒」这条门禁在测试里失效。
func ClientContext(ctx context.Context) context.Context {
	inboundRegistry := inbound.NewRegistry()
	direct.RegisterInbound(inboundRegistry)

	outboundRegistry := outbound.NewRegistry()
	direct.RegisterOutbound(outboundRegistry)
	block.RegisterOutbound(outboundRegistry)
	vless.RegisterOutbound(outboundRegistry)
	shadowsocks.RegisterOutbound(outboundRegistry)

	dnsRegistry := dns.NewTransportRegistry()
	transport.RegisterUDP(dnsRegistry)
	transport.RegisterTCP(dnsRegistry)
	local.RegisterTransport(dnsRegistry)

	return box.Context(ctx, inboundRegistry, outboundRegistry, endpoint.NewRegistry(),
		dnsRegistry, boxService.NewRegistry(), certificate.NewRegistry())
}

// FreePort 返回一个当前空闲的本机端口。
func FreePort(t testing.TB) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配端口失败：%v", err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	return port
}

// StartTCPEcho 启动一个回声 TCP 服务，作为「目标」。
func StartTCPEcho(t testing.TB) (addr string, port uint16) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 TCP 回声服务失败：%v", err)
	}
	t.Cleanup(func() { listener.Close() })
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
	tcpAddr := listener.Addr().(*net.TCPAddr)
	return tcpAddr.IP.String(), uint16(tcpAddr.Port)
}

// StartUDPEcho 启动一个回声 UDP 服务。
func StartUDPEcho(t testing.TB) (addr string, port uint16) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 UDP 回声服务失败：%v", err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buffer := make([]byte, 64*1024)
		for {
			n, from, readErr := conn.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			_, _ = conn.WriteTo(buffer[:n], from)
		}
	}()
	udpAddr := conn.LocalAddr().(*net.UDPAddr)
	return udpAddr.IP.String(), uint16(udpAddr.Port)
}

// StartUpstreamClient 用**上游** registry 启动一个客户端 Box。
//
// 客户端用 direct inbound 做端口转发：任何到该端口的 TCP/UDP 都经指定 outbound 转到目标，
// 测试侧因此不需要实现任何代理协议客户端。
func StartUpstreamClient(t testing.TB, config string) {
	t.Helper()
	ctx := ClientContext(context.Background())
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(config))
	if err != nil {
		t.Fatalf("解析客户端配置失败：%v", err)
	}
	instanceCtx, cancel := context.WithCancel(ctx)
	instance, err := box.New(box.Options{Context: instanceCtx, Options: options})
	if err != nil {
		cancel()
		t.Fatalf("创建客户端失败：%v", err)
	}
	if err = instance.Start(); err != nil {
		cancel()
		t.Fatalf("启动客户端失败：%v", err)
	}
	t.Cleanup(func() {
		instance.Close()
		cancel()
	})
	WaitPort(t, "127.0.0.1", clientListenPort(options))
}

func clientListenPort(options option.Options) uint16 {
	for _, inbound := range options.Inbounds {
		if typed, ok := inbound.Options.(*option.DirectInboundOptions); ok {
			return typed.ListenPort
		}
	}
	return 0
}

// WaitPort 等待端口可连接，避免用固定 sleep 制造 flaky 测试。
func WaitPort(t testing.TB, host string, port uint16) {
	t.Helper()
	if port == 0 {
		return
	}
	address := net.JoinHostPort(host, itoa(port))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待端口 %s 超时", address)
}

func itoa(value uint16) string {
	if value == 0 {
		return "0"
	}
	var digits [8]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}

func discardLogger() log.ContextLogger {
	return log.NewNOPFactory().Logger()
}

// Logger 返回一个静默 logger，避免测试输出被数据面日志淹没。
func Logger() log.ContextLogger { return discardLogger() }
