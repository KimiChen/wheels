// SPDX-License-Identifier: Apache-2.0

package protocol_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

const testSession = "sess-20260926-0001"

func sampleFacts() *metrics.Facts {
	return &metrics.Facts{
		Hostname: "node-example-01",
		OS:       "linux",
		Kernel:   "6.8.0-31-generic",
		Arch:     "amd64",
		Virt:     "kvm",
		CPUName:  "Virtual CPU",
		CPUCores: 4, AgentVersion: "0.1.0-dev",
		IPv4:      "192.0.2.10",
		IPv6:      "2001:db8::10",
		MemTotal:  4294967296,
		SwapTotal: 1073741824,
		DiskTotal: 107374182400,
	}
}

func sampleMetrics() *metrics.Metrics {
	return &metrics.Metrics{
		CollectedAt: 1790380800,
		CPU:         12.5,
		Load:        [3]float64{0.42, 0.38, 0.35},
		MemUsed:     1610612736,
		SwapUsed:    0,
		DiskUsed:    26843545600,
		MemTotal:    4294967296,
		SwapTotal:   1073741824,
		DiskTotal:   107374182400,
		NetRX:       1024.5,
		NetTX:       2048.25,
		NetRXTotal:  12345678901,
		NetTXTotal:  9876543210,
		BootID:      "9b2f3c1e-7a4d-4e5f-9c2b-1a2b3c4d5e6f",
		Iface:       "eth0",
		Uptime:      123456,
		TCP:         59,
		UDP:         14,
		Procs:       187,
		Quality:     &metrics.Quality{NetRate: metrics.QualityUnknown},
	}
}

func sampleExtensions() *protocol.Extensions {
	return &protocol.Extensions{FRP: &protocol.FRPExtension{
		ClientID:         "node-example-01",
		FRPVersion:       "0.71.0",
		ControlConnected: true,
		Proxies: []protocol.ProxyInfo{{
			Name:      "ssh",
			Type:      "tcp",
			LocalAddr: "127.0.0.1:22",
			Enabled:   true,
			Status:    "running",
		}},
	}}
}

func assertGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	path := filepath.Join("testdata", name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 golden 失败：%v", err)
	}
	if !bytes.Equal(append(got, '\n'), want) {
		t.Errorf("golden 不一致 %s\n实际：\n%s", name, got)
	}
}

func TestHelloGolden(t *testing.T) {
	params := &protocol.HelloParams{
		SchemaVersion:  protocol.SchemaVersion,
		AgentVersion:   "0.1.0-dev",
		FRPVersion:     "0.71.0",
		Capabilities:   []string{"metrics", "ping"},
		SessionID:      testSession,
		ReportInterval: 1,
		SentAt:         1790380800,
	}
	if err := params.Validate(); err != nil {
		t.Fatalf("样本应通过校验：%v", err)
	}
	id := uint64(1)
	req, err := protocol.NewRequest(&id, protocol.MethodHello, params)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	assertGolden(t, "hello.request.json", req)
}

func TestReportGolden(t *testing.T) {
	params := &protocol.ReportParams{
		SessionID:  testSession,
		Sequence:   1,
		SentAt:     1790380801,
		Facts:      sampleFacts(),
		Metrics:    sampleMetrics(),
		Extensions: sampleExtensions(),
	}
	if err := params.Validate(); err != nil {
		t.Fatalf("样本应通过校验：%v", err)
	}
	id := uint64(2)
	req, err := protocol.NewRequest(&id, protocol.MethodReport, params)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	assertGolden(t, "report.request.json", req)
}

func TestPingTasksGolden(t *testing.T) {
	params := &protocol.PingTasksParams{
		Version: 7,
		Tasks: []protocol.PingTask{
			{ID: "t-home", Target: "192.0.2.1:443", Interval: 30},
			{ID: "t-dns", Target: "dns.example.com:53", Interval: 60},
		},
	}
	if err := params.Validate(); err != nil {
		t.Fatalf("样本应通过校验：%v", err)
	}
	id := uint64(10)
	req, err := protocol.NewRequest(&id, protocol.MethodPingTasks, params)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	assertGolden(t, "ping_tasks.request.json", req)
}

func TestPingResultGolden(t *testing.T) {
	params := &protocol.PingResultParams{
		SessionID: testSession,
		Sequence:  2,
		SentAt:    1790380830,
		Results: []protocol.PingResult{
			{TaskID: "t-home", LatencyMS: 23},
			{TaskID: "t-dns", LatencyMS: protocol.LatencyFailed},
		},
	}
	if err := params.Validate(); err != nil {
		t.Fatalf("样本应通过校验：%v", err)
	}
	id := uint64(11)
	req, err := protocol.NewRequest(&id, protocol.MethodPingResult, params)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	assertGolden(t, "ping_result.request.json", req)
}

// golden 必须能按同一契约解析回来并通过校验。
func TestReportGoldenRoundtrip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "report.request.json"))
	if err != nil {
		t.Fatalf("读取 golden 失败：%v", err)
	}
	var req protocol.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("信封解析失败：%v", err)
	}
	if req.JSONRPC != protocol.JSONRPCVersion || req.Method != protocol.MethodReport {
		t.Fatalf("信封字段不符：%+v", req)
	}
	var params protocol.ReportParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("params 解析失败：%v", err)
	}
	if err := params.Validate(); err != nil {
		t.Fatalf("golden 校验失败：%v", err)
	}
}

func TestNotificationOmitsID(t *testing.T) {
	req, err := protocol.NewRequest(nil, protocol.MethodPingResult, &protocol.PingResultParams{
		SessionID: testSession,
		Sequence:  3,
		SentAt:    1790380831,
		Results:   []protocol.PingResult{{TaskID: "t-home", LatencyMS: 20}},
	})
	if err != nil {
		t.Fatalf("构造通知失败：%v", err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if bytes.Contains(raw, []byte(`"id"`)) {
		t.Errorf("通知不应携带 id：%s", raw)
	}
}

func TestValidateReject(t *testing.T) {
	t.Run("hello schema 越界", func(t *testing.T) {
		p := &protocol.HelloParams{SchemaVersion: 0, AgentVersion: "a", FRPVersion: "f", SessionID: "s", SentAt: 1}
		if err := p.Validate(); err == nil {
			t.Error("应被拒绝")
		}
	})
	t.Run("report 空载荷", func(t *testing.T) {
		p := &protocol.ReportParams{SessionID: "s", Sequence: 1, SentAt: 1}
		if err := p.Validate(); err == nil {
			t.Error("应被拒绝")
		}
	})
	t.Run("report 序号越界", func(t *testing.T) {
		p := &protocol.ReportParams{SessionID: "s", Sequence: 0, SentAt: 1, Metrics: sampleMetrics()}
		if err := p.Validate(); err == nil {
			t.Error("应被拒绝")
		}
	})
	t.Run("探测任务超上限", func(t *testing.T) {
		p := &protocol.PingTasksParams{Version: 1}
		for i := 0; i < protocol.MaxPingTasks+1; i++ {
			p.Tasks = append(p.Tasks, protocol.PingTask{
				ID:       "t" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
				Target:   "192.0.2.1:443",
				Interval: 30,
			})
		}
		if err := p.Validate(); err == nil {
			t.Error("应被拒绝")
		}
	})
	t.Run("探测间隔越界", func(t *testing.T) {
		p := &protocol.PingTasksParams{Version: 1, Tasks: []protocol.PingTask{
			{ID: "t", Target: "192.0.2.1:443", Interval: protocol.MinPingInterval - 1},
		}}
		if err := p.Validate(); err == nil {
			t.Error("应被拒绝")
		}
	})
	t.Run("探测任务 ID 重复", func(t *testing.T) {
		p := &protocol.PingTasksParams{Version: 1, Tasks: []protocol.PingTask{
			{ID: "t", Target: "192.0.2.1:443", Interval: 30},
			{ID: "t", Target: "192.0.2.2:443", Interval: 30},
		}}
		if err := p.Validate(); err == nil {
			t.Error("应被拒绝")
		}
	})
	t.Run("探测结果延迟越界", func(t *testing.T) {
		p := &protocol.PingResultParams{SessionID: "s", Sequence: 1, SentAt: 1, Results: []protocol.PingResult{
			{TaskID: "t", LatencyMS: protocol.LatencyFailed - 1},
		}}
		if err := p.Validate(); err == nil {
			t.Error("应被拒绝")
		}
	})
}
