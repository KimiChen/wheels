// SPDX-License-Identifier: Apache-2.0

// Package probe 实现 frp-monitor agent 的 TCP 探测调度（根 README §3 末段、§5）。
//
// 探测语义对齐 monitor-probe/agent：测量 TCP 握手而非 ICMP；成功为握手毫秒数，
// 失败为 protocol.LatencyFailed（-1）；DNS 超时为缺样，本轮不上报该任务；
// 普通解析错误按失败处理。每地址限时 900ms，最多尝试 3 个地址，
// 延迟不含 DNS 与此前地址失败耗时。
//
// 任务由 monitor 以版本化完整列表下发（ping.tasks）：版本单调递增，
// 每次 Apply 整体替换任务集合。每任务独立 ticker，首次探测带小随机延迟
// 避免多任务对齐；错过的 tick 由 ticker 自然丢弃，不集中补跑。
// 并发在飞探测有界，超限的轮次直接跳过并内部计数。
package probe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

const (
	// perAddrTimeout 为单地址握手限时（对齐参考 900ms）。
	perAddrTimeout = 900 * time.Millisecond
	// maxAddrs 为单次探测最多尝试的地址数（对齐参考 3）。
	maxAddrs = 3
	// maxInflight 为并发在飞探测上限；超限的轮次跳过。
	maxInflight = 16
	// firstDelayCap 为首次探测随机延迟上限。
	firstDelayCap = time.Second
)

// resolver 与 dialer 为网络操作的窄接口，测试可注入假实现。
type resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// task 为内部调度任务：间隔已换算为 Duration。
type task struct {
	id       string
	target   string
	interval time.Duration
}

// Manager 按版本化完整列表调度探测任务。
//
// 生命周期：NewManager →（任意次 Apply，可在 Run 之前）→ Run（阻塞到
// ctx 取消）。会话结束取消 ctx 即停止本轮全部探测。版本域随实例，
// 每次监控会话应新建 Manager，不跨会话重用。
type Manager struct {
	resolver resolver
	dialer   dialer

	mu        sync.Mutex
	version   uint64
	tasks     []task
	runCtx    context.Context
	onResult  func(protocol.PingResult)
	genCancel context.CancelFunc // 取消当前一代任务循环

	inflight chan struct{}
	skipped  int64 // 因并发超限被跳过的轮次（内部计数，atomic 访问）

	wg sync.WaitGroup
}

// NewManager 返回使用系统 DNS 与 TCP dialer 的 Manager。
func NewManager() *Manager {
	return &Manager{
		resolver: net.DefaultResolver,
		dialer:   &net.Dialer{},
		inflight: make(chan struct{}, maxInflight),
	}
}

// Apply 校验并整体替换任务列表；版本不回退（<= 当前版本拒绝并返回错误）。
func (m *Manager) Apply(p protocol.PingTasksParams) error {
	if err := p.Validate(); err != nil {
		return err
	}
	tasks := make([]task, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		tasks = append(tasks, task{
			id:       t.ID,
			target:   t.Target,
			interval: time.Duration(t.Interval) * time.Second,
		})
	}
	return m.apply(p.Version, tasks)
}

// apply 替换任务列表并重启调度（内部入口：测试可注入亚秒级间隔）。
func (m *Manager) apply(version uint64, tasks []task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if version <= m.version {
		return fmt.Errorf("probe: 任务列表版本 %d 未超过当前版本 %d", version, m.version)
	}
	m.version = version
	m.tasks = tasks
	m.restartLocked()
	return nil
}

// Run 启动调度并阻塞到 ctx 取消；返回前等待全部任务 goroutine 退出。
// 结果经 onResult 投递；回调在任务 goroutine 内同步执行，长时间阻塞
// 会推迟该任务的后续轮次（ticker 丢弃错过的 tick）。
func (m *Manager) Run(ctx context.Context, onResult func(protocol.PingResult)) {
	m.mu.Lock()
	m.runCtx = ctx
	m.onResult = onResult
	m.restartLocked()
	m.mu.Unlock()

	<-ctx.Done()

	m.mu.Lock()
	m.runCtx = nil
	m.onResult = nil
	if m.genCancel != nil {
		m.genCancel()
		m.genCancel = nil
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// restartLocked 取消上一代任务循环并按当前任务列表重启；调用方持有 m.mu。
func (m *Manager) restartLocked() {
	if m.genCancel != nil {
		m.genCancel()
		m.genCancel = nil
	}
	if m.runCtx == nil || len(m.tasks) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(m.runCtx)
	m.genCancel = cancel
	for _, t := range m.tasks {
		m.wg.Add(1)
		go m.taskLoop(ctx, t)
	}
}

// taskLoop 为单任务调度循环：首次探测带小随机延迟，此后按固定间隔触发。
func (m *Manager) taskLoop(ctx context.Context, t task) {
	defer m.wg.Done()
	if t.interval <= 0 {
		return
	}
	timer := time.NewTimer(firstDelay(t.interval))
	select {
	case <-ctx.Done():
		timer.Stop()
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		m.runRound(ctx, t)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runRound 执行一轮探测并按语义投递结果：DNS 超时缺样与 ctx 取消不投递；
// 并发超限时跳过本轮并计数。
func (m *Manager) runRound(ctx context.Context, t task) {
	select {
	case m.inflight <- struct{}{}:
	default:
		atomic.AddInt64(&m.skipped, 1)
		return
	}
	latency, missing := m.probeOnce(ctx, t.target)
	<-m.inflight
	if missing || ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	onResult := m.onResult
	m.mu.Unlock()
	if onResult != nil {
		onResult(protocol.PingResult{TaskID: t.id, LatencyMS: latency})
	}
}

// probeOnce 对 target（host:port）执行一次 TCP 握手探测。返回
// (握手毫秒, false) 成功；(protocol.LatencyFailed, false) 失败
// （含目标非法、普通解析错误与全部地址握手失败）；(0, true) 为
// DNS 超时缺样，本轮不上报。延迟只计成功地址的握手耗时，
// 不含 DNS 与此前地址失败耗时。
func (m *Manager) probeOnce(ctx context.Context, target string) (latency int64, missing bool) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return protocol.LatencyFailed, false
	}
	addrs, err := m.resolver.LookupHost(ctx, host)
	if err != nil {
		if isResolutionTimeout(err) {
			return 0, true
		}
		return protocol.LatencyFailed, false
	}
	if len(addrs) > maxAddrs {
		addrs = addrs[:maxAddrs]
	}
	for _, ip := range addrs {
		actx, cancel := context.WithTimeout(ctx, perAddrTimeout)
		start := time.Now()
		conn, derr := m.dialer.DialContext(actx, "tcp", net.JoinHostPort(ip, port))
		cancel()
		if derr != nil {
			continue // 拒绝与超时都尝试下一地址；失败耗时不计入延迟
		}
		_ = conn.Close()
		return time.Since(start).Milliseconds(), false
	}
	return protocol.LatencyFailed, false
}

// isResolutionTimeout 判定解析错误是否为超时（DNS 超时为缺样语义，
// 普通解析错误为失败）。
func isResolutionTimeout(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// firstDelay 返回 [0, min(interval, firstDelayCap)) 的随机延迟，
// 避免多任务首次探测对齐。
func firstDelay(interval time.Duration) time.Duration {
	d := min(interval, firstDelayCap)
	if d <= 0 {
		return 0
	}
	var b [2]byte
	_, _ = rand.Read(b[:])
	return time.Duration(float64(d) * float64(int(b[0])<<8|int(b[1])) / 65535.0)
}
