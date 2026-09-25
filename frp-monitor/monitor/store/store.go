// SPDX-License-Identifier: Apache-2.0

// Package store 维护 monitor 的节点最新状态内存快照（根 README §5、§6）。
//
// 职责：会话规则（新 hello 取代旧会话、report 序号严格递增）、指标新鲜度
// （max(10s, 3×ReportInterval)，以服务端接收时间为准）、FRP 注册表对账
// （只读、冲突不自动绑定）以及面向 SSE 消费方的变更广播。
// P2 起增加 SQLite 持久化（db.go）、分钟聚合（aggregate.go）、累计流量
// （traffic.go）与 TCP 探测状态（probe.go）：内存为下发与 DTO 的权威来源，
// 写库全部经单 worker 有界队列，DB 故障只降级不拖停接收路径。
package store

import (
	"errors"
	"log"
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
	// ErrNoPersistence 表示未启用持久化（DataDir 为空或 DB 初始化失败降级）。
	ErrNoPersistence = errors.New("store: 未启用持久化")
)

// minStaleAfter 为指标新鲜度阈值下限；有效阈值为
// max(minStaleAfter, 3×ReportInterval)，以服务端接收时间判断。
const minStaleAfter = 10 * time.Second

// Event 为一次状态变更通知。NodeID 为空表示 FRP 注册表快照变化
// （可能影响任意节点的对账结果，消费方应刷新全部节点）。
// Tunnels 为 true 表示隧道快照（proxy stats）变化，SSE 消费方应额外
// 发送 tunnels 事件。
// 合并窗口（1 秒）由消费方实现，hub 只负责尽快投递。
type Event struct {
	NodeID  string
	Tunnels bool
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
	// LastSequence 为当前会话已接受的最大 report 序号。
	LastSequence uint64
	// LastPingSequence 为当前会话已接受的最大 ping.result 序号
	// （与 report 序号各自独立递增）。
	LastPingSequence uint64
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
	frpProxies []FRPProxy

	// P3：事件检测状态（翻转基线）与内存环形缓冲；访问须持有 mu。
	prevClientOnline map[string]bool
	prevProxyOnline  map[string]bool
	events           eventRing

	subs map[chan Event]struct{}

	// P2：持久化与累计组件。db/agg 为 nil 时仅内存（DataDir 为空或 DB 初始化
	// 失败的降级路径）；traffic/probes 始终可用（内存模式全功能）。
	db      *DB
	agg     *Aggregator
	traffic *TrafficTracker
	probes  *probeRegistry

	closeOnce sync.Once
}

// New 创建空仓库（仅内存模式）。
func New() *Store {
	return &Store{
		nodes:   make(map[string]*NodeState),
		subs:    make(map[chan Event]struct{}),
		traffic: NewTrafficTracker(),
		probes:  newProbeRegistry(),
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
	n.LastPingSequence = 0
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
// 接收成功后：持久化节点 last_seen（facts 携带时一并 upsert）、分钟聚合、
// 流量差分累计（quality.net_total 有效才计入；unknown/缺失不覆盖基线）。
func (s *Store) Report(nodeID, sessionID string, sequence uint64,
	facts *metrics.Facts, m *metrics.Metrics, frp *protocol.FRPExtension, now time.Time) error {

	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok || n.SessionID != sessionID {
		s.mu.Unlock()
		return ErrStaleSession
	}
	if sequence <= n.LastSequence {
		s.mu.Unlock()
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
	s.mu.Unlock()

	// 持久化与累计（锁外：内存操作或非阻塞入队，失败只降级不拖停接收）。
	if s.db != nil {
		s.db.enqueueNodeUpsert(nodeID, facts, now)
		if m != nil && s.agg != nil {
			s.agg.Add(nodeID, m, now)
		}
	}
	if m != nil && groupKnown(m.Quality, "net_total") {
		day := dayString(now)
		s.traffic.Apply(nodeID, m.BootID, m.Iface, m.NetRXTotal, m.NetTXTotal, day)
		if s.db != nil {
			s.db.enqueueTraffic(nodeID, m.BootID, m.Iface, m.NetRXTotal, m.NetTXTotal, day, now)
		}
	}
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
// 同时对每条注册条目做 Online 翻转检测，翻转写入 FRP 状态事件
// （client_online/client_offline）。
func (s *Store) UpdateFRPClients(clients []FRPClient, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frpClients = clients
	s.detectClientFlips(clients, now)
	s.broadcast(Event{})
}

// UpdateFRPProxies 替换 FRP proxy 统计快照（service 层周期喂入）。
// 快照与上一份比较，仅在变化时广播「隧道变化」事件（Tunnels=true），
// 避免轮询空转冲刷 SSE；同时对每个 proxy 做 Online 翻转检测，
// 翻转写入 FRP 状态事件（tunnel_online/tunnel_offline）。
func (s *Store) UpdateFRPProxies(proxies []FRPProxy, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	canon := canonicalProxies(proxies)
	s.detectProxyFlips(canon, now)
	changed := !proxiesEqual(s.frpProxies, canon)
	s.frpProxies = canon
	if changed {
		s.broadcast(Event{Tunnels: true})
	}
}

// FRPProxies 返回当前 FRP proxy 统计快照（排序后的稳定顺序）。
func (s *Store) FRPProxies() []FRPProxy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]FRPProxy, len(s.frpProxies))
	copy(out, s.frpProxies)
	return out
}

// FRPClients 返回当前 FRP 注册表快照。
func (s *Store) FRPClients() []FRPClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]FRPClient, len(s.frpClients))
	copy(out, s.frpClients)
	return out
}

// Reconcile 按 user+clientID 组合键对账（根 README §4：不同 user 可有
// 同名 clientID，不能只按 clientID 建表）：返回服务端注册表中 User 与
// ClientID 都和节点上报的 extensions.frp 一致的条目。对账不认证：仅当
// 全库只有该节点声明此组合键且至少有一条匹配时 bound 为 true；多节点
// 冲突或无匹配时不绑定（bound=false），冲突只通过返回值呈现、不自动改绑。
// 本函数为纯函数，不修改任何状态。
func Reconcile(n NodeState, all []NodeState, clients []FRPClient) (matched []FRPClient, bound bool) {
	if n.FRP == nil || n.FRP.ClientID == "" {
		return nil, false
	}
	for _, c := range clients {
		if c.ClientID == n.FRP.ClientID && c.User == n.FRP.User {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		return nil, false
	}
	for _, o := range all {
		if o.NodeID != n.NodeID && o.FRP != nil &&
			o.FRP.ClientID == n.FRP.ClientID && o.FRP.User == n.FRP.User {
			return matched, false
		}
	}
	return matched, true
}

// ---------- P2：持久化、流量与探测 ----------

// AttachDB 接入 SQLite 持久化：启动分钟聚合器，并从库中恢复节点（离线态）、
// 探测任务版本与流量基线。单步恢复失败仅记日志降级，不影响其余组件。
// 须在开始服务前调用。
func (s *Store) AttachDB(db *DB) {
	s.db = db
	s.agg = NewAggregator(func(row MetricRow) { db.enqueueMetricsRow(row) })

	if rows, err := db.LoadNodes(); err != nil {
		log.Printf("store: 节点状态恢复失败（降级）：%v", err)
	} else {
		for _, r := range rows {
			s.restoreNode(r)
		}
	}
	if versions, tasks, err := db.LoadProbeTasks(); err != nil {
		log.Printf("store: 探测任务恢复失败（降级）：%v", err)
	} else {
		for _, nodeID := range sortedKeys(versions) {
			if err := s.probes.restore(nodeID, versions[nodeID], tasks[nodeID]); err != nil {
				log.Printf("store: 节点 %s 探测任务恢复被拒绝：%v", nodeID, err)
			}
		}
	}
	if states, daily, err := db.LoadTraffic(); err != nil {
		log.Printf("store: 流量状态恢复失败（降级）：%v", err)
	} else {
		s.traffic.Restore(states, daily)
	}
}

// restoreNode 把一个持久化节点装回内存：始终离线态，持久化的 last_seen
// 只进 LastReportAt（展示用），不会复活在线状态，也不设置指标接收时间
// （MetricsReceivedAt=0 → 指标按过期处理）。
func (s *Store) restoreNode(row NodeRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[row.NodeID]
	if !ok {
		n = &NodeState{NodeID: row.NodeID}
		s.nodes[row.NodeID] = n
	}
	n.Online = false
	if row.Facts != nil {
		n.Facts = row.Facts
	}
	if row.LastSeen > n.LastReportAt {
		n.LastReportAt = row.LastSeen
	}
}

// Close 冲刷聚合、清空写队列并关闭 DB。幂等。
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		if s.agg != nil {
			s.agg.Stop()
			s.agg.FlushAll()
		}
		if s.db != nil {
			s.db.Close()
		}
	})
}

// FlushMetrics 冲刷当前分钟聚合桶并等待写队列清空（测试与关停机用）。
func (s *Store) FlushMetrics() {
	if s.agg != nil {
		s.agg.FlushAll()
	}
	s.DrainWrites()
}

// DrainWrites 等待已入队的写任务全部落库（测试用）。
func (s *Store) DrainWrites() {
	if s.db != nil {
		s.db.Drain()
	}
}

// HistoryEnabled 报告历史聚合是否可用（DB 存在才可用）。
func (s *Store) HistoryEnabled() bool { return s.db != nil }

// MetricsRange 读取 [from, to) 分钟区间的聚合行；无 DB 返回错误。
func (s *Store) MetricsRange(nodeID string, from, to int64) ([]MetricRow, error) {
	if s.db == nil {
		return nil, ErrNoPersistence
	}
	return s.db.MetricsRange(nodeID, from, to)
}

// BackupDB 用 VACUUM INTO 把 SQLite 库的一致快照导出到 destPath
// （目标文件须不存在）；无 DB 返回 ErrNoPersistence。
func (s *Store) BackupDB(destPath string) error {
	if s.db == nil {
		return ErrNoPersistence
	}
	return s.db.Backup(destPath)
}

// Traffic 返回节点累计流量视图；ok=false 表示从未有有效计数器读数。
func (s *Store) Traffic(nodeID string, now time.Time) (TrafficView, bool) {
	return s.traffic.View(nodeID, dayString(now))
}

// TrafficDaily 返回最近 days 个 UTC 日（含今天，升序）中有数据的日统计。
func (s *Store) TrafficDaily(nodeID string, days int, now time.Time) []TrafficDay {
	return s.traffic.Daily(nodeID, recentDays(now, days))
}

// SetProbeHook 注册探测任务变更回调（ingest 下发用，启动期设置一次）。
func (s *Store) SetProbeHook(hook func(nodeID string)) {
	s.probes.mu.Lock()
	s.probes.hook = hook
	s.probes.mu.Unlock()
}

// SetProbeTasks 整体替换节点探测任务：版本 +1，复用
// protocol.PingTasksParams.Validate 校验；持久化、广播并触发下发。
func (s *Store) SetProbeTasks(nodeID string, tasks []protocol.PingTask) (uint64, error) {
	version, err := s.probes.set(nodeID, tasks)
	if err != nil {
		return 0, err
	}
	if s.db != nil {
		s.db.enqueueProbeTasksReplace(nodeID, version, tasks)
	}
	s.mu.Lock()
	s.broadcast(Event{NodeID: nodeID})
	s.mu.Unlock()
	s.probes.mu.Lock()
	hook := s.probes.hook
	s.probes.mu.Unlock()
	if hook != nil {
		hook(nodeID)
	}
	return version, nil
}

// ProbeTasks 返回节点当前任务版本与列表。
func (s *Store) ProbeTasks(nodeID string) (uint64, []protocol.PingTask) {
	return s.probes.get(nodeID)
}

// RestoreProbeTasks 恢复任务列表；版本不高于当前版本时返回
// ErrProbeVersionRegression（版本回退拒绝）。
func (s *Store) RestoreProbeTasks(nodeID string, version uint64, tasks []protocol.PingTask) error {
	return s.probes.restore(nodeID, version, tasks)
}

// MarkProbeTasksSent / MarkProbeTasksAcked 记录最近一次下发/确认版本。
func (s *Store) MarkProbeTasksSent(nodeID string, version uint64) {
	s.probes.markSent(nodeID, version)
}

func (s *Store) MarkProbeTasksAcked(nodeID string, version uint64) {
	s.probes.markAcked(nodeID, version)
}

// ProbeDispatch 返回最近一次下发/确认版本（观测与测试用）。
func (s *Store) ProbeDispatch(nodeID string) (sent, acked uint64) {
	return s.probes.dispatch(nodeID)
}

// RecordPingResults 校验会话与 ping 序号后记录一批探测结果：
// 更新滑窗统计并（有 DB 时）落库。会话不匹配返回 ErrStaleSession，
// 序号乱序返回 ErrOutOfOrder。
func (s *Store) RecordPingResults(nodeID, sessionID string, sequence uint64,
	results []protocol.PingResult, now time.Time) error {

	s.mu.Lock()
	n, ok := s.nodes[nodeID]
	if !ok || n.SessionID != sessionID {
		s.mu.Unlock()
		return ErrStaleSession
	}
	if sequence <= n.LastPingSequence {
		s.mu.Unlock()
		return ErrOutOfOrder
	}
	n.LastPingSequence = sequence
	s.broadcast(Event{NodeID: nodeID})
	s.mu.Unlock()

	s.probes.record(nodeID, results, now)
	if s.db != nil && len(results) > 0 {
		s.db.enqueueProbeResults(nodeID, results, now)
	}
	return nil
}

// ProbeStats 返回当前任务列表的近 15 分钟滑窗统计。
func (s *Store) ProbeStats(nodeID string, now time.Time) []ProbeStat {
	return s.probes.stats(nodeID, now)
}
