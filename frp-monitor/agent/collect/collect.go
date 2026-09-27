// Package collect samples Linux kernel counters without spawning per-sample workers.
// Its arithmetic and filtering follow the pinned reference documented in README.md;
// the implementation is independently written for the monitor quality contract.
// Algorithm/reference attribution: monitor-probe/agent at
// cebc5383abb5963abeb722877a2d82c1a7aea5c6, Copyright (c) 2026 stqfdyr.
// See LICENSE.monitor-probe for its MIT license.
package collect

import (
	"errors"
	"io"
	"math"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const maxFileBytes = 4 << 20
const maxDirEntries = 131072
const maxInterfaces = 4096
const maxMounts = 4096

// Config selects counted interfaces and the reported build version.
// Iface accepts comma-separated full names; -name exclusions take precedence.
type Config struct {
	Iface   string
	Version string
}

type diskStat struct{ Blocks, Bfree, Frsize, Bsize uint64 }
type interfaceAddress struct {
	Name string
	Up   bool
	IP   net.IP
}
type dependencies struct {
	read      func(string) (string, error)
	readDir   func(string) ([]string, error)
	exists    func(string) (bool, error)
	statvfs   func(string) (diskStat, error)
	addresses func() ([]interfaceAddress, error)
	now       func() time.Time
	supported bool
	arch      string
}

type cpuReading struct{ total, idle uint64 }
type counters struct{ rx, tx uint64 }
type netReading struct {
	values map[string]counters
	epoch  string
	at     time.Time
}

// Collector is safe for concurrent Facts and Metrics calls. Facts does not advance
// rate baselines. A failed metric read leaves the last valid baseline intact.
type Collector struct {
	mu      sync.Mutex
	config  Config
	filter  ifaceFilter
	deps    dependencies
	cpu     *cpuReading
	network *netReading
}

func New(config Config) (*Collector, error) { return newCollector(config, nativeDependencies()) }
func newCollector(config Config, deps dependencies) (*Collector, error) {
	filter, err := parseIface(config.Iface)
	if err != nil {
		return nil, err
	}
	if config.Version == "" {
		config.Version = "development"
	}
	if !validText(config.Version) {
		return nil, errors.New("invalid agent version")
	}
	config.Iface = filter.spec
	return &Collector{config: config, filter: filter, deps: deps}, nil
}

func good[T any](value T) shared.Field[T] {
	return shared.Field[T]{Value: &value, Quality: shared.QualityOK}
}
func missing[T any](quality shared.Quality, reason string) shared.Field[T] {
	return shared.Field[T]{Quality: quality, Reason: reason}
}
func unavailable[T any]() shared.Field[T] { return missing[T](shared.QualityUnavailable, "read_error") }
func unsupported[T any]() shared.Field[T] { return missing[T](shared.QualityUnsupported, "") }
func warming[T any](reason string) shared.Field[T] {
	return missing[T](shared.QualityWarmingUp, reason)
}
func validText(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && len(value) <= shared.MaxStringBytes && !strings.ContainsAny(value, "\x00\r\n")
}
func textField(text string, err error) shared.Field[string] {
	text = strings.TrimSpace(text)
	if err != nil || !validText(text) {
		return unavailable[string]()
	}
	return good(text)
}
func satAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
func satMul(a, b uint64) uint64 {
	if b != 0 && a > math.MaxUint64/b {
		return math.MaxUint64
	}
	return a * b
}
func satSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// readBounded avoids unbounded allocation when kernel/config files are malformed.
func readBounded(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxFileBytes {
		return "", errors.New("file exceeds collection limit")
	}
	if !utf8.Valid(data) {
		return "", errors.New("file is not UTF-8")
	}
	return string(data), nil
}
func dirBounded(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(maxDirEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > maxDirEntries {
		return nil, errors.New("directory exceeds collection limit")
	}
	return names, nil
}
func nativeDependencies() dependencies {
	return dependencies{read: readBounded, readDir: dirBounded, exists: func(path string) (bool, error) {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return false, nil
		}
		return err == nil, err
	}, statvfs: nativeStatvfs, addresses: nativeAddresses, now: time.Now, supported: nativeSupported, arch: runtime.GOARCH}
}
func nativeAddresses() ([]interfaceAddress, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	if len(interfaces) > maxInterfaces {
		return nil, errors.New("too many interfaces")
	}
	out := make([]interfaceAddress, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagRunning == 0 || skipIface(iface.Name) {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		if len(addresses) > maxInterfaces || len(out)+len(addresses) > maxInterfaces {
			return nil, errors.New("too many addresses")
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil {
				return nil, err
			}
			out = append(out, interfaceAddress{iface.Name, true, ip})
		}
	}
	return out, nil
}

// scope is intentionally conservative. Namespace equality with PID 1 does not
// prove that PID 1 belongs to the host, so absence of container markers is unknown.
func (c *Collector) scope() shared.Scope {
	for _, path := range []string{"/.dockerenv", "/run/.containerenv", "/proc/vz"} {
		if found, _ := c.deps.exists(path); found {
			return shared.ScopeNamespace
		}
	}
	if value, err := c.deps.read("/run/systemd/container"); err == nil && strings.TrimSpace(value) != "" {
		return shared.ScopeNamespace
	}
	if value, err := c.deps.read("/proc/1/cgroup"); err == nil {
		for _, marker := range []string{"docker", "kubepods", "lxc", "libpod", "containerd"} {
			if strings.Contains(value, marker) {
				return shared.ScopeNamespace
			}
		}
	}
	return shared.ScopeUnknown
}

func (c *Collector) Metrics() shared.Metrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := emptyMetrics()
	if !c.deps.supported {
		return m
	}
	m.Scope = c.scope()
	m.CPU = c.cpuMetric()
	mem, err := c.deps.read("/proc/meminfo")
	m.MemTotal, m.MemUsed, m.SwapTotal, m.SwapUsed = memoryMetrics(mem, err)
	m.DiskTotal, m.DiskUsed = c.diskMetrics()
	text, err := c.deps.read("/proc/loadavg")
	m.Load = loadMetric(text, err)
	text, err = c.deps.read("/proc/uptime")
	m.Uptime = uptimeMetric(text, err)
	m.TCP, m.UDP = c.socketMetrics()
	m.Procs = c.processMetric()
	c.networkMetrics(&m)
	return m
}
func emptyMetrics() shared.Metrics {
	u := unsupported[uint64]()
	return shared.Metrics{Scope: shared.ScopeUnknown, CPU: unsupported[float64](), Load: unsupported[[]float64](), MemTotal: u, MemUsed: u, SwapTotal: u, SwapUsed: u, DiskTotal: u, DiskUsed: u, NetRX: u, NetTX: u, NetRXTotal: u, NetTXTotal: u, BootID: unsupported[string](), Iface: unsupported[string](), Uptime: u, TCP: u, UDP: u, Procs: u}
}
func (c *Collector) cpuMetric() shared.Field[float64] {
	text, err := c.deps.read("/proc/stat")
	if err != nil {
		return unavailable[float64]()
	}
	current, err := parseCPU(text)
	if err != nil {
		return unavailable[float64]()
	}
	previous := c.cpu
	c.cpu = &current
	if previous == nil {
		return warming[float64]("no_baseline")
	}
	if current.total <= previous.total || current.idle < previous.idle {
		return warming[float64]("counter_reset")
	}
	total := current.total - previous.total
	idle := satSub(current.idle, previous.idle)
	busy := float64(satSub(total, idle)) * 100 / float64(total)
	return good(busy)
}
func (c *Collector) processMetric() shared.Field[uint64] {
	names, err := c.deps.readDir("/proc")
	if err != nil {
		return unavailable[uint64]()
	}
	var count uint64
	for _, name := range names {
		if name == "" {
			continue
		}
		numeric := true
		for _, ch := range name {
			if ch < '0' || ch > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			count++
		}
	}
	return good(count)
}
func (c *Collector) diskMetrics() (shared.Field[uint64], shared.Field[uint64]) {
	bad := unavailable[uint64]()
	text, err := c.deps.read("/proc/self/mounts")
	if err != nil {
		return bad, bad
	}
	mounts, err := parseMounts(text)
	if err != nil || len(mounts) == 0 {
		return bad, bad
	}
	var total, used uint64
	for _, mount := range mounts {
		s, err := c.deps.statvfs(mount)
		if err != nil {
			return bad, bad
		}
		size := s.Frsize
		if size == 0 {
			size = s.Bsize
		}
		if size == 0 {
			return bad, bad
		}
		total = satAdd(total, satMul(s.Blocks, size))
		used = satAdd(used, satMul(satSub(s.Blocks, s.Bfree), size))
	}
	return good(total), good(used)
}
func (c *Collector) socketMetrics() (shared.Field[uint64], shared.Field[uint64]) {
	v4, e4 := c.deps.read("/proc/net/sockstat")
	v6, e6 := c.deps.read("/proc/net/sockstat6")
	if e4 != nil {
		return unavailable[uint64](), unavailable[uint64]()
	}
	// Missing proc entries do not prove that a socket family is disabled.
	// Preserve unknown totals rather than silently dropping that family.
	if e6 != nil {
		return unavailable[uint64](), unavailable[uint64]()
	}
	tcp, udp, err := parseSockets(v4, v6, true)
	if err != nil {
		return unavailable[uint64](), unavailable[uint64]()
	}
	return good(tcp), good(udp)
}
