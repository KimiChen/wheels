// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

// TestTrafficTrackerSemantics 验证内存累计器：首见只建基线、同范围差分、
// 回退/范围变化只重建基线不清累计、跨天归属。
func TestTrafficTrackerSemantics(t *testing.T) {
	tt := NewTrafficTracker()

	// 首见只建基线
	tt.Apply("n1", "boot-a", "eth0", 1000, 2000, "2026-09-25")
	v, ok := tt.View("n1", "2026-09-25")
	if !ok || v.TotalRX != 0 || v.TodayRX != 0 {
		t.Fatalf("首见应只建基线：%+v ok=%v", v, ok)
	}
	if _, ok := tt.View("n2", "2026-09-25"); ok {
		t.Fatal("未知节点应 ok=false")
	}

	// 同范围差分
	tt.Apply("n1", "boot-a", "eth0", 1300, 2500, "2026-09-25")
	v, _ = tt.View("n1", "2026-09-25")
	if v.TodayRX != 300 || v.TodayTX != 500 || v.TotalRX != 300 || v.TotalTX != 500 {
		t.Fatalf("差分累计不符：%+v", v)
	}

	// 回退：重建基线不清累计
	tt.Apply("n1", "boot-a", "eth0", 50, 60, "2026-09-25")
	v, _ = tt.View("n1", "2026-09-25")
	if v.TotalRX != 300 || v.TodayRX != 300 {
		t.Fatalf("回退不应清累计：%+v", v)
	}

	// 范围变化（iface 变化）：重建基线
	tt.Apply("n1", "boot-a", "eth0,eth1", 9000, 9000, "2026-09-26")
	v, _ = tt.View("n1", "2026-09-26")
	if v.TotalRX != 300 || v.TodayRX != 0 {
		t.Fatalf("范围变化应重建基线、新一天从零开始：%+v", v)
	}

	// 新范围差分计入新的一天
	tt.Apply("n1", "boot-a", "eth0,eth1", 9100, 9200, "2026-09-26")
	v, _ = tt.View("n1", "2026-09-26")
	if v.TodayRX != 100 || v.TodayTX != 200 || v.TotalRX != 400 || v.TotalTX != 700 {
		t.Fatalf("跨天归属不符：%+v", v)
	}

	days := tt.Daily("n1", recentDays(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), 7))
	if len(days) != 2 || days[0].Day != "2026-09-25" || days[0].RX != 300 || days[1].Day != "2026-09-26" || days[1].TX != 200 {
		t.Fatalf("日统计不符：%+v", days)
	}
}

// TestTrafficRestoreRoundtrip 验证内存与 DB 持久化一致并可恢复。
func TestTrafficRestoreRoundtrip(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	st := New()
	st.AttachDB(db)
	t.Cleanup(st.Close)

	now := time.Unix(1790380800, 0) // 2026-09-26 UTC
	st.StartSession("n1", "s1", 1, now)
	mk := func(seq uint64, rx, tx uint64) *metrics.Metrics {
		return &metrics.Metrics{
			CollectedAt: now.Unix(), CPU: 1,
			NetRXTotal: rx, NetTXTotal: tx, BootID: "boot-a", Iface: "eth0",
		}
	}
	if err := st.Report("n1", "s1", 1, nil, mk(1, 1000, 2000), nil, now); err != nil {
		t.Fatal(err)
	}
	if err := st.Report("n1", "s1", 2, nil, mk(2, 1300, 2500), nil, now); err != nil {
		t.Fatal(err)
	}
	st.DrainWrites()

	states, daily, err := db.LoadTraffic()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].TotalRX != 300 || states[0].BaseRX != 1300 {
		t.Fatalf("traffic_state 不符：%+v", states)
	}
	if len(daily) != 1 || daily[0].Day != "2026-09-26" || daily[0].RX != 300 {
		t.Fatalf("traffic_daily 不符：%+v", daily)
	}

	// 恢复到新 tracker
	tt := NewTrafficTracker()
	tt.Restore(states, daily)
	v, ok := tt.View("n1", "2026-09-26")
	if !ok || v.TotalRX != 300 || v.TodayRX != 300 {
		t.Fatalf("恢复后视图不符：%+v ok=%v", v, ok)
	}
	// 恢复后继续差分（同一基线，不重复累计）
	tt.Apply("n1", "boot-a", "eth0", 1400, 2600, "2026-09-26")
	v, _ = tt.View("n1", "2026-09-26")
	if v.TotalRX != 400 || v.TodayRX != 400 {
		t.Fatalf("恢复后差分不符：%+v", v)
	}
}

// TestReportSkipsUnknownNetTotal 验证 quality.net_total=unknown 时不写基线
// （缺失读数不能以 0 覆盖基线）。
func TestReportSkipsUnknownNetTotal(t *testing.T) {
	st := New()
	now := time.Unix(1790380800, 0)
	st.StartSession("n1", "s1", 1, now)

	unknown := &metrics.Metrics{
		CollectedAt: now.Unix(), CPU: 1,
		NetRXTotal: 5000, NetTXTotal: 6000, BootID: "boot-a", Iface: "eth0",
		Quality: &metrics.Quality{NetTotal: metrics.QualityUnknown},
	}
	if err := st.Report("n1", "s1", 1, nil, unknown, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Traffic("n1", now); ok {
		t.Fatal("unknown 计数器不应建立流量状态")
	}

	// 有效读数：建基线
	okm := &metrics.Metrics{
		CollectedAt: now.Unix(), CPU: 1,
		NetRXTotal: 5000, NetTXTotal: 6000, BootID: "boot-a", Iface: "eth0",
	}
	if err := st.Report("n1", "s1", 2, nil, okm, nil, now); err != nil {
		t.Fatal(err)
	}
	v, ok := st.Traffic("n1", now)
	if !ok || v.TotalRX != 0 {
		t.Fatalf("首个有效读数应只建基线：%+v ok=%v", v, ok)
	}

	// 再次 unknown：不得覆盖已有基线/累计
	if err := st.Report("n1", "s1", 3, nil, &metrics.Metrics{
		CollectedAt: now.Unix(), CPU: 1,
		NetRXTotal: 99999, NetTXTotal: 99999, BootID: "boot-a", Iface: "eth0",
		Quality: &metrics.Quality{NetTotal: metrics.QualityUnknown},
	}, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := st.Report("n1", "s1", 4, nil, &metrics.Metrics{
		CollectedAt: now.Unix(), CPU: 1,
		NetRXTotal: 5100, NetTXTotal: 6100, BootID: "boot-a", Iface: "eth0",
	}, nil, now); err != nil {
		t.Fatal(err)
	}
	v, _ = st.Traffic("n1", now)
	if v.TotalRX != 100 || v.TotalTX != 100 {
		t.Fatalf("unknown 读数不得污染差分：%+v", v)
	}
}
