// SPDX-License-Identifier: Apache-2.0

//go:build linux

package collect

import (
	"fmt"
	"syscall"
)

// unameRelease 返回内核版本串（uname -r）。
func unameRelease() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "unknown"
	}
	return charsToString(u.Release)
}

// charsToString 将 syscall 定长字符数组转为字符串（截断于第一个 NUL）。
// linux/amd64 与 arm64 的 Utsname 字段均为 [65]int8。
func charsToString(arr [65]int8) string {
	b := make([]byte, 0, len(arr))
	for _, c := range arr {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// defaultMountStat 通过 syscall.Stat + Statfs 读取挂载点的设备号与用量。
// 块大小归一化：bs = f_frsize > 0 ? f_frsize : f_bsize（README §3）。
func defaultMountStat(path string) (mountStat, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return mountStat{}, err
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return mountStat{}, err
	}
	bs := fs.Bsize
	if fs.Frsize > 0 {
		bs = fs.Frsize
	}
	if bs <= 0 {
		return mountStat{}, fmt.Errorf("collect: %s 块大小非法：%d", path, bs)
	}
	return mountStat{
		dev:    uint64(st.Dev),
		blocks: fs.Blocks,
		bfree:  fs.Bfree,
		bsize:  uint64(bs),
	}, nil
}
