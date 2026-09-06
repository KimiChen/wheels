// 本文件复制自上游 sing-box 的 cmd/sing-box/cmd_check.go。
//
// 复制而非 import：github.com/sagernet/sing-box/cmd/sing-box 是 package main，Go 禁止导入；
// 而本项目必须保持 run / check / format / version 的 argv 与退出码与上游兼容（README §4.7）。
//
// 来源：github.com/SagerNet/sing-box@0b8995879f29a9b98ee027bc17b75e101445b238（v1.14.0）
// 复制日期：2026-09-06
// 本项目修改：
//   1. check() 在 box.New() 之前先跑 userstats.Validate —— 上游 sing-box check 不能当作配置
//      门禁的唯一实现（README §4.6 第 10 条），全部校验必须独立完成；
//   2. SIGHUP 重载路径经过本函数，因此在此比对 §4.6 第 11 条的重载不变量：不一致即返回错误，
//      由上游 run 循环记错误后 continue —— 拒绝本次重载、保留旧 Box 继续服务，
//      不得以进程退出处置，否则反而会丢掉未采集的尾账；
//   3. 独立 check 子命令没有进程级 registry，注入一个只用于校验的 registry，
//      使 box.New 能构造 user_stats 实例（该实例永不 Start、永不绑定 socket）。
//
// 本文件是 GPLv3 衍生物，义务见本子目录的 LICENSE 与 THIRD_PARTY_NOTICES.md。

package main

import (
	"context"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/service"

	"sing-box-plus/internal/userstats"

	"github.com/spf13/cobra"
)

var commandCheck = &cobra.Command{
	Use:   "check",
	Short: "Check configuration",
	Run: func(cmd *cobra.Command, args []string) {
		err := check()
		if err != nil {
			log.Fatal(err)
		}
	},
	Args: cobra.NoArgs,
}

func init() {
	mainCommand.AddCommand(commandCheck)
}

func check() error {
	options, err := readConfigAndMerge()
	if err != nil {
		return err
	}
	config, err := userstats.Validate(options)
	if err != nil {
		return err
	}
	if err = userstats.CheckReloadInvariant(statsConfig, config); err != nil {
		return err
	}
	if statsConfig != nil && config == nil {
		return errStatsRemoved
	}
	checkCtx := globalCtx
	if statsRegistry == nil && config != nil {
		checkCtx = service.ContextWith(checkCtx, userstats.NewValidationRegistry())
	}
	ctx, cancel := context.WithCancel(checkCtx)
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: options,
	})
	if err == nil {
		instance.Close()
	}
	cancel()
	return err
}
