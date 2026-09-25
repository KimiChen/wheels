// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"errors"
	"math"
	"strings"
)

// errNoValidMount 表示过滤后没有任何有效本地挂载。容器等受限命名空间
// 中常见（只挂 overlay/tmpfs），disk 组按 unknown 上报并在页面标注
// 采集范围（README §3 的容器标注要求），不能伪造为 0。
var errNoValidMount = errors.New("collect: 没有有效本地文件系统挂载")

// mountStat 为单个挂载点的设备号与用量，字段已按平台归一化：
// bsize 取 f_frsize > 0 ? f_frsize : f_bsize（README §3）。
type mountStat struct {
	dev    uint64
	blocks uint64
	bfree  uint64
	bsize  uint64
}

// pseudoFSTypes 为伪文件系统：不计入磁盘用量。
var pseudoFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"tmpfs": true, "cgroup": true, "cgroup2": true, "overlay": true,
	"squashfs": true, "ramfs": true, "autofs": true, "mqueue": true,
	"debugfs": true, "tracefs": true, "fusectl": true, "configfs": true,
	"securityfs": true, "pstore": true, "bpf": true, "nsfs": true,
	"shm": true, "binfmt_misc": true,
}

// remoteFSTypes 为远程/网络文件系统：用量不代表本机磁盘，不计入。
// fuse. 前缀（fuse.sshfs 等）在 filteredFSType 中单独处理。
var remoteFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true,
	"sshfs": true, "9p": true, "ceph": true, "glusterfs": true,
}

// filteredFSType 报告文件系统类型是否被过滤（伪文件系统或远程/网络文件系统）。
func filteredFSType(fstype string) bool {
	if pseudoFSTypes[fstype] || remoteFSTypes[fstype] {
		return true
	}
	return strings.HasPrefix(fstype, "fuse.")
}

// diskUsage 汇总有效本地文件系统用量：过滤伪/远程文件系统，按挂载点
// st_dev 去重（同一设备的重复挂载、bind 挂载只计第一次出现），
// total += blocks × bsize，used += max(blocks − bfree, 0) × bsize，
// 全程 uint64 饱和运算，不减 Bavail（README §3）。单个挂载点不可达
// （已卸载、权限受限）只跳过该挂载；没有任何有效挂载时返回
// errNoValidMount。
func (c *Collector) diskUsage() (total, used uint64, err error) {
	mounts, err := readMounts(c.procPath("self", "mounts"))
	if err != nil {
		return 0, 0, err
	}
	seen := make(map[uint64]struct{})
	for _, m := range mounts {
		if filteredFSType(m.fstype) {
			continue
		}
		st, err := c.statfs(m.target)
		if err != nil {
			continue
		}
		if _, dup := seen[st.dev]; dup {
			continue
		}
		seen[st.dev] = struct{}{}
		total = satAdd(total, satMul(st.blocks, st.bsize))
		used = satAdd(used, satMul(satSub(st.blocks, st.bfree), st.bsize))
	}
	if len(seen) == 0 {
		return 0, 0, errNoValidMount
	}
	return total, used, nil
}

// satAdd/satMul/satSub 为 uint64 饱和运算：溢出或下溢时钳制到边界值，
// 不回绕（异常计数器不能产生误导性的小读数）。
func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

func satMul(a, b uint64) uint64 {
	if a != 0 && b > math.MaxUint64/a {
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
