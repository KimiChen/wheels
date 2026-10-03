//go:build !linux

// 本文件复制自上游 sing-box 的 cmd/sing-box/cmd_run_userns_other.go。
//
// 复制而非 import：github.com/sagernet/sing-box/cmd/sing-box 是 package main，Go 禁止导入；
// 而本项目必须保持 run / check / format / version 的 argv 与退出码与上游兼容（README §4.7）。
//
// 来源：github.com/SagerNet/sing-box@af6e64c3b69e6132ebaee0e1a3d24e93903f6709（v1.14.2）
// 复制日期：2026-10-03
// 本项目修改：无（仅加本注释头）
//
// 本文件是 GPLv3 衍生物，义务见本子目录的 LICENSE 与 THIRD_PARTY_NOTICES.md。

package main

import "github.com/sagernet/sing-box/option"

func runInUserNamespaceIfNeeded(options option.Options, optionsList []*OptionsEntry) error {
	return nil
}
