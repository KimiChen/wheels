// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"math"
)

// 字符串字段长度上限（字节），与 monitor 端的帧/字段限制保持一致。
const (
	MaxHostnameLen = 253
	MaxVersionLen  = 64
	MaxOSLen       = 32
	MaxKernelLen   = 128
	MaxArchLen     = 32
	MaxVirtLen     = 64
	MaxCPUNameLen  = 256
	MaxAddrLen     = 64
	MaxBootIDLen   = 128
	MaxIfaceLen    = 512
)

// Validate 校验 Facts 的必填字段与长度上限。
func (f *Facts) Validate() error {
	if f.Hostname == "" {
		return fmt.Errorf("metrics: hostname 为空")
	}
	if err := checkLen("hostname", f.Hostname, MaxHostnameLen); err != nil {
		return err
	}
	if f.OS == "" || f.Kernel == "" || f.Arch == "" {
		return fmt.Errorf("metrics: os/kernel/arch 不能为空")
	}
	for field, v := range map[string]string{
		"os": f.OS, "kernel": f.Kernel, "arch": f.Arch, "virt": f.Virt,
		"cpu_name": f.CPUName, "agent_version": f.AgentVersion,
		"ipv4": f.IPv4, "ipv6": f.IPv6,
	} {
		limit := MaxVersionLen
		switch field {
		case "kernel":
			limit = MaxKernelLen
		case "virt":
			limit = MaxVirtLen
		case "cpu_name":
			limit = MaxCPUNameLen
		case "ipv4", "ipv6":
			limit = MaxAddrLen
		}
		if err := checkLen(field, v, limit); err != nil {
			return err
		}
	}
	if f.CPUCores < 1 {
		return fmt.Errorf("metrics: cpu_cores 越界：%d", f.CPUCores)
	}
	return nil
}

// Validate 校验 Metrics 的取值范围：拒绝非有限数、越界百分比与非法质量标记。
func (m *Metrics) Validate() error {
	if m.CollectedAt <= 0 {
		return fmt.Errorf("metrics: collected_at 必须为正 Unix 秒：%d", m.CollectedAt)
	}
	if !isFinite(m.CPU) || m.CPU < 0 || m.CPU > 100 {
		return fmt.Errorf("metrics: cpu 越界或非有限：%v", m.CPU)
	}
	for i, l := range m.Load {
		if !isFinite(l) || l < 0 {
			return fmt.Errorf("metrics: load[%d] 越界或非有限：%v", i, l)
		}
	}
	if !isFinite(m.NetRX) || m.NetRX < 0 {
		return fmt.Errorf("metrics: net_rx 越界或非有限：%v", m.NetRX)
	}
	if !isFinite(m.NetTX) || m.NetTX < 0 {
		return fmt.Errorf("metrics: net_tx 越界或非有限：%v", m.NetTX)
	}
	if err := checkLen("boot_id", m.BootID, MaxBootIDLen); err != nil {
		return err
	}
	if err := checkLen("iface", m.Iface, MaxIfaceLen); err != nil {
		return err
	}
	if m.Quality != nil {
		return m.Quality.Validate()
	}
	return nil
}

// Validate 校验质量标记取值合法。
func (q *Quality) Validate() error {
	if q == nil {
		return nil
	}
	for field, v := range map[string]string{
		"cpu": q.CPU, "mem": q.Mem, "swap": q.Swap, "disk": q.Disk,
		"net_rate": q.NetRate, "net_total": q.NetTotal, "sys": q.Sys,
	} {
		switch v {
		case "", QualityOK, QualityUnknown:
		default:
			return fmt.Errorf("metrics: 非法质量标记 %s=%q", field, v)
		}
	}
	return nil
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func checkLen(field, v string, limit int) error {
	if len(v) > limit {
		return fmt.Errorf("metrics: %s 长度 %d 超过上限 %d", field, len(v), limit)
	}
	return nil
}
