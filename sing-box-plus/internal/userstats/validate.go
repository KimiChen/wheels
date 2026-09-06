package userstats

import (
	"encoding/base64"
	"encoding/hex"
	"net/netip"
	"time"

	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
)

// ssMethodAllowlist 是硬编码白名单，不写成「shadowaead_2022.List 中的 method」。
//
// 2022-blake3-chacha20-poly1305 虽在该 List 中，但 NewMultiService 的 switch 只放行两个 AES
// 变体，配 users[] 启动即 invalid argument；method: none 配 users[] 则直接 unsupported method
// （README §2.4）。校验拒绝只是把上游的启动失败提前成一条明确报错。
var ssMethodAllowlist = map[string]struct{}{
	"2022-blake3-aes-128-gcm": {},
	"2022-blake3-aes-256-gcm": {},
}

// defaultListenAddr 是上游省略 listen 时的绑定地址。
var defaultListenAddr = netip.AddrFrom4([4]byte{127, 0, 0, 1})

// inboundTypeAllowlist 是 §2.4 的首期计费白名单。
var inboundTypeAllowlist = map[string]struct{}{
	"vless":       {},
	"shadowsocks": {},
}

// Config 是一次成功校验的产物，供 main 建 registry 与对账使用。
type Config struct {
	NodeID           string
	ListenPath       string
	QuotaListenPath  string
	MaxIdentities    int
	DrainTimeout     time.Duration
	Inbounds         []InboundSpec
	ServiceTag       string
	AccessLogEnabled bool
}

// Validate 实现 README §4.6 的全部校验路径。
//
// 统计是硬依赖而非可选旁路：任一条不满足即启动失败，不以部分覆盖或零归属模式运行。
// 全部检查都在 box.New() 之前独立完成——上游 sing-box check 不可作为唯一门禁
// （§4.6 第 10 条：实测 v1.14.0 在 ssm-api 的 servers 键缺前导 / 时会 panic 退出）。
func Validate(options option.Options) (*Config, error) {
	statsOptions, serviceTag, err := findStatsService(options)
	if err != nil {
		return nil, err
	}
	if statsOptions == nil {
		// 未配置 user_stats：不创建 registry、exporter 或任何附加包装，
		// 保持上游快路径与线协议不变（§4.6 第 6 条）。
		return nil, nil
	}
	if err = checkUpstreamLedgerExclusive(options); err != nil {
		return nil, err
	}
	if err = checkRouteActions(options); err != nil {
		return nil, err
	}
	if len(options.NetworkNamespaces) > 0 {
		// 上游把 netns holder 实现为隐藏子命令，自有 main 不复刻该子命令（§4.7）。
		return nil, E.New("启用统计时拒绝 network_namespaces：本项目不支持 netns holder 子命令")
	}

	inboundByTag := make(map[string]*option.Inbound, len(options.Inbounds))
	for index := range options.Inbounds {
		item := &options.Inbounds[index]
		if item.Tag == "" {
			return nil, E.New("inbounds[", index, "] 的 tag 为空：计费身份的 inbound tag 必须非空")
		}
		if _, duplicate := inboundByTag[item.Tag]; duplicate {
			return nil, E.New("inbound tag 在节点内重复：", item.Tag)
		}
		inboundByTag[item.Tag] = item
	}

	config := &Config{
		NodeID:           statsOptions.NodeID,
		ListenPath:       statsOptions.ListenPath,
		MaxIdentities:    statsOptions.MaxIdentities,
		DrainTimeout:     time.Duration(statsOptions.DrainTimeout),
		ServiceTag:       serviceTag,
		AccessLogEnabled: statsOptions.AccessLog != nil,
	}
	if statsOptions.QuotaControl != nil {
		config.QuotaListenPath = statsOptions.QuotaControl.ListenPath
	}
	for _, tag := range statsOptions.Inbounds {
		item, loaded := inboundByTag[tag]
		if !loaded {
			return nil, E.New("user_stats.inbounds 指向不存在的 inbound：", tag)
		}
		spec, specErr := validateBilledInbound(item)
		if specErr != nil {
			return nil, specErr
		}
		config.Inbounds = append(config.Inbounds, *spec)
	}
	return config, nil
}

func findStatsService(options option.Options) (*Options, string, error) {
	var found *Options
	var tag string
	for index, item := range options.Services {
		// 与 SSM API 互斥的第二层：解析成功后遍历 services，命中 SSM 类型即失败。
		// 不得改用「inbound 上有 managed: true」作为判据——service/ssmapi/server.go 只做
		// adapter.ManagedSSMServer 类型断言后 SetTracker，从不读 Managed 字段（§4.6 第 8 条）。
		if item.Type == "ssm-api" {
			return nil, "", E.New("services[", index, "] 是 ssm-api：与 user_stats 互斥（README §4.6 第 8 条）")
		}
		if item.Type != TypeUserStats {
			continue
		}
		if found != nil {
			return nil, "", E.New("配置中出现多个 user_stats 服务：只允许一个")
		}
		typed, ok := item.Options.(*Options)
		if !ok {
			return nil, "", E.New("services[", index, "] 的 user_stats 选项类型异常")
		}
		found = typed
		tag = item.Tag
	}
	if found == nil {
		return nil, "", nil
	}
	if err := found.normalize(); err != nil {
		return nil, "", err
	}
	return found, tag, nil
}

// checkUpstreamLedgerExclusive 实现 §4.6 第 7 条：与上游连接账本互斥。
//
// 全树仅 box.go:249 与 box.go:438 两处上游 AppendTracker，校验覆盖且仅覆盖这两条路径。
// 裁 tag 挡不住这一条：common/trafficcontrol 被 box.go:24 无条件 import，
// service/api 在 include/registry.go:146 无 build 约束地注册。
func checkUpstreamLedgerExclusive(options option.Options) error {
	for index, item := range options.Services {
		if item.Type == "api" {
			return E.New("services[", index, "] 是 api：会让上游 AppendTracker 与本项目 tracker 叠加（§4.6 第 7 条）")
		}
	}
	if options.Experimental == nil {
		return nil
	}
	if options.Experimental.ClashAPI != nil {
		return E.New("启用统计时拒绝 experimental.clash_api（§4.6 第 7 条）")
	}
	if options.Experimental.V2RayAPI != nil && options.Experimental.V2RayAPI.Listen != "" {
		return E.New("启用统计时拒绝 experimental.v2ray_api.listen（§4.6 第 7 条）")
	}
	return nil
}

// checkRouteActions 拒绝 hijack-dns 与 reject 两个动作。
//
// 它们在 route/route.go 的 tracker 循环之前 return，字节不经 tracker（§2.3）。
// 需同时读 DefaultRule 与 LogicalRule 的 RuleAction；**不需要递归遍历** logical 的 rules[]
// ——上游在反序列化期就拒绝嵌套规则携带任何 action 键，动作只可能出现在顶层。
func checkRouteActions(options option.Options) error {
	if options.Route == nil {
		return nil
	}
	for index, rule := range options.Route.Rules {
		action := rule.DefaultOptions.Action
		if rule.Type == "logical" {
			action = rule.LogicalOptions.Action
		}
		switch action {
		case "reject", "hijack-dns":
			return E.New("route.rules[", index, "] 的 action 是 ", action, "：该动作绕过 tracker，字节不入账（§2.3、§4.6 第 2 条）")
		}
	}
	return nil
}

func validateBilledInbound(item *option.Inbound) (*InboundSpec, error) {
	if _, allowed := inboundTypeAllowlist[item.Type]; !allowed {
		return nil, E.New("inbound ", item.Tag, " 的类型 ", item.Type, " 不在首期计费白名单内（§2.4）")
	}
	if err := validateToken("inbound tag", item.Tag); err != nil {
		return nil, err
	}
	spec := &InboundSpec{Tag: item.Tag, Type: item.Type}
	switch typed := item.Options.(type) {
	case *option.VLESSInboundOptions:
		if err := validateListen(item.Tag, typed.ListenOptions, spec); err != nil {
			return nil, err
		}
		if len(typed.Users) == 0 {
			return nil, E.New("inbound ", item.Tag, " 的用户集为空")
		}
		seen := make(map[string]struct{}, len(typed.Users))
		for index, user := range typed.Users {
			if err := validateBillingName(item.Tag, index, user.Name, seen); err != nil {
				return nil, err
			}
			spec.Users = append(spec.Users, user.Name)
		}
	case *option.ShadowsocksInboundOptions:
		if err := validateShadowsocks(item.Tag, typed, spec); err != nil {
			return nil, err
		}
	default:
		return nil, E.New("inbound ", item.Tag, " 的选项类型无法识别为可计费形态")
	}
	return spec, nil
}

func validateListen(tag string, listen option.ListenOptions, spec *InboundSpec) error {
	// listen_port 必须显式配置且在 1..=65535：为 0 或缺省时上游把 0 交给内核分配临时端口，
	// 且同一 inbound 的 TCP 与 UDP 会拿到两个不同的端口，单一 listen_port 字段连表达都做不到；
	// 而零补丁 wrapper 无法回读实际绑定端口，快照必然与实际不符（§4.6 第 2 条）。
	if listen.ListenPort == 0 {
		return E.New("inbound ", tag, " 的 listen_port 缺省或为 0：快照将与实际绑定不符")
	}
	if listen.Detour != "" {
		// 通用规则要求以 detour 末端 inbound 为计费身份；首期不实现该归属，直接拒绝。
		return E.New("inbound ", tag, " 配置了 listen.detour：首期不支持 detour 归属（§2.4）")
	}
	// 配置省略 listen 时上游按 127.0.0.1 绑定而非 0.0.0.0，快照必须按同一默认补齐后输出。
	spec.Listen = listen.Listen.Build(defaultListenAddr).String()
	spec.ListenPort = listen.ListenPort
	return nil
}

func validateBillingName(tag string, index int, name string, seen map[string]struct{}) error {
	if name == "" {
		// 上游在 name == "" 时只把索引写进日志、metadata.User 保持空串，
		// 且 newMultiInbound 全程不校验 name。「至少一个具名用户」覆盖不到「多用户中某一项无名」。
		return E.New("inbound ", tag, " 的 users[", index, "].name 为空")
	}
	if err := validateToken("users[].name", name); err != nil {
		return err
	}
	if _, duplicate := seen[name]; duplicate {
		return E.New("inbound ", tag, " 的 users[].name 重复：", name)
	}
	seen[name] = struct{}{}
	return nil
}

func validateShadowsocks(tag string, typed *option.ShadowsocksInboundOptions, spec *InboundSpec) error {
	if err := validateListen(tag, typed.ListenOptions, spec); err != nil {
		return err
	}
	if typed.Managed {
		// 启动时零具名用户，用户集由 SSM 运行时整体替换 h.users，与稳定 lineage 直接冲突，
		// 且该替换与数据面读取无锁（§2.4）。
		return E.New("inbound ", tag, " 是 managed Shadowsocks：与稳定计费身份冲突，拒绝")
	}
	if len(typed.Destinations) > 0 {
		// relay 的 metadata.User 是 destinations[].name，是中继目的地不是终端用户，
		// 且字节口径不是应用 payload（§2.4）。
		return E.New("inbound ", tag, " 是 Shadowsocks relay（destinations 非空）：不可按用户计费")
	}
	if len(typed.Users) == 0 {
		return E.New("inbound ", tag, " 是单用户 Shadowsocks：全程不写 metadata.User，不可按用户计费")
	}
	if _, allowed := ssMethodAllowlist[typed.Method]; !allowed {
		return E.New("inbound ", tag, " 的 method ", typed.Method,
			" 不在计费白名单内：只接受 2022-blake3-aes-128-gcm 与 2022-blake3-aes-256-gcm（§2.4）")
	}
	seenName := make(map[string]struct{}, len(typed.Users))
	seenKey := make(map[string]int, len(typed.Users))
	keyLength := 16
	if typed.Method == "2022-blake3-aes-256-gcm" {
		keyLength = 32
	}
	for index, user := range typed.Users {
		if err := validateBillingName(tag, index, user.Name, seenName); err != nil {
			return err
		}
		// uPSK 必须按归一化后的字节两两不同，而不是按 password 字符串比较：
		// 上游 UpdateUsers 以 uPSKHash[hash]=user 覆盖建表且无冲突检测，
		// 重复 uPSK 会把两个计费身份静默合并到配置中靠后的那一项（§4.6 第 2 条）。
		key, err := base64.StdEncoding.DecodeString(user.Password)
		if err != nil {
			return E.New("inbound ", tag, " 的 users[", index, "].password 不是合法 Base64")
		}
		if len(key) != keyLength {
			// 超长 uPSK 上游会派生密钥并启动成功，但任何标准客户端都连不上
			// （outbound 报 bad key length），属静默不可用配置。
			return E.New("inbound ", tag, " 的 users[", index, "] uPSK 长度为 ", len(key),
				" 字节，", typed.Method, " 要求 ", keyLength, " 字节")
		}
		fingerprint := hex.EncodeToString(key)
		if previous, duplicate := seenKey[fingerprint]; duplicate {
			return E.New("inbound ", tag, " 的 users[", index, "] 与 users[", previous,
				"] 归一化后 uPSK 相同：上游会静默合并两个计费身份")
		}
		seenKey[fingerprint] = index
		spec.Users = append(spec.Users, user.Name)
	}
	return nil
}

// ScanRawConfig 在解码之前扫描原始 JSON。
//
// 两个用途：(1) 未编译 with_user_stats 时该类型未注册，上游解码失败的文案把 service 误写成
// "inbound"，须替换为可读错误；(2) ssm-api 未注册，解析期即失败，错误文本不可依赖，
// 因此在解码前先扫描 services[].type（§4.6 第 3 条、第 8 条第一层）。
func ScanRawConfig(content []byte, statsRegistered bool) error {
	var probe struct {
		Services []struct {
			Type string `json:"type"`
		} `json:"services"`
	}
	if err := json.Unmarshal(content, &probe); err != nil {
		// 交给上游解码器给出精确的语法错误。
		return nil
	}
	for index, item := range probe.Services {
		switch item.Type {
		case "ssm-api":
			return E.New("services[", index, "] 是 ssm-api：本项目的 service registry 不注册该类型，与 user_stats 互斥（README §4.6 第 8 条）")
		case TypeUserStats:
			if !statsRegistered {
				return E.New("配置中出现 services[", index, "].type=user_stats，但本二进制未启用 with_user_stats 构建 tag")
			}
		}
	}
	return nil
}

// CheckReloadInvariant 实现 §4.6 第 11 条。
//
// user_stats 的 node_id、listen_path 与 quota_control.listen_path 必须与**进程启动时**的取值
// 逐字节相同。任一变化即在重载的配置检查阶段判定失败：**拒绝本次重载、保留旧 Box 继续服务
// 并告警，不得以进程退出处置**——上游在 check() 失败时是记错误后 continue，此时尚未 cancel()
// 与 Close()；改成退出反而会丢掉未采集的尾账。
func CheckReloadInvariant(process *Config, next *Config) error {
	if process == nil || next == nil {
		return nil
	}
	if process.NodeID != next.NodeID {
		return E.New("重载不得改变 user_stats.node_id：", process.NodeID, " -> ", next.NodeID,
			"（同一 runtime_id 内换 node_id 会一次性重置全部采集守卫）")
	}
	if process.ListenPath != next.ListenPath {
		return E.New("重载不得改变 user_stats.listen_path：", process.ListenPath, " -> ", next.ListenPath,
			"（采集端会直接失联）")
	}
	if process.QuotaListenPath != next.QuotaListenPath {
		return E.New("重载不得改变 user_stats.quota_control.listen_path：",
			process.QuotaListenPath, " -> ", next.QuotaListenPath, "（控制面会直接失联）")
	}
	return nil
}
