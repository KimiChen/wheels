package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func TestDestinationPolicy(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !allowed(netip.MustParseAddr(raw), false) {
			t.Errorf("public address rejected: %s", raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.4.5", "192.168.1.2", "fd00::1", "::ffff:127.0.0.1"} {
		ip := netip.MustParseAddr(raw)
		if allowed(ip, false) || !allowed(ip, true) {
			t.Errorf("private opt-in failed: %s", raw)
		}
	}
	for _, raw := range []string{"0.0.0.0", "0.1.2.3", "100.64.1.1", "169.254.169.254", "192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "255.255.255.255", "::", "ff02::1", "fe80::1", "fe80::1%en0", "2001:db8::1", "2002::1", "64:ff9b::a00:1", "2001::1", "3fff::1"} {
		for _, optIn := range []bool{false, true} {
			if allowed(netip.MustParseAddr(raw), optIn) {
				t.Errorf("reserved address allowed: %s", raw)
			}
		}
	}
}

func TestMeasureDNSAndFallbackSemantics(t *testing.T) {
	now := time.Unix(1000, 0)
	var attempts []string
	e := &Engine{dep: dependencies{now: func() time.Time { return now }, lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host != "probe.example" {
			t.Fatalf("unexpected host %q", host)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > Deadline {
			t.Fatal("DNS deadline absent")
		}
		now = now.Add(400 * time.Millisecond)
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("9.9.9.9")}, nil
	}, dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			t.Fatal("wrong protocol")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("dial deadline absent")
		}
		attempts = append(attempts, address)
		if len(attempts) == 1 {
			now = now.Add(Deadline)
			return nil, context.DeadlineExceeded
		}
		now = now.Add(17*time.Millisecond + 900*time.Microsecond)
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}}}
	got, present := e.measure(context.Background(), "probe.example:443")
	if !present || got != 17 || len(attempts) != 2 || attempts[1] != "1.1.1.1:443" {
		t.Fatalf("DNS/previous failure counted in latency: %v %v %v", got, present, attempts)
	}
	e.dep.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("must not dial after DNS error")
		return nil, nil
	}
	for _, tc := range []struct {
		err     error
		present bool
	}{
		{errors.New("no such host"), true}, {context.DeadlineExceeded, false}, {&net.DNSError{IsTimeout: true}, false},
	} {
		e.dep.lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, tc.err }
		latency, present := e.measure(context.Background(), "probe.example:443")
		if present != tc.present || (present && latency != -1) {
			t.Fatalf("wrong DNS result: %v %v", latency, present)
		}
	}
}

func TestAttemptLimitAndPolicyAfterDNS(t *testing.T) {
	var calls int
	e := &Engine{dep: dependencies{now: time.Now, lookup: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("9.9.9.9"), netip.MustParseAddr("8.8.4.4")}, nil
	}, dial: func(context.Context, string, string) (net.Conn, error) { calls++; return nil, errors.New("refused") }}}
	if got, present := e.measure(context.Background(), "probe.example:443"); got != -1 || !present || calls != MaxAddresses {
		t.Fatalf("attempt limit: %v %v %d", got, present, calls)
	}
	e.dep.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	calls = 0
	if _, present := e.measure(context.Background(), "probe.example:443"); present || calls != 0 {
		t.Fatal("DNS answer bypassed private policy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, present := e.measure(ctx, "8.8.8.8:443"); present || calls != 0 {
		t.Fatal("cancelled probe dialled")
	}
}

func TestActualTCPHandshakeAndRefusal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := New(context.Background(), Config{AllowPrivate: true})
	defer e.Close()
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
		close(accepted)
	}()
	got, present := e.measure(context.Background(), listener.Addr().String())
	if !present || got < 0 || got > 900 {
		t.Fatalf("real TCP failed: %v %v", got, present)
	}
	<-accepted
	listener.Close()
	if got, present := e.measure(context.Background(), listener.Addr().String()); !present || got != -1 {
		t.Fatalf("refusal was not failure: %v %v", got, present)
	}
}

func TestActualDNSAndDialDeadlinesRemainDistinct(t *testing.T) {
	e := &Engine{dep: dependencies{now: time.Now, lookup: func(ctx context.Context, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	begin := time.Now()
	if _, present := e.measure(context.Background(), "probe.example:443"); present {
		t.Fatal("DNS deadline became a failure sample")
	}
	if elapsed := time.Since(begin); elapsed < Deadline || elapsed > 2*time.Second {
		t.Fatal("DNS was not bounded", elapsed)
	}
	begin = time.Now()
	if got, present := e.measure(context.Background(), "8.8.8.8:443"); !present || got != -1 {
		t.Fatal("handshake deadline was not a failed sample")
	}
	if elapsed := time.Since(begin); elapsed < Deadline || elapsed > 2*time.Second {
		t.Fatal("handshake was not bounded", elapsed)
	}
}

func tasks(version uint64, count int) shared.PingTasks {
	list := shared.PingTasks{Meta: shared.Meta{Schema: 1, SessionID: "session-1", Sequence: version, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Version: version, Tasks: []shared.PingTask{}}
	for i := 0; i < count; i++ {
		list.Tasks = append(list.Tasks, shared.PingTask{ID: fmt.Sprintf("task-%d", i), Target: "probe.example:443", Interval: 5})
	}
	return list
}

func TestReplacementCancelsRunningTasksAndBoundsConcurrency(t *testing.T) {
	var active, maximum, cancelled atomic.Int32
	started := make(chan struct{}, 64)
	e := newEngine(context.Background(), Config{}, dependencies{now: time.Now, lookup: func(ctx context.Context, _ string) ([]netip.Addr, error) {
		n := active.Add(1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		started <- struct{}{}
		<-ctx.Done()
		active.Add(-1)
		cancelled.Add(1)
		return nil, ctx.Err()
	}, dial: func(context.Context, string, string) (net.Conn, error) {
		t.Error("cancelled DNS dialled")
		return nil, nil
	}})
	defer e.Close()
	if err := e.Replace(tasks(1, 64)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < Workers; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	if err := e.Replace(tasks(2, 0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 0 || maximum.Load() != Workers || cancelled.Load() != Workers {
		t.Fatalf("unbounded/uncancelled workers: %d %d %d", active.Load(), maximum.Load(), cancelled.Load())
	}
	if len(e.results) != 0 || e.Current(1, "task-0") || e.Replace(tasks(1, 1)) == nil {
		t.Fatal("old task version survived replacement")
	}
	if err := e.Replace(tasks(3, 65)); err == nil {
		t.Fatal("accepted too many tasks")
	}
}

func TestQueueFullDoesNotBlockWorkersAndClose(t *testing.T) {
	var attempts atomic.Int32
	e := newEngine(context.Background(), Config{}, dependencies{now: time.Now, lookup: func(context.Context, string) ([]netip.Addr, error) {
		attempts.Add(1)
		return nil, errors.New("no such host")
	}, dial: func(context.Context, string, string) (net.Conn, error) { return nil, nil }})
	defer e.Close()
	if err := e.Replace(tasks(1, 64)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(e.results) < 64 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if attempts.Load() != 64 {
		t.Fatal("scheduler starved tasks", attempts.Load())
	}
	// Expedite the next due time without weakening production interval limits.
	e.mu.Lock()
	for _, task := range e.tasks {
		task.next = time.Time{}
	}
	e.mu.Unlock()
	select {
	case e.changed <- struct{}{}:
	default:
	}
	deadline = time.Now().Add(3 * time.Second)
	for attempts.Load() < 128 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if attempts.Load() != 128 || len(e.results) != 64 {
		t.Fatalf("full queue blocked workers: attempts=%d queue=%d", attempts.Load(), len(e.results))
	}
	begin := time.Now()
	e.Close()
	if time.Since(begin) > time.Second {
		t.Fatal("full result queue blocked close")
	}
}

func TestProbePanicRemainsLocal(t *testing.T) {
	e := newEngine(context.Background(), Config{}, dependencies{now: time.Now, lookup: func(context.Context, string) ([]netip.Addr, error) { panic("resolver") }})
	defer e.Close()
	if err := e.Replace(tasks(1, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("probe panic left workers running")
	}
}
