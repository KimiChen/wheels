package userstats

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
)

// TestSaturatingCounter 断言饱和加法在溢出时钉住而不是回绕。
//
// 回绕会让采集端把一次溢出读成一次巨额负增量，按 §5.1 的单调性约束触发失败关闭，
// 却无法区分「真的溢出」与「实现写错」。
func TestSaturatingCounter(t *testing.T) {
	var counter saturatingUint64
	if truncated := counter.add(10); truncated || counter.load() != 10 {
		t.Fatalf("普通累加异常：truncated=%v value=%d", truncated, counter.load())
	}
	if truncated := counter.add(maxUint64 - 10); truncated || counter.load() != maxUint64 {
		t.Fatalf("累加到上限不应报截断：truncated=%v value=%d", truncated, counter.load())
	}
	if truncated := counter.add(1); !truncated || counter.load() != maxUint64 {
		t.Fatalf("溢出必须报截断且钉在上限：truncated=%v value=%d", truncated, counter.load())
	}
}

// TestQuotaCellWriteOrder 是 §4.9 纪律 2 的直接断言。
//
// 两个方向的写序都不得产生「误判为已闸断」的中间态。
func TestQuotaCellWriteOrder(t *testing.T) {
	var cell quotaCell
	cell.init()
	if cell.exhausted() {
		t.Fatal("初始状态应为无限额度")
	}
	if cell.consume(1 << 20) {
		t.Fatal("无限额度不应被扣减")
	}

	cell.setLimited(100)
	if cell.exhausted() {
		t.Fatal("刚下发 100 字节额度不应立即耗尽")
	}
	if cell.consume(99) {
		t.Fatal("扣减 99 后仍应剩 1 字节")
	}
	if !cell.consume(1) {
		t.Fatal("扣到 0 必须判定耗尽")
	}
	if !cell.exhausted() {
		t.Fatal("耗尽后 exhausted 必须为真")
	}

	// 补额：先写额度再置状态，因此不会出现「已限额但额度还是上一轮的 0」。
	cell.setLimited(50)
	if cell.exhausted() {
		t.Fatal("补额之后应立即恢复")
	}
	// 解限额：先清状态，解封立即生效。
	cell.setLimited(0)
	if !cell.exhausted() {
		t.Fatal("额度为 0 应判定耗尽")
	}
	cell.setUnlimited()
	if cell.exhausted() {
		t.Fatal("解限额应立即生效")
	}
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := NewRegistry("node-unit", 16)
	if err != nil {
		t.Fatalf("创建 registry 失败：%v", err)
	}
	if err = registry.Reconcile([]InboundSpec{{
		Tag: "in", Type: "vless", Listen: "127.0.0.1", ListenPort: 8443,
		Users: []string{"u1", "u2"},
	}}, true); err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	return registry
}

// TestTrackerFailClose 覆盖 §4.6 第 9 条：计费 inbound 上取不到计数器就不转发。
func TestTrackerFailClose(t *testing.T) {
	registry := testRegistry(t)
	tracker := NewTracker(registry, log.NewNOPFactory().Logger())

	cases := []struct {
		name string
		user string
	}{
		{"匿名连接（metadata.User 为空）", ""},
		{"未注册的计费身份", "ghost"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			metadata := adapter.InboundContext{
				Inbound:     "in",
				InboundType: "vless",
				User:        testCase.user,
				Destination: M.ParseSocksaddr("127.0.0.1:80"),
			}
			result := tracker.RoutedConnection(context.Background(), server, metadata, nil, nil)
			if _, rejected := result.(closedConn); !rejected {
				t.Fatalf("应返回立即报错的包装，实际 %T", result)
			}
			// 原连接必须已被关闭：未取得计数器就不转发。
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := client.Read(make([]byte, 1)); err == nil {
				t.Fatal("原连接应已被关闭")
			}
			if _, err := result.Write([]byte("x")); err == nil {
				t.Fatal("返回的包装必须立即报错")
			}
		})
	}

	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatalf("取快照失败：%v", err)
	}
	for _, inbound := range snapshot.Inbounds {
		for _, user := range inbound.Users {
			if user.TCPUplinkBytes != 0 || user.TCPDownlinkBytes != 0 {
				t.Fatalf("失败关闭的连接不得产生任何计数：%+v", user)
			}
		}
	}
}

// TestTrackerPassthroughForUnbilledInbound 断言未纳入统计的 inbound 保持上游快路径不变。
func TestTrackerPassthroughForUnbilledInbound(t *testing.T) {
	registry := testRegistry(t)
	tracker := NewTracker(registry, log.NewNOPFactory().Logger())
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	metadata := adapter.InboundContext{Inbound: "other", User: "", Destination: M.ParseSocksaddr("127.0.0.1:80")}
	result := tracker.RoutedConnection(context.Background(), server, metadata, nil, nil)
	if result != server {
		t.Fatalf("非计费 inbound 必须原样透传，实际 %T", result)
	}
}

// TestReconcileLineage 覆盖 §4.3：删除只切 active、同名重建复用计数器、tombstone 保留。
func TestReconcileLineage(t *testing.T) {
	registry := testRegistry(t)
	_, user := registry.lookup("in", "u1")
	if user == nil {
		t.Fatal("u1 应已注册")
	}
	user.tcpUplink.add(613)

	// 重载后移除 u1。
	if err := registry.Reconcile([]InboundSpec{{
		Tag: "in", Type: "vless", Listen: "127.0.0.1", ListenPort: 8443, Users: []string{"u2"},
	}}, false); err != nil {
		t.Fatalf("重载对账失败：%v", err)
	}
	snapshot, _ := registry.Snapshot()
	removed := findUser(t, snapshot, "in", "u1")
	if removed.Active {
		t.Fatal("移除的身份应切 active=false")
	}
	if removed.TCPUplinkBytes != 613 {
		t.Fatalf("tombstone 必须保留计数：%d", removed.TCPUplinkBytes)
	}

	// 同名重建复用原 lineage 与原计数器，并把 active 切回 true。
	if err := registry.Reconcile([]InboundSpec{{
		Tag: "in", Type: "vless", Listen: "127.0.0.1", ListenPort: 8443, Users: []string{"u1", "u2"},
	}}, false); err != nil {
		t.Fatalf("再次对账失败：%v", err)
	}
	snapshot, _ = registry.Snapshot()
	restored := findUser(t, snapshot, "in", "u1")
	if !restored.Active || restored.TCPUplinkBytes != 613 {
		t.Fatalf("同名重建应复用原计数器并切回 active：%+v", restored)
	}
}

// TestMaxIdentities 覆盖 §4.3 第 5 条的两种情形。
func TestMaxIdentities(t *testing.T) {
	registry, err := NewRegistry("node-unit", 2)
	if err != nil {
		t.Fatalf("创建 registry 失败：%v", err)
	}
	spec := func(users ...string) []InboundSpec {
		return []InboundSpec{{Tag: "in", Type: "vless", Listen: "127.0.0.1", ListenPort: 1, Users: users}}
	}
	// 启动期超限即失败关闭。
	if err = registry.Reconcile(spec("a", "b", "c"), true); err == nil {
		t.Fatal("启动期超出 max_identities 应失败关闭")
	}

	registry, _ = NewRegistry("node-unit", 2)
	if err = registry.Reconcile(spec("a", "b"), true); err != nil {
		t.Fatalf("未超限不应报错：%v", err)
	}
	// SIGHUP 期超限置粘滞位，且不得丢弃 lineage。
	if err = registry.Reconcile(spec("a", "b", "c"), false); err != nil {
		t.Fatalf("重载期超限不应报错，而应置位：%v", err)
	}
	snapshot, _ := registry.Snapshot()
	if !snapshot.Health.IdentityLimitReached {
		t.Fatal("重载期超限必须置位 identity_limit_reached")
	}
	if len(snapshot.Inbounds[0].Users) != 3 {
		t.Fatalf("超限也不得丢弃 lineage，实际保留 %d 个", len(snapshot.Inbounds[0].Users))
	}
	if !registry.unhealthy() {
		t.Fatal("粘滞位置位后 /healthz 必须判定 unhealthy")
	}
}

// TestValidationRegistryRefusesSnapshot 断言校验用 registry 永不产出快照。
func TestValidationRegistryRefusesSnapshot(t *testing.T) {
	registry := NewValidationRegistry()
	if !registry.IsValidationOnly() {
		t.Fatal("应标记为只用于校验")
	}
	if _, err := registry.Snapshot(); err == nil {
		t.Fatal("校验用 registry 不应提供快照")
	}
}

func findUser(t *testing.T, snapshot *Snapshot, tag string, name string) SnapshotUser {
	t.Helper()
	for _, inbound := range snapshot.Inbounds {
		if inbound.Tag != tag {
			continue
		}
		for _, user := range inbound.Users {
			if user.Name == name {
				return user
			}
		}
	}
	t.Fatalf("快照中没有 %s/%s", tag, name)
	return SnapshotUser{}
}

// quotaRegistry 造一个带 n 个身份的裸 registry：不建 Box，因此这些用例能进
// verify.sh 的无抑制 -race 轮（见 tests/race-round-c-exclusions.txt 的判据）。
func quotaRegistry(t *testing.T, users ...string) *Registry {
	t.Helper()
	registry, err := NewRegistry("node-quota", 64)
	if err != nil {
		t.Fatalf("建 registry 失败：%v", err)
	}
	if err = registry.Reconcile([]InboundSpec{{
		Tag: "in", Type: "shadowsocks", Listen: "127.0.0.1", ListenPort: 1, Users: users,
	}}, true); err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	return registry
}

func quotaTable(remaining int64, users ...string) []QuotaEntry {
	entries := make([]QuotaEntry, 0, len(users))
	for _, name := range users {
		entries = append(entries, QuotaEntry{InboundTag: "in", Name: name, RemainingBytes: remaining})
	}
	return entries
}

// TestApplyQuotaTableRejectsStaleEpoch 是 epoch 单调性的确定性用例。
//
// 它盯的是「旧表覆盖新表」这一后果，而不只是返回码：断言 epoch 没有被存回旧值，
// 且 cell 里仍是新表的额度——只断言错误码的话，一个先写 cell 再校验 epoch 的实现
// 照样能通过。
func TestApplyQuotaTableRejectsStaleEpoch(t *testing.T) {
	registry := quotaRegistry(t, "u1")
	if _, _, err := registry.ApplyQuotaTable(6, quotaTable(6000, "u1")); err != nil {
		t.Fatalf("epoch 6 本应被接受：%v", err)
	}
	_, _, err := registry.ApplyQuotaTable(5, quotaTable(5000, "u1"))
	if !errors.Is(err, errStaleEpoch) {
		t.Fatalf("epoch 5 本应以 errStaleEpoch 被拒，实际：%v", err)
	}
	if got := registry.quota.epoch.Load(); got != 6 {
		t.Fatalf("epoch 被回退到 %d，应保持 6", got)
	}
	_, user := registry.lookup("in", "u1")
	if got := user.quota.remaining.Load(); got != 6000 {
		t.Fatalf("cell 被旧表覆盖成 %d，应保持 6000", got)
	}
	// 相同 epoch 同样拒绝：重投一份等值的表不应被当成新表。
	if _, _, err = registry.ApplyQuotaTable(6, quotaTable(1, "u1")); !errors.Is(err, errStaleEpoch) {
		t.Fatalf("重复的 epoch 6 本应被拒，实际：%v", err)
	}
}

// TestApplyQuotaTableEpochUnderConcurrency 让两个 epoch 相邻的推送在屏障后同时进入。
//
// 判定不在返回码上而在最终状态：胜出的那一份必须同时赢下 epoch 与 cell 两项。
// 旧实现里判定在锁外，两份都能通过预检，后落的那份会把两者一起改回去。
func TestApplyQuotaTableEpochUnderConcurrency(t *testing.T) {
	const rounds = 200
	for round := 1; round <= rounds; round++ {
		registry := quotaRegistry(t, "u1")
		low, high := uint64(2*round), uint64(2*round+1)
		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		results := make([]error, 2)
		for index, epoch := range []uint64{low, high} {
			done.Add(1)
			go func(index int, epoch uint64) {
				defer done.Done()
				start.Wait()
				_, _, results[index] = registry.ApplyQuotaTable(epoch, quotaTable(int64(epoch), "u1"))
			}(index, epoch)
		}
		start.Done()
		done.Wait()

		accepted := []uint64{}
		for index, epoch := range []uint64{low, high} {
			if results[index] == nil {
				accepted = append(accepted, epoch)
			} else if !errors.Is(results[index], errStaleEpoch) {
				t.Fatalf("第 %d 轮：意外错误 %v", round, results[index])
			}
		}
		if len(accepted) == 0 {
			t.Fatalf("第 %d 轮：两份都被拒绝", round)
		}
		winner := accepted[len(accepted)-1]
		if got := registry.quota.epoch.Load(); got != winner {
			t.Fatalf("第 %d 轮：epoch 为 %d，最后被接受的是 %d", round, got, winner)
		}
		_, user := registry.lookup("in", "u1")
		if got := user.quota.remaining.Load(); got != int64(winner) {
			t.Fatalf("第 %d 轮：cell 为 %d，与胜出的 epoch %d 不符", round, got, winner)
		}
	}
}

// TestSnapshotSequenceAndCountersAreTakenTogether 让读快照与涨计数同时发生。
//
// 判据是 §5.1 结算方赖以工作的那个配对：把若干份并发快照按 sequence 排序后，
// 计数总和必须非递减。先取序号后读计数的实现会产出「序号更小而计数更大」的一份，
// 结算方按单调性约束 fail-closed，对着一个完全正确的进程停止入账。
//
// 用裸 registry 而非起 Box，以便这条能进无抑制 -race 轮。
func TestSnapshotSequenceAndCountersAreTakenTogether(t *testing.T) {
	registry := quotaRegistry(t, "u1")
	_, user := registry.lookup("in", "u1")

	stop := make(chan struct{})
	var traffic sync.WaitGroup
	traffic.Add(1)
	go func() {
		defer traffic.Done()
		for {
			select {
			case <-stop:
				return
			default:
				user.tcpUplink.add(1)
			}
		}
	}()

	const readers = 8
	const rounds = 40
	type pair struct {
		sequence uint64
		total    uint64
	}
	for round := 0; round < rounds; round++ {
		results := make([]pair, readers)
		var readersDone sync.WaitGroup
		var start sync.WaitGroup
		start.Add(1)
		for index := 0; index < readers; index++ {
			readersDone.Add(1)
			go func(index int) {
				defer readersDone.Done()
				start.Wait()
				snapshot, err := registry.Snapshot()
				if err != nil {
					return
				}
				got := snapshot.Inbounds[0].Users[0]
				results[index] = pair{
					sequence: snapshot.Sequence,
					total: got.TCPUplinkBytes + got.TCPDownlinkBytes +
						got.UDPUplinkBytes + got.UDPDownlinkBytes,
				}
			}(index)
		}
		start.Done()
		readersDone.Wait()

		sort.Slice(results, func(a, b int) bool { return results[a].sequence < results[b].sequence })
		for index := 1; index < len(results); index++ {
			if results[index].total < results[index-1].total {
				close(stop)
				traffic.Wait()
				t.Fatalf("第 %d 轮：seq %d 的计数 %d 小于 seq %d 的 %d——按 sequence 排序后计数回退了",
					round, results[index].sequence, results[index].total,
					results[index-1].sequence, results[index-1].total)
			}
		}
	}
	close(stop)
	traffic.Wait()
}
