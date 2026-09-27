package collect

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type ifaceFilter struct {
	spec          string
	only, exclude map[string]bool
}

func validIface(name string) bool {
	return name != "" && len(name) <= 15 && !strings.ContainsAny(name, "\x00/\\:") && !strings.HasPrefix(name, "-") && !strings.ContainsFunc(name, unicode.IsSpace) && name != "." && name != ".."
}
func parseIface(spec string) (ifaceFilter, error) {
	out := ifaceFilter{only: map[string]bool{}, exclude: map[string]bool{}}
	if len(spec) > shared.MaxStringBytes {
		return out, errors.New("interface specification is too long")
	}
	parts := make([]string, 0)
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name := entry
		list := out.only
		if strings.HasPrefix(entry, "-") {
			name = entry[1:]
			list = out.exclude
		}
		if !validIface(name) {
			return out, errors.New("invalid interface name; use comma-separated full names")
		}
		list[name] = true
		parts = append(parts, entry)
	}
	out.spec = strings.Join(parts, ",")
	return out, nil
}

var skippedInterfaces = strings.Fields("lo docker veth br- virbr tap tun wg tailscale cni flannel podman fwbr fwpr fwln ifb gretap erspan kube cali nerdctl lxc cilium zt")

func skipIface(name string) bool {
	for _, prefix := range skippedInterfaces {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
func stackedIface(name string) bool {
	if strings.Contains(name, ".") {
		return true
	}
	for _, prefix := range []string{"bond", "br", "vlan", "vmbr", "pppoe-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
func (c *Collector) counts(name string) bool {
	if c.filter.exclude[name] {
		return false
	}
	if len(c.filter.only) > 0 {
		return c.filter.only[name]
	}
	if skipIface(name) || stackedIface(name) {
		return false
	}
	dir := filepath.Join("/sys/class/net", name)
	entries, _ := c.deps.readDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry, "lower_") {
			return false
		}
	}
	uevent, _ := c.deps.read(filepath.Join(dir, "uevent"))
	devtype := func(values ...string) bool {
		for _, line := range strings.Split(uevent, "\n") {
			for _, v := range values {
				if line == "DEVTYPE="+v {
					return true
				}
			}
		}
		return false
	}
	if devtype("bridge", "bond") {
		return false
	}
	if hardware, _ := c.deps.exists(filepath.Join(dir, "device")); hardware {
		return true
	}
	if bridge, _ := c.deps.exists(filepath.Join(dir, "brport")); bridge {
		return false
	}
	kind, _ := c.deps.read(filepath.Join(dir, "type"))
	switch strings.TrimSpace(kind) {
	case "65534", "768", "769", "776", "778", "823":
		return false
	}
	return !devtype("vxlan", "geneve")
}
func (c *Collector) readNetwork(text string) (map[string]counters, error) {
	lines := strings.Split(text, "\n")
	if len(lines) < 3 {
		return nil, errMalformed
	}
	out := map[string]counters{}
	seen := map[string]bool{}
	for _, line := range lines[2:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || !validIface(name) || seen[name] {
			return nil, errMalformed
		}
		seen[name] = true
		if len(seen) > maxInterfaces {
			return nil, errMalformed
		}
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			return nil, errMalformed
		}
		rx, e1 := strconv.ParseUint(fields[0], 10, 64)
		tx, e2 := strconv.ParseUint(fields[8], 10, 64)
		if e1 != nil || e2 != nil {
			return nil, errMalformed
		}
		if c.counts(name) {
			out[name] = counters{rx, tx}
		}
	}
	return out, nil
}
func epoch(boot string, values map[string]counters) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strings.Join(names, "\n")))
	return fmt.Sprintf("%s/%016x", boot, hash.Sum64())
}
func (c *Collector) networkMetrics(m *shared.Metrics) {
	m.Iface = good(c.config.Iface)
	bad := unavailable[uint64]()
	m.NetRX, m.NetTX, m.NetRXTotal, m.NetTXTotal = bad, bad, bad, bad
	m.BootID = unavailable[string]()
	text, err := c.deps.read("/proc/net/dev")
	if err != nil {
		return
	}
	values, err := c.readNetwork(text)
	if err != nil {
		return
	}
	boot, err := c.deps.read("/proc/sys/kernel/random/boot_id")
	boot = strings.TrimSpace(boot)
	if err != nil || !validText(boot) || len(boot) > shared.MaxStringBytes-17 {
		return
	}
	current := netReading{values: values, epoch: epoch(boot, values), at: c.deps.now()}
	previous := c.network
	// Never replace a valid clock baseline with a non-monotonic injected reading.
	if previous != nil && !current.at.After(previous.at) {
		return
	}
	c.network = &current
	m.BootID = good(current.epoch)
	var rx, tx uint64
	for _, value := range values {
		rx = satAdd(rx, value.rx)
		tx = satAdd(tx, value.tx)
	}
	m.NetRXTotal, m.NetTXTotal = good(rx), good(tx)
	reason := ""
	if previous == nil {
		reason = "no_baseline"
	} else if previous.epoch != current.epoch {
		reason = "scope_changed"
	}
	if reason == "" {
		for name, value := range values {
			before := previous.values[name]
			if value.rx < before.rx || value.tx < before.tx {
				reason = "counter_reset"
				break
			}
		}
	}
	if reason != "" {
		m.NetRX, m.NetTX = warming[uint64](reason), warming[uint64](reason)
		return
	}
	rx, tx = 0, 0
	for name, value := range values {
		before := previous.values[name]
		rx = satAdd(rx, value.rx-before.rx)
		tx = satAdd(tx, value.tx-before.tx)
	}
	seconds := current.at.Sub(previous.at).Seconds()
	m.NetRX, m.NetTX = good(rate(rx, seconds)), good(rate(tx, seconds))
}
func rate(value uint64, seconds float64) uint64 {
	result := float64(value) / seconds
	if result >= float64(math.MaxUint64) {
		return math.MaxUint64
	}
	return uint64(result)
}
