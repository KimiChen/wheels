// SPDX-License-Identifier: Apache-2.0

package collect_test

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/agent/collect"
	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

var (
	fixtureProc = filepath.Join("..", "..", "tests", "fixtures", "proc")
	fixtureSys  = filepath.Join("..", "..", "tests", "fixtures", "sys")
)

// copyTree 复制 fixture 目录树到临时目录，便于改写计数器文件。
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatalf("复制 fixture 失败：%v", err)
	}
	return dst
}

// TestFactsFromFixtures 用 fixture 驱动 Facts：虚拟化、CPU、容量字段
// 与契约 golden 刻意一致。主机名/OS/内核/架构与地址取本机真实值，
// 只校验非空与格式；DiskTotal 依赖平台 statfs，由内部测试注入覆盖。
func TestFactsFromFixtures(t *testing.T) {
	c := collect.New(fixtureProc, fixtureSys, collect.IfaceFilter{})
	f, err := c.Facts(context.Background())
	if err != nil {
		t.Fatalf("Facts 失败：%v", err)
	}
	if f.Hostname == "" {
		t.Error("hostname 不应为空")
	}
	if f.OS != runtime.GOOS {
		t.Errorf("os = %q，期望 %q", f.OS, runtime.GOOS)
	}
	if f.Kernel == "" || f.Arch == "" {
		t.Errorf("kernel/arch 不应为空：%q/%q", f.Kernel, f.Arch)
	}
	if f.Virt != "kvm" {
		t.Errorf("virt = %q，期望 %q（fixture product_name 为 KVM）", f.Virt, "kvm")
	}
	if f.CPUName != "Virtual CPU" {
		t.Errorf("cpu_name = %q，期望 %q", f.CPUName, "Virtual CPU")
	}
	if f.CPUCores != 4 {
		t.Errorf("cpu_cores = %d，期望 4", f.CPUCores)
	}
	if f.AgentVersion != "" {
		t.Errorf("agent_version 应由调用方填写，采集器留空：%q", f.AgentVersion)
	}
	if f.MemTotal != 4294967296 {
		t.Errorf("mem_total = %d，期望 4294967296", f.MemTotal)
	}
	if f.SwapTotal != 1073741824 {
		t.Errorf("swap_total = %d，期望 1073741824", f.SwapTotal)
	}
	for _, s := range []string{f.IPv4, f.IPv6} {
		if s != "" {
			if _, err := netip.ParseAddr(s); err != nil {
				t.Errorf("地址 %q 非法：%v", s, err)
			}
		}
	}
	if err := f.Validate(); err != nil {
		t.Errorf("Facts 校验失败：%v", err)
	}
}

// TestMetricsFromFixturesFirstSample 用 fixture 驱动首次 Metrics：
// 与契约 golden 一致的字段逐一对账；首样本无差分基线，
// cpu 与 net_rate 必须为 unknown 且数值为 0。
func TestMetricsFromFixturesFirstSample(t *testing.T) {
	c := collect.New(fixtureProc, fixtureSys, collect.IfaceFilter{})
	m := c.Metrics(context.Background())
	if err := m.Validate(); err != nil {
		t.Fatalf("Metrics 校验失败：%v", err)
	}

	if m.MemUsed != 1610612736 {
		t.Errorf("mem_used = %d，期望 1610612736", m.MemUsed)
	}
	if m.SwapUsed != 0 {
		t.Errorf("swap_used = %d，期望 0", m.SwapUsed)
	}
	if m.MemTotal != 4294967296 || m.SwapTotal != 1073741824 {
		t.Errorf("mem_total/swap_total = %d/%d", m.MemTotal, m.SwapTotal)
	}
	if m.Load != [3]float64{0.42, 0.38, 0.35} {
		t.Errorf("load = %v，期望 [0.42 0.38 0.35]", m.Load)
	}
	if m.Procs != 187 {
		t.Errorf("procs = %d，期望 187", m.Procs)
	}
	if m.Uptime != 123456 {
		t.Errorf("uptime = %d，期望 123456", m.Uptime)
	}
	if m.TCP != 59 || m.UDP != 14 {
		t.Errorf("tcp/udp = %d/%d，期望 59/14", m.TCP, m.UDP)
	}
	if m.BootID != "9b2f3c1e-7a4d-4e5f-9c2b-1a2b3c4d5e6f" {
		t.Errorf("boot_id = %q", m.BootID)
	}
	if m.Iface != "eth0" {
		t.Errorf("iface = %q，期望 %q（其余接口应被默认过滤）", m.Iface, "eth0")
	}
	if m.NetRXTotal != 12345678901 || m.NetTXTotal != 9876543210 {
		t.Errorf("net_rx_total/net_tx_total = %d/%d", m.NetRXTotal, m.NetTXTotal)
	}
	if m.CPU != 0 || m.NetRX != 0 || m.NetTX != 0 {
		t.Errorf("首样本 cpu/net_rx/net_tx 应为 0：%v/%v/%v", m.CPU, m.NetRX, m.NetTX)
	}

	if m.Quality == nil {
		t.Fatal("首样本应登记质量降级（cpu、net_rate）")
	}
	if m.Quality.CPU != metrics.QualityUnknown {
		t.Errorf("quality.cpu = %q，期望 unknown", m.Quality.CPU)
	}
	if m.Quality.NetRate != metrics.QualityUnknown {
		t.Errorf("quality.net_rate = %q，期望 unknown", m.Quality.NetRate)
	}
	// 其余组（平台相关的 disk 除外，由内部测试覆盖）应为有效。
	for name, v := range map[string]string{
		"mem": m.Quality.Mem, "swap": m.Quality.Swap,
		"net_total": m.Quality.NetTotal, "sys": m.Quality.Sys,
	} {
		if v != "" {
			t.Errorf("quality.%s = %q，期望有效（空串）", name, v)
		}
	}
}

// TestMetricsMeminfoVariants 覆盖 meminfo 变体：MemAvailable 为 0 是
// 有效数据（used = total，质量 ok）；缺失 MemAvailable 时回退
// MemFree + Buffers + Cached。
func TestMetricsMeminfoVariants(t *testing.T) {
	cases := []struct {
		name     string
		variant  string
		wantUsed uint64
	}{
		{"MemAvailable 为 0 是有效数据", "meminfo.available-zero", 4294967296},
		// (262144 + 131072 + 2097152) kB × 1024 = 2550136832 可用，
		// used = 4294967296 − 2550136832。
		{"缺失 MemAvailable 回退", "meminfo.legacy", 1744830464},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proc := copyTree(t, fixtureProc)
			raw, err := os.ReadFile(filepath.Join(fixtureProc, tc.variant))
			if err != nil {
				t.Fatalf("读取变体失败：%v", err)
			}
			if err := os.WriteFile(filepath.Join(proc, "meminfo"), raw, 0o644); err != nil {
				t.Fatalf("覆盖 meminfo 失败：%v", err)
			}
			c := collect.New(proc, fixtureSys, collect.IfaceFilter{})
			m := c.Metrics(context.Background())
			if m.MemUsed != tc.wantUsed {
				t.Errorf("mem_used = %d，期望 %d", m.MemUsed, tc.wantUsed)
			}
			if m.Quality != nil && m.Quality.Mem == metrics.QualityUnknown {
				t.Error("mem 组应为有效")
			}
		})
	}
}

// TestMetricsCPUDiff 改写 /proc/stat 的 cpu 累计时间后第二次采样：
// user +100、idle +300 → Δtotal 400、Δidle 300 → 25%。
func TestMetricsCPUDiff(t *testing.T) {
	proc := copyTree(t, fixtureProc)
	c := collect.New(proc, fixtureSys, collect.IfaceFilter{})

	first := c.Metrics(context.Background())
	if first.Quality == nil || first.Quality.CPU != metrics.QualityUnknown {
		t.Fatalf("首样本 cpu 应为 unknown：%+v", first.Quality)
	}

	raw, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		t.Fatalf("读取 stat 失败：%v", err)
	}
	lines := strings.SplitN(string(raw), "\n", 2)
	lines[0] = "cpu  4805 356 1623 155630 1024 0 233 0 0 0"
	if err := os.WriteFile(filepath.Join(proc, "stat"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("改写 stat 失败：%v", err)
	}

	second := c.Metrics(context.Background())
	if second.Quality != nil && second.Quality.CPU == metrics.QualityUnknown {
		t.Fatal("第二样本 cpu 应有效")
	}
	if second.CPU != 25 {
		t.Errorf("cpu = %v，期望 25", second.CPU)
	}
	if err := second.Validate(); err != nil {
		t.Errorf("Metrics 校验失败：%v", err)
	}
}

// TestIfaceFilter 覆盖接口筛选：默认只计 eth0；Include 显式纳入可覆盖
// 默认前缀剔除；Exclude 优先于 Include。
func TestIfaceFilter(t *testing.T) {
	// 默认：lo、docker0、veth、tun0、br-、tailscale0 被剔除，仅计 eth0。
	def := collect.New(fixtureProc, fixtureSys, collect.IfaceFilter{})
	m := def.Metrics(context.Background())
	if m.Iface != "eth0" || m.NetRXTotal != 12345678901 || m.NetTXTotal != 9876543210 {
		t.Errorf("默认过滤：iface=%q rx=%d tx=%d", m.Iface, m.NetRXTotal, m.NetTXTotal)
	}

	// Include 显式纳入 docker0（覆盖默认前缀剔除）。
	inc := collect.New(fixtureProc, fixtureSys, collect.IfaceFilter{Include: []string{"eth0", "docker0"}})
	mi := inc.Metrics(context.Background())
	if mi.Iface != "docker0,eth0" {
		t.Errorf("iface = %q，期望 %q（排序后逗号连接）", mi.Iface, "docker0,eth0")
	}
	if want := uint64(12346018901); mi.NetRXTotal != want {
		t.Errorf("net_rx_total = %d，期望 %d（eth0 + docker0）", mi.NetRXTotal, want)
	}
	if want := uint64(9882143210); mi.NetTXTotal != want {
		t.Errorf("net_tx_total = %d，期望 %d", mi.NetTXTotal, want)
	}

	// Exclude 剔除后为空集：读取成功，net_total 仍有效，计数为 0。
	exc := collect.New(fixtureProc, fixtureSys, collect.IfaceFilter{Exclude: []string{"eth0"}})
	me := exc.Metrics(context.Background())
	if me.Iface != "" || me.NetRXTotal != 0 || me.NetTXTotal != 0 {
		t.Errorf("Exclude 后应为空集：iface=%q rx=%d tx=%d", me.Iface, me.NetRXTotal, me.NetTXTotal)
	}
	if me.Quality != nil && me.Quality.NetTotal == metrics.QualityUnknown {
		t.Error("空集合读取成功，net_total 不应为 unknown")
	}

	// 排除优先于纳入。
	both := collect.New(fixtureProc, fixtureSys, collect.IfaceFilter{
		Include: []string{"eth0"}, Exclude: []string{"eth0"},
	})
	mb := both.Metrics(context.Background())
	if mb.Iface != "" || mb.NetRXTotal != 0 {
		t.Errorf("排除应优先于纳入：iface=%q rx=%d", mb.Iface, mb.NetRXTotal)
	}
}

// TestMetricsReadFailureDegrades 全部数据源缺失时：永不整体失败，
// 各组质量降级为 unknown、数值为 0，结果仍通过契约校验。
func TestMetricsReadFailureDegrades(t *testing.T) {
	c := collect.New(t.TempDir(), t.TempDir(), collect.IfaceFilter{})
	m := c.Metrics(context.Background())
	if err := m.Validate(); err != nil {
		t.Fatalf("降级样本仍应通过校验：%v", err)
	}
	if m.Quality == nil {
		t.Fatal("全部读取失败应登记质量降级")
	}
	for name, v := range map[string]string{
		"cpu": m.Quality.CPU, "mem": m.Quality.Mem, "swap": m.Quality.Swap,
		"disk": m.Quality.Disk, "net_rate": m.Quality.NetRate,
		"net_total": m.Quality.NetTotal, "sys": m.Quality.Sys,
	} {
		if v != metrics.QualityUnknown {
			t.Errorf("quality.%s = %q，期望 unknown", name, v)
		}
	}
	if m.CPU != 0 || m.MemUsed != 0 || m.DiskUsed != 0 || m.NetRXTotal != 0 ||
		m.Uptime != 0 || m.TCP != 0 || m.Procs != 0 {
		t.Errorf("unknown 组数值应为 0：%+v", m)
	}
}

// TestContextCancelled ctx 取消时：Facts 返回错误；Metrics 不失败，
// 返回全部组 unknown 的空样本。
func TestContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := collect.New(fixtureProc, fixtureSys, collect.IfaceFilter{})
	if _, err := c.Facts(ctx); err == nil {
		t.Error("ctx 取消时 Facts 应返回错误")
	}
	m := c.Metrics(ctx)
	if err := m.Validate(); err != nil {
		t.Fatalf("空样本仍应通过校验：%v", err)
	}
	if m.Quality == nil || m.Quality.CPU != metrics.QualityUnknown ||
		m.Quality.Sys != metrics.QualityUnknown || m.Quality.NetTotal != metrics.QualityUnknown {
		t.Errorf("ctx 取消应返回全 unknown：%+v", m.Quality)
	}
}

// TestConcurrentFactsMetrics Facts/Metrics 并发调用不竞争、不 panic，
// 每个样本都通过契约校验（配合 -race 使用更有效）。
func TestConcurrentFactsMetrics(t *testing.T) {
	c := collect.New(fixtureProc, fixtureSys, collect.IfaceFilter{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := c.Facts(context.Background()); err != nil {
					t.Errorf("Facts 失败：%v", err)
				}
				m := c.Metrics(context.Background())
				if err := m.Validate(); err != nil {
					t.Errorf("Metrics 校验失败：%v", err)
				}
			}
		}()
	}
	wg.Wait()
}
