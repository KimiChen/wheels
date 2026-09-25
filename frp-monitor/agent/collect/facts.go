// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
)

// Facts 采集节点资产信息：每次连接先报，变化时重报。
//
// 仅关键身份字段失败才返回 error（主机名不可读、ctx 取消）；其余字段
// 缺失时留零值/空串——Facts 没有质量标记，消费方以零值识别缺失。
// AgentVersion 由调用方填写，本函数留空。
func (c *Collector) Facts(ctx context.Context) (*metrics.Facts, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("collect: 读取主机名失败：%w", err)
	}
	f := &metrics.Facts{
		Hostname: hostname,
		OS:       runtime.GOOS,
		Kernel:   unameRelease(),
		Arch:     runtime.GOARCH,
	}
	if name, cores, err := readCPUInfo(c.procPath("cpuinfo")); err == nil {
		f.CPUName, f.CPUCores = name, cores
	} else {
		// 非 Linux 或受限环境没有 cpuinfo：逻辑核数回退到 runtime 观测值。
		f.CPUCores = runtime.NumCPU()
	}
	f.Virt = detectVirt(c.sysRoot)
	if mi, err := readMemInfo(c.procPath("meminfo")); err == nil {
		f.MemTotal, f.SwapTotal = mi.total, mi.swapTotal
	}
	if total, _, err := c.diskUsage(); err == nil {
		f.DiskTotal = total
	}
	if addrs, err := systemIfaceAddrs(); err == nil {
		f.IPv4 = selectAddr(addrs, false, c.keepIface)
		f.IPv6 = selectAddr(addrs, true, c.keepIface)
	}
	return f, nil
}

// virtKeywords 为虚拟化标识关键字，按优先级匹配，命中返回规范小写名。
// microsoft 即 Hyper-V。
var virtKeywords = []string{
	"kvm", "qemu", "vmware", "virtualbox", "microsoft", "xen",
	"amazon", "google", "bochs",
}

// detectVirt 读取 sysRoot/class/dmi/id 下的 DMI 信息判断虚拟化环境：
// 先 product_name，再以 sys_vendor 佐证；物理机或无法判断时返回 ""。
func detectVirt(sysRoot string) string {
	for _, name := range []string{"product_name", "sys_vendor"} {
		raw, err := os.ReadFile(filepath.Join(sysRoot, "class", "dmi", "id", name))
		if err != nil {
			continue
		}
		if v := matchVirt(string(raw)); v != "" {
			return v
		}
	}
	return ""
}

// matchVirt 为纯函数：DMI 文本小写后的子串匹配，命中返回规范小写名。
func matchVirt(dmi string) string {
	s := strings.ToLower(dmi)
	for _, kw := range virtKeywords {
		if strings.Contains(s, kw) {
			return kw
		}
	}
	return ""
}
