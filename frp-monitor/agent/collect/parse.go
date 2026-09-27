package collect

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

var errMalformed = errors.New("malformed kernel sample")

func parseCPU(text string) (cpuReading, error) {
	line, _, _ := strings.Cut(text, "\n")
	columns := strings.Fields(line)
	if len(columns) < 6 || columns[0] != "cpu" {
		return cpuReading{}, errMalformed
	}
	values := make([]uint64, 0, 8)
	for _, column := range columns[1:] {
		if len(values) == 8 {
			break
		}
		n, err := strconv.ParseUint(column, 10, 64)
		if err != nil {
			return cpuReading{}, errMalformed
		}
		values = append(values, n)
	}
	var total uint64
	for _, n := range values {
		if math.MaxUint64-total < n {
			return cpuReading{}, errMalformed
		}
		total += n
	}
	return cpuReading{total: total, idle: values[3] + values[4]}, nil
}
func memoryMetrics(text string, err error) (shared.Field[uint64], shared.Field[uint64], shared.Field[uint64], shared.Field[uint64]) {
	bad := unavailable[uint64]()
	if err != nil {
		return bad, bad, bad, bad
	}
	values := map[string]uint64{}
	broken := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "MemTotal", "MemAvailable", "MemFree", "Buffers", "Cached", "SwapTotal", "SwapFree":
		default:
			continue
		}
		if _, seen := values[key]; seen {
			broken[key] = true
			continue
		}
		columns := strings.Fields(rest)
		if len(columns) != 2 || columns[1] != "kB" {
			broken[key] = true
			continue
		}
		n, e := strconv.ParseUint(columns[0], 10, 64)
		if e != nil || n > math.MaxUint64/1024 {
			broken[key] = true
			continue
		}
		values[key] = n * 1024
	}
	value := func(key string) (uint64, bool) { n, ok := values[key]; return n, ok && !broken[key] }
	mt, mu, st, su := bad, bad, bad, bad
	if total, ok := value("MemTotal"); ok && total > 0 {
		mt = good(total)
		available, ok := value("MemAvailable")
		if _, present := values["MemAvailable"]; !present && !broken["MemAvailable"] {
			free, fok := value("MemFree")
			buffers, bok := value("Buffers")
			cached, cok := value("Cached")
			available = satAdd(satAdd(free, buffers), cached)
			ok = fok && bok && cok
		}
		if ok {
			mu = good(satSub(total, available))
		}
	}
	if total, ok := value("SwapTotal"); ok {
		st = good(total)
		if free, ok := value("SwapFree"); ok {
			su = good(satSub(total, free))
		}
	}
	return mt, mu, st, su
}
func loadMetric(text string, err error) shared.Field[[]float64] {
	fields := strings.Fields(text)
	if err != nil || len(fields) < 3 {
		return unavailable[[]float64]()
	}
	out := make([]float64, 3)
	for i := range out {
		n, e := strconv.ParseFloat(fields[i], 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return unavailable[[]float64]()
		}
		out[i] = n
	}
	return good(out)
}
func uptimeMetric(text string, err error) shared.Field[uint64] {
	fields := strings.Fields(text)
	if err != nil || len(fields) == 0 {
		return unavailable[uint64]()
	}
	n, e := strconv.ParseFloat(fields[0], 64)
	if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n >= float64(math.MaxUint64) {
		return unavailable[uint64]()
	}
	return good(uint64(n))
}

var skippedFilesystems = strings.Fields("tmpfs devtmpfs proc sysfs cgroup cgroup2 devpts mqueue hugetlbfs debugfs tracefs securityfs pstore bpf configfs fusectl binfmt_misc autofs squashfs ramfs efivarfs nsfs overlay ecryptfs fuse rpc_pipefs nfs nfs4 cifs smb3 ceph glusterfs 9p")

func skipFilesystem(kind string) bool {
	for _, skip := range skippedFilesystems {
		if kind == skip || strings.HasPrefix(kind, skip+".") {
			return true
		}
	}
	return false
}
func decodeMount(value string) string {
	return strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\").Replace(value)
}
func parseMounts(text string) ([]string, error) {
	type row struct{ source, point, kind string }
	rows := make([]row, 0)
	last := map[string]int{}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, errMalformed
		}
		source, point := decodeMount(fields[0]), decodeMount(fields[1])
		if !strings.HasPrefix(point, "/") || strings.ContainsRune(point, 0) {
			return nil, errMalformed
		}
		last[point] = len(rows)
		rows = append(rows, row{source, point, fields[2]})
		if len(rows) > maxMounts {
			return nil, errMalformed
		}
	}
	out := make([]string, 0)
	seen := map[string]bool{}
	for i, row := range rows {
		if last[row.point] != i || skipFilesystem(row.kind) {
			continue
		}
		if !strings.HasPrefix(row.source, "/") && row.kind != "zfs" && row.kind != "btrfs" {
			continue
		}
		key := row.source
		if row.kind == "zfs" {
			key, _, _ = strings.Cut(key, "/")
			key = "zfs:" + key
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, row.point)
	}
	return out, nil
}
func parseSockets(v4, v6 string, hasV6 bool) (uint64, uint64, error) {
	get := func(text, prefix, key string) (uint64, error) {
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != prefix {
				continue
			}
			for i := 1; i+1 < len(fields); i += 2 {
				if fields[i] == key {
					return strconv.ParseUint(fields[i+1], 10, 64)
				}
			}
		}
		return 0, errMalformed
	}
	tcp, err := get(v4, "TCP:", "inuse")
	if err != nil {
		return 0, 0, err
	}
	tw, err := get(v4, "TCP:", "tw")
	if err != nil {
		return 0, 0, err
	}
	tcp = satAdd(tcp, tw)
	udp, err := get(v4, "UDP:", "inuse")
	if err != nil {
		return 0, 0, err
	}
	if hasV6 {
		t, e := get(v6, "TCP6:", "inuse")
		if e != nil {
			return 0, 0, e
		}
		u, e := get(v6, "UDP6:", "inuse")
		if e != nil {
			return 0, 0, e
		}
		tcp = satAdd(tcp, t)
		udp = satAdd(udp, u)
	}
	return tcp, udp, nil
}
