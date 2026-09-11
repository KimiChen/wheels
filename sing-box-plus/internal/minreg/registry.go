// Package minreg 构造本项目自有的最小 registry 集合。
//
// 不使用 include.Context()／include.*Registry()：前者内联自建全部 registry，调用方拿不到句柄，
// 既无法注册自有 user_stats 服务类型，也无法剔除 ssmapi（README §4.6 第 8 条第一层）；
// 后者无条件注册全部协议，把 VMess、Trojan、TUN、naive、ssh、tor、anytls、snell 等
// 非白名单类型一并编进二进制（README §6「registry 裁剪比裁 tag 更能瘦身」）。
//
// 这里的裁剪是**类型级**门禁：它能让非白名单 inbound 在配置解码期就失败，但拦不住
// Shadowsocks 的 relay／单用户／managed 三种形态——三者都注册为同一个 shadowsocks 类型，
// 在 NewInbound 内部按字段分派。README §4.6 第 2 条的逐项校验仍须自行实现，见 userstats.Validate。
package minreg

import (
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/protocol/block"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/protocol/vless"
)

// InboundRegistry 只注册 README §2.4 的首期计费白名单类型。
func InboundRegistry() *inbound.Registry {
	registry := inbound.NewRegistry()
	vless.RegisterInbound(registry)
	shadowsocks.RegisterInbound(registry)
	return registry
}

// OutboundRegistry 只保留转发与阻断两个终点。
func OutboundRegistry() *outbound.Registry {
	registry := outbound.NewRegistry()
	direct.RegisterOutbound(registry)
	block.RegisterOutbound(registry)
	return registry
}

// EndpointRegistry 为空：本项目不支持 WireGuard／Tailscale／OpenVPN 等 endpoint 形态。
func EndpointRegistry() *endpoint.Registry {
	return endpoint.NewRegistry()
}

// DNSTransportRegistry 只保留 udp／tcp／local 三种。
//
// 丢掉的 tls／https／hosts／mdns／fakeip／resolved 六个 transport 若生产配置需要，
// 按 README §6 末段的清单逐项补注册；补注册会同时放大二进制体积与攻击面，须走决策记录。
func DNSTransportRegistry() *dns.TransportRegistry {
	registry := dns.NewTransportRegistry()
	transport.RegisterUDP(registry)
	transport.RegisterTCP(registry)
	local.RegisterTransport(registry)
	return registry
}

// CertificateProviderRegistry 为空：不支持 ACME／Tailscale／origin-ca 证书提供方。
func CertificateProviderRegistry() *certificate.Registry {
	return certificate.NewRegistry()
}
