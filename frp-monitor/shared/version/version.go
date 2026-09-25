// SPDX-License-Identifier: Apache-2.0

// Package version 定义 frp-monitor 自身版本，与 FRP 基线版本分开展示。
package version

import (
	frpversion "github.com/fatedier/frp/pkg/util/version"
)

// Version 为 frp-monitor 扩展版本。
const Version = "0.1.0-dev"

// 扩展接线（frpmonitor build tag）激活时，本包被 agent/monitor service 引用，
// init 将自身版本附加到 frp 版本输出（如 "0.71.0 frp-monitor/0.1.0-dev"）。
// 未接线时本包不被引用，FRP 版本输出保持原生。
func init() {
	frpversion.Suffix = " frp-monitor/" + Version
}
