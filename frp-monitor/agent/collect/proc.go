// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// memInfo 为 /proc/meminfo 的解析结果，单位已换算为字节。
// seen 标记区分「字段缺失」与「字段为 0」：MemAvailable 为 0 是有效数据，
// 不能回退也不能标未知（README §3）。
type memInfo struct {
	total     uint64
	available uint64
	free      uint64
	buffers   uint64
	cached    uint64
	swapTotal uint64
	swapFree  uint64

	seenTotal     bool
	seenAvailable bool
	seenFree      bool
	seenBuffers   bool
	seenCached    bool
	seenSwapTotal bool
	seenSwapFree  bool
}

// readMemInfo 解析 /proc/meminfo，只读取采集所需的 kB 字段。
func readMemInfo(path string) (memInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return memInfo{}, err
	}
	defer f.Close()

	var mi memInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return memInfo{}, fmt.Errorf("collect: 解析 meminfo 行 %q 失败：%w", sc.Text(), err)
		}
		v *= 1024 // kB → 字节
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			mi.total, mi.seenTotal = v, true
		case "MemAvailable":
			mi.available, mi.seenAvailable = v, true
		case "MemFree":
			mi.free, mi.seenFree = v, true
		case "Buffers":
			mi.buffers, mi.seenBuffers = v, true
		case "Cached":
			mi.cached, mi.seenCached = v, true
		case "SwapTotal":
			mi.swapTotal, mi.seenSwapTotal = v, true
		case "SwapFree":
			mi.swapFree, mi.seenSwapFree = v, true
		}
	}
	if err := sc.Err(); err != nil {
		return memInfo{}, err
	}
	return mi, nil
}

// memAvailable 返回可用内存：优先 MemAvailable；仅当该字段缺失时回退
// MemFree + Buffers + Cached（三者齐全回退才有效）。
func (mi memInfo) memAvailable() (uint64, bool) {
	if mi.seenAvailable {
		return mi.available, true
	}
	if mi.seenFree && mi.seenBuffers && mi.seenCached {
		return mi.free + mi.buffers + mi.cached, true
	}
	return 0, false
}

// cpuSample 为 /proc/stat 首行 cpu 的累计时间（单位 jiffies）。
// idle 已并入 iowait；total 不含 guest/guest_nice（已计入 user/nice，
// 不重复相加，README §3）。
type cpuSample struct {
	idle  uint64
	total uint64
}

// readCPUSample 读取 /proc/stat 的整机 cpu 行（"cpu " 前缀，不含逐核行）。
func readCPUSample(path string) (cpuSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return cpuSample{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		// 至少需要 user nice system idle iowait 五列。
		if len(fields) < 5 {
			return cpuSample{}, fmt.Errorf("collect: stat 的 cpu 行列数不足：%q", line)
		}
		var v [8]uint64 // user nice system idle iowait irq softirq steal
		for i := 0; i < len(v) && i < len(fields); i++ {
			v[i], err = strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return cpuSample{}, fmt.Errorf("collect: 解析 stat 的 cpu 行失败：%w", err)
			}
		}
		s := cpuSample{idle: v[3] + v[4]}
		for _, x := range v {
			s.total += x
		}
		return s, nil
	}
	if err := sc.Err(); err != nil {
		return cpuSample{}, err
	}
	return cpuSample{}, fmt.Errorf("collect: %s 缺少整机 cpu 行", path)
}

// readLoadavg 解析 /proc/loadavg：前三列为 1/5/15 分钟负载，
// 第四列 running/total 的 total 为进程数。
func readLoadavg(path string) (load [3]float64, procs uint64, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return load, 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 4 {
		return load, 0, fmt.Errorf("collect: loadavg 列数不足：%q", strings.TrimSpace(string(raw)))
	}
	for i := 0; i < 3; i++ {
		load[i], err = strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return load, 0, fmt.Errorf("collect: 解析 loadavg 失败：%w", err)
		}
	}
	_, total, found := strings.Cut(fields[3], "/")
	if !found {
		return load, 0, fmt.Errorf("collect: loadavg 缺少 running/total 列：%q", fields[3])
	}
	procs, err = strconv.ParseUint(total, 10, 64)
	if err != nil {
		return load, 0, fmt.Errorf("collect: 解析 loadavg 进程数失败：%w", err)
	}
	return load, procs, nil
}

// readUptime 解析 /proc/uptime，取第一段并截断为秒。
func readUptime(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 1 {
		return 0, fmt.Errorf("collect: uptime 为空：%s", path)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("collect: 解析 uptime 失败：%q", fields[0])
	}
	return uint64(v), nil
}

// readSockstat 汇总 socket 计数：TCP = IPv4 inuse + tw + IPv6 inuse
// （含 TIME_WAIT，不是 established）；UDP = 两族 inuse 之和（README §3）。
// sockstat6 缺失（内核未启用 IPv6）按 0 计，不视为失败。
func readSockstat(path4, path6 string) (tcp, udp uint64, err error) {
	s4, err := readSockstatFile(path4)
	if err != nil {
		return 0, 0, err
	}
	s6, err := readSockstatFile(path6)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, 0, err
		}
		s6 = nil
	}
	tcp = s4["TCP"]["inuse"] + s4["TCP"]["tw"] + s6["TCP6"]["inuse"]
	udp = s4["UDP"]["inuse"] + s6["UDP6"]["inuse"]
	return tcp, udp, nil
}

// readSockstatFile 解析 sockstat 格式：每行「协议: 键 值 键 值 ...」。
func readSockstatFile(path string) (map[string]map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]map[string]uint64)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		label := strings.TrimSuffix(fields[0], ":")
		m := out[label]
		if m == nil {
			m = make(map[string]uint64)
			out[label] = m
		}
		for i := 1; i+1 < len(fields); i += 2 {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("collect: 解析 sockstat 行 %q 失败：%w", sc.Text(), err)
			}
			m[fields[i]] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ifaceCounters 为单个网卡的内核 lifetime 字节计数器。
type ifaceCounters struct {
	name string
	rx   uint64
	tx   uint64
}

// readNetDev 解析 /proc/net/dev，返回全部接口的收发字节计数器
// （不过滤；接口筛选在汇总时进行）。
func readNetDev(path string) ([]ifaceCounters, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []ifaceCounters
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "|") {
			continue // 两行表头
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue // 空行等
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			return nil, fmt.Errorf("collect: net/dev 行列数不足：%q", line)
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("collect: 解析 net/dev 接收计数失败：%w", err)
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("collect: 解析 net/dev 发送计数失败：%w", err)
		}
		out = append(out, ifaceCounters{name: strings.TrimSpace(line[:idx]), rx: rx, tx: tx})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// readCPUInfo 解析 /proc/cpuinfo：CPUCores 为逻辑处理器数
// （数 processor 条目），CPUName 取首个 model name。
func readCPUInfo(path string) (name string, cores int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		switch strings.TrimSpace(line[:idx]) {
		case "processor":
			cores++
		case "model name":
			if name == "" {
				name = strings.TrimSpace(line[idx+1:])
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", 0, err
	}
	if cores == 0 {
		return "", 0, fmt.Errorf("collect: %s 缺少 processor 条目", path)
	}
	return name, cores, nil
}

// readBootID 读取内核 boot ID（/proc/sys/kernel/random/boot_id）。
func readBootID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("collect: boot_id 为空：%s", path)
	}
	return id, nil
}

// mountEntry 为 /proc/self/mounts 的单条挂载记录。
type mountEntry struct {
	source string
	target string
	fstype string
}

// mountUnescape 还原挂载表路径中的八进制转义（空格、制表、换行、反斜杠）。
var mountUnescape = strings.NewReplacer(
	`\040`, " ",
	`\011`, "\t",
	`\012`, "\n",
	`\134`, `\`,
)

// readMounts 解析 /proc/self/mounts。
func readMounts(path string) ([]mountEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []mountEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("collect: mounts 行列数不足：%q", sc.Text())
		}
		out = append(out, mountEntry{
			source: fields[0],
			target: mountUnescape.Replace(fields[1]),
			fstype: fields[2],
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
