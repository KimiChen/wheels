//go:build !with_user_stats

package main

import boxService "github.com/sagernet/sing-box/adapter/service"

const statsRegistered = false

// 未编译 with_user_stats 时 service registry 为空：既不注册 user_stats，也不注册 ssmapi。
// 含任一类型的配置都会在解析期失败，由 userstats.ScanRawConfig 给出可读错误。
func newServiceRegistry() *boxService.Registry {
	return boxService.NewRegistry()
}
