// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// fakeResolver 与 fakeDialer 以函数注入解析/拨号行为。
type fakeResolver struct {
	lookup func(ctx context.Context, host string) ([]string, error)
}

func (r fakeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return r.lookup(ctx, host)
}

type fakeDialer struct {
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

func (d fakeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

// fakeConn 为无需真实连接的 net.Conn。
type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (fakeConn) Write([]byte) (int, error)        { return 0, io.EOF }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return nil }
func (fakeConn) RemoteAddr() net.Addr             { return nil }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

func newTestManager(r resolver, d dialer) *Manager {
	m := NewManager()
	if r != nil {
		m.resolver = r
	}
	if d != nil {
		m.dialer = d
	}
	return m
}

func okResolver(addrs ...string) resolver {
	return fakeResolver{lookup: func(context.Context, string) ([]string, error) {
		return addrs, nil
	}}
}

// recordDialer 记录拨号地址，按 perAddr 表决定成败，其次 def，缺省成功。
type recordDialer struct {
	mu        sync.Mutex
	attempted []string
	perAddr   map[string]func(ctx context.Context) (net.Conn, error)
	def       func(ctx context.Context) (net.Conn, error)
}

func (d *recordDialer) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	d.attempted = append(d.attempted, address)
	d.mu.Unlock()
	if fn := d.perAddr[address]; fn != nil {
		return fn(ctx)
	}
	if d.def != nil {
		return d.def(ctx)
	}
	return fakeConn{}, nil
}

func TestProbeOnceSuccess(t *testing.T) {
	m := newTestManager(okResolver("10.0.0.1"), fakeDialer{dial: func(context.Context, string, string) (net.Conn, error) {
		return fakeConn{}, nil
	}})
	latency, missing := m.probeOnce(context.Background(), "example.com:443")
	if missing {
		t.Fatal("成功探测不得缺样")
	}
	if latency < 0 {
		t.Fatalf("成功探测延迟须 >= 0，得到 %d", latency)
	}
}

func TestProbeOnceRefused(t *testing.T) {
	m := newTestManager(okResolver("10.0.0.1"), fakeDialer{dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}})
	latency, missing := m.probeOnce(context.Background(), "example.com:443")
	if missing || latency != protocol.LatencyFailed {
		t.Fatalf("拒绝须为失败(-1)非缺样，得到 latency=%d missing=%v", latency, missing)
	}
}

func TestProbeOnceDialTimeout(t *testing.T) {
	m := newTestManager(okResolver("10.0.0.1"), fakeDialer{dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, context.DeadlineExceeded
	}})
	latency, missing := m.probeOnce(context.Background(), "example.com:443")
	if missing || latency != protocol.LatencyFailed {
		t.Fatalf("握手超时须为失败(-1)非缺样，得到 latency=%d missing=%v", latency, missing)
	}
}

func TestProbeOnceDNSTimeoutMissing(t *testing.T) {
	m := newTestManager(fakeResolver{lookup: func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	}}, nil)
	latency, missing := m.probeOnce(context.Background(), "slow.example.com:443")
	if !missing {
		t.Fatalf("DNS 超时须缺样，得到 latency=%d missing=%v", latency, missing)
	}
}

func TestProbeOnceResolveError(t *testing.T) {
	cases := map[string]error{
		"NXDOMAIN": &net.DNSError{Err: "no such host", IsNotFound: true},
		"普通错误":     errors.New("resolver boom"),
	}
	for name, err := range cases {
		m := newTestManager(fakeResolver{lookup: func(context.Context, string) ([]string, error) {
			return nil, err
		}}, nil)
		latency, missing := m.probeOnce(context.Background(), "bad.example.com:443")
		if missing || latency != protocol.LatencyFailed {
			t.Fatalf("%s：普通解析错误须为失败(-1)非缺样，得到 latency=%d missing=%v", name, latency, missing)
		}
	}
}

func TestProbeOnceMultiAddrFallback(t *testing.T) {
	d := &recordDialer{perAddr: map[string]func(context.Context) (net.Conn, error){
		"10.0.0.1:443": func(context.Context) (net.Conn, error) { return nil, errors.New("refused") },
		"10.0.0.2:443": func(context.Context) (net.Conn, error) { return nil, context.DeadlineExceeded },
	}}
	m := newTestManager(okResolver("10.0.0.1", "10.0.0.2", "10.0.0.3"), d)
	latency, missing := m.probeOnce(context.Background(), "example.com:443")
	if missing || latency < 0 {
		t.Fatalf("第三地址成功须返回其延迟，得到 latency=%d missing=%v", latency, missing)
	}
	want := []string{"10.0.0.1:443", "10.0.0.2:443", "10.0.0.3:443"}
	if fmt.Sprint(d.attempted) != fmt.Sprint(want) {
		t.Fatalf("须按序尝试前两个失败地址后命中第三，实际 %v", d.attempted)
	}
}

func TestProbeOnceMaxThreeAddrs(t *testing.T) {
	d := &recordDialer{def: func(context.Context) (net.Conn, error) { return nil, errors.New("refused") }}
	m := newTestManager(okResolver("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"), d)
	latency, missing := m.probeOnce(context.Background(), "example.com:443")
	if missing || latency != protocol.LatencyFailed {
		t.Fatalf("全部地址失败须为 -1，得到 latency=%d missing=%v", latency, missing)
	}
	if len(d.attempted) != maxAddrs {
		t.Fatalf("最多尝试 %d 个地址，实际 %d：%v", maxAddrs, len(d.attempted), d.attempted)
	}
}

func TestProbeOnceLatencyExcludesDNSAndPriorFailures(t *testing.T) {
	resolver := fakeResolver{lookup: func(ctx context.Context, host string) ([]string, error) {
		time.Sleep(150 * time.Millisecond) // DNS 耗时不计入
		return []string{"10.0.0.1", "10.0.0.2"}, nil
	}}
	dialer := fakeDialer{dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "10.0.0.1:443" {
			time.Sleep(150 * time.Millisecond) // 此前地址失败耗时不计入
			return nil, errors.New("refused")
		}
		time.Sleep(50 * time.Millisecond)
		return fakeConn{}, nil
	}}
	m := newTestManager(resolver, dialer)
	start := time.Now()
	latency, missing := m.probeOnce(context.Background(), "example.com:443")
	elapsed := time.Since(start)
	if missing || latency < 40 {
		t.Fatalf("成功地址握手约 50ms，得到 latency=%d missing=%v", latency, missing)
	}
	if latency >= 300 {
		t.Fatalf("延迟不得含 DNS 与此前地址失败耗时（共 300ms），得到 %dms", latency)
	}
	if elapsed < 350*time.Millisecond {
		t.Fatalf("总耗时应包含 DNS+失败+成功（>=350ms），实际 %v", elapsed)
	}
}

func TestProbeOnceInvalidTarget(t *testing.T) {
	m := newTestManager(okResolver("10.0.0.1"), nil)
	latency, missing := m.probeOnce(context.Background(), "no-port")
	if missing || latency != protocol.LatencyFailed {
		t.Fatalf("非法目标须为失败(-1)，得到 latency=%d missing=%v", latency, missing)
	}
}

func TestApplyVersionMonotonic(t *testing.T) {
	m := NewManager()
	p := func(v uint64) protocol.PingTasksParams {
		return protocol.PingTasksParams{Version: v, Tasks: []protocol.PingTask{
			{ID: "a", Target: "example.com:443", Interval: 5},
		}}
	}
	if err := m.Apply(p(1)); err != nil {
		t.Fatalf("首次 Apply 须成功：%v", err)
	}
	if err := m.Apply(p(1)); err == nil {
		t.Fatal("同版本重发须拒绝")
	}
	if err := m.Apply(p(3)); err != nil {
		t.Fatalf("递增版本须成功：%v", err)
	}
	if err := m.Apply(p(2)); err == nil {
		t.Fatal("版本回退须拒绝")
	}
}

func TestApplyRejectsInvalidTasks(t *testing.T) {
	mk := func(tasks ...protocol.PingTask) protocol.PingTasksParams {
		return protocol.PingTasksParams{Version: 1, Tasks: tasks}
	}
	many := make([]protocol.PingTask, 0, protocol.MaxPingTasks+1)
	for i := 0; i <= protocol.MaxPingTasks; i++ {
		many = append(many, protocol.PingTask{ID: fmt.Sprintf("t%d", i), Target: "h:80", Interval: 5})
	}
	cases := map[string]protocol.PingTasksParams{
		"版本为零":  {Version: 0, Tasks: []protocol.PingTask{{ID: "a", Target: "h:80", Interval: 5}}},
		"间隔过小":  mk(protocol.PingTask{ID: "a", Target: "h:80", Interval: protocol.MinPingInterval - 1}),
		"间隔过大":  mk(protocol.PingTask{ID: "a", Target: "h:80", Interval: protocol.MaxPingInterval + 1}),
		"超过上限":  mk(many...),
		"ID 重复": mk(protocol.PingTask{ID: "a", Target: "h:80", Interval: 5}, protocol.PingTask{ID: "a", Target: "h:81", Interval: 5}),
		"ID 为空": mk(protocol.PingTask{ID: "", Target: "h:80", Interval: 5}),
		"目标为空":  mk(protocol.PingTask{ID: "a", Target: "", Interval: 5}),
	}
	for name, p := range cases {
		if err := NewManager().Apply(p); err == nil {
			t.Fatalf("%s：须拒绝非法任务列表", name)
		}
	}
}

func TestApplyReplacesEntireList(t *testing.T) {
	m := NewManager()
	err := m.Apply(protocol.PingTasksParams{Version: 1, Tasks: []protocol.PingTask{
		{ID: "a", Target: "h:80", Interval: 5},
		{ID: "b", Target: "h:81", Interval: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = m.Apply(protocol.PingTasksParams{Version: 2, Tasks: []protocol.PingTask{
		{ID: "c", Target: "h:82", Interval: 15},
	}})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tasks) != 1 || m.tasks[0].id != "c" {
		t.Fatalf("Apply 须整体替换任务列表，实际 %+v", m.tasks)
	}
	if m.tasks[0].interval != 15*time.Second {
		t.Fatalf("间隔须换算为秒，实际 %v", m.tasks[0].interval)
	}
}

// collectResults 运行 Manager 并把结果汇入通道，返回停止函数。
func collectResults(t *testing.T, m *Manager) (chan protocol.PingResult, func()) {
	t.Helper()
	results := make(chan protocol.PingResult, 256)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx, func(r protocol.PingResult) { results <- r })
		close(done)
	}()
	return results, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("ctx 取消后 Run 未及时返回")
		}
	}
}

func TestRunSchedulesAndStops(t *testing.T) {
	m := newTestManager(okResolver("10.0.0.1"), fakeDialer{dial: func(context.Context, string, string) (net.Conn, error) {
		return fakeConn{}, nil
	}})
	if err := m.apply(1, []task{{id: "a", target: "h:80", interval: 100 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	results, stop := collectResults(t, m)
	for i := 0; i < 3; i++ {
		select {
		case r := <-results:
			if r.TaskID != "a" || r.LatencyMS < 0 {
				t.Fatalf("结果非法：%+v", r)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 个结果超时未到", i+1)
		}
	}
	stop()
	// ctx 取消即停：Run 返回后不会再有新结果；先排干取消前已入缓冲的，
	// 之后通道须保持为空。
drain:
	for {
		select {
		case <-results:
		default:
			break drain
		}
	}
	select {
	case r := <-results:
		t.Fatalf("取消后仍收到结果：%+v", r)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRunRestartsOnApply(t *testing.T) {
	m := newTestManager(okResolver("10.0.0.1"), fakeDialer{dial: func(context.Context, string, string) (net.Conn, error) {
		return fakeConn{}, nil
	}})
	if err := m.apply(1, []task{{id: "a", target: "h:80", interval: 100 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	results, stop := collectResults(t, m)
	defer stop()
	// 等旧任务产出，随后整体替换为任务 b。
	select {
	case r := <-results:
		if r.TaskID != "a" {
			t.Fatalf("首代任务须为 a，得到 %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("首代任务未产出")
	}
	if err := m.apply(2, []task{{id: "b", target: "h:80", interval: 100 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	// 收到首个 b 后，旧任务 a 不得再产出（同代取消，在飞轮次被 ctx 拦截）。
	deadline := time.After(2 * time.Second)
	for {
		select {
		case r := <-results:
			if r.TaskID == "b" {
				goto sawB
			}
		case <-deadline:
			t.Fatal("第二代任务未产出")
		}
	}
sawB:
	// 固定收集窗：断言窗口内只有 b 产出（窗口随 b 的结果推进，不用安静期）。
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case r := <-results:
			if r.TaskID != "b" {
				t.Fatalf("整体替换后旧任务仍产出：%+v", r)
			}
		case <-timer.C:
			return
		}
	}
}

func TestRunSkipsWhenInflightFull(t *testing.T) {
	// dialer 阻塞到 ctx 取消，占满 16 个在飞名额后其余轮次须跳过。
	m := newTestManager(okResolver("10.0.0.1"), fakeDialer{dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	tasks := make([]task, 0, maxInflight+8)
	for i := 0; i < maxInflight+8; i++ {
		tasks = append(tasks, task{id: fmt.Sprintf("t%d", i), target: "h:80", interval: 50 * time.Millisecond})
	}
	if err := m.apply(1, tasks); err != nil {
		t.Fatal(err)
	}
	_, stop := collectResults(t, m)
	time.Sleep(300 * time.Millisecond)
	stop()
	if got := atomic.LoadInt64(&m.skipped); got == 0 {
		t.Fatal("并发超限时须跳过并计数")
	}
}
