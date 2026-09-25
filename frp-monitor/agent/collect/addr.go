// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"net"
	"net/netip"
)

// ifaceAddr 为接口名与其地址的组合，是地址选择纯函数的输入。
type ifaceAddr struct {
	iface string
	ip    netip.Addr
}

// selectAddr 为单族（want6 区分 IPv4/IPv6）选择一个本机地址：
// 公网优先（全局单播且非私网），没有公网地址时回退到私网/ULA；
// 被过滤网卡与回环、链路本地、组播等非全局单播地址不参与选择；
// 没有候选时返回 ""。本机地址不等于 NAT 出口地址（README §3）。
func selectAddr(addrs []ifaceAddr, want6 bool, keep func(string) bool) string {
	var fallback string
	for _, a := range addrs {
		ip := a.ip.Unmap()
		if ip.Is6() != want6 || !keep(a.iface) || !ip.IsGlobalUnicast() {
			continue
		}
		if !ip.IsPrivate() {
			return ip.String()
		}
		if fallback == "" {
			fallback = ip.String()
		}
	}
	return fallback
}

// systemIfaceAddrs 枚举本机各接口的地址（生产路径；测试直接构造
// ifaceAddr 列表驱动 selectAddr）。
func systemIfaceAddrs() ([]ifaceAddr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []ifaceAddr
	for _, ifi := range ifs {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip netip.Addr
			var ok bool
			switch v := a.(type) {
			case *net.IPNet:
				ip, ok = netip.AddrFromSlice(v.IP)
			case *net.IPAddr:
				ip, ok = netip.AddrFromSlice(v.IP)
			}
			if ok {
				out = append(out, ifaceAddr{iface: ifi.Name, ip: ip})
			}
		}
	}
	return out, nil
}
