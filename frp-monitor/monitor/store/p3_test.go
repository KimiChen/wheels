// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"database/sql"

	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// TestReconcileCompositeKey 验证 user+clientID 组合键：同 clientID 不同
// user 不串节点。
func TestReconcileCompositeKey(t *testing.T) {
	mkNode := func(id, user, clientID string) NodeState {
		return NodeState{NodeID: id, FRP: &protocol.FRPExtension{User: user, ClientID: clientID}}
	}
	all := []NodeState{mkNode("n1", "u1", "c1"), mkNode("n2", "u2", "c1")}
	clients := []FRPClient{
		{User: "u1", ClientID: "c1", RunID: "r1", Online: true},
		{User: "u2", ClientID: "c1", RunID: "r2", Online: false},
	}

	matched, bound := Reconcile(all[0], all, clients)
	if !bound || len(matched) != 1 || matched[0].RunID != "r1" {
		t.Fatalf("n1 应只匹配 u1/c1：bound=%v matched=%+v", bound, matched)
	}
	matched, bound = Reconcile(all[1], all, clients)
	if !bound || len(matched) != 1 || matched[0].RunID != "r2" {
		t.Fatalf("n2 应只匹配 u2/c1：bound=%v matched=%+v", bound, matched)
	}

	// 空 user 也是合法绑定键：节点 user 为空时不匹配 user 非空的注册条目
	nu := NodeState{NodeID: "n3", FRP: &protocol.FRPExtension{ClientID: "c1"}}
	if matched, bound := Reconcile(nu, all, clients); bound || len(matched) != 0 {
		t.Fatalf("空 user 不应匹配 u1/u2：bound=%v matched=%+v", bound, matched)
	}
}

// TestReconcileTunnels 验证隧道对账：绑定键匹配、本地目标按 name 填充、
// 未匹配节点 NodeID 为空、冲突不归因。
func TestReconcileTunnels(t *testing.T) {
	all := []NodeState{
		{NodeID: "n1", FRP: &protocol.FRPExtension{User: "u1", ClientID: "c1",
			Proxies: []protocol.ProxyInfo{{Name: "ssh", Type: "tcp", LocalAddr: "127.0.0.1:22"}}}},
		{NodeID: "n2", FRP: &protocol.FRPExtension{User: "u2", ClientID: "c1"}}, // 同 clientID 不同 user
		{NodeID: "n3"}, // 无 FRP 扩展
	}
	proxies := []FRPProxy{
		{Name: "ssh", Type: "tcp", User: "u1", ClientID: "c1", Online: true, CurConns: 3, TodayTrafficIn: 123, TodayTrafficOut: 456},
		{Name: "web", Type: "http", User: "u2", ClientID: "c1"},
		{Name: "orphan", Type: "tcp", User: "u9", ClientID: "c9"},
	}
	tunnels := ReconcileTunnels(all, proxies)
	if len(tunnels) != 3 {
		t.Fatalf("应有 3 条隧道：%+v", tunnels)
	}
	byName := make(map[string]Tunnel, 3)
	for _, tn := range tunnels {
		byName[tn.Proxy.Name] = tn
	}
	ssh := byName["ssh"]
	if ssh.NodeID != "n1" {
		t.Fatalf("ssh 应对账到 n1：%+v", ssh)
	}
	if ssh.LocalAddr == nil || *ssh.LocalAddr != "127.0.0.1:22" {
		t.Fatalf("ssh 应填充本地目标：%+v", ssh.LocalAddr)
	}
	web := byName["web"]
	if web.NodeID != "n2" || web.LocalAddr != nil {
		t.Fatalf("web 应对账到 n2 且无本地目标：%+v", web)
	}
	if orph := byName["orphan"]; orph.NodeID != "" {
		t.Fatalf("orphan 不应匹配节点：%+v", orph)
	}

	// 冲突：两个节点声明同一绑定键 → 隧道不归因
	conflict := []NodeState{
		{NodeID: "n1", FRP: &protocol.FRPExtension{User: "u1", ClientID: "c1"}},
		{NodeID: "n2", FRP: &protocol.FRPExtension{User: "u1", ClientID: "c1"}},
	}
	tunnels = ReconcileTunnels(conflict, proxies)
	for _, tn := range tunnels {
		if tn.Proxy.Name == "ssh" && tn.NodeID != "" {
			t.Fatalf("冲突时隧道不应归因：%+v", tn)
		}
	}

	// 输出稳定：排序按 (NodeID, Name)，未匹配（空 NodeID）在前
	tunnels = ReconcileTunnels(all, proxies)
	if tunnels[0].NodeID != "" || tunnels[1].NodeID != "n1" || tunnels[2].NodeID != "n2" {
		t.Fatalf("排序不符：%+v", tunnels)
	}
}

// TestProxyFlipEvents 验证隧道事件：只在 Online 翻转时记一条（去重）、
// 归因节点、新的在前、limit 生效。
func TestProxyFlipEvents(t *testing.T) {
	s := New()
	t0 := time.Unix(1790380800, 0)
	s.StartSession("n1", "s1", 1, t0)
	if err := s.Report("n1", "s1", 1, nil, nil,
		&protocol.FRPExtension{User: "u1", ClientID: "c1"}, t0); err != nil {
		t.Fatal(err)
	}

	mk := func(online bool) []FRPProxy {
		return []FRPProxy{{Name: "ssh", Type: "tcp", User: "u1", ClientID: "c1", Online: online}}
	}

	// 基线：首次出现不记事件
	s.UpdateFRPProxies(mk(true), t0)
	if got := s.FRPEvents("n1", 50); len(got) != 0 {
		t.Fatalf("基线不应记事件：%+v", got)
	}
	// 同状态重复喂入不记事件
	s.UpdateFRPProxies(mk(true), t0.Add(time.Second))
	if got := s.FRPEvents("n1", 50); len(got) != 0 {
		t.Fatalf("同状态不应记事件：%+v", got)
	}

	// 翻转 offline → 一条 tunnel_offline
	s.UpdateFRPProxies(mk(false), t0.Add(2*time.Second))
	evs := s.FRPEvents("n1", 50)
	if len(evs) != 1 || evs[0].Kind != EventTunnelOffline || evs[0].Name != "ssh" ||
		evs[0].TS != t0.Add(2*time.Second).Unix() || evs[0].NodeID != "n1" {
		t.Fatalf("翻转事件不符：%+v", evs)
	}
	// 再次 offline → 无新增
	s.UpdateFRPProxies(mk(false), t0.Add(3*time.Second))
	if got := s.FRPEvents("n1", 50); len(got) != 1 {
		t.Fatalf("重复 offline 不应记事件：%+v", got)
	}
	// 翻转 online → 新事件在前
	s.UpdateFRPProxies(mk(true), t0.Add(4*time.Second))
	evs = s.FRPEvents("n1", 50)
	if len(evs) != 2 || evs[0].Kind != EventTunnelOnline || evs[1].Kind != EventTunnelOffline {
		t.Fatalf("倒序不符：%+v", evs)
	}
	// limit 生效
	if got := s.FRPEvents("n1", 1); len(got) != 1 || got[0].Kind != EventTunnelOnline {
		t.Fatalf("limit=1 应只返回最新一条：%+v", got)
	}

	// 未匹配节点的隧道事件不归因
	s.UpdateFRPProxies([]FRPProxy{{Name: "x", Type: "tcp", User: "u9", ClientID: "c9", Online: true}}, t0.Add(5*time.Second))
	s.UpdateFRPProxies([]FRPProxy{{Name: "x", Type: "tcp", User: "u9", ClientID: "c9", Online: false}}, t0.Add(6*time.Second))
	if got := s.FRPEvents("n1", 50); len(got) != 2 {
		t.Fatalf("未匹配隧道事件不应归到 n1：%+v", got)
	}
	if got := s.FRPEvents("", 50); len(got) != 1 || got[0].Name != "x" {
		t.Fatalf("未匹配隧道事件应归到空节点：%+v", got)
	}
}

// TestClientFlipEvents 验证注册表客户端事件：Online 翻转记
// client_online/client_offline，消失再出现不记事件。
func TestClientFlipEvents(t *testing.T) {
	s := New()
	t0 := time.Unix(1790380800, 0)
	s.StartSession("n1", "s1", 1, t0)
	if err := s.Report("n1", "s1", 1, nil, nil,
		&protocol.FRPExtension{User: "u1", ClientID: "c1"}, t0); err != nil {
		t.Fatal(err)
	}

	online := []FRPClient{{User: "u1", ClientID: "c1", RunID: "r1", Online: true}}
	s.UpdateFRPClients(online, t0) // 基线
	s.UpdateFRPClients(online, t0.Add(time.Second))
	if got := s.FRPEvents("n1", 50); len(got) != 0 {
		t.Fatalf("基线/重复不应记事件：%+v", got)
	}

	s.UpdateFRPClients([]FRPClient{{User: "u1", ClientID: "c1", RunID: "r1", Online: false}}, t0.Add(2*time.Second))
	evs := s.FRPEvents("n1", 50)
	if len(evs) != 1 || evs[0].Kind != EventClientOffline || evs[0].Name != "c1" || evs[0].NodeID != "n1" {
		t.Fatalf("client_offline 不符：%+v", evs)
	}

	// 重连（新 runID，同绑定键）→ client_online
	s.UpdateFRPClients([]FRPClient{{User: "u1", ClientID: "c1", RunID: "r2", Online: true}}, t0.Add(3*time.Second))
	evs = s.FRPEvents("n1", 50)
	if len(evs) != 2 || evs[0].Kind != EventClientOnline || evs[1].Kind != EventClientOffline {
		t.Fatalf("client_online 不符：%+v", evs)
	}

	// 临时条目（无 clientID）消失不记事件
	s.UpdateFRPClients([]FRPClient{{User: "u2", RunID: "tmp-1", Online: true}}, t0.Add(4*time.Second))
	s.UpdateFRPClients(nil, t0.Add(5*time.Second))
	if got := s.FRPEvents("n1", 50); len(got) != 2 {
		t.Fatalf("临时连接消失不应记事件：%+v", got)
	}
}

// TestEventRingCapacity 验证环形缓冲容量：超出 1000 条后最旧被覆盖。
func TestEventRingCapacity(t *testing.T) {
	s := New()
	t0 := time.Unix(1790380800, 0)
	for i := 0; i < eventRingCapacity+5; i++ {
		online := i%2 == 0
		s.UpdateFRPProxies([]FRPProxy{{Name: "ssh", Type: "tcp", User: "u1", ClientID: "c1", Online: online}},
			t0.Add(time.Duration(i)*time.Second))
	}
	got := s.FRPEvents("", eventRingCapacity+5)
	if len(got) != eventRingCapacity {
		t.Fatalf("环形缓冲应保留 %d 条，得到 %d", eventRingCapacity, len(got))
	}
	// 最新一条对应最后一次翻转（i=1004，online=true → tunnel_online）
	if got[0].Kind != EventTunnelOnline || got[0].TS != t0.Add(1004*time.Second).Unix() {
		t.Fatalf("最新事件不符：%+v", got[0])
	}
	// 最旧一条对应 i=5（online=false → tunnel_offline；i=0..4 已被覆盖）
	last := got[len(got)-1]
	if last.Kind != EventTunnelOffline || last.TS != t0.Add(5*time.Second).Unix() {
		t.Fatalf("最旧事件应为第 5 次翻转：%+v", last)
	}
}

// TestMigrateV1ToV2 验证旧 v1 库打开后迁移到 v2（frp_snapshots 可用）。
func TestMigrateV1ToV2(t *testing.T) {
	dir := t.TempDir()
	// 手工构造 v1 库
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO nodes (node_id, first_seen, last_seen, facts_json)
		VALUES ('n1', 1, 2, '')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t, dir)
	var v int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("迁移后 user_version 应为 %d，得到 %d", schemaVersion, v)
	}
	// 旧数据保留
	rows, err := db.LoadNodes()
	if err != nil || len(rows) != 1 || rows[0].NodeID != "n1" {
		t.Fatalf("v1 数据应保留：%v %+v", err, rows)
	}
	// 新表可用
	db.enqueueFRPEvent(FRPEvent{TS: 100, NodeID: "n1", Kind: EventTunnelOnline, Name: "ssh", Detail: "d"})
	db.Drain()
	evs, err := db.LoadFRPEvents("n1", 10)
	if err != nil || len(evs) != 1 || evs[0].Kind != EventTunnelOnline {
		t.Fatalf("frp_snapshots 应可用：%v %+v", err, evs)
	}
}

// TestFRPEventsPersistence 验证事件写库与按节点倒序读取、limit 生效。
func TestFRPEventsPersistence(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	db.enqueueFRPEvent(FRPEvent{TS: 100, NodeID: "n1", Kind: EventTunnelOnline, Name: "ssh", Detail: "up"})
	db.enqueueFRPEvent(FRPEvent{TS: 150, NodeID: "n2", Kind: EventClientOnline, Name: "c2", Detail: "other"})
	db.enqueueFRPEvent(FRPEvent{TS: 200, NodeID: "n1", Kind: EventTunnelOffline, Name: "ssh", Detail: "down"})
	db.Drain()

	evs, err := db.LoadFRPEvents("n1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].TS != 200 || evs[1].TS != 100 {
		t.Fatalf("应倒序返回 n1 的两条事件：%+v", evs)
	}
	if evs[0].Detail != "down" || evs[1].Detail != "up" {
		t.Fatalf("detail 往返不符：%+v", evs)
	}
	if evs, err = db.LoadFRPEvents("n1", 1); err != nil || len(evs) != 1 || evs[0].TS != 200 {
		t.Fatalf("limit=1 不符：%v %+v", err, evs)
	}
	if evs, err = db.LoadFRPEvents("ghost", 10); err != nil || len(evs) != 0 {
		t.Fatalf("未知节点应为空：%v %+v", err, evs)
	}

	// store 层：有库优先读库
	s := New()
	s.AttachDB(db)
	t.Cleanup(s.Close)
	s.StartSession("n1", "s1", 1, time.Unix(300, 0))
	if err := s.Report("n1", "s1", 1, nil, nil,
		&protocol.FRPExtension{User: "u1", ClientID: "c1"}, time.Unix(300, 0)); err != nil {
		t.Fatal(err)
	}
	s.UpdateFRPProxies([]FRPProxy{{Name: "web", Type: "http", User: "u1", ClientID: "c1", Online: true}}, time.Unix(300, 0))
	s.UpdateFRPProxies([]FRPProxy{{Name: "web", Type: "http", User: "u1", ClientID: "c1", Online: false}}, time.Unix(301, 0))
	s.DrainWrites()
	got := s.FRPEvents("n1", 10)
	// 库里共有 n1 的 3 条：手动两条 + 新翻转一条；最新为 tunnel_offline(web)
	if len(got) != 3 || got[0].Kind != EventTunnelOffline || got[0].Name != "web" {
		t.Fatalf("有库时应读库并含新事件：%+v", got)
	}
}

// TestBackupDB 验证 VACUUM INTO 备份：产物为合法 SQLite 文件且含数据。
func TestBackupDB(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	db.enqueueNodeUpsert("n1", nil, time.Unix(1790380800, 0))
	db.Drain()

	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(dest); err != nil {
		t.Fatalf("备份失败：%v", err)
	}
	head, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) < 16 || string(head[:15]) != "SQLite format 3" {
		t.Fatalf("备份文件应为 SQLite：%q", head[:16])
	}

	// 备份内容可读
	raw, err := sql.Open("sqlite", "file:"+dest+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var lastSeen int64
	if err := raw.QueryRow("SELECT last_seen FROM nodes WHERE node_id = 'n1'").Scan(&lastSeen); err != nil {
		t.Fatalf("备份库应含节点行：%v", err)
	}
	if lastSeen != 1790380800 {
		t.Fatalf("备份库数据不符：%d", lastSeen)
	}

	// store 层：无库返回 ErrNoPersistence
	if err := New().BackupDB(dest); err != ErrNoPersistence {
		t.Fatalf("无库应返回 ErrNoPersistence：%v", err)
	}
}
