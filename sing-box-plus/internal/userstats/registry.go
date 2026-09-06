package userstats

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

// generationFixed 是 README §3／§4.3 第 4 条的保留维度：本项目恒输出 1，但不得从采集键中省略。
const generationFixed uint64 = 1

// InboundSpec 是一次配置对账的输入，由 Validate 从 option.Options 中抽取。
type InboundSpec struct {
	Tag        string
	Type       string
	Listen     string
	ListenPort uint16
	Users      []string
}

type userRecord struct {
	name       string
	generation uint64
	active     atomic.Bool

	tcpUplink   saturatingUint64
	tcpDownlink saturatingUint64
	udpUplink   saturatingUint64
	udpDownlink saturatingUint64

	quota quotaCell
}

type inboundRecord struct {
	tag        string
	generation uint64
	active     atomic.Bool

	// 以下四项来自配置，重载时按新配置覆盖；快照按 §4.5 输出配置值而非实际绑定结果。
	inboundType string
	listen      string
	listenPort  uint16

	tcpSessions atomic.Int64
	udpSessions atomic.Int64

	mu    sync.RWMutex
	users map[string]*userRecord
}

// Registry 是进程级的统计与闸断状态持有者。
//
// 生命周期是**进程级**而非 Box 级（README §4.4）：runtime_id、started_at_unix_ms 与 sequence
// 三者必须由自有 main 在进入 run 循环之前生成并由本对象保存。挂在 services[] 实例上的状态
// 每次 SIGHUP 都会重置，三种错误拆法的后果见 §4.4。
type Registry struct {
	nodeID          string
	runtimeID       string
	startedAtUnixMs uint64
	maxIdentities   int
	validationOnly  bool

	sequence atomic.Uint64

	counterOverflow      atomic.Bool
	sequenceOverflow     atomic.Bool
	identityLimitReached atomic.Bool

	mu       sync.RWMutex
	inbounds map[string]*inboundRecord

	quota quotaState

	audit        atomic.Pointer[auditWriter]
	fatalHandler atomic.Pointer[func(error)]
	draining     atomic.Bool
}

// NewRegistry 创建进程级 registry 并就地捕获信封字段。
func NewRegistry(nodeID string, maxIdentities int) (*Registry, error) {
	if maxIdentities <= 0 {
		return nil, E.New("max_identities 必须为正数")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, E.Cause(err, "生成 runtime_id")
	}
	registry := &Registry{
		nodeID:          nodeID,
		runtimeID:       hex.EncodeToString(raw[:]),
		startedAtUnixMs: uint64(time.Now().UnixMilli()),
		maxIdentities:   maxIdentities,
		inbounds:        make(map[string]*inboundRecord),
	}
	registry.quota.init()
	return registry, nil
}

// NewValidationRegistry 仅供 check 子命令与 SIGHUP 重载前的配置检查使用。
//
// box.New 会构造 services[] 实例，而 user_stats 实例禁止自建信封字段（§4.7）；
// check 路径又必然没有进程级 registry 可用。用一个显式标记为「只用于校验」的实例填补，
// 它永不 Start、永不绑定 socket、永不参与结算，且拒绝提供快照。
func NewValidationRegistry() *Registry {
	registry := &Registry{
		nodeID:         "validation-only",
		runtimeID:      "00000000000000000000000000000000",
		maxIdentities:  1 << 20,
		validationOnly: true,
		inbounds:       make(map[string]*inboundRecord),
	}
	registry.quota.init()
	return registry
}

func (r *Registry) NodeID() string    { return r.nodeID }
func (r *Registry) RuntimeID() string { return r.runtimeID }
func (r *Registry) IsValidationOnly() bool {
	return r.validationOnly
}

// Reconcile 按新配置对账 lineage 集合（README §4.3 第 1／2／5 条、§4.4 第 2 条）。
//
// startup 为真时超出 max_identities 即报错（失败关闭）；为假（SIGHUP 重载）时置位
// health.identity_limit_reached 并保留 lineage，绝不丢弃。
func (r *Registry) Reconcile(specs []InboundSpec, startup bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	seenInbound := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		seenInbound[spec.Tag] = struct{}{}
		record, loaded := r.inbounds[spec.Tag]
		if !loaded {
			record = &inboundRecord{
				tag:        spec.Tag,
				generation: generationFixed,
				users:      make(map[string]*userRecord, len(spec.Users)),
			}
			r.inbounds[spec.Tag] = record
		}
		record.inboundType = spec.Type
		record.listen = spec.Listen
		record.listenPort = spec.ListenPort
		record.active.Store(true)

		record.mu.Lock()
		seenUser := make(map[string]struct{}, len(spec.Users))
		for _, name := range spec.Users {
			seenUser[name] = struct{}{}
			user, exists := record.users[name]
			if !exists {
				user = &userRecord{name: name, generation: generationFixed}
				user.quota.init()
				record.users[name] = user
			}
			// 同名重建复用原 lineage 与原计数器，只把 active 切回 true（§4.3 第 2 条）。
			user.active.Store(true)
		}
		for name, user := range record.users {
			if _, keep := seenUser[name]; !keep {
				// 删除或停用只切 active，记录保留在快照中（§4.3 第 1 条）。
				user.active.Store(false)
			}
		}
		record.mu.Unlock()
	}
	for tag, record := range r.inbounds {
		if _, keep := seenInbound[tag]; !keep {
			record.active.Store(false)
			record.mu.RLock()
			for _, user := range record.users {
				user.active.Store(false)
			}
			record.mu.RUnlock()
		}
	}

	total := r.countLineagesLocked()
	if total > r.maxIdentities {
		if startup {
			return E.New("计费身份数 ", total, " 超过 max_identities ", r.maxIdentities)
		}
		// 该位粘滞，置位后该 runtime 余下时间全部不可入账（§4.3 第 5 条）。
		r.identityLimitReached.Store(true)
	}
	return nil
}

func (r *Registry) countLineagesLocked() int {
	var total int
	for _, record := range r.inbounds {
		record.mu.RLock()
		total += len(record.users)
		record.mu.RUnlock()
	}
	return total
}

// lookup 返回计费身份对应的记录；未注册时返回 nil。
//
// 每条连接只在认证成功后查一次，之后数据面只持有该指针（§4.1）。
func (r *Registry) lookup(tag string, name string) (*inboundRecord, *userRecord) {
	r.mu.RLock()
	record, loaded := r.inbounds[tag]
	r.mu.RUnlock()
	if !loaded {
		return nil, nil
	}
	record.mu.RLock()
	user := record.users[name]
	record.mu.RUnlock()
	return record, user
}

// markOverflow 在任一 *_bytes 发生饱和截断时置位粘滞的 counter_overflow。
func (r *Registry) markOverflow() {
	r.counterOverflow.Store(true)
}

// nextSequence 推进快照序号；饱和时置位 sequence_overflow。
func (r *Registry) nextSequence() uint64 {
	for {
		current := r.sequence.Load()
		if current == maxUint64 {
			r.sequenceOverflow.Store(true)
			return current
		}
		if r.sequence.CompareAndSwap(current, current+1) {
			return current + 1
		}
	}
}

func (r *Registry) sortedInbounds() []*inboundRecord {
	r.mu.RLock()
	records := make([]*inboundRecord, 0, len(r.inbounds))
	for _, record := range r.inbounds {
		records = append(records, record)
	}
	r.mu.RUnlock()
	sort.Slice(records, func(i, j int) bool {
		if records[i].tag != records[j].tag {
			return records[i].tag < records[j].tag
		}
		return records[i].generation < records[j].generation
	})
	return records
}

func (record *inboundRecord) sortedUsers() []*userRecord {
	record.mu.RLock()
	users := make([]*userRecord, 0, len(record.users))
	for _, user := range record.users {
		users = append(users, user)
	}
	record.mu.RUnlock()
	sort.Slice(users, func(i, j int) bool {
		if users[i].name != users[j].name {
			return users[i].name < users[j].name
		}
		return users[i].generation < users[j].generation
	})
	return users
}

// setAudit / auditWriterRef 让 §4.8 的 JSONL 旁路挂在进程级 registry 上。
//
// tracker 由 main 在 box.New 之后注入、跨 SIGHUP 存活，而 audit writer 属于 services[] 实例、
// 每次重载重建；用一次原子读把两者的生命周期解耦，读发生在每条连接建立时而非每次 I/O。
func (r *Registry) setAudit(writer *auditWriter) {
	r.audit.Store(writer)
}

func (r *Registry) auditWriterRef() *auditWriter {
	return r.audit.Load()
}

// configureQuota 由 user_stats 实例在 Start 时按配置启用 §4.9 的闸断链路。
func (r *Registry) configureQuota(options QuotaControlOptions) {
	r.quota.enabled = true
	r.quota.startupAction = QuotaAction(options.StartupAction)
	r.quota.staleAction = QuotaAction(options.StaleAction)
	r.quota.staleAfter = time.Duration(options.StaleAfter)
	r.quota.reconnectThrottle = time.Duration(options.ReconnectThrottle)
}

// SetFatalHandler 注册致命错误回调，由自有 main 提供：exporter 意外退出、panic 或连续 accept()
// 失败时整个进程必须失败退出（README §4.6 第 5 条）。
func (r *Registry) SetFatalHandler(handler func(error)) {
	r.fatalHandler.Store(&handler)
}

func (r *Registry) fatal(err error) {
	if handler := r.fatalHandler.Load(); handler != nil {
		(*handler)(err)
	}
}

// unhealthy 是 /healthz 的判据：health 三位任一为真即 503。
func (r *Registry) unhealthy() bool {
	return r.counterOverflow.Load() || r.sequenceOverflow.Load() || r.identityLimitReached.Load()
}

// BeginDrain 进入排空状态：拒绝新的计费连接，已有连接继续跑完。
//
// 这是 §4.4 第 3 条在**零补丁形态下能做到的那一半**。真正的「停止 accept」需要拿到 listener，
// 而上游没有导出路径；因此这里只能在 tracker 处拒绝新连接——被拒的连接 0 字节、不入账，
// 但它仍然会完成协议握手并向目的地拨号（与 §4.6 第 9 条同一条已知差异）。
// 运维流程里的「停止接入新连接」仍应由下线或防火墙先行完成，本机制是兜底而非替代。
func (r *Registry) BeginDrain() {
	r.draining.Store(true)
}

// Draining 供 tracker 与运维查询。
func (r *Registry) Draining() bool {
	return r.draining.Load()
}

// ActiveSessions 返回当前活跃的 TCP 与 UDP 会话总数。
//
// 与快照里的 tcp_sessions / udp_sessions 同源，是 §5.3 第 2 步的排空判据。
func (r *Registry) ActiveSessions() int64 {
	var total int64
	for _, record := range r.sortedInbounds() {
		total += record.tcpSessions.Load()
		total += record.udpSessions.Load()
	}
	return total
}

// WaitDrained 轮询至会话归零或超时，返回是否已排空。
//
// 超时强切是允许的——已计字节不会丢失——但调用方必须把该窗口标记为未排空以便审计。
func (r *Registry) WaitDrained(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if r.ActiveSessions() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
