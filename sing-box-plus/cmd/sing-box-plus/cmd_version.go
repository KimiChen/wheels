// 本文件复制自上游 sing-box 的 cmd/sing-box/cmd_version.go。
//
// 复制而非 import：github.com/sagernet/sing-box/cmd/sing-box 是 package main，Go 禁止导入；
// 而本项目必须保持 run / check / format / version 的 argv 与退出码与上游兼容（README §4.7）。
//
// 来源：github.com/SagerNet/sing-box@0b8995879f29a9b98ee027bc17b75e101445b238（v1.14.0）
// 复制日期：2026-09-06
// 本项目修改：输出改为 README §4.7 规定的固定四行。
//   上游 commit 必须由本项目自己的 -X 变量携带，不能指望 debug.ReadBuildInfo()——
//   vcs.revision 指向本仓库自身的 commit，且 -buildvcs=false 会把该字段整个抹掉。
//
// 本文件是 GPLv3 衍生物，义务见本子目录的 LICENSE 与 THIRD_PARTY_NOTICES.md。

package main

import (
	"os"
	"runtime"
	"runtime/debug"

	C "github.com/sagernet/sing-box/constant"

	"github.com/spf13/cobra"
)

// 三个变量由构建脚本以 -X 注入；未注入时显示 unknown，使「忘了注入」可见而不是伪装成正常。
var (
	Version        = "unknown"
	UpstreamCommit = "unknown"
)

var commandVersion = &cobra.Command{
	Use:   "version",
	Short: "Print current version of sing-box-plus",
	Run:   printVersion,
	Args:  cobra.NoArgs,
}

var nameOnly bool

func init() {
	commandVersion.Flags().BoolVarP(&nameOnly, "name", "n", false, "print version name only")
	mainCommand.AddCommand(commandVersion)
}

func shortCommit(commit string) string {
	if len(commit) >= 12 {
		return commit[:12]
	}
	return commit
}

func printVersion(cmd *cobra.Command, args []string) {
	if nameOnly {
		os.Stdout.WriteString(Version + "\n")
		return
	}
	var tags string
	if debugInfo, loaded := debug.ReadBuildInfo(); loaded {
		for _, setting := range debugInfo.Settings {
			if setting.Key == "-tags" {
				tags = setting.Value
			}
		}
	}
	// C.Version 由构建脚本从 upstream.lock 读取后以跨 module 的 -X 注入；
	// wrapper module 里没有 sing-box 的 git 树，不注入时它会打印 unknown（README §4.7）。
	output := "sing-box-plus version " + Version + "\n" +
		"Upstream: sing-box " + C.Version + " (" + shortCommit(UpstreamCommit) + ")\n" +
		"Environment: " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + "\n" +
		"Tags: " + tags + "\n"
	os.Stdout.WriteString(output)
}
