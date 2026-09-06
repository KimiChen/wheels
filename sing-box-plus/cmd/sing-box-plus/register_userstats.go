//go:build with_user_stats

package main

import (
	boxService "github.com/sagernet/sing-box/adapter/service"

	"sing-box-plus/internal/userstats"
)

// statsRegistered 供原始配置预扫描判断：未编译本 tag 时，配置里出现 user_stats
// 必须给出可读错误，而不是让上游把 service 误写成 "inbound" 的解码失败文案透出来
// （README §4.6 第 3 条）。
const statsRegistered = true

// newServiceRegistry 自建 service registry 并**不注册** ssmapi。
//
// ssm-api 在上游 include/registry.go:148 无条件注册，D4 的 tag 裁剪排除不掉它；
// 只有自建 registry 才能让含 ssm-api 的配置在解析期即失败关闭（§4.6 第 8 条第一层）。
func newServiceRegistry() *boxService.Registry {
	registry := boxService.NewRegistry()
	userstats.RegisterService(registry)
	return registry
}
