package userstats

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"
)

func decodeOptions(t *testing.T, configJSON string) (option.Options, error) {
	t.Helper()
	ctx := serverContext(context.Background())
	return singjson.UnmarshalExtendedContext[option.Options](ctx, []byte(configJSON))
}

// mustReject 断言配置在**解码期或校验期**被拒绝，并且错误信息包含指定片段。
func mustReject(t *testing.T, name string, configJSON string, fragment string) {
	t.Helper()
	options, err := decodeOptions(t, configJSON)
	if err != nil {
		if fragment != "" && !strings.Contains(err.Error(), fragment) {
			t.Fatalf("%s：解码期报错但信息不含 %q：%v", name, fragment, err)
		}
		return
	}
	_, err = Validate(options)
	if err == nil {
		t.Fatalf("%s：配置本应被拒绝，实际通过", name)
	}
	if fragment != "" && !strings.Contains(err.Error(), fragment) {
		t.Fatalf("%s：错误信息不含 %q：%v", name, fragment, err)
	}
}

func statsService(extra string) string {
	if extra != "" {
		extra = ", " + extra
	}
	return fmt.Sprintf(`{"type":"user_stats","tag":"stats","node_id":"n1","listen_path":"/tmp/x.sock","inbounds":["in"]%s}`, extra)
}

func configWith(inbound string, services string, extra string) string {
	if extra != "" {
		extra = ",\n  " + extra
	}
	return fmt.Sprintf(`{
  "inbounds": [%s],
  "outbounds": [{"type":"direct","tag":"out"}],
  "services": [%s]%s
}`, inbound, services, extra)
}

const vlessOK = `{"type":"vless","tag":"in","listen":"127.0.0.1","listen_port":8443,"users":[{"name":"u1","uuid":"11111111-1111-4111-8111-111111111111"}]}`

// TestValidateAcceptsWhitelist 是正例：白名单形态必须通过，并把 listen/listen_port 原样带出。
func TestValidateAcceptsWhitelist(t *testing.T) {
	options, err := decodeOptions(t, configWith(vlessOK, statsService(""), ""))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	config, err := Validate(options)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if len(config.Inbounds) != 1 {
		t.Fatalf("应有 1 个计费 inbound，实际 %d", len(config.Inbounds))
	}
	spec := config.Inbounds[0]
	if spec.Listen != "127.0.0.1" || spec.ListenPort != 8443 || spec.Type != "vless" {
		t.Fatalf("inbound 规格不符：%+v", spec)
	}
	if len(spec.Users) != 1 || spec.Users[0] != "u1" {
		t.Fatalf("用户集不符：%+v", spec.Users)
	}
}

// TestValidateOmittedListenDefaults 断言省略 listen 时快照按上游默认补 127.0.0.1 而非 0.0.0.0。
func TestValidateOmittedListenDefaults(t *testing.T) {
	inbound := `{"type":"vless","tag":"in","listen_port":8443,"users":[{"name":"u1","uuid":"11111111-1111-4111-8111-111111111111"}]}`
	options, err := decodeOptions(t, configWith(inbound, statsService(""), ""))
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	config, err := Validate(options)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if config.Inbounds[0].Listen != "127.0.0.1" {
		t.Fatalf("省略 listen 时应补 127.0.0.1，实际 %q", config.Inbounds[0].Listen)
	}
}

// TestValidateRejections 覆盖 README §4.6 的失败关闭路径。
func TestValidateRejections(t *testing.T) {
	ss := func(body string) string {
		return `{"type":"shadowsocks","tag":"in","listen":"127.0.0.1","listen_port":8388,` + body + `}`
	}
	cases := []struct {
		name     string
		config   string
		fragment string
	}{
		{
			"非白名单 inbound 类型",
			configWith(`{"type":"trojan","tag":"in","listen":"127.0.0.1","listen_port":443,"users":[{"name":"u1","password":"p"}]}`, statsService(""), ""),
			"", // 最小 registry 在解码期即拒绝
		},
		{
			"listen_port 缺省",
			configWith(`{"type":"vless","tag":"in","listen":"127.0.0.1","users":[{"name":"u1","uuid":"11111111-1111-4111-8111-111111111111"}]}`, statsService(""), ""),
			"listen_port",
		},
		{
			"listen.detour 非空",
			configWith(`{"type":"vless","tag":"in","listen":"127.0.0.1","listen_port":8443,"detour":"other","users":[{"name":"u1","uuid":"11111111-1111-4111-8111-111111111111"}]}`, statsService(""), ""),
			"detour",
		},
		{
			"用户集为空",
			configWith(`{"type":"vless","tag":"in","listen":"127.0.0.1","listen_port":8443}`, statsService(""), ""),
			"用户集为空",
		},
		{
			"users[].name 为空",
			configWith(`{"type":"vless","tag":"in","listen":"127.0.0.1","listen_port":8443,"users":[{"name":"","uuid":"11111111-1111-4111-8111-111111111111"}]}`, statsService(""), ""),
			"name 为空",
		},
		{
			"users[].name 重复",
			configWith(`{"type":"vless","tag":"in","listen":"127.0.0.1","listen_port":8443,"users":[{"name":"u1","uuid":"11111111-1111-4111-8111-111111111111"},{"name":"u1","uuid":"22222222-2222-4222-8222-222222222222"}]}`, statsService(""), ""),
			"重复",
		},
		{
			"Shadowsocks 单用户形态",
			configWith(ss(`"method":"2022-blake3-aes-128-gcm","password":"AQIDBAUGBwgJCgsMDQ4PEA=="`), statsService(""), ""),
			"单用户",
		},
		{
			"Shadowsocks legacy AEAD",
			configWith(ss(`"method":"aes-128-gcm","password":"x","users":[{"name":"s1","password":"y"}]`), statsService(""), ""),
			"不在计费白名单",
		},
		{
			"Shadowsocks 2022-chacha 变体",
			configWith(ss(`"method":"2022-blake3-chacha20-poly1305","password":"AQIDBAUGBwgJCgsMDQ4PEA==","users":[{"name":"s1","password":"EA8ODQwLCgkIBwYFBAMCAQ=="}]`), statsService(""), ""),
			"不在计费白名单",
		},
		{
			"Shadowsocks relay",
			configWith(ss(`"method":"2022-blake3-aes-128-gcm","password":"AQIDBAUGBwgJCgsMDQ4PEA==","destinations":[{"name":"d1","password":"EA8ODQwLCgkIBwYFBAMCAQ==","server":"127.0.0.1","server_port":1}]`), statsService(""), ""),
			"relay",
		},
		{
			"Shadowsocks managed",
			configWith(ss(`"method":"2022-blake3-aes-128-gcm","password":"AQIDBAUGBwgJCgsMDQ4PEA==","managed":true,"users":[{"name":"s1","password":"EA8ODQwLCgkIBwYFBAMCAQ=="}]`), statsService(""), ""),
			"managed",
		},
		{
			"uPSK 归一化后重复",
			configWith(ss(`"method":"2022-blake3-aes-128-gcm","password":"AQIDBAUGBwgJCgsMDQ4PEA==","users":[{"name":"s1","password":"AAAAAAAAAAAAAAAAAAAAAA=="},{"name":"s2","password":"AAAAAAAAAAAAAAAAAAAAAB=="}]`), statsService(""), ""),
			"uPSK 相同",
		},
		{
			"uPSK 长度不符",
			configWith(ss(`"method":"2022-blake3-aes-128-gcm","password":"AQIDBAUGBwgJCgsMDQ4PEA==","users":[{"name":"s1","password":"AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHw=="}]`), statsService(""), ""),
			"uPSK 长度",
		},
		{
			"services 含 api",
			configWith(vlessOK, statsService("")+`,{"type":"api","tag":"api"}`, ""),
			"api",
		},
		{
			"services 含 ssm-api",
			configWith(vlessOK, statsService("")+`,{"type":"ssm-api","tag":"ssm"}`, ""),
			"",
		},
		{
			"experimental.clash_api",
			configWith(vlessOK, statsService(""), `"experimental": {"clash_api": {"external_controller": "127.0.0.1:9090"}}`),
			"clash_api",
		},
		{
			"route.rules 含 reject",
			configWith(vlessOK, statsService(""), `"route": {"rules": [{"action": "reject"}]}`),
			"reject",
		},
		{
			"route.rules 含 hijack-dns",
			configWith(vlessOK, statsService(""), `"route": {"rules": [{"action": "hijack-dns"}]}`),
			"hijack-dns",
		},
		{
			"user_stats.inbounds 指向不存在的 inbound",
			configWith(`{"type":"vless","tag":"other","listen":"127.0.0.1","listen_port":8443,"users":[{"name":"u1","uuid":"11111111-1111-4111-8111-111111111111"}]}`, statsService(""), ""),
			"不存在的 inbound",
		},
		{
			"user_stats.inbounds 为空",
			configWith(vlessOK, `{"type":"user_stats","tag":"stats","node_id":"n1","listen_path":"/tmp/x.sock","inbounds":[]}`, ""),
			"inbounds 不能为空",
		},
		{
			"user_stats 未知字段",
			configWith(vlessOK, `{"type":"user_stats","tag":"stats","node_id":"n1","listen_path":"/tmp/x.sock","inbounds":["in"],"unknown_key":1}`, ""),
			"",
		},
		{
			"quota_control 与快照共用 socket",
			configWith(vlessOK, statsService(`"quota_control":{"listen_path":"/tmp/x.sock"}`), ""),
			"不得与 listen_path 相同",
		},
		{
			"quota_control.startup_action 非法",
			configWith(vlessOK, statsService(`"quota_control":{"listen_path":"/tmp/q.sock","startup_action":"maybe"}`), ""),
			"startup_action",
		},
		{
			"network_namespaces 非空",
			configWith(vlessOK, statsService(""), `"network_namespaces": [{"name": "ns1", "path": "/var/run/netns/ns1"}]`),
			"network_namespaces",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mustReject(t, testCase.name, testCase.config, testCase.fragment)
		})
	}
}

// TestScanRawConfig 覆盖解码前的原始 JSON 预扫描。
func TestScanRawConfig(t *testing.T) {
	ssm := []byte(`{"services":[{"type":"ssm-api","tag":"a"}]}`)
	if err := ScanRawConfig(ssm, true); err == nil {
		t.Fatal("含 ssm-api 的配置应在预扫描期被拒")
	}
	stats := []byte(`{"services":[{"type":"user_stats","tag":"a"}]}`)
	if err := ScanRawConfig(stats, false); err == nil {
		t.Fatal("未编译 with_user_stats 时应给出可读错误")
	} else if !strings.Contains(err.Error(), "with_user_stats") {
		t.Fatalf("错误信息应点明缺少构建 tag：%v", err)
	}
	if err := ScanRawConfig(stats, true); err != nil {
		t.Fatalf("已注册时不应报错：%v", err)
	}
}

// TestCheckReloadInvariant 覆盖 §4.6 第 11 条。
func TestCheckReloadInvariant(t *testing.T) {
	base := &Config{NodeID: "n1", ListenPath: "/run/a.sock", QuotaListenPath: "/run/q.sock"}
	same := &Config{NodeID: "n1", ListenPath: "/run/a.sock", QuotaListenPath: "/run/q.sock"}
	if err := CheckReloadInvariant(base, same); err != nil {
		t.Fatalf("相同取值不应报错：%v", err)
	}
	for name, next := range map[string]*Config{
		"node_id":                   {NodeID: "n2", ListenPath: "/run/a.sock", QuotaListenPath: "/run/q.sock"},
		"listen_path":               {NodeID: "n1", ListenPath: "/run/b.sock", QuotaListenPath: "/run/q.sock"},
		"quota_control.listen_path": {NodeID: "n1", ListenPath: "/run/a.sock", QuotaListenPath: "/run/z.sock"},
	} {
		if err := CheckReloadInvariant(base, next); err == nil {
			t.Fatalf("重载改变 %s 应被拒绝", name)
		}
	}
}
