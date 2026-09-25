// SPDX-License-Identifier: Apache-2.0

// Package metrics 定义 agent 上报的 Facts / Metrics 数据契约。
//
// 字段名、单位与口径对齐 monitor-probe/agent（见根 README §3），
// 并在此之上增加质量标记与 FRP 扩展区。所有累计字节数为内核计数器原值；
// 浏览器侧 DTO 由 monitor 另行转换为十进制字符串，本契约面向 Go 双端。
package metrics

// 质量标记取值。空串等价于 QualityOK，序列化时省略。
const (
	// QualityOK 表示该组数据为有效样本。
	QualityOK = "ok"
	// QualityUnknown 表示该组数据未知：首报无差分基线或读取失败。
	// 未知不能伪造为 0；质量为 unknown 时对应数值字段仅为占位，
	// 消费方必须忽略并展示为「未知」。
	QualityUnknown = "unknown"
)

// Facts 为节点资产信息，每次连接先报，变化时重报。
//
// 容量字段（MemTotal/SwapTotal/DiskTotal）同时出现在周期 Metrics 中。
type Facts struct {
	// Hostname 为主机名，长度不超过 253 字节。
	Hostname string `json:"hostname"`
	// OS 为操作系统标识，当前支持 "linux"。
	OS string `json:"os"`
	// Kernel 为内核版本串（uname -r）。
	Kernel string `json:"kernel"`
	// Arch 为 GOARCH 风格的架构标识，如 "amd64"、"arm64"。
	Arch string `json:"arch"`
	// Virt 为虚拟化环境标识，无法判断时为空串。
	Virt string `json:"virt"`
	// CPUName 为 CPU 型号描述。
	CPUName string `json:"cpu_name"`
	// CPUCores 为逻辑处理器数，非物理核数。
	CPUCores int `json:"cpu_cores"`
	// AgentVersion 为 agent 自身版本，与 FRPVersion 分开。
	AgentVersion string `json:"agent_version"`

	// IPv4/IPv6 为每族一个本机地址，公网优先；不等于 NAT 出口地址。
	// 缺失族为空串。
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`

	// MemTotal/SwapTotal/DiskTotal 为容量，单位字节。
	MemTotal  uint64 `json:"mem_total"`
	SwapTotal uint64 `json:"swap_total"`
	DiskTotal uint64 `json:"disk_total"`
}

// Quality 按数据组携带质量标记。仅登记非 QualityOK 的组；
// 缺省即全部有效。组名与 Metrics 字段的对应见各字段注释。
type Quality struct {
	CPU      string `json:"cpu,omitempty"`
	Mem      string `json:"mem,omitempty"`
	Swap     string `json:"swap,omitempty"`
	Disk     string `json:"disk,omitempty"`
	NetRate  string `json:"net_rate,omitempty"`
	NetTotal string `json:"net_total,omitempty"`
	Sys      string `json:"sys,omitempty"`
}

// Metrics 为周期采样的主机指标。
type Metrics struct {
	// CollectedAt 为采集时刻（agent 本机 Unix 秒）。服务端的新鲜度判断
	// 以自身接收时间为准，本字段仅用于诊断时钟偏差。
	CollectedAt int64 `json:"collected_at"`

	// CPU 为整机 CPU 使用百分比 [0,100]，由累计时间差分得到，
	// iowait 计入 idle、guest 不重复相加。组标记：cpu。
	CPU float64 `json:"cpu"`
	// Load 为 1/5/15 分钟负载。组标记：sys。
	Load [3]float64 `json:"load"`

	// MemUsed = MemTotal − MemAvailable；仅当 MemAvailable 缺失时回退到
	// MemFree + Buffers + Cached。MemAvailable 为 0 是有效数据。单位字节。
	// 组标记：mem。
	MemUsed uint64 `json:"mem_used"`
	// SwapUsed = SwapTotal − SwapFree。单位字节。组标记：swap。
	SwapUsed uint64 `json:"swap_used"`
	// DiskUsed 为有效本地文件系统使用量汇总（挂载去重、伪/远程文件系统过滤），
	// 单位字节。组标记：disk。
	DiskUsed uint64 `json:"disk_used"`

	// 容量随周期指标重复上报，便于 Facts 未到或变化时的自洽展示。
	MemTotal  uint64 `json:"mem_total"`
	SwapTotal uint64 `json:"swap_total"`
	DiskTotal uint64 `json:"disk_total"`

	// NetRX/NetTX 为所计网卡的收发速率，单位字节/秒，由计数器差分
	// 除以实际单调时间差得到。组标记：net_rate。
	NetRX float64 `json:"net_rx"`
	NetTX float64 `json:"net_tx"`
	// NetRXTotal/NetTXTotal 为所计网卡的内核 lifetime 字节计数器之和。
	// 组标记：net_total。
	NetRXTotal uint64 `json:"net_rx_total"`
	NetTXTotal uint64 `json:"net_tx_total"`

	// BootID 为内核 boot ID；与 Iface 一起标识计数器范围。
	// 任一变化必须重建流量基线；不得用它判断 agent 进程重启。
	BootID string `json:"boot_id"`
	// Iface 为所计网卡集合摘要：网卡名排序后以英文逗号连接。
	Iface string `json:"iface"`

	// Uptime 为系统启动至今秒数。组标记：sys。
	Uptime uint64 `json:"uptime"`
	// TCP 为 IPv4 inuse + tw + IPv6 inuse 的 socket 汇总（含 TIME_WAIT），
	// 不能标为 established。UDP 为两族 inuse 之和。组标记：sys。
	TCP uint64 `json:"tcp"`
	UDP uint64 `json:"udp"`
	// Procs 为进程数。组标记：sys。
	Procs uint64 `json:"procs"`

	// Quality 登记非有效组；nil 表示全部有效。
	Quality *Quality `json:"quality,omitempty"`
}
