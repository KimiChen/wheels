// SPDX-License-Identifier: Apache-2.0

// TCP 探测状态（根 README §3、§5）：任务列表版本由服务端按节点单调递增分配，
// 内存为下发的唯一权威来源，DB 用于重启恢复；ping.result 按会话/序号校验后
// 更新每任务近 15 分钟滑窗统计并（有 DB 时）落库。
package store

import (
	"errors"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// ErrProbeVersionRegression 表示恢复/回填的探测任务版本不高于当前版本。
var ErrProbeVersionRegression = errors.New("store: 探测任务版本回退")

// probeWindow 为滑窗统计的窗口长度（近 15 分钟）。
const probeWindow = 15 * time.Minute

// probeSample 为一次探测结果（latency = protocol.LatencyFailed 表示失败）。
type probeSample struct {
	ts      int64
	latency int64
}

// probeNode 为单节点探测状态。
type probeNode struct {
	version uint64
	tasks   []protocol.PingTask
	// sentVersion/ackedVersion 为最近一次下发/被确认的版本（观测用）。
	sentVersion  uint64
	ackedVersion uint64
	windows      map[string][]probeSample
}

// probeRegistry 为全部节点的探测状态。并发安全。
type probeRegistry struct {
	mu    sync.Mutex
	nodes map[string]*probeNode
	hook  func(nodeID string) // 任务变更时回调（ingest 下发）
}

func newProbeRegistry() *probeRegistry {
	return &probeRegistry{nodes: make(map[string]*probeNode)}
}

func (r *probeRegistry) getOrCreate(nodeID string) *probeNode {
	n, ok := r.nodes[nodeID]
	if !ok {
		n = &probeNode{windows: make(map[string][]probeSample)}
		r.nodes[nodeID] = n
	}
	return n
}

func cloneTasks(tasks []protocol.PingTask) []protocol.PingTask {
	out := make([]protocol.PingTask, len(tasks))
	copy(out, tasks)
	return out
}

// set 整体替换任务列表：版本 +1 并用 protocol.PingTasksParams.Validate 校验
// （校验失败不产生新版本）。返回新版本号。
func (r *probeRegistry) set(nodeID string, tasks []protocol.PingTask) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.getOrCreate(nodeID)
	version := n.version + 1
	params := &protocol.PingTasksParams{Version: version, Tasks: tasks}
	if err := params.Validate(); err != nil {
		return 0, err
	}
	n.version = version
	n.tasks = cloneTasks(tasks)
	return version, nil
}

// restore 恢复任务列表；版本不高于当前版本时拒绝（版本回退拒绝）。
func (r *probeRegistry) restore(nodeID string, version uint64, tasks []protocol.PingTask) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.getOrCreate(nodeID)
	if version <= n.version {
		return ErrProbeVersionRegression
	}
	n.version = version
	n.tasks = cloneTasks(tasks)
	return nil
}

func (r *probeRegistry) get(nodeID string) (uint64, []protocol.PingTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[nodeID]
	if !ok {
		return 0, []protocol.PingTask{}
	}
	return n.version, cloneTasks(n.tasks)
}

func (r *probeRegistry) markSent(nodeID string, version uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok && version > n.sentVersion {
		n.sentVersion = version
	}
}

func (r *probeRegistry) markAcked(nodeID string, version uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok && version > n.ackedVersion {
		n.ackedVersion = version
	}
}

// dispatch 返回最近一次下发/确认版本（观测与测试用）。
func (r *probeRegistry) dispatch(nodeID string) (sent, acked uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[nodeID]
	if !ok {
		return 0, 0
	}
	return n.sentVersion, n.ackedVersion
}

// record 把一批结果写入滑窗。recv 为服务端接收时间。
func (r *probeRegistry) record(nodeID string, results []protocol.PingResult, recv time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.getOrCreate(nodeID)
	ts := recv.Unix()
	cutoff := recv.Add(-probeWindow).Unix()
	for _, res := range results {
		w := n.windows[res.TaskID]
		w = append(w, probeSample{ts: ts, latency: res.LatencyMS})
		n.windows[res.TaskID] = pruneWindow(w, cutoff)
	}
}

func pruneWindow(w []probeSample, cutoff int64) []probeSample {
	out := w[:0]
	for _, s := range w {
		if s.ts > cutoff {
			out = append(out, s)
		}
	}
	return out
}

// ProbeStat 为单任务的滑窗统计。
type ProbeStat struct {
	ID     string
	Target string
	// LastLatency 为窗口内最近一次成功延迟（毫秒）；无成功样本为 nil。
	LastLatency *int64
	// FailRate 为窗口内失败率（失败数/样本数）；无样本为 nil。
	FailRate *float64
	// Samples 为窗口内样本数（成功 + 失败；DNS 超时缺样不计）。
	Samples int
}

// stats 返回当前任务列表（按配置顺序）的滑窗统计。
func (r *probeRegistry) stats(nodeID string, now time.Time) []ProbeStat {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[nodeID]
	if !ok {
		return []ProbeStat{}
	}
	cutoff := now.Add(-probeWindow).Unix()
	out := make([]ProbeStat, 0, len(n.tasks))
	for _, t := range n.tasks {
		w := pruneWindow(n.windows[t.ID], cutoff)
		st := ProbeStat{ID: t.ID, Target: t.Target, Samples: len(w)}
		if len(w) > 0 {
			fails := 0
			for _, s := range w {
				if s.latency == protocol.LatencyFailed {
					fails++
				}
			}
			rate := float64(fails) / float64(len(w))
			st.FailRate = &rate
			for i := len(w) - 1; i >= 0; i-- {
				if w[i].latency >= 0 {
					lat := w[i].latency
					st.LastLatency = &lat
					break
				}
			}
		}
		out = append(out, st)
	}
	return out
}
