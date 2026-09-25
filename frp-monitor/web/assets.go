// SPDX-License-Identifier: Apache-2.0

// Package web 提供 frp-monitor 的监控页面静态资源，随 monitor 二进制嵌入发布。
// 页面与权限设计见 frp-monitor/README.md §7。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var staticFS embed.FS

// Static 返回 static 子树：根下为 index.html / node.html / admin.html、
// assets/（web-standard-kit 快照与业务样式）和 src/（原生 ES Modules）。
func Static() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
