// SPDX-License-Identifier: Apache-2.0

// store 的 SQLite 持久化层（根 README §6）。
//
// 打开即迁移（PRAGMA user_version，当前 v1），WAL + busy_timeout。
// 全部写操作经单 worker 的有界队列（4096）执行：队列满时丢弃并记日志，
// 写库错误只记日志降级，绝不拖停接收/转发路径。读查询（历史 API、
// 恢复加载）直接走 database/sql，由 busy_timeout 与单连接串行化兜底。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

const (
	// dbFileName 为 DataDir 下的库文件名。
	dbFileName = "frp-monitor.db"
	// dbQueueSize 为写队列上限（有界：DB 变慢时丢弃写任务而非拖停接收）。
	dbQueueSize = 4096
	// dbDrainTimeout 为 Close 等待写队列清空的时限。
	dbDrainTimeout = 2 * time.Second
	// retentionSweepInterval 为保留清理周期（每小时）。
	retentionSweepInterval = time.Hour
)

// schemaVersion 为当前 schema 版本（PRAGMA user_version）。v1 为首个版本。
const schemaVersion = 1

// writeJob 为一个写库任务，在 worker 单 goroutine 中执行。
type writeJob func(db *sql.DB) error

// DB 为 SQLite 持久化句柄。并发安全；Close 幂等。
type DB struct {
	sql           *sql.DB
	retentionDays int

	queue      chan writeJob
	stop       chan struct{}
	workerDone chan struct{}
	closeOnce  sync.Once

	dropped atomic.Uint64
}

// OpenDB 打开（必要时创建）DataDir 下的 SQLite 库并执行迁移；
// retentionDays 为 metrics_1m 与 probe_results 的保留天数，0 取默认 7 天。
// 启动写 worker 后立即入队一次保留清理，之后每小时清理一次。
func OpenDB(dataDir string, retentionDays int) (*DB, error) {
	if retentionDays <= 0 {
		retentionDays = 7
	}
	if retentionDays > 365 {
		retentionDays = 365
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("store: 数据目录创建失败：%w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := url.URL{Scheme: "file", Path: filepath.Join(dataDir, dbFileName), RawQuery: q.Encode()}

	sqlDB, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("store: SQLite 打开失败：%w", err)
	}
	// 单连接串行化读写；WAL 下读不阻塞写，写本就单 worker。
	sqlDB.SetMaxOpenConns(1)

	db := &DB{
		sql:           sqlDB,
		retentionDays: retentionDays,
		queue:         make(chan writeJob, dbQueueSize),
		stop:          make(chan struct{}),
		workerDone:    make(chan struct{}),
	}
	if err := db.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	go db.worker()
	db.enqueue(db.sweepJob(time.Now()))
	return db, nil
}

// Close 停止 worker、限时清空队列并关闭库。幂等。
func (db *DB) Close() {
	db.closeOnce.Do(func() {
		close(db.stop)
		select {
		case <-db.workerDone:
		case <-time.After(dbDrainTimeout):
			log.Printf("store: 关闭时等待写队列清空超时（%s）", dbDrainTimeout)
		}
		_ = db.sql.Close()
	})
}

// Drain 等待已入队的写任务全部执行完（测试与优雅关闭用）。
func (db *DB) Drain() {
	done := make(chan struct{})
	select {
	case db.queue <- func(*sql.DB) error { close(done); return nil }:
	case <-db.workerDone:
		return
	}
	select {
	case <-done:
	case <-db.workerDone:
	}
}

// enqueue 非阻塞入队；队列满时丢弃并限频记日志（监控降级，不影响转发）。
func (db *DB) enqueue(job writeJob) {
	select {
	case db.queue <- job:
	default:
		if n := db.dropped.Add(1); n == 1 || n%dbQueueSize == 0 {
			log.Printf("store: 写队列已满，累计丢弃 %d 个写任务（监控降级，不影响转发）", n)
		}
	}
}

// worker 为唯一写 goroutine；stop 后清空剩余任务再退出。
func (db *DB) worker() {
	defer close(db.workerDone)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("store: 写 worker panic（已降级）：%v", r)
		}
	}()
	sweep := time.NewTicker(retentionSweepInterval)
	defer sweep.Stop()
	for {
		select {
		case job := <-db.queue:
			db.run(job)
		case <-sweep.C:
			db.run(db.sweepJob(time.Now()))
		case <-db.stop:
			for {
				select {
				case job := <-db.queue:
					db.run(job)
				default:
					return
				}
			}
		}
	}
}

// run 执行单个写任务：错误与 panic 都只记日志。
func (db *DB) run(job writeJob) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("store: 写库任务 panic（已降级）：%v", r)
		}
	}()
	if err := job(db.sql); err != nil {
		log.Printf("store: 写库失败（已降级）：%v", err)
	}
}

// sweepJob 删除早于保留期的 metrics_1m 与 probe_results。
// ts_min 与 ts 均为服务端接收时刻的 Unix 秒。
func (db *DB) sweepJob(now time.Time) writeJob {
	cutoff := now.Add(-time.Duration(db.retentionDays) * 24 * time.Hour).Unix()
	return func(s *sql.DB) error {
		if _, err := s.Exec("DELETE FROM metrics_1m WHERE ts_min < ?", cutoff); err != nil {
			return fmt.Errorf("store: metrics_1m 保留清理失败：%w", err)
		}
		if _, err := s.Exec("DELETE FROM probe_results WHERE ts < ?", cutoff); err != nil {
			return fmt.Errorf("store: probe_results 保留清理失败：%w", err)
		}
		return nil
	}
}

// ---------- 迁移 ----------

// schemaV1 为首个版本的建表语句（幂等：IF NOT EXISTS）。
const schemaV1 = `
CREATE TABLE IF NOT EXISTS nodes (
  node_id    TEXT PRIMARY KEY,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  facts_json TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS metrics_1m (
  node_id       TEXT NOT NULL,
  ts_min        INTEGER NOT NULL,
  samples       INTEGER NOT NULL,
  cover_ms      INTEGER NOT NULL,
  cpu_avg       REAL,
  cpu_max       REAL,
  load1_avg     REAL,
  load5_avg     REAL,
  load15_avg    REAL,
  mem_used_avg  REAL,
  swap_used_avg REAL,
  disk_used_avg REAL,
  net_rx_avg    REAL,
  net_tx_avg    REAL,
  tcp_avg       REAL,
  udp_avg       REAL,
  procs_avg     REAL,
  PRIMARY KEY (node_id, ts_min)
);
CREATE TABLE IF NOT EXISTS traffic_state (
  node_id     TEXT PRIMARY KEY,
  boot_id     TEXT NOT NULL,
  iface       TEXT NOT NULL,
  baseline_rx INTEGER NOT NULL,
  baseline_tx INTEGER NOT NULL,
  total_rx    INTEGER NOT NULL,
  total_tx    INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS traffic_daily (
  node_id TEXT NOT NULL,
  day     TEXT NOT NULL,
  rx      INTEGER NOT NULL,
  tx      INTEGER NOT NULL,
  PRIMARY KEY (node_id, day)
);
CREATE TABLE IF NOT EXISTS probe_state (
  node_id TEXT PRIMARY KEY,
  version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS probe_tasks (
  node_id  TEXT NOT NULL,
  task_id  TEXT NOT NULL,
  target   TEXT NOT NULL,
  interval INTEGER NOT NULL,
  version  INTEGER NOT NULL,
  PRIMARY KEY (node_id, task_id)
);
CREATE TABLE IF NOT EXISTS probe_results (
  node_id    TEXT NOT NULL,
  task_id    TEXT NOT NULL,
  ts         INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_probe_results_node_ts ON probe_results (node_id, ts);
`

// migrate 按 PRAGMA user_version 迁移到当前版本。重复执行幂等。
func (db *DB) migrate() error {
	var v int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("store: 读取 user_version 失败：%w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("store: 数据库 schema 版本 %d 高于本程序支持的 %d", v, schemaVersion)
	}
	if v == schemaVersion {
		return nil
	}
	if _, err := db.sql.Exec(schemaV1); err != nil {
		return fmt.Errorf("store: schema v1 迁移失败：%w", err)
	}
	if _, err := db.sql.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("store: 写入 user_version 失败：%w", err)
	}
	return nil
}

// ---------- nodes ----------

// NodeRow 为 nodes 表的读取结果；facts_json 为空或非法时 Facts 为 nil。
type NodeRow struct {
	NodeID    string
	FirstSeen int64
	LastSeen  int64
	Facts     *metrics.Facts
}

// enqueueNodeUpsert 持久化一次接收：last_seen 总是更新；
// facts 非 nil 时一并更新 facts_json；首次插入记录 first_seen。
func (db *DB) enqueueNodeUpsert(nodeID string, facts *metrics.Facts, now time.Time) {
	var factsJSON string
	if facts != nil {
		if b, err := json.Marshal(facts); err == nil {
			factsJSON = string(b)
		}
	}
	ts := now.Unix()
	db.enqueue(func(s *sql.DB) error {
		_, err := s.Exec(`INSERT INTO nodes (node_id, first_seen, last_seen, facts_json)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(node_id) DO UPDATE SET
			  last_seen = excluded.last_seen,
			  facts_json = CASE WHEN excluded.facts_json != '' THEN excluded.facts_json ELSE nodes.facts_json END`,
			nodeID, ts, ts, factsJSON)
		return err
	})
}

// LoadNodes 读取全部节点（重启恢复用；加载后不使节点在线）。
func (db *DB) LoadNodes() ([]NodeRow, error) {
	rows, err := db.sql.Query("SELECT node_id, first_seen, last_seen, facts_json FROM nodes")
	if err != nil {
		return nil, fmt.Errorf("store: nodes 读取失败：%w", err)
	}
	defer rows.Close()
	var out []NodeRow
	for rows.Next() {
		var r NodeRow
		var factsJSON string
		if err := rows.Scan(&r.NodeID, &r.FirstSeen, &r.LastSeen, &factsJSON); err != nil {
			return nil, fmt.Errorf("store: nodes 扫描失败：%w", err)
		}
		if factsJSON != "" {
			var f metrics.Facts
			if err := json.Unmarshal([]byte(factsJSON), &f); err == nil {
				r.Facts = &f
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- metrics_1m ----------

// MetricRow 为一行分钟聚合。字段为 NULL 表示该质量组在本分钟内无有效样本
// （unknown 组不纳入平均）。
type MetricRow struct {
	NodeID  string
	TsMin   int64 // 分钟起点的 Unix 秒
	Samples int64 // 本分钟内接收到的 Metrics 样本总数
	CoverMS int64 // 首末样本接收时间差（毫秒）

	CPUAvg, CPUMax                sql.NullFloat64
	Load1Avg, Load5Avg, Load15Avg sql.NullFloat64
	MemUsedAvg, SwapUsedAvg       sql.NullFloat64
	DiskUsedAvg                   sql.NullFloat64
	NetRXAvg, NetTXAvg            sql.NullFloat64
	TCPAvg, UDPAvg, ProcsAvg      sql.NullFloat64
}

// metricsAvgCols 为按样本数加权合并的均值列；cpu_max 单独取最大。
var metricsAvgCols = []string{
	"cpu_avg", "load1_avg", "load5_avg", "load15_avg",
	"mem_used_avg", "swap_used_avg", "disk_used_avg",
	"net_rx_avg", "net_tx_avg", "tcp_avg", "udp_avg", "procs_avg",
}

// metricsUpsertSQL 为分钟聚合行的写入语句。冲突（重启后同一分钟被重复
// 聚合）时按样本数加权合并均值，cpu_max 取最大；excluded 为 NULL 的列不
// 覆盖已有值。
var metricsUpsertSQL = func() string {
	cols := "node_id, ts_min, samples, cover_ms, cpu_avg, cpu_max, " + strings.Join(metricsAvgCols[1:], ", ")
	var sb strings.Builder
	sb.WriteString("INSERT INTO metrics_1m (" + cols + ") VALUES (?" + strings.Repeat(", ?", len(metricsAvgCols)+4) + ")\n")
	sb.WriteString("ON CONFLICT(node_id, ts_min) DO UPDATE SET\n")
	sb.WriteString("  samples = samples + excluded.samples,\n")
	sb.WriteString("  cover_ms = cover_ms + excluded.cover_ms,\n")
	sb.WriteString("  cpu_max = CASE WHEN excluded.cpu_max IS NULL THEN cpu_max\n")
	sb.WriteString("                 WHEN cpu_max IS NULL THEN excluded.cpu_max\n")
	sb.WriteString("                 ELSE MAX(cpu_max, excluded.cpu_max) END")
	for _, c := range metricsAvgCols {
		sb.WriteString(",\n  " + c + " = CASE\n")
		sb.WriteString("    WHEN excluded." + c + " IS NULL THEN " + c + "\n")
		sb.WriteString("    WHEN " + c + " IS NULL THEN excluded." + c + "\n")
		sb.WriteString("    ELSE (" + c + " * samples + excluded." + c + " * excluded.samples) / (samples + excluded.samples) END")
	}
	return sb.String()
}()

// enqueueMetricsRow 写入一行分钟聚合。
func (db *DB) enqueueMetricsRow(r MetricRow) {
	db.enqueue(func(s *sql.DB) error {
		_, err := s.Exec(metricsUpsertSQL,
			r.NodeID, r.TsMin, r.Samples, r.CoverMS,
			r.CPUAvg, r.CPUMax, r.Load1Avg, r.Load5Avg, r.Load15Avg,
			r.MemUsedAvg, r.SwapUsedAvg, r.DiskUsedAvg,
			r.NetRXAvg, r.NetTXAvg, r.TCPAvg, r.UDPAvg, r.ProcsAvg)
		return err
	})
}

// MetricsRange 读取 [from, to) 分钟区间内的聚合行（历史 API 用）。
func (db *DB) MetricsRange(nodeID string, from, to int64) ([]MetricRow, error) {
	rows, err := db.sql.Query(`SELECT ts_min, samples, cpu_avg, cpu_max, load1_avg, load5_avg, load15_avg,
		mem_used_avg, swap_used_avg, disk_used_avg, net_rx_avg, net_tx_avg, tcp_avg, udp_avg, procs_avg
		FROM metrics_1m WHERE node_id = ? AND ts_min >= ? AND ts_min < ? ORDER BY ts_min`, nodeID, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: metrics_1m 查询失败：%w", err)
	}
	defer rows.Close()
	var out []MetricRow
	for rows.Next() {
		r := MetricRow{NodeID: nodeID}
		if err := rows.Scan(&r.TsMin, &r.Samples, &r.CPUAvg, &r.CPUMax,
			&r.Load1Avg, &r.Load5Avg, &r.Load15Avg,
			&r.MemUsedAvg, &r.SwapUsedAvg, &r.DiskUsedAvg,
			&r.NetRXAvg, &r.NetTXAvg, &r.TCPAvg, &r.UDPAvg, &r.ProcsAvg); err != nil {
			return nil, fmt.Errorf("store: metrics_1m 扫描失败：%w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- traffic ----------

// TrafficStateRow 为 traffic_state 表的一行。
type TrafficStateRow struct {
	NodeID, BootID, Iface string
	BaseRX, BaseTX        uint64
	TotalRX, TotalTX      uint64
	UpdatedAt             int64
}

// TrafficDailyRow 为 traffic_daily 表的一行（day 为 UTC 日期，YYYY-MM-DD）。
type TrafficDailyRow struct {
	NodeID, Day string
	RX, TX      uint64
}

// enqueueTraffic 持久化一次计数器读数（同事务语义见 applyTrafficTx）。
func (db *DB) enqueueTraffic(nodeID, bootID, iface string, rxTotal, txTotal uint64, day string, now time.Time) {
	ts := now.Unix()
	db.enqueue(func(s *sql.DB) error {
		tx, err := s.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := applyTrafficTx(tx, nodeID, bootID, iface, rxTotal, txTotal, day, ts); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// applyTrafficTx 在单个事务内应用一次计数器读数（根 README §6）：
//   - 首见只建基线（累计为 0）；
//   - boot_id/iface 变化或计数器回退时只重建基线，不清累计；
//   - 同范围差分累计：基线、累计与当日统计同事务更新，任一步失败整体回滚；
//   - 调用方保证 quality.net_total 为有效（unknown/缺失不进入本函数）。
func applyTrafficTx(tx *sql.Tx, nodeID, bootID, iface string, rx, txv uint64, day string, now int64) error {
	var stBoot, stIface string
	var baseRX, baseTX int64
	err := tx.QueryRow(`SELECT boot_id, iface, baseline_rx, baseline_tx FROM traffic_state WHERE node_id = ?`,
		nodeID).Scan(&stBoot, &stIface, &baseRX, &baseTX)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO traffic_state
			(node_id, boot_id, iface, baseline_rx, baseline_tx, total_rx, total_tx, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, 0, ?)`,
			nodeID, bootID, iface, int64(rx), int64(txv), now)
		return err
	case err != nil:
		return err
	}
	if stBoot != bootID || stIface != iface || int64(rx) < baseRX || int64(txv) < baseTX {
		_, err := tx.Exec(`UPDATE traffic_state
			SET boot_id = ?, iface = ?, baseline_rx = ?, baseline_tx = ?, updated_at = ?
			WHERE node_id = ?`, bootID, iface, int64(rx), int64(txv), now, nodeID)
		return err
	}
	drx := int64(rx) - baseRX
	dtx := int64(txv) - baseTX
	if _, err := tx.Exec(`UPDATE traffic_state
		SET baseline_rx = ?, baseline_tx = ?, total_rx = total_rx + ?, total_tx = total_tx + ?, updated_at = ?
		WHERE node_id = ?`, int64(rx), int64(txv), drx, dtx, now, nodeID); err != nil {
		return err
	}
	if drx > 0 || dtx > 0 {
		_, err := tx.Exec(`INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)
			ON CONFLICT(node_id, day) DO UPDATE SET rx = rx + excluded.rx, tx = tx + excluded.tx`,
			nodeID, day, drx, dtx)
		return err
	}
	return nil
}

// LoadTraffic 读取全部流量状态与日统计（重启恢复用）。
func (db *DB) LoadTraffic() (states []TrafficStateRow, daily []TrafficDailyRow, err error) {
	srows, err := db.sql.Query("SELECT node_id, boot_id, iface, baseline_rx, baseline_tx, total_rx, total_tx, updated_at FROM traffic_state")
	if err != nil {
		return nil, nil, fmt.Errorf("store: traffic_state 读取失败：%w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var r TrafficStateRow
		var baseRX, baseTX, totalRX, totalTX int64
		if err := srows.Scan(&r.NodeID, &r.BootID, &r.Iface, &baseRX, &baseTX, &totalRX, &totalTX, &r.UpdatedAt); err != nil {
			return nil, nil, fmt.Errorf("store: traffic_state 扫描失败：%w", err)
		}
		r.BaseRX, r.BaseTX, r.TotalRX, r.TotalTX = uint64(baseRX), uint64(baseTX), uint64(totalRX), uint64(totalTX)
		states = append(states, r)
	}
	if err := srows.Err(); err != nil {
		return nil, nil, err
	}
	drows, err := db.sql.Query("SELECT node_id, day, rx, tx FROM traffic_daily")
	if err != nil {
		return nil, nil, fmt.Errorf("store: traffic_daily 读取失败：%w", err)
	}
	defer drows.Close()
	for drows.Next() {
		var r TrafficDailyRow
		var rx, tx int64
		if err := drows.Scan(&r.NodeID, &r.Day, &rx, &tx); err != nil {
			return nil, nil, fmt.Errorf("store: traffic_daily 扫描失败：%w", err)
		}
		r.RX, r.TX = uint64(rx), uint64(tx)
		daily = append(daily, r)
	}
	return states, daily, drows.Err()
}

// TrafficDailyRange 读取 nodeID 自 fromDay（含，UTC YYYY-MM-DD）以来的日统计。
func (db *DB) TrafficDailyRange(nodeID, fromDay string) ([]TrafficDailyRow, error) {
	rows, err := db.sql.Query(`SELECT node_id, day, rx, tx FROM traffic_daily
		WHERE node_id = ? AND day >= ? ORDER BY day`, nodeID, fromDay)
	if err != nil {
		return nil, fmt.Errorf("store: traffic_daily 查询失败：%w", err)
	}
	defer rows.Close()
	var out []TrafficDailyRow
	for rows.Next() {
		var r TrafficDailyRow
		var rx, tx int64
		if err := rows.Scan(&r.NodeID, &r.Day, &rx, &tx); err != nil {
			return nil, fmt.Errorf("store: traffic_daily 扫描失败：%w", err)
		}
		r.RX, r.TX = uint64(rx), uint64(tx)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- probe ----------

// enqueueProbeTasksReplace 整体替换一个节点的探测任务并推进版本（同事务）。
func (db *DB) enqueueProbeTasksReplace(nodeID string, version uint64, tasks []protocol.PingTask) {
	db.enqueue(func(s *sql.DB) error {
		tx, err := s.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec("DELETE FROM probe_tasks WHERE node_id = ?", nodeID); err != nil {
			return err
		}
		for _, t := range tasks {
			if _, err := tx.Exec(`INSERT INTO probe_tasks (node_id, task_id, target, interval, version)
				VALUES (?, ?, ?, ?, ?)`, nodeID, t.ID, t.Target, t.Interval, int64(version)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO probe_state (node_id, version) VALUES (?, ?)
			ON CONFLICT(node_id) DO UPDATE SET version = excluded.version`, nodeID, int64(version)); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// enqueueProbeResults 写入一批探测结果（latency_ms = -1 为失败样本；
// DNS 超时缺样不上报，本函数不会出现）。
func (db *DB) enqueueProbeResults(nodeID string, results []protocol.PingResult, recv time.Time) {
	ts := recv.Unix()
	db.enqueue(func(s *sql.DB) error {
		tx, err := s.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, r := range results {
			if _, err := tx.Exec(`INSERT INTO probe_results (node_id, task_id, ts, latency_ms)
				VALUES (?, ?, ?, ?)`, nodeID, r.TaskID, ts, r.LatencyMS); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// LoadProbeTasks 读取全部节点的探测任务版本与任务列表（重启恢复用）。
// 任务按 task_id 排序返回，保证输出稳定。
func (db *DB) LoadProbeTasks() (versions map[string]uint64, tasks map[string][]protocol.PingTask, err error) {
	versions = make(map[string]uint64)
	tasks = make(map[string][]protocol.PingTask)
	vrows, err := db.sql.Query("SELECT node_id, version FROM probe_state")
	if err != nil {
		return nil, nil, fmt.Errorf("store: probe_state 读取失败：%w", err)
	}
	defer vrows.Close()
	for vrows.Next() {
		var id string
		var v int64
		if err := vrows.Scan(&id, &v); err != nil {
			return nil, nil, fmt.Errorf("store: probe_state 扫描失败：%w", err)
		}
		versions[id] = uint64(v)
	}
	if err := vrows.Err(); err != nil {
		return nil, nil, err
	}
	trows, err := db.sql.Query("SELECT node_id, task_id, target, interval FROM probe_tasks ORDER BY node_id, task_id")
	if err != nil {
		return nil, nil, fmt.Errorf("store: probe_tasks 读取失败：%w", err)
	}
	defer trows.Close()
	for trows.Next() {
		var id string
		var t protocol.PingTask
		if err := trows.Scan(&id, &t.ID, &t.Target, &t.Interval); err != nil {
			return nil, nil, fmt.Errorf("store: probe_tasks 扫描失败：%w", err)
		}
		tasks[id] = append(tasks[id], t)
	}
	return versions, tasks, trows.Err()
}
