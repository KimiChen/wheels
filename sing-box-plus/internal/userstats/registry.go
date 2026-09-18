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
	// maxTotalLineages 是含墓碑在内的总量上限，由 maxIdentities 派生而非独立配置：
	// 墓碑是 §4.3 要求保留的（结算方要靠它把最后一段字节收尾），但它不该占用业务名额。
	maxTotalLineages int
	validationOnly   bool

	sequence atomic.Uint64

	// snapshotMu 把「取序号」与「读计数」绑成一次原子动作。
	//
	// 两者若不成对，两个并发 GET /v3/snapshot 可以产出 seq 小而计数大的一份：
	// A 先取到 seq=5，B 随后取到 seq=6 并先读完计数，A 再读时计数已经涨了。
	// 结算方按 seq 排序后会看到计数回退，据 §5.1 的单调性约束 fail-closed——
	// 对着一个完全正确的进程停止入账。
	//
	// 注意不能改成「把 nextSequence 挪到最后」：那样两个读者可以按一种顺序读计数、
	// 按相反顺序取序号，同样的倒序照旧成立，而且连序列化都没有了。只有互斥能给出
	// 结算方假定的那一对。控制面每 15 秒一次，互斥的代价可以忽略。
	snapshotMu sync.Mutex

	counterOverflow      atomic.Bool
	sequenceOverflow     atomic.Bool
	identityLimitReached atomic.Bool

	mu       sync.RWMutex
	inbounds map[string]*inboundRecord

	quota quotaState

	audit atomic.Pointer[auditWriter]
	// auditDroppedSticky 跨 writer 生命周期保留 audit_dropped。
	//
	// 该位的文档承诺是「该 runtime 余下时间粘滞」，而 writer 每次 SIGHUP 都会重建：
	// 只读当前 writer 的计数器，这个位会跟着清零，承诺不成立。同一处也解决另一半——
	// SIGHUP 期间投给已关闭 writer 的记录是静默丢弃的：不写 ev=gap 行（写不进去了），
	// 也不增加任何计数，于是证据链上那个洞连信号都没有。
	auditDroppedSticky atomic.Bool
	fatalHandler       atomic.Pointer[func(error)]
	draining           atomic.Bool
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
		nodeID:           nodeID,
		runtimeID:        hex.EncodeToString(raw[:]),
		startedAtUnixMs:  uint64(time.Now().UnixMilli()),
		maxIdentities:    maxIdentities,
		maxTotalLineages: maxIdentities * 4,
		inbounds:         make(map[string]*inboundRecord),
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
		nodeID:           "validation-only",
		runtimeID:        "00000000000000000000000000000000",
		maxIdentities:    1 << 20,
		maxTotalLineages: 1 << 22,
		validationOnly:   true,
		inbounds:         make(map[string]*inboundRecord),
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
		record.active.Store(true)

		record.mu.Lock()
		// 这三个字段与 users 同受 record.mu 保护。此前它们在锁外赋值，而 Snapshot
		// 经 sortedInbounds 拿到指针后同样在锁外读——撕裂的 string 头是崩溃而不只是
		// 读到旧值。当前进程内走不到（run 循环总是先 Close 再 Reconcile），但那是个
		// 既未写明也未强制的不变量，而 Reconcile 是导出方法。
		record.inboundType = spec.Type
		record.listen = spec.Listen
		record.listenPort = spec.ListenPort
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

	// max_identities 只约束**活跃**血统。
	//
	// 此前它数的是 len(record.users)，而 Reconcile 从不删除记录——停用的身份只把 active
	// 置 false 后长期留存。于是每一轮身份轮换都让这个数单调增长，越过上限后
	// identityLimitReached 置位且永不复位，/healthz 在该 runtime 余下时间里恒为 503，
	// 结算方据此拒绝入账：计费瘫痪，而唯一的恢复手段是重启进程。
	//
	// 那个粘滞位的语义是「快照里的数字不可信」（见 Health 的文档串），可墓碑并没有让任何
	// 数字失真——Reconcile 不截断任何东西。所以它是按构造过度触发的。
	if active := r.countActiveLineagesLocked(); active > r.maxIdentities {
		if startup {
			return E.New("活跃计费身份数 ", active, " 超过 max_identities ", r.maxIdentities)
		}
		r.identityLimitReached.Store(true)
	}
	// 总量（含墓碑）仍需有界，否则长期轮换会把内存吃光。越过这条才是真正的
	// 「这个 runtime 已经不可信」，此时置粘滞位是恰当的。
	if total := r.countAllLineagesLocked(); total > r.maxTotalLineages {
		if startup {
			return E.New("计费身份总数（含已停用）", total, " 超过上限 ", r.maxTotalLineages)
		}
		r.identityLimitReached.Store(true)
	}
	return nil
}

// countActiveLineagesLocked 只数 active 的血统，用于 max_identities。
// spec 在 record.mu 的读锁下取出三个可变的 inbound 描述字段。
func (record *inboundRecord) spec() (inboundType string, listen string, listenPort uint16) {
	record.mu.RLock()
	defer record.mu.RUnlock()
	return record.inboundType, record.listen, record.listenPort
}

func (r *Registry) countActiveLineagesLocked() int {
	var total int
	for _, record := range r.inbounds {
		record.mu.RLock()
		for _, user := range record.users {
			if user.active.Load() {
				total++
			}
		}
		record.mu.RUnlock()
	}
	return total
}

// countAllLineagesLocked 数全部血统（含墓碑），用于内存上限。
func (r *Registry) countAllLineagesLocked() int {
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
func (r *Registry) configureQuota(options *QuotaControlOptions) {
	if options == nil {
		// 无条件安装一份策略：否则移除 quota_control 之后 enabled 会一直停在 true。
		// 当前这条走不到——CheckReloadInvariant 不允许把 quota 路径改成空——属防御性写法，
		// 不要据此新增不变量：新增就必须同步改 update-network-remote.py 的 RELOAD_INVARIANTS，
		// 否则编排会计划一次节点随后拒绝的 reload，却报告 runtime_preserved: true。
		r.quota.policy.Store(&quotaPolicy{startupAction: QuotaActionAllow, staleAction: QuotaActionAllow})
		return
	}
	r.quota.policy.Store(&quotaPolicy{
		enabled:           true,
		startupAction:     QuotaAction(options.StartupAction),
		staleAction:       QuotaAction(options.StaleAction),
		staleAfter:        time.Duration(options.StaleAfter),
		reconnectThrottle: time.Duration(options.ReconnectThrottle),
	})
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

// unhealthy 是 /healthz 的判据：**只看前三位**，audit_dropped 不在内。
//
// 审计丢弃不影响计费计数器的正确性。把它算进 503 会让一次写失败停掉整条入账链路，
// 那是拿可用性去换一个本该只是告警的信号。它只出现在快照的 health.audit_dropped 里。
func (r *Registry) unhealthy() bool {
	return r.counterOverflow.Load() || r.sequenceOverflow.Load() || r.identityLimitReached.Load()
}

// auditDropped 读 §4.8 旁路的粘滞丢弃计数；未启用审计时恒为 false。
//
// droppedWrite 只增不减，所以这一位天然粘滞，与另外三位一致：置位后该 runtime 余下时间保持为真，
// 清除它的唯一方式是重启进程，这样运维不会因为「刚才那一下已经过去了」而漏掉证据链缺口。
func (r *Registry) auditDropped() bool {
	if r.auditDroppedSticky.Load() {
		return true
	}
	writer := r.auditWriterRef()
	if writer != nil && writer.droppedWrite.Load() > 0 {
		// 记进粘滞位，使它不随 writer 一起消失。
		r.auditDroppedSticky.Store(true)
		return true
	}
	return false
}

// noteAuditDropped 由审计侧在「连 gap 行都写不出去」时调用，直接置粘滞位。
func (r *Registry) noteAuditDropped() {
	r.auditDroppedSticky.Store(true)
}

// QuotaStatus 是配额控制面的可观测状态。
//
// 它刻意不进 GET /v3/snapshot：快照键集是结算契约的闭集，两端都按严格相等校验，
// 加一个键就等于 schema v4，需要先在控制器与每个节点上同步升级解析器。
// 这些数字是运维遥测而不是结算输入，单开一条附加路由代价小得多。
type QuotaStatus struct {
	SchemaVersion int `json:"schema_version"`
	// Enabled 为假时下面的计数全部无意义。
	Enabled  bool   `json:"enabled"`
	Accepted bool   `json:"accepted"`
	Epoch    uint64 `json:"epoch"`
	// LastAppliedUnixMs 为 0 表示本 runtime 尚未成功应用过任何一张表。
	LastAppliedUnixMs int64 `json:"last_applied_unix_ms"`
	AppliedEntries    int64 `json:"applied_entries"`
	// 以下四项是拒绝计数，排查「这个身份为什么连不上」时最先要看的东西。
	RejectedByRuntime int64 `json:"rejected_by_runtime"`
	RejectedByEpoch   int64 `json:"rejected_by_epoch"`
	RejectedByUnknown int64 `json:"rejected_by_unknown"`
	ThrottledConns    int64 `json:"throttled_conns"`
	// 活跃与总血统数：越过前者即 max_identities，越过后者才置 identity_limit_reached。
	// 放在这里让运维能提前看到逼近，而不是等 /healthz 变 503 才发现。
	ActiveLineages int `json:"active_lineages"`
	TotalLineages  int `json:"total_lineages"`
}

// QuotaStatus 取一份配额控制面快照。它不推进 sequence——那是结算用的序号，
// 只有 GET /v3/snapshot 该推进它。
func (r *Registry) QuotaStatus() QuotaStatus {
	policy := r.quota.policy.Load()
	r.mu.RLock()
	active := r.countActiveLineagesLocked()
	total := r.countAllLineagesLocked()
	r.mu.RUnlock()
	return QuotaStatus{
		SchemaVersion:     SchemaVersion,
		Enabled:           policy.enabled,
		Accepted:          r.quota.accepted.Load(),
		Epoch:             r.quota.epoch.Load(),
		LastAppliedUnixMs: r.quota.lastAppliedUnixMs.Load(),
		AppliedEntries:    r.quota.appliedEntries.Load(),
		RejectedByRuntime: r.quota.rejectedByRuntime.Load(),
		RejectedByEpoch:   r.quota.rejectedByEpoch.Load(),
		RejectedByUnknown: r.quota.rejectedByUnknown.Load(),
		ThrottledConns:    r.quota.throttledConnCount.Load(),
		ActiveLineages:    active,
		TotalLineages:     total,
	}
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

// Identities 返回当前已注册的全部计费身份名。
//
// 供审计的启动期文件名碰撞检查使用：那类检查必须在 Start 期一次性做完并失败关闭，
// 等到运行期第一次写文件才发现两个人共用一个文件就晚了。
func (r *Registry) Identities() []string {
	var names []string
	for _, record := range r.sortedInbounds() {
		for _, user := range record.sortedUsers() {
			names = append(names, user.name)
		}
	}
	return names
}
