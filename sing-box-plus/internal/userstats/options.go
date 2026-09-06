package userstats

import (
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
	defaultAuditMaxBytes     = 256 * 1024 * 1024
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
type AccessLogOptions struct {
	Path            string `json:"path"`
	MaxBytes        int64  `json:"max_bytes,omitempty"`
	MaxTotalBytes   int64  `json:"max_total_bytes,omitempty"`
	FlushIntervalMs int    `json:"flush_interval_ms,omitempty"`
	QueueSize       int    `json:"queue_size,omitempty"`
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
	if o.Path == "" {
		return E.New("user_stats.access_log.path 不能为空")
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = defaultAuditMaxBytes
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
