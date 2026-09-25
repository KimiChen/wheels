// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"sort"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

// Metrics 采集周期主机指标。永不整体失败：任一数据组读取失败只把该组
// 标记为 QualityUnknown 且数值为 0，其余组不受影响（README §5）；
// 绝不 panic。ctx 取消时跳过采集，返回全部组 unknown 的空样本。
func (c *Collector) Metrics(ctx context.Context) metrics.Metrics {
	c.mu.Lock()
	defer c.mu.Unlock()

	m := metrics.Metrics{CollectedAt: c.now().Unix()}
	if err := ctx.Err(); err != nil {
		m.Quality = &metrics.Quality{
			CPU:      metrics.QualityUnknown,
			Mem:      metrics.QualityUnknown,
			Swap:     metrics.QualityUnknown,
			Disk:     metrics.QualityUnknown,
			NetRate:  metrics.QualityUnknown,
			NetTotal: metrics.QualityUnknown,
			Sys:      metrics.QualityUnknown,
		}
		return m
	}

	var q metrics.Quality

	// CPU 组：/proc/stat 累计时间差分，首样本无基线时为 unknown。
	if s, err := readCPUSample(c.procPath("stat")); err != nil {
		q.CPU = metrics.QualityUnknown
	} else {
		if v, ok := cpuDiff(c.cpuPrev, s, c.cpuOK); ok {
			m.CPU = v
		} else {
			q.CPU = metrics.QualityUnknown
		}
		c.cpuPrev, c.cpuOK = s, true
	}

	// mem / swap 组：/proc/meminfo。used = total − available（或回退值），
	// MemAvailable 为 0 是有效数据（used = total）。
	if mi, err := readMemInfo(c.procPath("meminfo")); err != nil {
		q.Mem = metrics.QualityUnknown
		q.Swap = metrics.QualityUnknown
	} else {
		if avail, ok := mi.memAvailable(); ok && mi.seenTotal {
			m.MemTotal = mi.total
			m.MemUsed = satSub(mi.total, avail)
		} else {
			q.Mem = metrics.QualityUnknown
		}
		if mi.seenSwapTotal && mi.seenSwapFree {
			m.SwapTotal = mi.swapTotal
			m.SwapUsed = satSub(mi.swapTotal, mi.swapFree)
		} else {
			q.Swap = metrics.QualityUnknown
		}
	}

	// disk 组：有效本地文件系统用量汇总。没有有效挂载（容器受限场景）
	// 或挂载表不可读时为 unknown。
	if total, used, err := c.diskUsage(); err != nil {
		q.Disk = metrics.QualityUnknown
	} else {
		m.DiskTotal = total
		m.DiskUsed = used
	}

	c.collectNet(&m, &q)

	// sys 组：loadavg、uptime、sockstat 任一失败整组降级。
	load, procs, errLoad := readLoadavg(c.procPath("loadavg"))
	uptime, errUptime := readUptime(c.procPath("uptime"))
	tcp, udp, errSock := readSockstat(c.procPath("net", "sockstat"), c.procPath("net", "sockstat6"))
	if errLoad == nil && errUptime == nil && errSock == nil {
		m.Load = load
		m.Uptime = uptime
		m.TCP = tcp
		m.UDP = udp
		m.Procs = procs
	} else {
		q.Sys = metrics.QualityUnknown
	}

	if q != (metrics.Quality{}) {
		m.Quality = &q
	}
	return m
}

// collectNet 采集网络组：net_total 为所计网卡的内核 lifetime 字节计数器
// 之和，读取成功即有效；net_rate 为计数器差分除以实际单调时间差，
// 首样本、计数器范围（boot ID + 网卡集合）变化或计数器回退时只有基线、
// 速率为 unknown。调用方须持有 c.mu。
func (c *Collector) collectNet(m *metrics.Metrics, q *metrics.Quality) {
	counters, err := readNetDev(c.procPath("net", "dev"))
	bootID, errBoot := readBootID(c.procPath("sys", "kernel", "random", "boot_id"))
	if err != nil || errBoot != nil {
		// boot ID 不可读时计数器范围无法标识，net_total 一并降级。
		q.NetTotal = metrics.QualityUnknown
		q.NetRate = metrics.QualityUnknown
		c.netPrev = netSample{}
		return
	}

	var rx, tx uint64
	names := make([]string, 0, len(counters))
	for _, ic := range counters {
		if !c.keepIface(ic.name) {
			continue
		}
		names = append(names, ic.name)
		rx = satAdd(rx, ic.rx)
		tx = satAdd(tx, ic.tx)
	}
	sort.Strings(names)
	m.NetRXTotal = rx
	m.NetTXTotal = tx
	m.BootID = bootID
	m.Iface = strings.Join(names, ",")

	scope := bootID + "\x00" + m.Iface
	now := c.now()
	if c.netPrev.valid && c.netPrev.scope == scope {
		dt := now.Sub(c.netPrev.when).Seconds()
		if dt > 0 && rx >= c.netPrev.rx && tx >= c.netPrev.tx {
			m.NetRX = float64(rx-c.netPrev.rx) / dt
			m.NetTX = float64(tx-c.netPrev.tx) / dt
		} else {
			// 时钟异常或计数器回退：重建基线，本期速率标未知。
			q.NetRate = metrics.QualityUnknown
		}
	} else {
		// 首样本或计数器范围变化：只建基线。
		q.NetRate = metrics.QualityUnknown
	}
	c.netPrev = netSample{when: now, scope: scope, rx: rx, tx: tx, valid: true}
}

// cpuDiff 由相邻两次累计时间计算整机 CPU 百分比 [0,100]：
// (Δtotal − Δidle) / Δtotal × 100。没有基线、计数器回退或没有
// 时间流逝时返回 ok=false（调用方标 unknown）。
func cpuDiff(prev, cur cpuSample, hasPrev bool) (float64, bool) {
	if !hasPrev || cur.total <= prev.total || cur.idle < prev.idle {
		return 0, false
	}
	totald := cur.total - prev.total
	idled := cur.idle - prev.idle
	if idled > totald {
		return 0, false
	}
	return float64(totald-idled) / float64(totald) * 100, true
}
