// 本文件复制自上游 sing-box 的 cmd/sing-box/cmd.go。
//
// 复制而非 import：github.com/sagernet/sing-box/cmd/sing-box 是 package main，Go 禁止导入；
// 而本项目必须保持 run / check / format / version 的 argv 与退出码与上游兼容（README §4.7）。
//
// 来源：github.com/SagerNet/sing-box@0b8995879f29a9b98ee027bc17b75e101445b238（v1.14.0）
// 复制日期：2026-09-06
// 本项目修改：
//   1. mainCommand.Use 改为 sing-box-plus；
//   2. preRun 末行的 include.Context() 换成 box.Context() + 自有最小 registry 与自有
//      service registry（不注册 ssmapi），这是 §4.6 第 8 条第一层与 §6 registry 裁剪的落点。
//
// 本文件是 GPLv3 衍生物，义务见本子目录的 LICENSE 与 THIRD_PARTY_NOTICES.md。

package main

import (
	"context"
	"os"
	"os/user"
	"strconv"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/experimental/deprecated"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"

	"sing-box-plus/internal/minreg"

	"github.com/spf13/cobra"
)

var (
	globalCtx         context.Context
	configPaths       []string
	configDirectories []string
	workingDir        string
	disableColor      bool
)

var mainCommand = &cobra.Command{
	Use:              "sing-box-plus",
	PersistentPreRun: preRun,
}

func init() {
	mainCommand.PersistentFlags().StringArrayVarP(&configPaths, "config", "c", nil, "set configuration file path")
	mainCommand.PersistentFlags().StringArrayVarP(&configDirectories, "config-directory", "C", nil, "set configuration directory path")
	mainCommand.PersistentFlags().StringVarP(&workingDir, "directory", "D", "", "set working directory")
	mainCommand.PersistentFlags().BoolVarP(&disableColor, "disable-color", "", false, "disable color output")
}

func preRun(cmd *cobra.Command, args []string) {
	globalCtx = context.Background()
	sudoUser := os.Getenv("SUDO_USER")
	sudoUID, _ := strconv.Atoi(os.Getenv("SUDO_UID"))
	sudoGID, _ := strconv.Atoi(os.Getenv("SUDO_GID"))
	if sudoUID == 0 && sudoGID == 0 && sudoUser != "" {
		sudoUserObject, _ := user.Lookup(sudoUser)
		if sudoUserObject != nil {
			sudoUID, _ = strconv.Atoi(sudoUserObject.Uid)
			sudoGID, _ = strconv.Atoi(sudoUserObject.Gid)
		}
	}
	if sudoUID > 0 && sudoGID > 0 {
		globalCtx = filemanager.WithDefault(globalCtx, "", "", sudoUID, sudoGID)
	}
	if disableColor {
		logFactory := log.NewDefaultFactory(context.Background(), log.Formatter{BaseTime: time.Now(), DisableColors: true}, os.Stderr, "", nil, false)
		common.Must(logFactory.Start())
		log.SetStdLogger(logFactory.Logger())
	}
	if workingDir != "" {
		_, err := os.Stat(workingDir)
		if err != nil {
			filemanager.MkdirAll(globalCtx, workingDir, 0o777)
		}
		err = os.Chdir(workingDir)
		if err != nil {
			log.Fatal(err)
		}
	}
	if len(configPaths) == 0 && len(configDirectories) == 0 {
		configPaths = append(configPaths, "config.json")
	}
	// registry 必须先进 ctx —— option.Options 的解码依赖它（inbounds[] / services[] 都靠
	// registry 解 union）。不能用 include.Context()：它内联自建全部 registry，调用方拿不到句柄，
	// 既无法注册自有 user_stats 服务类型，也无法剔除 ssmapi（README §4.6 第 8 条、§4.7）。
	globalCtx = box.Context(
		service.ContextWith(globalCtx, deprecated.NewStderrManager(log.StdLogger())),
		minreg.InboundRegistry(),
		minreg.OutboundRegistry(),
		minreg.EndpointRegistry(),
		minreg.DNSTransportRegistry(),
		newServiceRegistry(),
		minreg.CertificateProviderRegistry(),
	)
}
