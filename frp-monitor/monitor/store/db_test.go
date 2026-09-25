// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

func openTestDB(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := OpenDB(dir, 7)
	if err != nil {
		t.Fatalf("OpenDB 失败：%v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestMigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)

	var v int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version 应为 %d，得到 %d", schemaVersion, v)
	}

	// 写入一行节点，关闭后重开：迁移幂等且数据保留
	db.enqueueNodeUpsert("n1", &metrics.Facts{Hostname: "h1", OS: "linux"}, time.Unix(1790380800, 0))
	db.Drain()
	db.Close()

	db2 := openTestDB(t, dir)
	rows, err := db2.LoadNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].NodeID != "n1" || rows[0].Facts == nil || rows[0].Facts.Hostname != "h1" {
		t.Fatalf("重开后节点应保留：%+v", rows)
	}
	if rows[0].FirstSeen != 1790380800 || rows[0].LastSeen != 1790380800 {
		t.Fatalf("first_seen/last_seen 不符：%+v", rows[0])
	}
}

// TestNodeUpsertKeepsFacts 验证无 facts 的 report 不覆盖已有 facts_json。
func TestNodeUpsertKeepsFacts(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	db.enqueueNodeUpsert("n1", &metrics.Facts{Hostname: "h1", OS: "linux"}, time.Unix(1790380800, 0))
	db.enqueueNodeUpsert("n1", nil, time.Unix(1790380860, 0))
	db.Drain()

	rows, err := db.LoadNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Facts == nil || rows[0].Facts.Hostname != "h1" {
		t.Fatalf("无 facts 的 upsert 不得覆盖已有 facts：%+v", rows)
	}
	if rows[0].LastSeen != 1790380860 {
		t.Fatalf("last_seen 应更新：%+v", rows[0])
	}
}

// queryTrafficState 直接读 traffic_state（测试辅助）。
func queryTrafficState(t *testing.T, db *DB, nodeID string) (TrafficStateRow, bool) {
	t.Helper()
	var r TrafficStateRow
	var baseRX, baseTX, totalRX, totalTX int64
	err := db.sql.QueryRow(`SELECT node_id, boot_id, iface, baseline_rx, baseline_tx, total_rx, total_tx, updated_at
		FROM traffic_state WHERE node_id = ?`, nodeID).
		Scan(&r.NodeID, &r.BootID, &r.Iface, &baseRX, &baseTX, &totalRX, &totalTX, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return TrafficStateRow{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	r.BaseRX, r.BaseTX, r.TotalRX, r.TotalTX = uint64(baseRX), uint64(baseTX), uint64(totalRX), uint64(totalTX)
	return r, true
}

// applyTrafficCommit 直接在事务内应用并提交（同步测试 applyTrafficTx）。
func applyTrafficCommit(t *testing.T, db *DB, nodeID, bootID, iface string, rx, txv uint64, day string, now int64) error {
	t.Helper()
	tx, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := applyTrafficTx(tx, nodeID, bootID, iface, rx, txv, day, now); err != nil {
		return err
	}
	return tx.Commit()
}

// TestTrafficTxSemantics 覆盖：首见只建基线、同范围差分累计、回退重建、
// boot 变化重建（不清累计）、跨天归属。
func TestTrafficTxSemantics(t *testing.T) {
	db := openTestDB(t, t.TempDir())

	// 首见只建基线：累计为 0，无日统计
	if err := applyTrafficCommit(t, db, "n1", "boot-a", "eth0", 1000, 2000, "2026-09-25", 1790300000); err != nil {
		t.Fatal(err)
	}
	st, ok := queryTrafficState(t, db, "n1")
	if !ok || st.TotalRX != 0 || st.TotalTX != 0 || st.BaseRX != 1000 || st.BaseTX != 2000 {
		t.Fatalf("首见应只建基线：%+v", st)
	}

	// 同范围差分累计
	if err := applyTrafficCommit(t, db, "n1", "boot-a", "eth0", 1300, 2500, "2026-09-25", 1790300060); err != nil {
		t.Fatal(err)
	}
	st, _ = queryTrafficState(t, db, "n1")
	if st.TotalRX != 300 || st.TotalTX != 500 || st.BaseRX != 1300 || st.BaseTX != 2500 {
		t.Fatalf("差分累计不符：%+v", st)
	}

	// 计数器回退：只重建基线，不清累计，不写日统计
	if err := applyTrafficCommit(t, db, "n1", "boot-a", "eth0", 50, 60, "2026-09-25", 1790300120); err != nil {
		t.Fatal(err)
	}
	st, _ = queryTrafficState(t, db, "n1")
	if st.TotalRX != 300 || st.TotalTX != 500 || st.BaseRX != 50 || st.BaseTX != 60 {
		t.Fatalf("回退应重建基线不清累计：%+v", st)
	}

	// boot_id 变化：重建基线
	if err := applyTrafficCommit(t, db, "n1", "boot-b", "eth0", 9000, 8000, "2026-09-26", 1790380000); err != nil {
		t.Fatal(err)
	}
	st, _ = queryTrafficState(t, db, "n1")
	if st.BootID != "boot-b" || st.TotalRX != 300 || st.TotalTX != 500 {
		t.Fatalf("boot 变化应重建基线不清累计：%+v", st)
	}

	// 跨天归属：新基线上的差分计入新的一天
	if err := applyTrafficCommit(t, db, "n1", "boot-b", "eth0", 9100, 8200, "2026-09-26", 1790380100); err != nil {
		t.Fatal(err)
	}
	st, _ = queryTrafficState(t, db, "n1")
	if st.TotalRX != 400 || st.TotalTX != 700 {
		t.Fatalf("累计应继续：%+v", st)
	}
	daily, err := db.TrafficDailyRange("n1", "2026-09-20")
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 2 {
		t.Fatalf("应有两天的日统计：%+v", daily)
	}
	if daily[0].Day != "2026-09-25" || daily[0].RX != 300 || daily[0].TX != 500 {
		t.Fatalf("2026-09-25 日统计不符：%+v", daily[0])
	}
	if daily[1].Day != "2026-09-26" || daily[1].RX != 100 || daily[1].TX != 200 {
		t.Fatalf("2026-09-26 日统计不符：%+v", daily[1])
	}
}

// TestTrafficTxAtomicity 注入失败（删表）验证基线与累计同事务：失败时
// 基线不得推进、累计不得写入（不半截写入）。
func TestTrafficTxAtomicity(t *testing.T) {
	db := openTestDB(t, t.TempDir())

	if err := applyTrafficCommit(t, db, "n1", "boot-a", "eth0", 1000, 2000, "2026-09-26", 1790380000); err != nil {
		t.Fatal(err)
	}
	// 破坏 traffic_daily：后续同事务写入必然失败
	if _, err := db.sql.Exec("DROP TABLE traffic_daily"); err != nil {
		t.Fatal(err)
	}
	if err := applyTrafficCommit(t, db, "n1", "boot-a", "eth0", 1500, 2600, "2026-09-26", 1790380060); err == nil {
		t.Fatal("写入失败应返回 error")
	}
	st, ok := queryTrafficState(t, db, "n1")
	if !ok {
		t.Fatal("traffic_state 行应存在")
	}
	if st.BaseRX != 1000 || st.BaseTX != 2000 || st.TotalRX != 0 || st.TotalTX != 0 {
		t.Fatalf("事务回滚后基线/累计不得变化：%+v", st)
	}
}

// TestRetentionSweep 验证每小时清理删除早于保留期的 metrics_1m 与 probe_results。
func TestRetentionSweep(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Unix(1790380800, 0)
	old := now.Add(-8 * 24 * time.Hour).Unix()    // 8 天前，应被清理
	recent := now.Add(-6 * 24 * time.Hour).Unix() // 6 天前，应保留
	for _, ts := range []int64{old, recent} {
		if _, err := db.sql.Exec(`INSERT INTO metrics_1m (node_id, ts_min, samples, cover_ms) VALUES ('n1', ?, 1, 0)`, ts); err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`INSERT INTO probe_results (node_id, task_id, ts, latency_ms) VALUES ('n1', 't1', ?, 10)`, ts); err != nil {
			t.Fatal(err)
		}
	}

	db.run(db.sweepJob(now))

	var metricsCount, resultsCount int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM metrics_1m").Scan(&metricsCount); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM probe_results").Scan(&resultsCount); err != nil {
		t.Fatal(err)
	}
	if metricsCount != 1 || resultsCount != 1 {
		t.Fatalf("清理后应各剩 1 行：metrics=%d results=%d", metricsCount, resultsCount)
	}
	var remainMin int64
	if err := db.sql.QueryRow("SELECT ts_min FROM metrics_1m").Scan(&remainMin); err != nil {
		t.Fatal(err)
	}
	if remainMin != recent {
		t.Fatalf("应保留 6 天前的行，得到 ts_min=%d", remainMin)
	}
}

// TestProbePersistenceRoundtrip 验证探测任务与结果的持久化往返。
func TestProbePersistenceRoundtrip(t *testing.T) {
	db := openTestDB(t, t.TempDir())

	tasks := []protocol.PingTask{
		{ID: "t2", Target: "example.com:443", Interval: 30},
		{ID: "t1", Target: "10.0.0.1:22", Interval: 5},
	}
	db.enqueueProbeTasksReplace("n1", 3, tasks)
	db.enqueueProbeResults("n1", []protocol.PingResult{
		{TaskID: "t1", LatencyMS: 23},
		{TaskID: "t2", LatencyMS: protocol.LatencyFailed},
	}, time.Unix(1790380800, 0))
	db.Drain()

	versions, loaded, err := db.LoadProbeTasks()
	if err != nil {
		t.Fatal(err)
	}
	if versions["n1"] != 3 {
		t.Fatalf("版本应为 3：%v", versions)
	}
	// 按 task_id 排序加载
	lt := loaded["n1"]
	if len(lt) != 2 || lt[0].ID != "t1" || lt[1].ID != "t2" || lt[0].Interval != 5 || lt[1].Target != "example.com:443" {
		t.Fatalf("任务往返不符：%+v", lt)
	}

	var cnt, fails int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM probe_results WHERE node_id='n1'").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM probe_results WHERE latency_ms=-1").Scan(&fails); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 || fails != 1 {
		t.Fatalf("结果行数/失败数不符：cnt=%d fails=%d", cnt, fails)
	}
}
