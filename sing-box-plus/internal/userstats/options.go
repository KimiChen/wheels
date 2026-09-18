package userstats

import (
	"os/user"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json/badoption"
)

// TypeUserStats 是本项目自有的 services[] 类型名。
//
// 走 services[] 而不是顶层键：option.Options 对顶层未知字段硬失败
// （option/options.go:41 的 decoder.DisallowUnknownFields()），零补丁形态下顶层 user_stats 键
// 不可行；而 Register[Options] 让自有字段自动获得「未知字段即失败」与 format 往返保真
// （README §4.6 第 1 条）。
const TypeUserStats = "user_stats"

// 默认值。全部可在配置中显式覆盖，但不会静默回落——未知字段仍然硬失败。
const (
	defaultMaxIdentities     = 4096
	defaultReadTimeout       = 5 * time.Second
	defaultWriteTimeout      = 5 * time.Second
	defaultMaxConcurrency    = 8
	defaultMaxRequestBytes   = 64 * 1024
	defaultQuotaRequestBytes = 4 * 1024 * 1024
	defaultAuditQueueSize    = 8192
	defaultAuditFlushMs      = 1000
	// 逐用户分文件之后单个文件小得多：实测每个身份约 1.4 MB/天。
	// 沿用 256 MiB 会让一个身份半年才轮转一次，等于纪律 9/10 在生产上从不触发。
	defaultAuditMaxBytes     = 16 * 1024 * 1024
	defaultAuditMaxOpenFiles = 256

	// maxUnixPathBytes 取两个平台里更严格的那个，使同一份配置在 Linux 与 darwin 上行为一致。
	// 定义在这里而不是 listen_unix.go：后者带 //go:build unix，而本文件无 build tag，
	// 常量放在那边会让 !unix 构建直接编译失败，listen_other.go 里那条「明确报错」永远到不了。
	maxUnixPathBytes = 103
)

// Options 是 user_stats 服务的配置。
type Options struct {
	NodeID        string `json:"node_id"`
	ListenPath    string `json:"listen_path"`
	SocketGroup   string `json:"socket_group,omitempty"`
	SocketMode    string `json:"socket_mode,omitempty"`
	MaxIdentities int    `json:"max_identities,omitempty"`

	// Inbounds 是被统计的 inbound tag 名单（README §4.6 末段）。
	// 名单为空即整个统计不成立，因此 normalize 要求至少一项。
	Inbounds []string `json:"inbounds"`

	// DrainTimeout 为 0 表示不排空，保持上游的「收到信号即关」语义。
	DrainTimeout badoption.Duration `json:"drain_timeout,omitempty"`

	ReadTimeout     badoption.Duration `json:"read_timeout,omitempty"`
	WriteTimeout    badoption.Duration `json:"write_timeout,omitempty"`
	MaxConcurrency  int                `json:"max_concurrency,omitempty"`
	MaxRequestBytes int                `json:"max_request_bytes,omitempty"`

	AccessLog    *AccessLogOptions    `json:"access_log,omitempty"`
	QuotaControl *QuotaControlOptions `json:"quota_control,omitempty"`
}

// AccessLogOptions 是 §4.8 基础访问审计的配置，缺省即整条旁路不存在。
//
// 逐计费身份一个文件（数据主体分离）：文件名是 access-<base64url(身份名)>.jsonl，
// 落在 directory 下。目录必须已存在——属主与权限是部署决策，由 tmpfiles/StateDirectory 负责。
type AccessLogOptions struct {
	Directory       string `json:"directory"`
	MaxBytes        int64  `json:"max_bytes,omitempty"`
	MaxTotalBytes   int64  `json:"max_total_bytes,omitempty"`
	FlushIntervalMs int    `json:"flush_interval_ms,omitempty"`
	QueueSize       int    `json:"queue_size,omitempty"`
	MaxOpenFiles    int    `json:"max_open_files,omitempty"`

	// ExcludeHosts 是域名后缀名单，ExcludeIPs 是网段/地址名单。命中的连接不写审计。
	//
	// 这两项**只影响审计**：计数器与配额扣减在 connState 里无条件执行，
	// 被排掉的流量照样计费、照样扣额度。换来的代价是审计不再是全量，
	// 出现用量争议时解释不了被排掉的那部分——取舍见 docs/ACCESS_AUDIT.md。
	ExcludeHosts []string `json:"exclude_hosts,omitempty"`
	ExcludeIPs   []string `json:"exclude_ips,omitempty"`
}

// QuotaControlOptions 是 §4.9 配额闸断的配置，缺省即整个能力关闭。
type QuotaControlOptions struct {
	ListenPath        string             `json:"listen_path"`
	StartupAction     string             `json:"startup_action,omitempty"`
	StaleAfter        badoption.Duration `json:"stale_after,omitempty"`
	StaleAction       string             `json:"stale_action,omitempty"`
	ReconnectThrottle badoption.Duration `json:"reconnect_throttle,omitempty"`
	MaxRequestBytes   int                `json:"max_request_bytes,omitempty"`
}

// normalize 补齐默认值并校验取值范围。不做静默回落：非法取值一律报错。
func (o *Options) normalize() error {
	if err := validateToken("node_id", o.NodeID); err != nil {
		return err
	}
	if err := validateSocketPath("user_stats.listen_path", o.ListenPath); err != nil {
		return err
	}
	if len(o.Inbounds) == 0 {
		return E.New("user_stats.inbounds 不能为空：统计是硬依赖，不接受零归属模式")
	}
	seenInbound := make(map[string]struct{}, len(o.Inbounds))
	for _, tag := range o.Inbounds {
		if err := validateToken("user_stats.inbounds[]", tag); err != nil {
			return err
		}
		if _, duplicate := seenInbound[tag]; duplicate {
			return E.New("user_stats.inbounds 出现重复 tag：", tag)
		}
		seenInbound[tag] = struct{}{}
	}
	if o.MaxIdentities == 0 {
		o.MaxIdentities = defaultMaxIdentities
	}
	if o.MaxIdentities < 0 {
		return E.New("user_stats.max_identities 必须为正数")
	}
	if time.Duration(o.DrainTimeout) < 0 {
		return E.New("user_stats.drain_timeout 不能为负")
	}
	if time.Duration(o.ReadTimeout) == 0 {
		o.ReadTimeout = badoption.Duration(defaultReadTimeout)
	}
	if time.Duration(o.WriteTimeout) == 0 {
		o.WriteTimeout = badoption.Duration(defaultWriteTimeout)
	}
	if o.MaxConcurrency == 0 {
		o.MaxConcurrency = defaultMaxConcurrency
	}
	if o.MaxConcurrency < 1 {
		return E.New("user_stats.max_concurrency 必须为正数")
	}
	if o.MaxRequestBytes == 0 {
		o.MaxRequestBytes = defaultMaxRequestBytes
	}
	if o.MaxRequestBytes < 256 {
		return E.New("user_stats.max_request_bytes 过小")
	}
	if o.SocketMode != "" && o.SocketMode != "0600" && o.SocketMode != "0660" {
		return E.New("user_stats.socket_mode 只接受 0600 或 0660，实际：", o.SocketMode)
	}
	if o.SocketGroup != "" {
		// 0600 下组权限位全关，设了组也授予不了任何访问——这正是「看起来生效、实际没生效」，
		// 按 §4.6 不做静默回落，直接拒绝这个组合。
		if o.SocketMode != "0660" {
			actual := o.SocketMode
			if actual == "" {
				actual = "0600（默认）"
			}
			return E.New("user_stats.socket_group 需要 socket_mode=0660，实际：", actual)
		}
		if _, err := user.LookupGroup(o.SocketGroup); err != nil {
			return E.Cause(err, "user_stats.socket_group 无法解析：", o.SocketGroup)
		}
	}
	if o.AccessLog != nil {
		if err := o.AccessLog.normalize(); err != nil {
			return err
		}
	}
	if o.QuotaControl != nil {
		if err := o.QuotaControl.normalize(); err != nil {
			return err
		}
		if o.QuotaControl.ListenPath == o.ListenPath {
			// 只读快照与只写控制面的权限档次不同，共用一个 socket 等于把写权限降到读权限
			// （README §4.9 控制端点契约）。
			return E.New("user_stats.quota_control.listen_path 不得与 listen_path 相同")
		}
	}
	return nil
}

func (o *AccessLogOptions) normalize() error {
	if o.Directory == "" {
		return E.New("user_stats.access_log.directory 不能为空")
	}
	if !strings.HasPrefix(o.Directory, "/") {
		return E.New("user_stats.access_log.directory 必须是绝对路径：", o.Directory)
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = defaultAuditMaxBytes
	}
	if o.MaxOpenFiles == 0 {
		o.MaxOpenFiles = defaultAuditMaxOpenFiles
	}
	if o.MaxOpenFiles < 1 {
		return E.New("user_stats.access_log.max_open_files 必须为正数")
	}
	if o.MaxBytes < 64*1024 {
		return E.New("user_stats.access_log.max_bytes 过小")
	}
	if o.MaxTotalBytes != 0 && o.MaxTotalBytes < o.MaxBytes {
		return E.New("user_stats.access_log.max_total_bytes 必须不小于 max_bytes")
	}
	if o.FlushIntervalMs == 0 {
		o.FlushIntervalMs = defaultAuditFlushMs
	}
	if o.FlushIntervalMs < 10 {
		return E.New("user_stats.access_log.flush_interval_ms 过小")
	}
	if o.QueueSize == 0 {
		o.QueueSize = defaultAuditQueueSize
	}
	if o.QueueSize < 1 {
		return E.New("user_stats.access_log.queue_size 必须为正数")
	}
	// 在 normalize 阶段就编译一次，让非法规则在启动时硬失败而不是运行到一半才发现。
	if _, err := newExcludeFilter(o.ExcludeHosts, o.ExcludeIPs); err != nil {
		return err
	}
	return nil
}

func (o *QuotaControlOptions) normalize() error {
	if err := validateSocketPath("user_stats.quota_control.listen_path", o.ListenPath); err != nil {
		return err
	}
	if o.StartupAction == "" {
		o.StartupAction = string(QuotaActionAllow)
	}
	if o.StaleAction == "" {
		o.StaleAction = string(QuotaActionAllow)
	}
	for name, value := range map[string]string{
		"startup_action": o.StartupAction,
		"stale_action":   o.StaleAction,
	} {
		if value != string(QuotaActionAllow) && value != string(QuotaActionDeny) {
			return E.New("user_stats.quota_control.", name, " 只接受 allow 或 deny，实际：", value)
		}
	}
	// deny 配上默认的 stale_after=0 会让闸断彻底失效：admit 里的判定要求 staleAfter > 0。
	// 运维选 deny 正是为了「控制面失联后不再放行」，拿到的却是放行——这是 fail-closed
	// 被静默翻成 fail-open。不给默认值而是硬失败：给了默认值，生效策略就在配置里看不见了。
	if o.StaleAction == string(QuotaActionDeny) && time.Duration(o.StaleAfter) == 0 {
		return E.New("user_stats.quota_control.stale_action=deny 必须同时设置 stale_after，" +
			"否则陈旧判定永不触发")
	}
	if time.Duration(o.StaleAfter) < 0 {
		return E.New("user_stats.quota_control.stale_after 不能为负")
	}
	if time.Duration(o.ReconnectThrottle) < 0 {
		return E.New("user_stats.quota_control.reconnect_throttle 不能为负")
	}
	if o.MaxRequestBytes == 0 {
		o.MaxRequestBytes = defaultQuotaRequestBytes
	}
	if o.MaxRequestBytes < 256 {
		return E.New("user_stats.quota_control.max_request_bytes 过小")
	}
	return nil
}

// validateToken 实现 README §3 的标识约束：非空、最多 128 字节、每字节为 ASCII 可显示非空白字符。
func validateToken(field string, value string) error {
	if value == "" {
		return E.New(field, " 不能为空")
	}
	if len(value) > 128 {
		return E.New(field, " 超过 128 字节：", len(value))
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char <= 0x20 || char >= 0x7f {
			return E.New(field, " 含非 ASCII 可显示字符（偏移 ", index, "）")
		}
	}
	return nil
}

// validateSocketPath 在配置校验期就挡住超长 UDS 路径。
//
// 等到 Start 才失败也算失败关闭，但那时内核只回 EINVAL，运维看到的是
// "bind: invalid argument"，归因成本高得离谱。
func validateSocketPath(field string, path string) error {
	if path == "" {
		return E.New(field, " 不能为空")
	}
	if !strings.HasPrefix(path, "/") {
		return E.New(field, " 必须是绝对路径：", path)
	}
	if len(path) > maxUnixPathBytes {
		return E.New(field, " 超过 ", maxUnixPathBytes, " 字节上限（实际 ", len(path),
			"）：AF_UNIX 的 sun_path 放不下")
	}
	return nil
}
