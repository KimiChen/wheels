// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

// SampleFacts 为 golden 测试共享的 Facts 样本；取值全部为虚构。
func SampleFacts() *metrics.Facts {
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

// SampleMetrics 为 golden 测试共享的 Metrics 样本。
// NetRate 标记 unknown 用于固定「首报无差分基线」的线格式。
func SampleMetrics() *metrics.Metrics {
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

// golden 文件与 MarshalIndent 输出逐字节一致（文件末尾多一个换行）。
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

func TestFactsGolden(t *testing.T) {
	f := SampleFacts()
	if err := f.Validate(); err != nil {
		t.Fatalf("样本应通过校验：%v", err)
	}
	assertGolden(t, "facts.json", f)
}

func TestMetricsGolden(t *testing.T) {
	m := SampleMetrics()
	if err := m.Validate(); err != nil {
		t.Fatalf("样本应通过校验：%v", err)
	}
	assertGolden(t, "metrics.json", m)
}

func TestMetricsRoundtrip(t *testing.T) {
	raw, err := json.Marshal(SampleMetrics())
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var m metrics.Metrics
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("回读后校验失败：%v", err)
	}
	if m.NetRXTotal != 12345678901 || m.Quality == nil || m.Quality.NetRate != metrics.QualityUnknown {
		t.Errorf("回读字段不符：%+v", m)
	}
}

func TestFactsValidateReject(t *testing.T) {
	cases := map[string]func(*metrics.Facts){
		"hostname 为空":  func(f *metrics.Facts) { f.Hostname = "" },
		"os 为空":        func(f *metrics.Facts) { f.OS = "" },
		"cpu_cores 越界": func(f *metrics.Facts) { f.CPUCores = 0 },
	}
	for name, mutate := range cases {
		f := SampleFacts()
		mutate(f)
		if err := f.Validate(); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
}

func TestMetricsValidateReject(t *testing.T) {
	cases := map[string]func(*metrics.Metrics){
		"cpu 越界":     func(m *metrics.Metrics) { m.CPU = 100.5 },
		"cpu NaN":    func(m *metrics.Metrics) { m.CPU = math.NaN() },
		"load 负值":    func(m *metrics.Metrics) { m.Load[1] = -0.1 },
		"load Inf":   func(m *metrics.Metrics) { m.Load[0] = math.Inf(1) },
		"net_rx 负值":  func(m *metrics.Metrics) { m.NetRX = -1 },
		"采集时间为零":     func(m *metrics.Metrics) { m.CollectedAt = 0 },
		"非法质量标记":     func(m *metrics.Metrics) { m.Quality.Disk = "half" },
		"boot_id 超长": func(m *metrics.Metrics) { m.BootID = string(make([]byte, 200)) },
	}
	for name, mutate := range cases {
		m := SampleMetrics()
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
}

// 大计数器经 JSON 往返必须保持精确（Go 双端契约；浏览器 DTO 另用十进制字符串）。
func TestCountersPrecision(t *testing.T) {
	m := SampleMetrics()
	m.NetRXTotal = math.MaxUint64
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var back metrics.Metrics
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if back.NetRXTotal != math.MaxUint64 {
		t.Errorf("uint64 计数器精度丢失：%d", back.NetRXTotal)
	}
}
