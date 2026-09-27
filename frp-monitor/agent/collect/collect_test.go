package collect

import (
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func expectValue[T comparable](t *testing.T, field shared.Field[T], want T) {
	t.Helper()
	if field.Quality != shared.QualityOK || field.Value == nil || *field.Value != want {
		t.Fatalf("field = %+v, want %v", field, want)
	}
}
func TestReadFailuresPreserveRateBaselines(t *testing.T) {
	fake := newFake()
	c := fake.collector(t, "")
	first := c.Metrics()
	if first.CPU.Reason != "no_baseline" || first.NetRX.Reason != "no_baseline" {
		t.Fatal("first rates must be unknown")
	}
	cpu, network := fake.files["/proc/stat"], fake.files["/proc/net/dev"]
	delete(fake.files, "/proc/stat")
	delete(fake.files, "/proc/net/dev")
	fake.now = fake.now.Add(time.Second)
	failed := c.Metrics()
	if failed.CPU.Quality != shared.QualityUnavailable || failed.NetRXTotal.Quality != shared.QualityUnavailable {
		t.Fatal("read failure disguised as a value")
	}
	fake.files["/proc/stat"] = strings.Replace(cpu, "100 0 50 700", "150 0 100 750", 1)
	fake.files["/proc/net/dev"] = strings.Replace(strings.Replace(network, "1000", "1200", 1), "2000", "2400", 1)
	fake.now = fake.now.Add(time.Second)
	recovered := c.Metrics()
	expectValue(t, recovered.NetRX, uint64(100))
	expectValue(t, recovered.NetTX, uint64(200))
	if recovered.CPU.Value == nil || math.Abs(*recovered.CPU.Value-200.0/3) > 0.000001 {
		t.Fatalf("CPU must span valid samples: %+v", recovered.CPU)
	}
}
func TestNetworkResetScopeAndClock(t *testing.T) {
	fake := newFake()
	c := fake.collector(t, "")
	first := c.Metrics()
	fake.now = fake.now.Add(time.Second)
	fake.files["/proc/net/dev"] = netText(map[string][2]uint64{"eth0": {10, 20}})
	reset := c.Metrics()
	if reset.NetRX.Reason != "counter_reset" {
		t.Fatalf("missing rollback: %+v", reset.NetRX)
	}
	fake.now = fake.now.Add(time.Second)
	fake.files["/proc/net/dev"] = netText(map[string][2]uint64{"eth0": {110, 220}})
	expectValue(t, c.Metrics().NetRX, uint64(100))
	fake.now = fake.now.Add(time.Second)
	fake.files["/proc/sys/kernel/random/boot_id"] = "second-boot"
	reboot := c.Metrics()
	if reboot.NetRX.Reason != "scope_changed" || *reboot.BootID.Value == *first.BootID.Value {
		t.Fatal("boot change reused network baseline")
	}
	fake.files["/proc/net/dev"] = netText(map[string][2]uint64{"eth0": {210, 420}})
	frozen := c.Metrics()
	if frozen.NetRX.Quality != shared.QualityUnavailable || frozen.NetTX.Quality != shared.QualityUnavailable || frozen.NetRX.Reason != "" {
		t.Fatal("equal monotonic timestamps produced a rate")
	}
	expectValue(t, frozen.NetRXTotal, uint64(210))
	expectValue(t, frozen.NetTXTotal, uint64(420))
	expectValue(t, frozen.BootID, *reboot.BootID.Value)
	fake.now = fake.now.Add(time.Second)
	expectValue(t, c.Metrics().NetRX, uint64(100))
}
func TestMalformedNetworkAndBootDoNotReplaceBaseline(t *testing.T) {
	fake := newFake()
	c := fake.collector(t, "")
	c.Metrics()
	fake.now = fake.now.Add(time.Second)
	fake.files["/proc/net/dev"] = "header\nheader\neth0: broken\n"
	if got := c.Metrics(); got.NetRXTotal.Value != nil {
		t.Fatal("malformed total accepted")
	}
	fake.files["/proc/net/dev"] = netText(map[string][2]uint64{"eth0": {1300, 2600}})
	delete(fake.files, "/proc/sys/kernel/random/boot_id")
	fake.now = fake.now.Add(time.Second)
	if got := c.Metrics(); got.NetRXTotal.Value != nil {
		t.Fatal("total without boot ID accepted")
	}
	fake.files["/proc/sys/kernel/random/boot_id"] = "test-boot"
	fake.now = fake.now.Add(time.Second)
	expectValue(t, c.Metrics().NetRX, uint64(100))
}
func TestMemoryMissingFieldsAndMalformedAvailable(t *testing.T) {
	for _, test := range []struct {
		name, text string
		totalOK    bool
	}{
		{"empty", "", false},
		{"missing available fallback", "MemTotal: 100 kB\nMemFree: 10 kB\nCached: 20 kB\n", true},
		{"malformed available not fallback", "MemTotal: 100 kB\nMemAvailable: broken kB\nMemFree: 10 kB\nBuffers: 10 kB\nCached: 10 kB\n", true},
		{"overflow", "MemTotal: 18446744073709551615 kB\nMemAvailable: 1 kB\n", false},
		{"duplicate", "MemTotal: 100 kB\nMemTotal: 100 kB\nMemAvailable: 1 kB\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFake()
			fake.files["/proc/meminfo"] = test.text
			m := fake.collector(t, "").Metrics()
			if (m.MemTotal.Quality == shared.QualityOK) != test.totalOK || m.MemUsed.Value != nil || m.SwapUsed.Value != nil {
				t.Fatalf("missing/malformed fields must be unknown: %+v", m)
			}
		})
	}
}
func TestDiskFailuresAndNoMountsAreUnknown(t *testing.T) {
	fake := newFake()
	c := fake.collector(t, "")
	fake.files["/proc/self/mounts"] = "overlay / overlay rw 0 0\n"
	if c.Metrics().DiskTotal.Value != nil {
		t.Fatal("empty real storage set reported zero")
	}
	fake.files["/proc/self/mounts"] = "/dev/a / ext4 rw 0 0\n/dev/b /data xfs rw 0 0\n"
	if c.Metrics().DiskTotal.Value != nil {
		t.Fatal("partial disk sum reported as complete")
	}
	fake.disks["/data"] = diskStat{Blocks: math.MaxUint64, Bfree: 0, Frsize: 4096}
	m := c.Metrics()
	expectValue(t, m.DiskTotal, uint64(math.MaxUint64))
	expectValue(t, m.DiskUsed, uint64(math.MaxUint64))
	fake.files["/proc/self/mounts"] = "/dev/a / ext4 rw 0 0\nmalformed\n"
	if c.Metrics().DiskTotal.Value != nil {
		t.Fatal("truncated mounts accepted")
	}
}
func TestIfaceValidationAndTopology(t *testing.T) {
	for _, spec := range []string{"eth0 eth1", "-", "eth0,-", "--eth0", "../eth0", "eth0/../../", "eth0:1", "a\x00b", strings.Repeat("a", 16)} {
		if _, err := New(Config{Iface: spec}); err == nil {
			t.Fatalf("accepted %q", spec)
		}
	}
	fake := newFake()
	fake.files["/proc/net/dev"] = netText(map[string][2]uint64{"odd": {10, 20}, "wan": {20, 40}, "mv0": {30, 60}, "br0": {100, 200}})
	fake.files["/sys/class/net/odd/type"] = "778"
	fake.dirs["/sys/class/net/wan"] = []string{"device", "lower_eth0"}
	fake.paths["/sys/class/net/wan/device"] = true
	c := fake.collector(t, "")
	expectValue(t, c.Metrics().NetRXTotal, uint64(30))
	c = fake.collector(t, " odd,wan,br0,-wan ")
	m := c.Metrics()
	expectValue(t, m.NetRXTotal, uint64(110))
	expectValue(t, m.Iface, "odd,wan,br0,-wan")
	c = fake.collector(t, "eth9")
	expectValue(t, c.Metrics().NetRXTotal, uint64(0))
}
func TestEpochCanonicalSet(t *testing.T) {
	if epoch("boot", map[string]counters{}) != "boot/cbf29ce484222325" {
		t.Fatal("wrong FNV-1a basis")
	}
	a := epoch("boot", map[string]counters{"eth0": {1, 2}, "eth1": {3, 4}})
	b := epoch("boot", map[string]counters{"eth1": {30, 40}, "eth0": {10, 20}})
	if a != b || a == epoch("boot", map[string]counters{"eth0": {1, 2}}) {
		t.Fatal("epoch must identify the set, independently of counters/order")
	}
}
func TestFactsAddressAndScope(t *testing.T) {
	fake := newFake()
	fake.ips = []interfaceAddress{
		{"docker0", true, net.ParseIP("1.1.1.1")}, {"eth0", false, net.ParseIP("8.8.8.8")},
		{"eth0", true, net.ParseIP("198.18.0.1")}, {"eth0", true, net.ParseIP("10.0.0.1")}, {"vmbr0", true, net.ParseIP("203.0.113.7")},
		{"eth0", true, net.ParseIP("fd42::1")}, {"eth0", true, net.ParseIP("2401::2")}, {"br0", true, net.ParseIP("2401::3")},
	}
	fake.files["/proc/net/if_inet6"] = "24010000000000000000000000000002 02 40 00 101 eth0\n24010000000000000000000000000003 02 40 00 00 br0\n"
	c := fake.collector(t, "")
	f := c.Facts()
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	expectValue(t, f.IPv4, "203.0.113.7")
	expectValue(t, f.IPv6, "2401::3")
	expectValue(t, f.CPUCores, uint32(2))
	expectValue(t, f.CPUName, "Test CPU")
	expectValue(t, f.Arch, "x86_64")
	expectValue(t, f.OS, "Test Linux")
	if f.Scope != shared.ScopeUnknown {
		t.Fatal("absence of container markers does not prove host scope")
	}
	fake.paths["/.dockerenv"] = true
	f = c.Facts()
	if f.Scope != shared.ScopeNamespace || c.Metrics().Scope != shared.ScopeNamespace {
		t.Fatal("container must be labeled namespace")
	}
	expectValue(t, f.Virt, "docker")
}
func TestFactsDoNotAdvanceBaselinesAndMissingFacts(t *testing.T) {
	fake := newFake()
	c := fake.collector(t, "")
	c.Facts()
	if m := c.Metrics(); m.CPU.Reason != "no_baseline" || m.NetRX.Reason != "no_baseline" {
		t.Fatal("Facts advanced a rate baseline")
	}
	delete(fake.files, "/proc/cpuinfo")
	delete(fake.files, "/etc/os-release")
	delete(fake.files, "/proc/sys/kernel/hostname")
	fake.ips = nil
	f := c.Facts()
	if f.CPUCores.Value != nil || f.CPUName.Value != nil || f.Hostname.Value != nil || f.OS.Value != nil || f.IPv4.Value != nil || f.Virt.Value != nil {
		t.Fatal("missing facts were fabricated")
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestInvalidReadingsAreUnknown(t *testing.T) {
	for path, value := range map[string]string{"/proc/stat": "cpu 100 0 bad 100 0\n", "/proc/loadavg": "NaN 1 2", "/proc/uptime": "-5", "/proc/net/sockstat": "TCP: inuse 3\nUDP: inuse 1\n"} {
		fake := newFake()
		fake.files[path] = value
		m := fake.collector(t, "").Metrics()
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
		switch path {
		case "/proc/stat":
			if m.CPU.Value != nil {
				t.Fatal("malformed CPU")
			}
		case "/proc/loadavg":
			if m.Load.Value != nil {
				t.Fatal("invalid load")
			}
		case "/proc/uptime":
			if m.Uptime.Value != nil {
				t.Fatal("negative uptime")
			}
		default:
			if m.TCP.Value != nil {
				t.Fatal("missing socket key")
			}
		}
	}
	fake := newFake()
	deps := fake.deps()
	deps.readDir = func(string) ([]string, error) { return nil, errors.New("denied") }
	c, err := newCollector(Config{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if c.Metrics().Procs.Value != nil {
		t.Fatal("process read failure became zero")
	}
}
func TestConcurrentCalls(t *testing.T) {
	fake := newFake()
	c := fake.collector(t, "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := c.Facts().Validate(); err != nil {
					t.Error(err)
				}
				if err := c.Metrics().Validate(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
func TestBoundedReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample")
	if err := os.WriteFile(path, []byte("valid"), 0600); err != nil {
		t.Fatal(err)
	}
	if value, err := readBounded(path); err != nil || value != "valid" {
		t.Fatal("read small file")
	}
	if err := os.Truncate(path, maxFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(path); err == nil {
		t.Fatal("oversized file accepted")
	}
	if err := os.WriteFile(path, []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(path); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}
func TestNativeContract(t *testing.T) {
	c, err := New(Config{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f, m := c.Facts(), c.Metrics()
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if !nativeSupported {
		if m.CPU.Quality != shared.QualityUnsupported || f.Hostname.Quality != shared.QualityUnsupported {
			t.Fatal("non-Linux needs explicit unsupported fields")
		}
	} else if m.CPU.Reason != "no_baseline" || m.MemTotal.Value == nil {
		t.Fatal("Linux native smoke collection failed")
	}
}

func TestMissingSocketFamilyIsUnknown(t *testing.T) {
	fake := newFake()
	delete(fake.files, "/proc/net/sockstat6")
	m := fake.collector(t, "").Metrics()
	if m.TCP.Value != nil || m.UDP.Value != nil {
		t.Fatal("missing IPv6 data silently became zero")
	}
}
