package collect

import (
	"encoding/hex"
	"math"
	"net"
	"strconv"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func (c *Collector) Facts() shared.Facts {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := unsupported[string]()
	u := unsupported[uint64]()
	f := shared.Facts{Scope: shared.ScopeUnknown, Hostname: s, OS: s, Kernel: s, Arch: s, Virt: s, CPUName: s, CPUCores: unsupported[uint32](), AgentVersion: good(c.config.Version), IPv4: s, IPv6: s, MemTotal: u, SwapTotal: u, DiskTotal: u}
	if !c.deps.supported {
		return f
	}
	f.Scope = c.scope()
	f.Hostname = textField(c.deps.read("/proc/sys/kernel/hostname"))
	f.Kernel = textField(c.deps.read("/proc/sys/kernel/osrelease"))
	arch := c.deps.arch
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	f.Arch = textField(arch, nil)
	f.OS = unavailable[string]()
	if text, err := c.deps.read("/etc/os-release"); err == nil {
		for _, line := range strings.Split(text, "\n") {
			if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				f.OS = textField(strings.Trim(value, "\"'"), nil)
				break
			}
		}
	}
	cpu, err := c.deps.read("/proc/cpuinfo")
	f.CPUName, f.CPUCores = cpuFacts(cpu, err)
	f.Virt = c.virtualization(cpu, err)
	mem, err := c.deps.read("/proc/meminfo")
	f.MemTotal, _, f.SwapTotal, _ = memoryMetrics(mem, err)
	f.DiskTotal, _ = c.diskMetrics()
	f.IPv4, f.IPv6 = c.addresses()
	return f
}
func cpuFacts(text string, err error) (shared.Field[string], shared.Field[uint32]) {
	name := unavailable[string]()
	cores := unavailable[uint32]()
	if err != nil {
		return name, cores
	}
	var count uint64
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "processor" {
			count++
		}
		if name.Value == nil {
			switch key {
			case "model name", "Model", "cpu model", "Hardware":
				name = textField(value, nil)
			}
		}
	}
	if count > 0 && count <= math.MaxUint32 {
		cores = good(uint32(count))
	}
	return name, cores
}
func (c *Collector) virtualization(cpu string, cpuErr error) shared.Field[string] {
	for _, item := range []struct{ path, name string }{{"/proc/vz", "openvz"}, {"/proc/xen", "xen"}, {"/.dockerenv", "docker"}, {"/run/.containerenv", "container"}} {
		if exists, _ := c.deps.exists(item.path); exists {
			return good(item.name)
		}
	}
	for _, path := range []string{"/run/systemd/container", "/sys/hypervisor/type"} {
		if text, err := c.deps.read(path); err == nil && validText(strings.TrimSpace(text)) {
			return good(strings.ToLower(strings.TrimSpace(text)))
		}
	}
	for _, path := range []string{"/sys/class/dmi/id/product_name", "/sys/class/dmi/id/sys_vendor"} {
		text, err := c.deps.read(path)
		if err != nil {
			continue
		}
		text = strings.ToLower(text)
		for _, name := range []string{"kvm", "vmware", "virtualbox", "qemu", "hyper-v", "xen", "bochs", "amazon", "google"} {
			if strings.Contains(text, name) {
				return good(name)
			}
		}
	}
	if cpuErr != nil {
		return unavailable[string]()
	}
	if strings.Contains(cpu, "hypervisor") {
		return good("vm")
	}
	// "none" means no indicator detected, not a guarantee of bare metal.
	return good("none")
}
func (c *Collector) addresses() (shared.Field[string], shared.Field[string]) {
	v4, v6 := unavailable[string](), unavailable[string]()
	addresses, err := c.deps.addresses()
	if err != nil {
		return v4, v6
	}
	transient := map[string]bool{}
	if text, err := c.deps.read("/proc/net/if_inet6"); err == nil {
		transient = transientIPv6(text)
	}
	score4, score6 := 10, 10
	for _, address := range addresses {
		ip := address.IP
		if !address.Up || skipIface(address.Name) || ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			continue
		}
		score := 0
		if !publicAddress(ip) {
			score = 2
		}
		if transient[ip.String()] {
			score++
		}
		if ip.To4() != nil {
			if score < score4 {
				v4 = good(ip.String())
				score4 = score
			}
		} else if ip.To16() != nil && score < score6 {
			v6 = good(ip.String())
			score6 = score
		}
	}
	return v4, v6
}
func transientIPv6(text string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 {
			continue
		}
		bytes, err := hex.DecodeString(fields[0])
		if err != nil || len(bytes) != 16 {
			continue
		}
		flags, err := strconv.ParseUint(fields[4], 16, 32)
		if err != nil {
			continue
		}
		if flags&(0x01|0x08|0x20|0x40) != 0 {
			out[net.IP(bytes).String()] = true
		}
	}
	return out
}

// The public preference follows the pinned reference's ranges. It does not
// consult an external service and does not claim a NAT guest's exit address.
func publicAddress(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		a, b, c := v4[0], v4[1], v4[2]
		return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && a != 0 && a < 224 && !(a == 100 && b&0xc0 == 64) && !(a == 192 && b == 0 && c == 0) && !(a == 198 && b&0xfe == 18)
	}
	v6 := ip.To16()
	return v6 != nil && v6[0]&0xe0 == 0x20
}
