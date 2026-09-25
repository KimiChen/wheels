// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

func TestSetProbeTasksVersions(t *testing.T) {
	st := New()
	v, err := st.SetProbeTasks("n1", []protocol.PingTask{{ID: "t1", Target: "example.com:443", Interval: 5}})
	if err != nil || v != 1 {
		t.Fatalf("首个版本应为 1：v=%d err=%v", v, err)
	}
	v, err = st.SetProbeTasks("n1", []protocol.PingTask{{ID: "t1", Target: "example.com:443", Interval: 10}})
	if err != nil || v != 2 {
		t.Fatalf("版本应单调递增为 2：v=%d err=%v", v, err)
	}
	// 非法任务（interval 越界）拒绝且版本不动
	if _, err := st.SetProbeTasks("n1", []protocol.PingTask{{ID: "t1", Target: "x:1", Interval: 1}}); err == nil {
		t.Fatal("interval 越界应拒绝")
	}
	if got, tasks := st.ProbeTasks("n1"); got != 2 || len(tasks) != 1 || tasks[0].Interval != 10 {
		t.Fatalf("版本/任务不应变化：v=%d tasks=%+v", got, tasks)
	}
	// 重复 ID 拒绝
	if _, err := st.SetProbeTasks("n1", []protocol.PingTask{
		{ID: "t1", Target: "a:1", Interval: 5}, {ID: "t1", Target: "b:2", Interval: 5},
	}); err == nil {
		t.Fatal("重复任务 ID 应拒绝")
	}
	// 清空任务列表也推进版本
	v, err = st.SetProbeTasks("n1", nil)
	if err != nil || v != 3 {
		t.Fatalf("清空应推进版本：v=%d err=%v", v, err)
	}
}

func TestRestoreProbeTasksRejectsRegression(t *testing.T) {
	st := New()
	if err := st.RestoreProbeTasks("n1", 5, []protocol.PingTask{{ID: "t1", Target: "a:1", Interval: 5}}); err != nil {
		t.Fatalf("首次恢复应成功：%v", err)
	}
	if err := st.RestoreProbeTasks("n1", 5, nil); !errors.Is(err, ErrProbeVersionRegression) {
		t.Fatalf("同版本恢复应拒绝：%v", err)
	}
	if err := st.RestoreProbeTasks("n1", 4, nil); !errors.Is(err, ErrProbeVersionRegression) {
		t.Fatalf("回退恢复应拒绝：%v", err)
	}
	if v, _ := st.ProbeTasks("n1"); v != 5 {
		t.Fatalf("版本应保持 5：%d", v)
	}
}

func TestRecordPingResultsSessionAndSequence(t *testing.T) {
	st := New()
	now := time.Unix(1790380800, 0)

	// 未建会话 → ErrStaleSession
	if err := st.RecordPingResults("n1", "s1", 1, []protocol.PingResult{{TaskID: "t1", LatencyMS: 10}}, now); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("未建会话应 ErrStaleSession：%v", err)
	}

	st.StartSession("n1", "s1", 1, now)
	// 错误会话 → ErrStaleSession
	if err := st.RecordPingResults("n1", "sX", 1, nil, now); !errors.Is(err, ErrStaleSession) {
		t.Fatalf("错误会话应 ErrStaleSession：%v", err)
	}
	// 序号严格递增
	if err := st.RecordPingResults("n1", "s1", 1, []protocol.PingResult{{TaskID: "t1", LatencyMS: 10}}, now); err != nil {
		t.Fatalf("seq=1 应成功：%v", err)
	}
	if err := st.RecordPingResults("n1", "s1", 1, []protocol.PingResult{{TaskID: "t1", LatencyMS: 20}}, now); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("重复 seq 应 ErrOutOfOrder：%v", err)
	}

	// 新会话 ping 序号重置（与 report 序号独立）
	st.StartSession("n1", "s2", 1, now)
	if err := st.RecordPingResults("n1", "s2", 1, nil, now); err != nil {
		t.Fatalf("新会话 ping 序号应从 1 开始：%v", err)
	}
	// report 序号不受 ping.result 影响
	if err := st.Report("n1", "s2", 1, nil, testMetrics(), nil, now); err != nil {
		t.Fatalf("report 序号应独立：%v", err)
	}
}

func TestProbeStatsSlidingWindow(t *testing.T) {
	st := New()
	now := time.Unix(1790380800, 0)
	st.StartSession("n1", "s1", 1, now)
	if _, err := st.SetProbeTasks("n1", []protocol.PingTask{
		{ID: "t1", Target: "a:1", Interval: 5},
		{ID: "t2", Target: "b:2", Interval: 5},
	}); err != nil {
		t.Fatal(err)
	}

	// t1：失败、23ms、16 分钟前的旧样本（应被滑窗排除）；t2：无样本
	results := []protocol.PingResult{{TaskID: "t1", LatencyMS: protocol.LatencyFailed}, {TaskID: "t1", LatencyMS: 23}}
	if err := st.RecordPingResults("n1", "s1", 1, results, now); err != nil {
		t.Fatal(err)
	}
	st.StartSession("n1", "s2", 1, now)
	if err := st.RecordPingResults("n1", "s2", 1,
		[]protocol.PingResult{{TaskID: "t1", LatencyMS: 99}}, now.Add(-16*time.Minute)); err != nil {
		t.Fatal(err)
	}

	stats := st.ProbeStats("n1", now)
	if len(stats) != 2 {
		t.Fatalf("应有两个任务的统计：%+v", stats)
	}
	var t1, t2 ProbeStat
	for _, s := range stats {
		if s.ID == "t1" {
			t1 = s
		}
		if s.ID == "t2" {
			t2 = s
		}
	}
	if t1.Samples != 2 { // 16 分钟前的样本被滑窗排除
		t.Fatalf("t1 滑窗样本应为 2：%+v", t1)
	}
	if t1.LastLatency == nil || *t1.LastLatency != 23 {
		t.Fatalf("t1 最近成功延迟应为 23：%+v", t1.LastLatency)
	}
	if t1.FailRate == nil || *t1.FailRate != 0.5 {
		t.Fatalf("t1 失败率应为 0.5：%+v", t1.FailRate)
	}
	if t2.Samples != 0 || t2.LastLatency != nil || t2.FailRate != nil {
		t.Fatalf("t2 无样本应为 null 统计：%+v", t2)
	}
}

// TestProbeResultsPersisted 验证结果落库（latency -1 计失败）。
func TestProbeResultsPersisted(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	st := New()
	st.AttachDB(db)
	t.Cleanup(st.Close)

	now := time.Unix(1790380800, 0)
	st.StartSession("n1", "s1", 1, now)
	if err := st.RecordPingResults("n1", "s1", 1, []protocol.PingResult{
		{TaskID: "t1", LatencyMS: 23}, {TaskID: "t1", LatencyMS: protocol.LatencyFailed},
	}, now); err != nil {
		t.Fatal(err)
	}
	st.DrainWrites()

	var cnt int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM probe_results WHERE node_id='n1' AND task_id='t1'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Fatalf("应有 2 行结果：%d", cnt)
	}
	var failed int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM probe_results WHERE latency_ms = -1`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("应有 1 行失败：%d", failed)
	}
}

// TestSetProbeTasksPersisted 验证任务列表持久化与恢复。
func TestSetProbeTasksPersisted(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	st := New()
	st.AttachDB(db)

	if _, err := st.SetProbeTasks("n1", []protocol.PingTask{{ID: "t1", Target: "a:1", Interval: 5}}); err != nil {
		t.Fatal(err)
	}
	st.DrainWrites()
	st.Close() // 含 db.Close（幂等）

	// 重开恢复
	db2, err := OpenDB(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	st2 := New()
	st2.AttachDB(db2)
	defer st2.Close()

	v, tasks := st2.ProbeTasks("n1")
	if v != 1 || len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("恢复后任务不符：v=%d tasks=%+v", v, tasks)
	}
	// 恢复后版本继续单调递增
	v2, err := st2.SetProbeTasks("n1", nil)
	if err != nil || v2 != 2 {
		t.Fatalf("恢复后 PUT 应推进到版本 2：v=%d err=%v", v2, err)
	}
}
