// SPDX-License-Identifier: Apache-2.0

// FRP 状态事件（根 README §6 frp_snapshots）：轮询 FRP 注册表与内存统计
// 时检测 Online 翻转，记录隧道/客户端上下线。内存环形缓冲 1000 条；
// DataDir 开启时同时写库，读取优先读库。
package store

import "log"

// FRP 状态事件 kind（DTO 原样输出）。
const (
	EventTunnelOnline  = "tunnel_online"
	EventTunnelOffline = "tunnel_offline"
	EventClientOnline  = "client_online"
	EventClientOffline = "client_offline"
)

// eventRingCapacity 为内存事件环形缓冲容量。
const eventRingCapacity = 1000

// FRPEvent 为一次 FRP 状态变化（隧道/客户端在线翻转）。
type FRPEvent struct {
	TS     int64  // 服务端观测时间（Unix 秒）
	NodeID string // 翻转时对账到的节点；未匹配或冲突为空
	Kind   string
	Name   string // 隧道名或 clientID（临时连接为 runID）
	Detail string
}

// eventRing 为定容环形缓冲，新事件覆盖最旧事件。
// 不加锁：全部访问须持有所在 Store 的 mu。
type eventRing struct {
	buf  [eventRingCapacity]FRPEvent
	head int // 下一个写入下标
	n    int // 已写入总数（达到容量后恒为容量）
}

func (r *eventRing) add(ev FRPEvent) {
	r.buf[r.head] = ev
	r.head = (r.head + 1) % eventRingCapacity
	if r.n < eventRingCapacity {
		r.n++
	}
}

// list 返回 nodeID 的事件，新的在前（同刻后写在前），最多 limit 条。
func (r *eventRing) list(nodeID string, limit int) []FRPEvent {
	out := make([]FRPEvent, 0, limit)
	for i := 0; i < r.n && len(out) < limit; i++ {
		ev := r.buf[(r.head-1-i+eventRingCapacity)%eventRingCapacity]
		if ev.NodeID == nodeID {
			out = append(out, ev)
		}
	}
	return out
}

// recordEvent 写入内存环形缓冲，并在 DB 开启时入队持久化。
// 调用方须持有 s.mu。
func (s *Store) recordEvent(ev FRPEvent) {
	s.events.add(ev)
	if s.db != nil {
		s.db.enqueueFRPEvent(ev)
	}
}

// FRPEvents 返回节点最近的 FRP 状态事件（新的在前）。优先读库；
// 无库或读库失败降级为内存环形缓冲。
func (s *Store) FRPEvents(nodeID string, limit int) []FRPEvent {
	if limit <= 0 {
		limit = 50
	}
	if s.db != nil {
		evs, err := s.db.LoadFRPEvents(nodeID, limit)
		if err == nil {
			return evs
		}
		log.Printf("store: 事件读库失败，降级为内存缓冲：%v", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.events.list(nodeID, limit)
}
