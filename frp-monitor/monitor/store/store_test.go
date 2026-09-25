// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

func testMetrics() *metrics.Metrics {
	return &metrics.Metrics{CollectedAt: 1790380800, CPU: 1}
}

func TestStartSessionReplacesOld(t *testing.T) {
	s := New()
	now := time.Unix(1790380800, 0)

	if old := s.StartSession("n1", "s1", 1, now); old != "" {
		t.Fatalf("首次建会话不应有旧会话，得到 %q", old)
	}
	if err := s.Report("n1", "s1", 1, nil, testMetrics(), nil, now); err != nil {
		t.Fatalf("当前会话 report 应成功：%v", err)
	}

	// 新 hello 取代旧会话
	if old := s.StartSession("n1", "s2", 1, now); old != "s1" {
		t.Fatalf("应返回被取代的旧会话 s1，得到 %q", old)
	}

	// 旧会话 report 被拒绝
	if err := s.Report("n1", "s1", 2, nil, testMetrics(), nil, now); err != ErrStaleSession {
		t.Fatalf("旧会话 report 应返回 ErrStaleSession，得到 %v", err)
	}
	// 新会话序号从 1 重新开始
	if err := s.Report("n1", "s2", 1, nil, testMetrics(), nil, now); err != nil {
		t.Fatalf("新会话 report 应成功：%v", err)
	}

	// 旧连接迟到退出不得把新会话标记离线
	s.EndSession("n1", "s1")
	n, _ := s.Get("n1")
	if !n.Online {
		t.Fatal("旧会话退出不应影响新会话在线状态")
	}
	// 旧会话数据保留（Facts/Metrics 连续性），序号已重置
	if n.Metrics == nil {
		t.Fatal("会话取代后旧 Metrics 应保留")
	}

	s.EndSession("n1", "s2")
	n, _ = s.Get("n1")
	if n.Online {
		t.Fatal("当前会话退出后节点应离线")
	}
}

func TestReportSequenceStrictlyIncreasing(t *testing.T) {
	s := New()
	now := time.Unix(1790380800, 0)
	s.StartSession("n1", "s1", 1, now)

	if err := s.Report("n1", "s1", 1, nil, testMetrics(), nil, now); err != nil {
		t.Fatalf("seq=1 应成功：%v", err)
	}
	if err := s.Report("n1", "s1", 1, nil, testMetrics(), nil, now); err != ErrOutOfOrder {
		t.Fatalf("重复 seq=1 应乱序，得到 %v", err)
	}
	if err := s.Report("n1", "s1", 3, nil, testMetrics(), nil, now); err != nil {
		t.Fatalf("seq=3 跳号仍属递增，应成功：%v", err)
	}
	if err := s.Report("n1", "s1", 2, nil, testMetrics(), nil, now); err != ErrOutOfOrder {
		t.Fatalf("seq=2 回退应乱序，得到 %v", err)
	}
	n, _ := s.Get("n1")
	if n.LastSequence != 3 {
		t.Fatalf("乱序不得推进序号，LastSequence=%d", n.LastSequence)
	}
}

func TestReportUnknownNode(t *testing.T) {
	s := New()
	if err := s.Report("ghost", "s1", 1, nil, testMetrics(), nil, time.Now()); err != ErrStaleSession {
		t.Fatalf("未建会话节点 report 应返回 ErrStaleSession，得到 %v", err)
	}
}

func TestMetricsStaleThreshold(t *testing.T) {
	now := time.Unix(1790380800, 0)

	// 间隔 1s：阈值 = max(10s, 3s) = 10s
	n := NodeState{ReportInterval: 1, MetricsReceivedAt: now.Unix()}
	if n.MetricsStale(now.Add(10 * time.Second)) {
		t.Fatal("恰好 10s 不应过期（大于阈值才过期）")
	}
	if !n.MetricsStale(now.Add(10*time.Second + time.Millisecond)) {
		t.Fatal("超过 10s 应过期")
	}

	// 间隔 10s：阈值 = max(10s, 30s) = 30s
	n = NodeState{ReportInterval: 10, MetricsReceivedAt: now.Unix()}
	if n.MetricsStale(now.Add(20 * time.Second)) {
		t.Fatal("20s < 30s 不应过期")
	}
	if !n.MetricsStale(now.Add(31 * time.Second)) {
		t.Fatal("31s > 30s 应过期")
	}

	// 从未收到指标即过期
	n = NodeState{ReportInterval: 1}
	if !n.MetricsStale(now) {
		t.Fatal("从未收到指标应为过期")
	}
}

func TestReconcileReadOnly(t *testing.T) {
	s := New()
	now := time.Unix(1790380800, 0)
	s.StartSession("n1", "s1", 1, now)
	s.StartSession("n2", "s2", 1, now)
	frp1 := &protocol.FRPExtension{ClientID: "client-a", ControlConnected: true}
	if err := s.Report("n1", "s1", 1, nil, nil, frp1, now); err != nil {
		t.Fatal(err)
	}
	s.UpdateFRPClients([]FRPClient{
		{User: "u1", ClientID: "client-a", RunID: "r1", Online: true},
		{User: "u2", ClientID: "client-b", RunID: "r2", Online: false},
	})

	snap := s.Snapshot()
	clients := s.FRPClients()
	n1, _ := s.Get("n1")
	n2, _ := s.Get("n2")

	// 唯一声明 + 有匹配 → 绑定
	matched, bound := Reconcile(n1, snap, clients)
	if !bound || len(matched) != 1 || matched[0].RunID != "r1" {
		t.Fatalf("n1 应绑定 client-a：bound=%v matched=%+v", bound, matched)
	}
	// 无 FRP 扩展 → 不绑定
	if matched, bound := Reconcile(n2, snap, clients); bound || len(matched) != 0 {
		t.Fatalf("n2 不应绑定：bound=%v matched=%+v", bound, matched)
	}
	// 未上报 client_id 与注册表不匹配 → 有声明无匹配
	n2ext := &protocol.FRPExtension{ClientID: "client-c"}
	if err := s.Report("n2", "s2", 1, nil, nil, n2ext, now); err != nil {
		t.Fatal(err)
	}
	n2, _ = s.Get("n2")
	if matched, bound := Reconcile(n2, s.Snapshot(), clients); bound || len(matched) != 0 {
		t.Fatalf("无匹配注册项不应绑定：bound=%v matched=%+v", bound, matched)
	}

	// 冲突：另一节点声明同一 client_id → 不绑定，但仍返回匹配条目
	if err := s.Report("n2", "s2", 2, nil, nil, frp1, now); err != nil {
		t.Fatal(err)
	}
	snap = s.Snapshot()
	matched, bound = Reconcile(n1, snap, clients)
	if bound || len(matched) != 1 {
		t.Fatalf("冲突时不绑定但保留匹配条目：bound=%v matched=%+v", bound, matched)
	}

	// 只读：对账后 store 内容不变
	n1After, _ := s.Get("n1")
	if n1After.SessionID != "s1" || n1After.FRP.ClientID != "client-a" {
		t.Fatal("对账不得修改节点状态")
	}
}

func TestSubscribeBroadcast(t *testing.T) {
	s := New()
	ch, cancel := s.Subscribe()
	defer cancel()

	now := time.Unix(1790380800, 0)
	s.StartSession("n1", "s1", 1, now)
	select {
	case ev := <-ch:
		if ev.NodeID != "n1" {
			t.Fatalf("事件 NodeID 应为 n1，得到 %q", ev.NodeID)
		}
	case <-time.After(time.Second):
		t.Fatal("StartSession 应广播事件")
	}

	s.UpdateFRPClients(nil)
	select {
	case ev := <-ch:
		if ev.NodeID != "" {
			t.Fatalf("注册表事件 NodeID 应为空，得到 %q", ev.NodeID)
		}
	case <-time.After(time.Second):
		t.Fatal("UpdateFRPClients 应广播事件")
	}
}
