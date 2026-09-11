package userstats

import (
	"testing"
)

// BenchmarkCountUplinkNoQuota 度量未启用配额时的计数回调成本。
//
// 这是全进程最热的路径，也是「禁止在回调里查 map、取全局锁或做任何分配」这条纪律的
// 回归观察点：allocs/op 必须恒为 0。
func BenchmarkCountUplinkNoQuota(b *testing.B) {
	state := benchState(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		state.countUplink(1500)
	}
}

// BenchmarkCountUplinkWithQuota 度量启用配额闸断后多出来的两次原子操作。
func BenchmarkCountUplinkWithQuota(b *testing.B) {
	state := benchState(b, true)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		state.countUplink(1500)
	}
}

// BenchmarkSnapshot 度量快照序列化随身份数增长的成本。
func BenchmarkSnapshot(b *testing.B) {
	registry, err := NewRegistry("bench", 4096)
	if err != nil {
		b.Fatal(err)
	}
	users := make([]string, 512)
	for index := range users {
		users[index] = "user-" + itoaBench(index)
	}
	if err = registry.Reconcile([]InboundSpec{{
		Tag: "in", Type: "vless", Listen: "127.0.0.1", ListenPort: 8443, Users: users,
	}}, true); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err = registry.Snapshot(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchState(b *testing.B, quota bool) *connState {
	b.Helper()
	registry, err := NewRegistry("bench", 16)
	if err != nil {
		b.Fatal(err)
	}
	if err = registry.Reconcile([]InboundSpec{{
		Tag: "in", Type: "vless", Listen: "127.0.0.1", ListenPort: 8443, Users: []string{"u1"},
	}}, true); err != nil {
		b.Fatal(err)
	}
	record, user := registry.lookup("in", "u1")
	if user == nil {
		b.Fatal("身份未注册")
	}
	if quota {
		registry.quota.enabled = true
		// 额度给得足够大，使基准衡量的是「扣减」而不是「闸断后立刻短路」。
		user.quota.setLimited(1 << 62)
	}
	return &connState{
		registry: registry,
		inbound:  record,
		user:     user,
		network:  "tcp",
	}
}

func itoaBench(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [12]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
