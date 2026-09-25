// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

func nullFloat(v float64) sql.NullFloat64 {
	return sql.NullFloat64{Float64: v, Valid: true}
}

// collectAgg 返回一个把冲刷行收集到切片的 Aggregator。
func collectAgg(rows *[]MetricRow) *Aggregator {
	return NewAggregator(func(r MetricRow) { *rows = append(*rows, r) })
}

// TestAggregatorAverages 验证窗口内样本均值、cpu_max、cover_ms，
// 以及 unknown 质量组不纳入平均（该字段为 NULL）。
func TestAggregatorAverages(t *testing.T) {
	var rows []MetricRow
	agg := NewAggregator(func(r MetricRow) { rows = append(rows, r) })
	t.Cleanup(agg.Stop)

	base := time.Unix(1790380800, 0) // 分钟对齐
	// 三个样本 net_rate 均为 unknown（不纳入平均，写库为 NULL）；
	// 第 3 个样本 mem 也 unknown（不纳入 mem 平均，但计入 samples）。
	mk := func(sec int64, cpu float64, mem uint64, memUnknown bool) *metrics.Metrics {
		q := &metrics.Quality{NetRate: metrics.QualityUnknown}
		if memUnknown {
			q.Mem = metrics.QualityUnknown
		}
		return &metrics.Metrics{
			CollectedAt: base.Unix() + sec, CPU: cpu,
			Load: [3]float64{1, 2, 3}, MemUsed: mem,
			NetRX: 100, NetTX: 200, TCP: 10, UDP: 5, Procs: 3,
			Quality: q,
		}
	}
	agg.Add("n1", mk(10, 10, 100, false), base.Add(10*time.Second))
	agg.Add("n1", mk(20, 20, 200, false), base.Add(20*time.Second))
	agg.Add("n1", mk(30, 30, 9999, true), base.Add(30*time.Second))
	agg.FlushAll()

	if len(rows) != 1 {
		t.Fatalf("应冲刷 1 行：%d", len(rows))
	}
	r := rows[0]
	if r.TsMin != base.Unix() || r.Samples != 3 {
		t.Fatalf("行头不符：%+v", r)
	}
	if r.CoverMS != 20000 {
		t.Fatalf("cover_ms 应为 20000（首末样本差）：%d", r.CoverMS)
	}
	if !r.CPUAvg.Valid || r.CPUAvg.Float64 != 20 {
		t.Fatalf("cpu_avg 应为 20：%+v", r.CPUAvg)
	}
	if !r.CPUMax.Valid || r.CPUMax.Float64 != 30 {
		t.Fatalf("cpu_max 应为 30：%+v", r.CPUMax)
	}
	if !r.MemUsedAvg.Valid || r.MemUsedAvg.Float64 != 150 {
		t.Fatalf("mem_used_avg 应为 (100+200)/2=150（unknown 不计入）：%+v", r.MemUsedAvg)
	}
	if r.NetRXAvg.Valid || r.NetTXAvg.Valid {
		t.Fatalf("net_rate 整组 unknown 应为 NULL：%+v %+v", r.NetRXAvg, r.NetTXAvg)
	}
	if !r.TCPAvg.Valid || r.TCPAvg.Float64 != 10 {
		t.Fatalf("tcp_avg 应为 10：%+v", r.TCPAvg)
	}
	if !r.Load5Avg.Valid || r.Load5Avg.Float64 != 2 {
		t.Fatalf("load5_avg 应为 2：%+v", r.Load5Avg)
	}
}

// TestAggregatorMinuteRollover 验证跨分钟时旧桶自动冲刷。
func TestAggregatorMinuteRollover(t *testing.T) {
	var rows []MetricRow
	agg := collectAgg(&rows)
	t.Cleanup(agg.Stop)

	m1 := time.Unix(1790380800, 0)
	m2 := m1.Add(time.Minute)
	agg.Add("n1", &metrics.Metrics{CollectedAt: m1.Unix(), CPU: 1}, m1)
	agg.Add("n1", &metrics.Metrics{CollectedAt: m2.Unix(), CPU: 2}, m2)

	if len(rows) != 1 || rows[0].TsMin != m1.Unix() {
		t.Fatalf("跨分钟应冲刷旧桶：%+v", rows)
	}
	agg.FlushAll()
	if len(rows) != 2 || rows[1].TsMin != m2.Unix() {
		t.Fatalf("FlushAll 应冲刷剩余桶：%+v", rows)
	}
}

// TestAggregatorSweep 验证节点停止上报后清扫 goroutine 冲刷过期桶。
func TestAggregatorSweep(t *testing.T) {
	var rows []MetricRow
	agg := collectAgg(&rows)
	t.Cleanup(agg.Stop)

	old := time.Now().Add(-2 * time.Minute)
	agg.Add("n1", &metrics.Metrics{CollectedAt: old.Unix(), CPU: 1}, old)
	agg.sweep(time.Now())
	if len(rows) != 1 {
		t.Fatalf("清扫应冲刷过期桶：%+v", rows)
	}
}

// TestMetricsRangeRead 验证写入 metrics_1m 后可按区间读取（含 NULL 字段）。
func TestMetricsRangeRead(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	st := New()
	st.AttachDB(db)
	t.Cleanup(st.Close)

	db.enqueueMetricsRow(MetricRow{NodeID: "n1", TsMin: 1790380800, Samples: 2,
		CPUAvg: nullFloat(20), CPUMax: nullFloat(30), MemUsedAvg: nullFloat(150)})
	db.enqueueMetricsRow(MetricRow{NodeID: "n1", TsMin: 1790380860, Samples: 1,
		CPUAvg: nullFloat(40), CPUMax: nullFloat(40), NetRXAvg: nullFloat(5), NetTXAvg: nullFloat(6)})
	db.Drain()

	got, err := st.MetricsRange("n1", 1790380800, 1790380920)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 行：%d", len(got))
	}
	if got[0].CPUAvg.Float64 != 20 || got[0].NetRXAvg.Valid {
		t.Fatalf("第一行不符：%+v", got[0])
	}
	if got[1].NetRXAvg.Float64 != 5 {
		t.Fatalf("第二行不符：%+v", got[1])
	}
}

// TestMetricsUpsertMerge 验证同一分钟重复写入（重启重叠）按样本数加权合并。
func TestMetricsUpsertMerge(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	db.enqueueMetricsRow(MetricRow{NodeID: "n1", TsMin: 1790380800, Samples: 2,
		CPUAvg: nullFloat(10), CPUMax: nullFloat(20)})
	db.enqueueMetricsRow(MetricRow{NodeID: "n1", TsMin: 1790380800, Samples: 1,
		CPUAvg: nullFloat(40), CPUMax: nullFloat(15), NetRXAvg: nullFloat(7)})
	db.Drain()

	got, err := db.MetricsRange("n1", 1790380800, 1790380860)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应合并为 1 行：%d", len(got))
	}
	r := got[0]
	if r.Samples != 3 {
		t.Fatalf("samples 应累加为 3：%d", r.Samples)
	}
	// (10*2 + 40*1) / 3 = 20
	if !r.CPUAvg.Valid || r.CPUAvg.Float64 != 20 {
		t.Fatalf("cpu_avg 加权合并应为 20：%+v", r.CPUAvg)
	}
	if !r.CPUMax.Valid || r.CPUMax.Float64 != 20 {
		t.Fatalf("cpu_max 应取最大 20：%+v", r.CPUMax)
	}
	if !r.NetRXAvg.Valid || r.NetRXAvg.Float64 != 7 {
		t.Fatalf("excluded NULL 不覆盖、新增列应写入：%+v", r.NetRXAvg)
	}
}
