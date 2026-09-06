package userstats

import (
	"sync"
	"sync/atomic"
	"time"
)

// 闸断状态的两个取值。用 uint32 而非 bool 是为了让 setLimited／setUnlimited 的写序可读。
const (
	quotaUnlimited uint32 = 0
	quotaLimited   uint32 = 1
)

// quotaCell 是逐 lineage 的闸断单元（README §4.9 纪律 1）。
//
// 它挂在 userRecord 上、生命周期与 lineage 相同，因此数据面在认证成功时捕获一次指针后，
// 热路径只做原子操作：不查 map、不取锁、不分配。
type quotaCell struct {
	state     atomic.Uint32
	remaining atomic.Int64

	// throttleUntilNanos 是被闸断 lineage 的重连节流截止时刻（单调时钟纳秒）。
	throttleUntilNanos atomic.Int64
}

func (c *quotaCell) init() {
	c.state.Store(quotaUnlimited)
}

// consume 在计数回调内调用：扣减额度并返回本次是否已耗尽。
//
// 这是全进程最热的路径，每次 copy 迭代与每次 splice 循环都会跑（README §4.9 纪律 1、4）。
func (c *quotaCell) consume(delta int64) (exhausted bool) {
	if c.state.Load() == quotaUnlimited {
		return false
	}
	return c.remaining.Add(-delta) <= 0
}

// exhausted 供新连接准入判断使用。
func (c *quotaCell) exhausted() bool {
	if c.state.Load() == quotaUnlimited {
		return false
	}
	return c.remaining.Load() <= 0
}

// setLimited 与 setUnlimited 的写序是 README §4.9 纪律 2 的逐 lineage 实现。
//
// 换页不得出现「其他用户被误判」的中间态。两个方法各自选择了不会产生误判的写序：
// 置限额时先写额度再置状态——绝不会出现「已标记限额但额度还是上一轮的 0」；
// 解限额时先清状态再写额度——解封立即生效，额度值随后归零不影响判断。
// 因此逐 lineage 的两次原子写，在「是否会错误闸断」这一属性上等价于整表原子换页。
func (c *quotaCell) setLimited(remaining int64) {
	c.remaining.Store(remaining)
	c.state.Store(quotaLimited)
}

func (c *quotaCell) setUnlimited() {
	c.state.Store(quotaUnlimited)
	c.remaining.Store(0)
}

// throttled 判断该 lineage 是否处在重连冷却窗口内。
func (c *quotaCell) throttled(nowNanos int64) bool {
	until := c.throttleUntilNanos.Load()
	return until != 0 && nowNanos < until
}

func (c *quotaCell) markThrottle(untilNanos int64) {
	c.throttleUntilNanos.Store(untilNanos)
}

// QuotaAction 是「没有额度信息时怎么办」的处置，README §4.9。
type QuotaAction string

const (
	QuotaActionAllow QuotaAction = "allow"
	QuotaActionDeny  QuotaAction = "deny"
)

// quotaState 是 registry 上的配额控制面状态。
type quotaState struct {
	// enabled 为假时整条闸断链路不存在：准入判断直接返回放行，不读任何字段。
	enabled bool

	startupAction     QuotaAction
	staleAction       QuotaAction
	staleAfter        time.Duration
	reconnectThrottle time.Duration

	epoch              atomic.Uint64
	accepted           atomic.Bool
	lastAcceptedNanos  atomic.Int64
	appliedEntries     atomic.Int64
	lastAppliedUnixMs  atomic.Int64
	rejectedByRuntime  atomic.Int64
	rejectedByEpoch    atomic.Int64
	rejectedByUnknown  atomic.Int64
	throttledConnCount atomic.Int64

	// apply 串行化整表覆盖。它只在控制面请求路径上取，不进数据面热路径。
	apply sync.Mutex
}

func (q *quotaState) init() {
	q.startupAction = QuotaActionAllow
	q.staleAction = QuotaActionAllow
}

// QuotaEntry 是 PUT /v2/quota 请求体中的一项。
type QuotaEntry struct {
	InboundTag     string `json:"inbound_tag"`
	Name           string `json:"name"`
	RemainingBytes int64  `json:"remaining_bytes"`
}

// admitVerdict 是准入判断的结果。
type admitVerdict int

const (
	admitAllow admitVerdict = iota
	admitDenyExhausted
	admitDenyThrottled
	admitDenyNoTable
	admitDenyStale
)

func (v admitVerdict) reason() string {
	switch v {
	case admitDenyExhausted:
		return "配额已耗尽"
	case admitDenyThrottled:
		return "重连节流窗口内"
	case admitDenyNoTable:
		return "尚未收到配额全量表且 startup_action=deny"
	case admitDenyStale:
		return "配额全量表已过期且 stale_action=deny"
	default:
		return ""
	}
}

// admit 判断一条新连接是否放行。未启用配额控制时恒放行。
func (r *Registry) admit(user *userRecord, nowNanos int64) admitVerdict {
	if !r.quota.enabled {
		return admitAllow
	}
	if user.quota.exhausted() {
		return admitDenyExhausted
	}
	if user.quota.throttled(nowNanos) {
		return admitDenyThrottled
	}
	if !r.quota.accepted.Load() {
		if r.quota.startupAction == QuotaActionDeny {
			return admitDenyNoTable
		}
		return admitAllow
	}
	if r.quota.staleAfter > 0 && r.quota.staleAction == QuotaActionDeny {
		if nowNanos-r.quota.lastAcceptedNanos.Load() > int64(r.quota.staleAfter) {
			return admitDenyStale
		}
	}
	return admitAllow
}

// noteDenied 在拒绝一条连接后记下冷却窗口。
func (r *Registry) noteDenied(user *userRecord, nowNanos int64) {
	if r.quota.reconnectThrottle <= 0 {
		return
	}
	user.quota.markThrottle(nowNanos + int64(r.quota.reconnectThrottle))
}

// ApplyQuotaTable 应用一份全量剩余额度表（README §4.9 控制端点契约）。
//
// 语义是全量覆盖而非增量：未出现在 entries 中的 lineage 视为无限额度。
// 任一 entry 指向当前配置中不存在的 (inbound_tag, name) 时整份拒绝，并返回前若干条未知项，
// 因为全量表本应与进程读的是同一份配置真相，对不上说明控制面与数据面已经不一致。
func (r *Registry) ApplyQuotaTable(epoch uint64, entries []QuotaEntry) (applied int, unknown []string, err error) {
	r.quota.apply.Lock()
	defer r.quota.apply.Unlock()

	// 第一遍：全部解析并校验，任何一条不认识就整份拒绝，不写入任何 cell。
	type resolvedEntry struct {
		user      *userRecord
		remaining int64
	}
	resolved := make([]resolvedEntry, 0, len(entries))
	seen := make(map[*userRecord]struct{}, len(entries))
	for _, entry := range entries {
		_, user := r.lookup(entry.InboundTag, entry.Name)
		if user == nil {
			if len(unknown) < 8 {
				unknown = append(unknown, entry.InboundTag+"/"+entry.Name)
			}
			continue
		}
		resolved = append(resolved, resolvedEntry{user: user, remaining: entry.RemainingBytes})
		seen[user] = struct{}{}
	}
	if len(unknown) > 0 {
		r.quota.rejectedByUnknown.Add(1)
		return 0, unknown, errUnknownLineage
	}

	// 第二遍：先解除不在表内的 lineage（解封方向永不误判），再逐条置限额。
	for _, record := range r.sortedInbounds() {
		for _, user := range record.sortedUsers() {
			if _, limited := seen[user]; !limited {
				user.quota.setUnlimited()
			}
		}
	}
	for _, item := range resolved {
		item.user.quota.setLimited(item.remaining)
		// 额度重新下发即解除该 lineage 的重连冷却，否则补额之后仍会被自己的节流挡住。
		item.user.quota.markThrottle(0)
	}

	r.quota.epoch.Store(epoch)
	r.quota.accepted.Store(true)
	r.quota.lastAcceptedNanos.Store(monotonicNanos())
	r.quota.lastAppliedUnixMs.Store(time.Now().UnixMilli())
	r.quota.appliedEntries.Store(int64(len(resolved)))
	return len(resolved), nil, nil
}

var startMonotonic = time.Now()

// monotonicNanos 返回单调时钟纳秒。闸断与节流不使用墙钟：时钟回拨会让冷却窗口变成永久拒绝。
func monotonicNanos() int64 {
	return int64(time.Since(startMonotonic))
}
