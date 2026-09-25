// SPDX-License-Identifier: Apache-2.0

// Package collect 实现 frp-monitor agent 的 Linux 主机采集：
// 直读 /proc、/sys 并调用 statfs，输出 shared/metrics 契约定义的
// Facts 与 Metrics。字段名、单位与口径对齐根 README §3。
//
// 质量语义（README §5）：Metrics 永不整体失败——任一数据组读取失败
// 只把该组标记为 QualityUnknown 且数值置 0，不影响其他组；首次采样
// 没有差分基线时 cpu / net_rate 组同样为 unknown（上游参考实现的首
// 样本展示为 0，本实现按契约显式标未知，这项展示差异是刻意的）。
//
// 采集器面向 Linux 宿主机（amd64/arm64）。容器内 /proc、/sys 与挂载
// 命名空间受限，读数只代表容器命名空间范围，不能声称代表宿主机；
// 受限环境可能没有任何有效本地挂载，此时 disk 组按 unknown 上报。
package collect

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// IfaceFilter 为网卡显式筛选：完整接口名匹配，排除优先于纳入。
//
// 判定顺序：Exclude 命中的接口一律剔除（最高优先级）；Include 非空时
// 仅纳入命中的接口（显式纳入可覆盖默认前缀剔除，例如把 "wg0" 加入
// 白名单）；Include 为空时应用默认剔除规则（lo 与容器/隧道/叠加
// 接口前缀，见 keepIface）。流量计数与本机地址选择共用同一判定。
type IfaceFilter struct {
	// Include 非空时为白名单：仅列出的接口纳入统计。
	Include []string
	// Exclude 命中的接口一律剔除，优先于 Include 与默认规则。
	Exclude []string
}

// defaultIfaceDeniedPrefixes 为默认剔除的接口名前缀：容器、隧道、
// 网桥与叠加接口，避免与物理/主接口重复统计。
var defaultIfaceDeniedPrefixes = []string{
	"docker", "veth", "br-", "tun", "tap", "wg", "tailscale", "zt",
	"virbr", "cni", "flannel", "cali", "kube-", "gre", "ip6tnl", "sit",
}

// defaultIfaceDenied 报告接口是否被默认规则剔除（lo 精确匹配，其余前缀匹配）。
func defaultIfaceDenied(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range defaultIfaceDeniedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Collector 从 procRoot/sysRoot 读取主机数据。生产环境使用
// New("/proc", "/sys", filter)；测试可指向 fixture 目录树。
//
// Collector 可并发使用：Facts 与 Metrics 可能被不同 goroutine 调用，
// 内部差分基线由互斥锁保护。
type Collector struct {
	procRoot string
	sysRoot  string
	filter   IfaceFilter

	// 测试注入点：now 为速率差分的单调时钟，statfs 为挂载点设备号与
	// 用量统计。生产路径分别为 time.Now 与平台实现 defaultMountStat。
	now    func() time.Time
	statfs func(path string) (mountStat, error)

	mu      sync.Mutex
	cpuPrev cpuSample
	cpuOK   bool
	netPrev netSample
}

// New 创建采集器。procRoot/sysRoot 为 /proc、/sys 的根路径。
func New(procRoot, sysRoot string, filter IfaceFilter) *Collector {
	return &Collector{
		procRoot: procRoot,
		sysRoot:  sysRoot,
		filter:   filter,
		now:      time.Now,
		statfs:   defaultMountStat,
	}
}

// procPath 拼接 procRoot 下的相对路径。
func (c *Collector) procPath(elem ...string) string {
	return filepath.Join(append([]string{c.procRoot}, elem...)...)
}

// keepIface 判定接口是否纳入统计：Exclude 优先；Include 非空时为白名单；
// 否则应用默认剔除规则。
func (c *Collector) keepIface(name string) bool {
	for _, x := range c.filter.Exclude {
		if name == x {
			return false
		}
	}
	if len(c.filter.Include) > 0 {
		for _, i := range c.filter.Include {
			if name == i {
				return true
			}
		}
		return false
	}
	return !defaultIfaceDenied(name)
}

// netSample 为网卡计数器的差分基线；scope 标识计数器范围
// （boot ID + 所计网卡集合），范围变化必须重建基线。
type netSample struct {
	when  time.Time
	scope string
	rx    uint64
	tx    uint64
	valid bool
}
