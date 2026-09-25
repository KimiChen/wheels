// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

var (
	fixtureProcDir = filepath.Join("..", "..", "tests", "fixtures", "proc")
	fixtureSysDir  = filepath.Join("..", "..", "tests", "fixtures", "sys")
)

// copyProcFixture 复制 proc fixture 目录树到临时目录，便于改写计数器文件。
func copyProcFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(fixtureProcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(fixtureProcDir, path)
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

// writeNetDev 改写临时 proc 树的 net/dev：lo 保持固定，eth0 计数器
// 由参数给出，extra 可追加额外接口行。
func writeNetDev(t *testing.T, procRoot string, rx, tx uint64, extra string) {
	t.Helper()
	content := fmt.Sprintf("Inter-|   Receive                                                |  Transmit\n"+
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"+
		"    lo: 1050000   12000    0    0    0     0          0         0  1050000   12000    0    0    0     0       0          0\n"+
		"  eth0: %d 9876543    0    2    0     0          0         0 %d 8765432    0    0    0     0       0          0\n%s",
		rx, tx, extra)
	if err := os.WriteFile(filepath.Join(procRoot, "net", "dev"), []byte(content), 0o644); err != nil {
		t.Fatalf("改写 net/dev 失败：%v", err)
	}
}

// writeFile 改写临时 proc 树中的单个文件。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败：%v", path, err)
	}
}

// TestMetricsNetRateDiff 注入假时钟，验证网速差分除以实际时间差。
func TestMetricsNetRateDiff(t *testing.T) {
	proc := copyProcFixture(t)
	c := New(proc, fixtureSysDir, IfaceFilter{})
	now := time.Unix(1790380800, 0)
	c.now = func() time.Time { return now }

	m1 := c.Metrics(context.Background())
	if m1.Quality == nil || m1.Quality.NetRate != metrics.QualityUnknown {
		t.Fatalf("首样本 net_rate 应为 unknown：%+v", m1.Quality)
	}
	if m1.NetRXTotal != 12345678901 || m1.NetTXTotal != 9876543210 {
		t.Errorf("net_total 首样本读取成功即有效：rx=%d tx=%d", m1.NetRXTotal, m1.NetTXTotal)
	}

	// 计数器 +400000/+200000，时间前进 2 秒 → 200000/100000 B/s。
	writeNetDev(t, proc, 12345678901+400000, 9876543210+200000, "")
	now = now.Add(2 * time.Second)
	m2 := c.Metrics(context.Background())
	if m2.Quality != nil && m2.Quality.NetRate == metrics.QualityUnknown {
		t.Fatalf("第二样本 net_rate 应有效：%+v", m2.Quality)
	}
	if m2.NetRX != 200000 || m2.NetTX != 100000 {
		t.Errorf("net_rx/net_tx = %v/%v，期望 200000/100000", m2.NetRX, m2.NetTX)
	}
}

// TestMetricsNetScopeReset 覆盖基线重建：boot_id 变化、计数器回退、
// 网卡集合变化后，下一期速率重新为 unknown。
func TestMetricsNetScopeReset(t *testing.T) {
	proc := copyProcFixture(t)
	c := New(proc, fixtureSysDir, IfaceFilter{})
	now := time.Unix(1790380800, 0)
	c.now = func() time.Time { return now }

	c.Metrics(context.Background()) // 建基线

	// boot_id 变化 → 计数器范围变化 → 重建基线。
	const newBootID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writeFile(t, filepath.Join(proc, "sys", "kernel", "random", "boot_id"), newBootID+"\n")
	now = now.Add(time.Second)
	m := c.Metrics(context.Background())
	if m.Quality == nil || m.Quality.NetRate != metrics.QualityUnknown {
		t.Fatalf("boot_id 变化后 net_rate 应为 unknown：%+v", m.Quality)
	}
	if m.BootID != newBootID {
		t.Errorf("boot_id = %q，期望 %q", m.BootID, newBootID)
	}

	// 同范围再采样 → 速率恢复（+1000/+2000，1 秒）。
	writeNetDev(t, proc, 12345678901+1000, 9876543210+2000, "")
	now = now.Add(time.Second)
	m = c.Metrics(context.Background())
	if m.Quality != nil && m.Quality.NetRate == metrics.QualityUnknown {
		t.Fatal("同范围第二样本 net_rate 应有效")
	}
	if m.NetRX != 1000 || m.NetTX != 2000 {
		t.Errorf("net_rx/net_tx = %v/%v，期望 1000/2000", m.NetRX, m.NetTX)
	}

	// 计数器回退 → 重建基线；totals 仍上报当前读数。
	writeNetDev(t, proc, 100, 200, "")
	now = now.Add(time.Second)
	m = c.Metrics(context.Background())
	if m.Quality == nil || m.Quality.NetRate != metrics.QualityUnknown {
		t.Fatal("计数器回退后 net_rate 应为 unknown")
	}
	if m.NetRXTotal != 100 || m.NetTXTotal != 200 {
		t.Errorf("net_total = %d/%d，期望当前读数 100/200", m.NetRXTotal, m.NetTXTotal)
	}

	// 网卡集合变化（新增 eth1）→ 重建基线。
	writeNetDev(t, proc, 300, 400, "  eth1: 5000 60    0    0    0     0          0         0  7000 70    0    0    0     0       0          0\n")
	now = now.Add(time.Second)
	m = c.Metrics(context.Background())
	if m.Iface != "eth0,eth1" {
		t.Errorf("iface = %q，期望 %q", m.Iface, "eth0,eth1")
	}
	if m.NetRXTotal != 5300 || m.NetTXTotal != 7400 {
		t.Errorf("net_total = %d/%d，期望 5300/7400", m.NetRXTotal, m.NetTXTotal)
	}
	if m.Quality == nil || m.Quality.NetRate != metrics.QualityUnknown {
		t.Fatal("网卡集合变化后 net_rate 应为 unknown")
	}
}

// TestDiskUsageFixtureMounts 用 fixture 挂载表 + 注入的 statfs 验证
// 汇总逻辑：伪文件系统（proc/sysfs/tmpfs）、overlay、NFS 被过滤；
// /var/lib/docker 与 / 同设备（st_dev 相同），去重后只计 / 一次。
func TestDiskUsageFixtureMounts(t *testing.T) {
	c := New(fixtureProcDir, fixtureSysDir, IfaceFilter{})
	c.statfs = func(path string) (mountStat, error) {
		switch path {
		case "/":
			return mountStat{dev: 100, blocks: 1000, bfree: 250, bsize: 4096}, nil
		case "/var/lib/docker":
			return mountStat{dev: 100, blocks: 9999, bfree: 9999, bsize: 4096}, nil
		default:
			return mountStat{}, fmt.Errorf("过滤后不应到达的挂载点：%s", path)
		}
	}

	total, used, err := c.diskUsage()
	if err != nil {
		t.Fatalf("diskUsage 失败：%v", err)
	}
	if want := uint64(1000 * 4096); total != want {
		t.Errorf("disk_total = %d，期望 %d（同设备去重只计一次）", total, want)
	}
	if want := uint64((1000 - 250) * 4096); used != want {
		t.Errorf("disk_used = %d，期望 %d", used, want)
	}

	m := c.Metrics(context.Background())
	if m.DiskTotal != 1000*4096 || m.DiskUsed != 750*4096 {
		t.Errorf("Metrics 磁盘组 = %d/%d", m.DiskTotal, m.DiskUsed)
	}
	if m.Quality != nil && m.Quality.Disk == metrics.QualityUnknown {
		t.Error("disk 组应有效")
	}

	f, err := c.Facts(context.Background())
	if err != nil {
		t.Fatalf("Facts 失败：%v", err)
	}
	if f.DiskTotal != 1000*4096 {
		t.Errorf("Facts disk_total = %d，期望 %d", f.DiskTotal, 1000*4096)
	}
}

// TestDiskUsageSaturation 覆盖 uint64 饱和运算：blocks × bsize 溢出时
// 钳制到 MaxUint64；bfree > blocks 的异常读数下 used 饱和为 0，不回绕。
func TestDiskUsageSaturation(t *testing.T) {
	proc := t.TempDir()
	writeFile(t, filepath.Join(proc, "self", "mounts"), "/dev/sda1 / ext4 rw,relatime 0 0\n")

	c := New(proc, proc, IfaceFilter{})
	c.statfs = func(string) (mountStat, error) {
		return mountStat{dev: 100, blocks: math.MaxUint64/4096 + 2, bfree: 2, bsize: 4096}, nil
	}
	total, used, err := c.diskUsage()
	if err != nil {
		t.Fatalf("diskUsage 失败：%v", err)
	}
	if total != math.MaxUint64 {
		t.Errorf("溢出应饱和到 MaxUint64：total = %d", total)
	}
	if want := uint64(math.MaxUint64 / 4096 * 4096); used != want {
		t.Errorf("used = %d，期望 %d", used, want)
	}

	c.statfs = func(string) (mountStat, error) {
		return mountStat{dev: 100, blocks: 100, bfree: 250, bsize: 4096}, nil
	}
	total, used, err = c.diskUsage()
	if err != nil {
		t.Fatalf("diskUsage 失败：%v", err)
	}
	if total != 100*4096 || used != 0 {
		t.Errorf("bfree > blocks：total/used = %d/%d，期望 %d/0", total, used, 100*4096)
	}
}

// TestDiskUsageNoValidMounts 没有任何有效本地挂载（容器受限场景）：
// diskUsage 返回 errNoValidMount，Metrics 的 disk 组降级为 unknown。
func TestDiskUsageNoValidMounts(t *testing.T) {
	proc := t.TempDir()
	writeFile(t, filepath.Join(proc, "self", "mounts"),
		"proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0\n"+
			"tmpfs /run tmpfs rw,nosuid,nodev 0 0\n")

	c := New(proc, proc, IfaceFilter{})
	c.statfs = func(path string) (mountStat, error) {
		t.Errorf("过滤后不应调用 statfs：%s", path)
		return mountStat{}, errors.New("unexpected")
	}
	if _, _, err := c.diskUsage(); !errors.Is(err, errNoValidMount) {
		t.Fatalf("err = %v，期望 errNoValidMount", err)
	}

	m := c.Metrics(context.Background())
	if m.Quality == nil || m.Quality.Disk != metrics.QualityUnknown {
		t.Fatalf("disk 组应为 unknown：%+v", m.Quality)
	}
	if m.DiskTotal != 0 || m.DiskUsed != 0 {
		t.Errorf("unknown 组数值应为 0：%d/%d", m.DiskTotal, m.DiskUsed)
	}
}

// TestSelectAddr 覆盖地址选择纯函数：公网优先、私网/ULA 回退、
// 回环与链路本地跳过、4in6 归一化、网卡过滤（排除优先）。
func TestSelectAddr(t *testing.T) {
	keepAll := func(string) bool { return true }
	keepDefault := New("", "", IfaceFilter{}).keepIface
	keepExcludeEth0 := New("", "", IfaceFilter{Exclude: []string{"eth0"}}).keepIface

	addr := func(iface, ip string) ifaceAddr {
		return ifaceAddr{iface: iface, ip: netip.MustParseAddr(ip)}
	}
	cases := []struct {
		name  string
		addrs []ifaceAddr
		want6 bool
		keep  func(string) bool
		want  string
	}{
		{"公网优先于私网", []ifaceAddr{addr("eth0", "10.0.0.8"), addr("eth0", "192.0.2.10")}, false, keepAll, "192.0.2.10"},
		{"仅私网则回退", []ifaceAddr{addr("eth0", "10.0.0.8")}, false, keepAll, "10.0.0.8"},
		{"回环与链路本地跳过", []ifaceAddr{addr("eth0", "127.0.0.1"), addr("eth0", "169.254.1.1")}, false, keepAll, ""},
		{"v6 公网优先", []ifaceAddr{addr("eth0", "fe80::1"), addr("eth0", "2001:db8::10")}, true, keepAll, "2001:db8::10"},
		{"v6 仅链路本地", []ifaceAddr{addr("eth0", "fe80::1")}, true, keepAll, ""},
		{"v6 ULA 回退", []ifaceAddr{addr("eth0", "fd00::10")}, true, keepAll, "fd00::10"},
		{"v4 地址不混入 v6", []ifaceAddr{addr("eth0", "192.0.2.10")}, true, keepAll, ""},
		{"4in6 归一化为 v4", []ifaceAddr{addr("eth0", "::ffff:192.0.2.10")}, false, keepAll, "192.0.2.10"},
		{"默认过滤网卡跳过", []ifaceAddr{addr("docker0", "192.0.2.10")}, false, keepDefault, ""},
		{"Exclude 优先", []ifaceAddr{addr("eth0", "192.0.2.10")}, false, keepExcludeEth0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectAddr(tc.addrs, tc.want6, tc.keep); got != tc.want {
				t.Errorf("selectAddr = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestMatchVirt 覆盖虚拟化关键字匹配：小写子串命中返回规范名。
func TestMatchVirt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"KVM", "kvm"},
		{"QEMU Standard PC (i440FX + PIIX, 1996)", "qemu"},
		{"VMware Virtual Platform", "vmware"},
		{"VirtualBox", "virtualbox"},
		{"Microsoft Corporation", "microsoft"},
		{"Xen HVM domU", "xen"},
		{"Amazon EC2", "amazon"},
		{"Google Compute Engine", "google"},
		{"Bochs", "bochs"},
		{"PowerEdge R740", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := matchVirt(tc.in); got != tc.want {
			t.Errorf("matchVirt(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestSatArithmetic 覆盖 uint64 饱和运算的边界。
func TestSatArithmetic(t *testing.T) {
	if got := satAdd(math.MaxUint64-1, 2); got != math.MaxUint64 {
		t.Errorf("satAdd 溢出 = %d", got)
	}
	if got := satAdd(1, 2); got != 3 {
		t.Errorf("satAdd = %d", got)
	}
	if got := satMul(math.MaxUint64/2+1, 2); got != math.MaxUint64 {
		t.Errorf("satMul 溢出 = %d", got)
	}
	if got := satMul(3, 4); got != 12 {
		t.Errorf("satMul = %d", got)
	}
	if got := satSub(10, 11); got != 0 {
		t.Errorf("satSub 下溢 = %d", got)
	}
	if got := satSub(10, 4); got != 6 {
		t.Errorf("satSub = %d", got)
	}
}

// TestMountUnescape 覆盖挂载表路径的八进制转义还原。
func TestMountUnescape(t *testing.T) {
	cases := map[string]string{
		`/mnt/my\040dir`: "/mnt/my dir",
		`/a\134b`:        `/a\b`,
		`/mnt/plain`:     "/mnt/plain",
	}
	for in, want := range cases {
		if got := mountUnescape.Replace(in); got != want {
			t.Errorf("unescape(%q) = %q，期望 %q", in, got, want)
		}
	}
}
