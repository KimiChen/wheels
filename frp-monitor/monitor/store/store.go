// SPDX-License-Identifier: Apache-2.0

// Package store 维护 monitor 的节点最新状态内存快照（根 README §5、§6）。
//
// 职责：会话规则（新 hello 取代旧会话、report 序号严格递增）、指标新鲜度
// （max(10s, 3×ReportInterval)，以服务端接收时间为准）、FRP 注册表对账
// （只读、冲突不自动绑定）以及面向 SSE 消费方的变更广播。
// 秒级数据只驻留内存；SQLite 历史聚合属 P2 范围。
package store

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// 会话相关错误。两者都映射到 protocol.CodeStaleSession，ingest 据此
// 区分「旧会话」（关闭连接）与「同会话乱序」（仅回错误）。
var (
	// ErrStaleSession 表示 report/ping.result 的 session_id 不是该节点当前会话。
	ErrStaleSession = errors.New("store: 会话已失效（非当前会话）")
	// ErrOutOfOrder 表示同会话内序号未严格递增。
	ErrOutOfOrder = errors.New("store: 序号乱序（未严格递增）")
)

// minStaleAfter 为指标新鲜度阈值下限；有效阈值为
// max(minStaleAfter, 3×ReportInterval)，以服务端接收时间判断。
const minStaleAfter = 10 * time.Second

// Event 为一次状态变更通知。NodeID 为空表示 FRP 注册表快照变化
// （可能影响任意节点的对账结果，消费方应刷新全部节点）。
// 合并窗口（1 秒）由消费方实现，hub 只负责尽快投递。
type Event struct {
	NodeID string
}

// FRPClient 为服务端 FRP 注册表条目的只读快照（由 service 层的
// FRPRegistrySource 适配器周期性喂入）。
type FRPClient struct {
	User             string
	ClientID         string // FRP raw client ID
	RunID            string
	Version          string
	Online           bool
	FirstConnectedAt int64 // Unix 秒
	LastConnectedAt  int64 // Unix 秒
}

// NodeState 为单节点最新状态。Facts/Metrics/FRP 指针指向的负载在写入后
// 不再被 store 修改（更新即整体替换指针），快照消费者只读使用即安全。
type NodeState struct {
	NodeID    string
	Online    bool // 监控会话在线
	SessionID string
	// LastSequence 为当前会话已接受的最大序号。
	LastSequence uint64
	// ConnectedAt 为当前会话建立时间（服务端 Unix 秒）。
	ConnectedAt int64
	// LastReportAt 为最近一次成功 report 的服务端接收时间。
	LastReportAt int64
	// MetricsReceivedAt 为最近一次携带 Metrics 的 report 的服务端接收时间；
	// 0 表示本会话（或本进程）尚未收到指标。
	MetricsReceivedAt int64
	// FRPReceivedAt 为最近一次携带 extensions.frp 的 report 的服务端接收时间。
	FRPReceivedAt int64
	// ReportInterval 为 hello 声明的上报间隔秒数，范围 [1,3600]。
	ReportInterval int

	Facts   *metrics.Facts
	Metrics *metrics.Metrics
	// FRP 为最近一次上报的 extensions.frp 载荷。
	FRP *protocol.FRPExtension
}

// MetricsStale 判断指标新鲜度：从未收到指标即过期；否则超过
// max(10s, 3×ReportInterval) 未收到新指标即过期。以服务端接收时间为准。
func (n NodeState) MetricsStale(now time.Time) bool {
	if n.MetricsReceivedAt == 0 {
		return true
	}
	threshold := minStaleAfter
	if d := 3 * time.Duration(n.ReportInterval) * time.Second; d > threshold {
		threshold = d
	}
	return now.Sub(time.Unix(n.MetricsReceivedAt, 0)) > threshold
}

// Store 为线程安全的节点最新状态仓库。
type Store struct {
	mu         sync.RWMutex
	nodes      map[string]*NodeState
	frpClients []FRPClient

	subs map[chan Event]struct{}
}

// New 创建空仓库。
func New() *Store {
	return &Store{
		nodes: make(map[string]*NodeState),
		subs:  make(map[chan Event]struct{}),
	}
}

// Subscribe 订阅变更通知。返回的 channel 有缓冲；缓冲满时事件被丢弃
// （消费方按 1 秒窗口合并，偶发丢弃在下一窗口自愈）。cancel 取消订阅。
func (s *Store) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
	return ch, cancel
}

// broadcast 非阻塞地向所有订阅者投递事件；须在持有锁的调用方之外理解
// 为「状态已变更」的信号，不携带负载。
func (s *Store) broadcast(ev Event) {
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// StartSession 处理 hello：为节点建立新会话并取代旧会话。
// 返回被取代的旧 sessionID（无旧会话时为空串）；关闭旧连接由调用方负责。
// 旧会话已上报的 Facts/Metrics 保留展示，由新鲜度阈值自然过期。
func (s *Store) StartSession(nodeID, sessionID string, reportInterval int, now time.Time) (replaced string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.nodes[nodeID]
	if !ok {
		n = &NodeState{NodeID: nodeID}
		s.nodes[nodeID] = n
	}
	replaced = n.SessionID
	n.SessionID = sessionID
	n.Online = true
	n.LastSequence = 0
	n.ConnectedAt = now.Unix()
	if reportInterval > 0 {
		n.ReportInterval = reportInterval
	}
	s.broadcast(Event{NodeID: nodeID})
	return replaced
}

// Report 写入一次 report 载荷（收到时间 = 服务端当前时间 now）。
// sessionID 不匹配当前会话返回 ErrStaleSession；序号未严格递增返回
// ErrOutOfOrder。三个载荷指针各自独立更新，nil 表示本次未携带。
func (s *Store) Report(nodeID, sessionID string, sequence uint64,
	facts *metrics.Facts, m *metrics.Metrics, frp *protocol.FRPExtension, now time.Time) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.nodes[nodeID]
	if !ok || n.SessionID != sessionID {
		return ErrStaleSession
	}
	if sequence <= n.LastSequence {
		return ErrOutOfOrder
	}
	n.LastSequence = sequence
	n.LastReportAt = now.Unix()
	if facts != nil {
		n.Facts = facts
	}
	if m != nil {
		n.Metrics = m
		n.MetricsReceivedAt = now.Unix()
	}
	if frp != nil {
		n.FRP = frp
		n.FRPReceivedAt = now.Unix()
	}
	s.broadcast(Event{NodeID: nodeID})
	return nil
}

// EndSession 处理连接关闭：仅当 sessionID 仍是该节点当前会话时才标记离线；
// 旧连接的迟到退出不得覆盖新会话。
func (s *Store) EndSession(nodeID, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.nodes[nodeID]
	if !ok || n.SessionID != sessionID {
		return
	}
	n.Online = false
	s.broadcast(Event{NodeID: nodeID})
}

// Get 返回单节点快照。
func (s *Store) Get(nodeID string) (NodeState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[nodeID]
	if !ok {
		return NodeState{}, false
	}
	return *n, true
}

// Snapshot 返回全部节点快照，按 NodeID 排序保证输出稳定。
func (s *Store) Snapshot() []NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]NodeState, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// UpdateFRPClients 替换 FRP 注册表快照（service 层周期喂入）。
// 对账在读取时进行，因此这里只保存并广播一次「注册表变化」事件。
func (s *Store) UpdateFRPClients(clients []FRPClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frpClients = clients
	s.broadcast(Event{})
}

// FRPClients 返回当前 FRP 注册表快照。
func (s *Store) FRPClients() []FRPClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]FRPClient, len(s.frpClients))
	copy(out, s.frpClients)
	return out
}

// Reconcile 按 RawClientID 对账：返回服务端注册表中 ClientID 与节点上报的
// extensions.frp.client_id 相同的条目。对账不认证：仅当全库只有该节点
// 声明此 client_id 且至少有一条匹配时 bound 为 true；多节点冲突或
// 无匹配时不绑定（bound=false），冲突只通过返回值呈现、不自动改绑。
// 本函数为纯函数，不修改任何状态。
func Reconcile(n NodeState, all []NodeState, clients []FRPClient) (matched []FRPClient, bound bool) {
	if n.FRP == nil || n.FRP.ClientID == "" {
		return nil, false
	}
	for _, c := range clients {
		if c.ClientID == n.FRP.ClientID {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		return nil, false
	}
	for _, o := range all {
		if o.NodeID != n.NodeID && o.FRP != nil && o.FRP.ClientID == n.FRP.ClientID {
			return matched, false
		}
	}
	return matched, true
}
