// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

// 字符串字段长度上限（字节）。
const (
	MaxSessionIDLen    = 64
	MaxVersionLen      = 64
	MaxCapabilities    = 32
	MaxCapabilityLen   = 64
	MaxPingTaskIDLen   = 64
	MaxPingTargetLen   = 512
	MaxProxyNameLen    = 128
	MaxProxyTypeLen    = 32
	MaxProxyAddrLen    = 256
	MaxProxyStatusLen  = 32
	MaxProxiesPerAgent = 256
)

// TCP 探测限制（对齐 monitor-probe/agent）：每节点任务上限与间隔范围。
const (
	MaxPingTasks    = 64
	MinPingInterval = 5
	MaxPingInterval = 3600

	// LatencyFailed 表示 TCP 握手失败（含普通 DNS 解析错误）。
	// DNS 超时不上报结果（缺样），不得以失败填充。
	LatencyFailed = -1
)

// HelloParams 为每次 WSS 连接的首条消息，协商 schema 与能力；
// Facts 随 hello 之后的首个 report 上报，变化时重报。
type HelloParams struct {
	SchemaVersion int      `json:"schema_version"`
	AgentVersion  string   `json:"agent_version"`
	FRPVersion    string   `json:"frp_version"`
	Capabilities  []string `json:"capabilities"`
	// SessionID 由 agent 为每次监控连接生成；服务端只接受当前会话。
	SessionID string `json:"session_id"`
	// SentAt 为 agent 本机 Unix 秒，仅用于诊断时钟偏差。
	SentAt int64 `json:"sent_at"`
}

// HelloResult 为服务端对 hello 的应答；schema 不支持时返回
// CodeUnsupportedSchema 错误而非正常结果。
type HelloResult struct {
	SchemaVersion int      `json:"schema_version"`
	ServerVersion string   `json:"server_version"`
	Capabilities  []string `json:"capabilities"`
	// ServerTime 为服务端 Unix 秒，供 agent 诊断时钟偏差。
	ServerTime int64 `json:"server_time"`
}

// ReportParams 为周期上报。Facts 仅在连接首次与变化时携带；
// Metrics 与 Extensions 按各自调度携带，三者至少有一个非空。
type ReportParams struct {
	SessionID string `json:"session_id"`
	// Sequence 在会话内从 1 递增；服务端拒绝乱序与旧会话覆盖。
	Sequence uint64 `json:"sequence"`
	SentAt   int64  `json:"sent_at"`

	Facts      *metrics.Facts   `json:"facts,omitempty"`
	Metrics    *metrics.Metrics `json:"metrics,omitempty"`
	Extensions *Extensions      `json:"extensions,omitempty"`
}

// Extensions 为自有扩展区，与上游对齐字段隔离演化。
type Extensions struct {
	FRP *FRPExtension `json:"frp,omitempty"`
}

// FRPExtension 为 agent 侧 FRP 只读状态（根 README §3 末段）。
type FRPExtension struct {
	// ClientID 为稳定 FRP Client ID；未设置稳定 clientID 的普通 frpc
	// 只能作为临时连接记录，不参与对账。
	ClientID   string `json:"client_id"`
	FRPVersion string `json:"frp_version"`
	// ControlConnected 为 FRP 控制连接状态；断连时隧道与监控状态须分别展示。
	ControlConnected bool `json:"control_connected"`
	// Proxies 为本地 Proxy 快照；本地目标只允许出现在认证后的管理视图。
	Proxies []ProxyInfo `json:"proxies,omitempty"`
}

// ProxyInfo 为单个本地 Proxy 的只读状态。
type ProxyInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	LocalAddr string `json:"local_addr"`
	Enabled   bool   `json:"enabled"`
	// Status 为客户端运行状态描述（如 "running"、"error"），不枚举取值，
	// 展示层原样呈现。
	Status string `json:"status"`
}

// PingTasksParams 为 monitor 下发的探测任务完整列表（版本化）；
// 列表变化时整体替换，Version 单调递增。
type PingTasksParams struct {
	Version uint64     `json:"version"`
	Tasks   []PingTask `json:"tasks"`
}

// PingTask 为单个 TCP 探测任务：测量 TCP 握手延迟而非 ICMP。
type PingTask struct {
	ID string `json:"id"`
	// Target 为 host:port；每地址限时 900ms，最多尝试三个地址，
	// 延迟不含 DNS 与此前地址失败耗时。
	Target string `json:"target"`
	// Interval 为探测间隔秒数，范围 [MinPingInterval, MaxPingInterval]。
	Interval int `json:"interval"`
}

// PingResultParams 为探测结果上报；DNS 超时的任务不出现在 Results 中。
type PingResultParams struct {
	SessionID string       `json:"session_id"`
	Sequence  uint64       `json:"sequence"`
	SentAt    int64        `json:"sent_at"`
	Results   []PingResult `json:"results"`
}

// PingResult 为单个任务的一次探测结果。
type PingResult struct {
	TaskID string `json:"task_id"`
	// LatencyMS 为握手成功毫秒数；失败为 LatencyFailed。
	LatencyMS int64 `json:"latency_ms"`
}
