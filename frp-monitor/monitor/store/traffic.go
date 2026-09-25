// SPDX-License-Identifier: Apache-2.0

// 累计流量（根 README §6）：同一计数器范围（boot_id + iface）用累计值差分；
// 首见只建基线；范围变化或计数器回退只重建基线、不清累计；unknown/缺失
// 由调用方跳过（不进入 Apply）。日归属按服务端接收时间的 UTC 日期。
//
// 内存 tracker 是 DTO 的唯一数据源（有无 DB 行为一致）；DB 侧
// applyTrafficTx 以同事务持久化基线与累计，用于重启恢复。
package store

import (
	"sort"
	"sync"
	"time"
)

// TrafficView 为节点累计流量的 DTO 视图。
type TrafficView struct {
	TodayRX, TodayTX uint64
	TotalRX, TotalTX uint64
}

// TrafficDay 为一天的收发量（day 为 UTC YYYY-MM-DD）。
type TrafficDay struct {
	Day    string
	RX, TX uint64
}

// trafficNode 为单节点流量状态。
type trafficNode struct {
	bootID, iface    string
	baseRX, baseTX   uint64
	totalRX, totalTX uint64
	daily            map[string][2]uint64
}

// TrafficTracker 为内存流量累计器。并发安全。
type TrafficTracker struct {
	mu    sync.Mutex
	nodes map[string]*trafficNode
}

// NewTrafficTracker 创建空 tracker。
func NewTrafficTracker() *TrafficTracker {
	return &TrafficTracker{nodes: make(map[string]*trafficNode)}
}

// Apply 应用一次计数器读数，语义与 applyTrafficTx 一致。
// day 为接收时刻的 UTC 日期。
func (t *TrafficTracker) Apply(nodeID, bootID, iface string, rx, tx uint64, day string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[nodeID]
	if !ok {
		// 首见只建基线
		t.nodes[nodeID] = &trafficNode{
			bootID: bootID, iface: iface, baseRX: rx, baseTX: tx,
			daily: make(map[string][2]uint64),
		}
		return
	}
	if n.bootID != bootID || n.iface != iface || rx < n.baseRX || tx < n.baseTX {
		// 范围变化或回退：重建基线，不清累计
		n.bootID, n.iface = bootID, iface
		n.baseRX, n.baseTX = rx, tx
		return
	}
	drx := rx - n.baseRX
	dtx := tx - n.baseTX
	n.baseRX, n.baseTX = rx, tx
	n.totalRX += drx
	n.totalTX += dtx
	if drx > 0 || dtx > 0 {
		d := n.daily[day]
		d[0] += drx
		d[1] += dtx
		n.daily[day] = d
	}
}

// View 返回节点累计视图；ok=false 表示从未有过有效计数器读数。
func (t *TrafficTracker) View(nodeID, day string) (TrafficView, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[nodeID]
	if !ok {
		return TrafficView{}, false
	}
	v := TrafficView{TotalRX: n.totalRX, TotalTX: n.totalTX}
	if d, ok := n.daily[day]; ok {
		v.TodayRX, v.TodayTX = d[0], d[1]
	}
	return v, true
}

// Daily 返回最近若干 UTC 日（含 today，升序）中有数据的行。
func (t *TrafficTracker) Daily(nodeID string, days []string) []TrafficDay {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[nodeID]
	if !ok {
		return []TrafficDay{}
	}
	out := make([]TrafficDay, 0, len(days))
	for _, d := range days {
		if v, ok := n.daily[d]; ok {
			out = append(out, TrafficDay{Day: d, RX: v[0], TX: v[1]})
		}
	}
	return out
}

// Restore 从 DB 加载的流量状态恢复（重启后内存与库一致）。
func (t *TrafficTracker) Restore(states []TrafficStateRow, daily []TrafficDailyRow) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range states {
		t.nodes[r.NodeID] = &trafficNode{
			bootID: r.BootID, iface: r.Iface,
			baseRX: r.BaseRX, baseTX: r.BaseTX,
			totalRX: r.TotalRX, totalTX: r.TotalTX,
			daily: make(map[string][2]uint64),
		}
	}
	for _, r := range daily {
		n, ok := t.nodes[r.NodeID]
		if !ok {
			n = &trafficNode{daily: make(map[string][2]uint64)}
			t.nodes[r.NodeID] = n
		}
		n.daily[r.Day] = [2]uint64{r.RX, r.TX}
	}
}

// dayString 为接收时刻的 UTC 日期（YYYY-MM-DD）。
func dayString(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// recentDays 返回以 today 结尾的连续 count 个 UTC 日（升序）。
func recentDays(now time.Time, count int) []string {
	if count < 1 {
		count = 1
	}
	today := now.UTC().Truncate(24 * time.Hour)
	out := make([]string, 0, count)
	for i := count - 1; i >= 0; i-- {
		out = append(out, today.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return out
}

// sortedKeys 为 map 键排序（稳定输出用）。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
