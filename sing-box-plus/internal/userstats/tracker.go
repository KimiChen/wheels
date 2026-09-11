package userstats

import (
	"context"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// Tracker 实现 adapter.ConnectionTracker，挂在认证与路由之后、交给 outbound handler 之前。
//
// 由自有 main 在 box.New() 之后、Start() 之前追加，因此恒定是包装链最外层（README §4.1、§4.7）。
type Tracker struct {
	registry *Registry
	logger   log.ContextLogger
}

// NewTracker 创建统一 tracker。
//
// §4.8 的访问审计挂在进程级 registry 上：tracker 跨 SIGHUP 存活，而 audit writer 属于
// services[] 实例、每次重载重建，两者的生命周期用一次原子读解耦。
func NewTracker(registry *Registry, logger log.ContextLogger) *Tracker {
	return &Tracker{registry: registry, logger: logger}
}

// RoutedFlow 固定返回 nil：第三条路径只在 TUN/L3 形态可达，口径是 IP 包全长且 metadata.User
// 恒空，不参与计费（README §2.3、§4.1）。
func (t *Tracker) RoutedFlow(ctx context.Context, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) tun.FlowTracker {
	return nil
}

func (t *Tracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	record, user, rejected := t.resolve(ctx, conn, metadata, "tcp")
	if record == nil {
		// 非计费 inbound：保持上游快路径与线协议不变（README §4.6 第 6 条）。
		return conn
	}
	if rejected != nil {
		return rejected
	}
	state := newConnState(t, record, user, metadata, "tcp", conn)
	record.tcpSessions.Add(1)
	counted := bufio.NewCounterConn(conn, []N.CountFunc{state.countUplink}, []N.CountFunc{state.countDownlink})
	state.closer = counted
	return &trackedConn{ExtendedConn: counted, state: state}
}

func (t *Tracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	record, user, rejected := t.resolve(ctx, conn, metadata, "udp")
	if record == nil {
		return conn
	}
	if rejected != nil {
		return &rejectedPacketConn{err: os.ErrClosed}
	}
	state := newConnState(t, record, user, metadata, "udp", conn)
	record.udpSessions.Add(1)
	counted := bufio.NewCounterPacketConn(conn, []N.CountFunc{state.countUplink}, []N.CountFunc{state.countDownlink})
	state.closer = counted
	return &trackedPacketConn{PacketConn: counted, state: state}
}

// resolve 做计费身份归属与失败关闭判定。
//
// 返回 (nil, nil, nil) 表示该 inbound 不在统计名单内，调用方原样透传。
// 返回的第三个值非 nil 表示本次连接被拒绝，字节数为 0 且不入账。
func (t *Tracker) resolve(ctx context.Context, closer any, metadata adapter.InboundContext, network string) (*inboundRecord, *userRecord, net.Conn) {
	record, user := t.registry.lookup(metadata.Inbound, metadata.User)
	if record == nil {
		return nil, nil, nil
	}
	// 计费 inbound 上 metadata.User 为空即失败关闭：未取得计数器就不转发（README §4.6 第 9 条）。
	// 这里位于「已选定 outbound、尚未交给 outbound handler」之处，UDP 首包已搬进
	// bufio.CachedPacketConn 但一个字节都还没外发。
	if metadata.User == "" {
		t.reject(ctx, closer, metadata, network, "匿名连接：计费 inbound 上 metadata.User 为空")
		return record, nil, closedConn{}
	}
	if user == nil {
		t.reject(ctx, closer, metadata, network, "未注册的计费身份："+metadata.User)
		return record, nil, closedConn{}
	}
	nowNanos := monotonicNanos()
	if verdict := t.registry.admit(user, nowNanos); verdict != admitAllow {
		t.registry.noteDenied(user, nowNanos)
		t.registry.quota.throttledConnCount.Add(1)
		t.reject(ctx, closer, metadata, network, "配额闸断："+verdict.reason())
		return record, nil, closedConn{}
	}
	return record, user, nil
}

func (t *Tracker) reject(ctx context.Context, closer any, metadata adapter.InboundContext, network string, reason string) {
	if c, ok := closer.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	if t.logger != nil {
		t.logger.WarnContext(ctx, "拒绝转发 ", network, " 连接 inbound=", metadata.Inbound, " user=", metadata.User, "：", reason)
	}
}

// connState 持有一条连接的计费上下文。数据面只持有该指针，不再查用户表。
type connState struct {
	tracker  *Tracker
	registry *Registry
	inbound  *inboundRecord
	user     *userRecord
	network  string

	audit      *auditWriter
	connUp     atomic.Int64
	connDown   atomic.Int64
	startNanos int64
	host       string
	hostSource string
	port       uint16

	closer    any
	enforced  atomic.Bool
	closeOnce sync.Once
}

func newConnState(tracker *Tracker, record *inboundRecord, user *userRecord, metadata adapter.InboundContext, network string, closer any) *connState {
	state := &connState{
		tracker:    tracker,
		registry:   tracker.registry,
		inbound:    record,
		user:       user,
		network:    network,
		startNanos: monotonicNanos(),
		closer:     closer,
	}
	if audit := tracker.registry.auditWriterRef(); audit != nil {
		state.audit = audit
		state.host, state.hostSource = auditHost(metadata)
		state.port = metadata.Destination.Port
	}
	return state
}

func (s *connState) countUplink(n int64) {
	if n <= 0 {
		return
	}
	value := uint64(n)
	var truncated bool
	if s.network == "tcp" {
		truncated = s.user.tcpUplink.add(value)
	} else {
		truncated = s.user.udpUplink.add(value)
	}
	if truncated {
		s.registry.markOverflow()
	}
	if s.audit != nil {
		s.connUp.Add(n)
	}
	if s.user.quota.consume(n) {
		s.enforce()
	}
}

func (s *connState) countDownlink(n int64) {
	if n <= 0 {
		return
	}
	value := uint64(n)
	var truncated bool
	if s.network == "tcp" {
		truncated = s.user.tcpDownlink.add(value)
	} else {
		truncated = s.user.udpDownlink.add(value)
	}
	if truncated {
		s.registry.markOverflow()
	}
	if s.audit != nil {
		s.connDown.Add(n)
	}
	if s.user.quota.consume(n) {
		s.enforce()
	}
}

// enforce 是 README §4.9 纪律 4 的落点。
//
// 闸断只能实施在计数回调内，不能写在包装层的 Read/Write 里：sing 的 bufio.Copy 会把本包装
// 一路解包到底层 conn、只把 CountFunc 摘走（common/bufio/copy.go:36-37、:265-266），
// splice 路径同样只调 CountFunc（common/bufio/splice_linux.go:88-92），
// 因此包装层的 I/O 方法在快路径上根本不执行。
// N.CountFunc 的签名是 func(n int64)、没有 error 返回（common/network/counter.go:10），
// 所以中止的唯一手段是关闭本连接，让正在跑的 copy 在下一次 I/O 上自然出错退出。
func (s *connState) enforce() {
	if s.enforced.Swap(true) {
		return
	}
	s.registry.noteDenied(s.user, monotonicNanos())
	if closer, ok := s.closer.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// finish 在连接关闭时收敛会话计数并投递审计记录。
//
// Close() 会重入：route/conn.go 的 common.Close(source, destination) 在 done 守卫之外，
// UDP 上下行两个 goroutine 各调用一次（README §4.8 纪律 1），因此必须用 sync.Once 包住。
func (s *connState) finish() {
	s.closeOnce.Do(func() {
		if s.network == "tcp" {
			s.inbound.tcpSessions.Add(-1)
		} else {
			s.inbound.udpSessions.Add(-1)
		}
		if s.audit == nil {
			return
		}
		s.audit.submit(auditRecord{
			user:       s.user.name,
			inboundTag: s.inbound.tag,
			network:    s.network,
			host:       s.host,
			hostSource: s.hostSource,
			port:       s.port,
			up:         s.connUp.Load(),
			down:       s.connDown.Load(),
			durationMs: (monotonicNanos() - s.startNanos) / int64(time.Millisecond),
		})
	})
}

// trackedConn 是包装链最外层。
//
// 它只实现 Close() + Upstream() + Reader/WriterReplaceable() = true，
// 以免挡住 splice/direct 快路径（README §4.8「挂点与写入路径」）。
type trackedConn struct {
	N.ExtendedConn
	state *connState
}

func (c *trackedConn) Close() error {
	c.state.finish()
	return c.ExtendedConn.Close()
}

func (c *trackedConn) Upstream() any           { return c.ExtendedConn }
func (c *trackedConn) ReaderReplaceable() bool { return true }
func (c *trackedConn) WriterReplaceable() bool { return true }
func (c *trackedConn) NeedAdditionalReadDeadline() bool {
	return false
}

type trackedPacketConn struct {
	N.PacketConn
	state *connState
}

func (c *trackedPacketConn) Close() error {
	c.state.finish()
	return c.PacketConn.Close()
}

func (c *trackedPacketConn) Upstream() any           { return c.PacketConn }
func (c *trackedPacketConn) ReaderReplaceable() bool { return true }
func (c *trackedPacketConn) WriterReplaceable() bool { return true }

// closedConn 是失败关闭时返回的包装：立即报错，且不持有任何计数器。
//
// 客户端看到的是「连接建立后被重置」而非协议层认证失败——tracker 位于握手之后；
// 且 TCP 仍会向目的地拨号，因为 ConnectionManager.NewConnection 先拨号后 copy
// （README §2.3 末条、§4.6 第 9 条 (a)(b)）。
type closedConn struct{}

func (closedConn) Read(b []byte) (int, error)         { return 0, os.ErrClosed }
func (closedConn) Write(b []byte) (int, error)        { return 0, os.ErrClosed }
func (closedConn) Close() error                       { return nil }
func (closedConn) LocalAddr() net.Addr                { return zeroAddr{} }
func (closedConn) RemoteAddr() net.Addr               { return zeroAddr{} }
func (closedConn) SetDeadline(t time.Time) error      { return nil }
func (closedConn) SetReadDeadline(t time.Time) error  { return nil }
func (closedConn) SetWriteDeadline(t time.Time) error { return nil }

type zeroAddr struct{}

func (zeroAddr) Network() string { return "closed" }
func (zeroAddr) String() string  { return "closed" }

type rejectedPacketConn struct {
	err error
}

func (c *rejectedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	return M.Socksaddr{}, c.err
}

func (c *rejectedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	buffer.Release()
	return c.err
}

func (c *rejectedPacketConn) Close() error                       { return nil }
func (c *rejectedPacketConn) LocalAddr() net.Addr                { return zeroAddr{} }
func (c *rejectedPacketConn) SetDeadline(t time.Time) error      { return nil }
func (c *rejectedPacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *rejectedPacketConn) SetWriteDeadline(t time.Time) error { return nil }
