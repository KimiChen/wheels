// Package probe runs bounded, cancellable TCP handshake measurements.
package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const (
	Workers      = 4
	Deadline     = 900 * time.Millisecond
	MaxAddresses = 3
)

type Config struct{ AllowPrivate bool }
type Result struct {
	TaskVersion uint64
	TaskID      string
	LatencyMS   float64
	CollectedAt time.Time
}
type dependencies struct {
	lookup func(context.Context, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
	now    func() time.Time
}
type task struct {
	spec    shared.PingTask
	next    time.Time
	running bool
}
type job struct {
	ctx     context.Context
	version uint64
	spec    shared.PingTask
}

// Engine has one scheduler and a fixed worker pool. A slow consumer loses samples
// rather than blocking measurements, host reports, or WebSocket heartbeats.
type Engine struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cfg       Config
	dep       dependencies
	mu        sync.Mutex
	version   uint64
	tasks     []*task
	batch     context.Context
	stopBatch context.CancelFunc
	jobs      chan job
	results   chan Result
	changed   chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
}

func New(ctx context.Context, cfg Config) *Engine {
	resolver := &net.Resolver{PreferGo: true}
	dialer := &net.Dialer{}
	return newEngine(ctx, cfg, dependencies{
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return resolver.LookupNetIP(ctx, "ip", host)
		},
		dial: dialer.DialContext, now: time.Now,
	})
}
func newEngine(ctx context.Context, cfg Config, dep dependencies) *Engine {
	child, cancel := context.WithCancel(ctx)
	e := &Engine{ctx: child, cancel: cancel, cfg: cfg, dep: dep, jobs: make(chan job), results: make(chan Result, shared.MaxProbeTasks), changed: make(chan struct{}, 1), done: make(chan struct{})}
	e.wg.Add(Workers + 1)
	go e.guarded(e.schedule)
	for i := 0; i < Workers; i++ {
		go e.guarded(e.worker)
	}
	go func() { e.wg.Wait(); close(e.done) }()
	return e
}
func (e *Engine) guarded(run func()) {
	defer e.wg.Done()
	defer func() {
		if recover() != nil {
			e.cancel()
		}
	}()
	run()
}
func (e *Engine) Close()                 { e.cancel(); <-e.done }
func (e *Engine) Results() <-chan Result { return e.results }

// Replace accepts only a newer complete list. Every old operation is cancelled,
// including an unchanged target, so no result can cross a task-version boundary.
func (e *Engine) Replace(list shared.PingTasks) error {
	if err := list.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	if list.Version <= e.version {
		return errors.New("probe task version must increase")
	}
	if e.stopBatch != nil {
		e.stopBatch()
	}
	e.batch, e.stopBatch = context.WithCancel(e.ctx)
	e.version = list.Version
	e.tasks = make([]*task, 0, len(list.Tasks))
	for _, spec := range list.Tasks {
		e.tasks = append(e.tasks, &task{spec: spec})
	}
drain:
	for {
		select {
		case <-e.results:
		default:
			break drain
		}
	}
	select {
	case e.changed <- struct{}{}:
	default:
	}
	return nil
}
func (e *Engine) Current(version uint64, id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current(version, id)
}
func (e *Engine) current(version uint64, id string) bool {
	if e.ctx.Err() != nil || version != e.version {
		return false
	}
	for _, t := range e.tasks {
		if t.spec.ID == id {
			return true
		}
	}
	return false
}
func (e *Engine) schedule() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		case <-e.changed:
		}
		e.mu.Lock()
		now := e.dep.now()
		// Oldest due task wins, so slow endpoints cannot starve later IDs.
		sort.SliceStable(e.tasks, func(i, j int) bool { return e.tasks[i].next.Before(e.tasks[j].next) })
		for _, t := range e.tasks {
			if t.running || now.Before(t.next) {
				continue
			}
			select {
			case e.jobs <- job{ctx: e.batch, version: e.version, spec: t.spec}:
				t.running = true
				t.next = now.Add(time.Duration(t.spec.Interval) * time.Second)
			default:
			}
		}
		e.mu.Unlock()
	}
}
func (e *Engine) worker() {
	for {
		select {
		case <-e.ctx.Done():
			return
		case j := <-e.jobs:
			latency, present := e.measure(j.ctx, j.spec.Target)
			at := e.dep.now()
			e.mu.Lock()
			if e.current(j.version, j.spec.ID) {
				for _, t := range e.tasks {
					if t.spec.ID == j.spec.ID {
						t.running = false
						// A missed period is delayed, never replayed as a burst.
						if !at.Before(t.next) {
							t.next = at.Add(time.Duration(t.spec.Interval) * time.Second)
						}
					}
				}
				if present {
					select {
					case e.results <- Result{j.version, j.spec.ID, latency, at.UTC()}:
					default:
					}
				}
			}
			e.mu.Unlock()
			select {
			case e.changed <- struct{}{}:
			default:
			}
		}
	}
}

// measure resolves once, then dials numeric addresses so an intervening DNS
// answer cannot bypass the destination policy. DNS timeout and policy rejection
// produce no sample; a normal DNS error or exhausted handshakes produce -1.
func (e *Engine) measure(ctx context.Context, target string) (float64, bool) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return 0, false
	}
	var addresses []netip.Addr
	if address, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{address}
	} else {
		lookupCtx, cancel := context.WithTimeout(ctx, Deadline)
		addresses, err = e.dep.lookup(lookupCtx, host)
		timedOut := errors.Is(lookupCtx.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil || timedOut {
			return 0, false
		}
		if err != nil {
			var dnsErr *net.DNSError
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &dnsErr) && dnsErr.IsTimeout) {
				return 0, false
			}
			return -1, true
		}
	}
	if len(addresses) == 0 {
		return -1, ctx.Err() == nil
	}
	if len(addresses) > MaxAddresses {
		addresses = addresses[:MaxAddresses]
	}
	attempted := false
	for _, address := range addresses {
		if ctx.Err() != nil {
			return 0, false
		}
		if !allowed(address, e.cfg.AllowPrivate) {
			continue
		}
		attempted = true
		dialCtx, cancel := context.WithTimeout(ctx, Deadline)
		started := e.dep.now()
		conn, err := e.dep.dial(dialCtx, "tcp", net.JoinHostPort(address.Unmap().String(), port))
		elapsed := e.dep.now().Sub(started)
		withinDeadline := dialCtx.Err() == nil && elapsed <= Deadline
		cancel()
		if conn != nil {
			conn.Close()
		}
		if ctx.Err() != nil {
			return 0, false
		}
		if err == nil && withinDeadline {
			return float64(elapsed.Milliseconds()), true
		}
	}
	return -1, attempted
}

var deniedV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}
var globalV6 = netip.MustParsePrefix("2000::/3")
var deniedV6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func allowed(address netip.Addr, allowPrivate bool) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if address.IsLoopback() || address.IsPrivate() {
		return allowPrivate
	}
	if !address.IsGlobalUnicast() || address.IsLinkLocalUnicast() {
		return false
	}
	if address.Is4() {
		for _, prefix := range deniedV4 {
			if prefix.Contains(address) {
				return false
			}
		}
		return true
	}
	if !globalV6.Contains(address) {
		return false
	}
	for _, prefix := range deniedV6 {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
