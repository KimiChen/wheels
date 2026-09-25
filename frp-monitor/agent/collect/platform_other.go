// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package collect

import "errors"

// errUnsupportedPlatform 表示当前平台没有 /proc/statfs 实现。
// 采集器首版仅支持 Linux（README §3）；其他平台上内核版本与磁盘组
// 不可用，相关组按 unknown 降级，不影响其余采集。
var errUnsupportedPlatform = errors.New("collect: 当前平台不支持（仅 Linux）")

// unameRelease 在非 Linux 平台无法提供内核版本串，返回占位值
// （Facts.Validate 只要求非空；采集器目标平台为 Linux）。
func unameRelease() string { return "unknown" }

// defaultMountStat 在非 Linux 平台恒失败：disk 组降级为 unknown。
// 测试可通过注入 statfs 覆盖。
func defaultMountStat(string) (mountStat, error) {
	return mountStat{}, errUnsupportedPlatform
}
