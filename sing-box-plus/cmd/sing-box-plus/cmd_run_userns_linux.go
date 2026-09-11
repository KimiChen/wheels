// 本文件复制自上游 sing-box 的 cmd/sing-box/cmd_run_userns_linux.go。
//
// 复制而非 import：github.com/sagernet/sing-box/cmd/sing-box 是 package main，Go 禁止导入；
// 而本项目必须保持 run / check / format / version 的 argv 与退出码与上游兼容（README §4.7）。
//
// 来源：github.com/SagerNet/sing-box@0b8995879f29a9b98ee027bc17b75e101445b238（v1.14.0）
// 复制日期：2026-09-06
// 本项目修改：无（仅加本注释头）
//
// 本文件是 GPLv3 衍生物，义务见本子目录的 LICENSE 与 THIRD_PARTY_NOTICES.md。

package main

import (
	"bytes"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/sys/unix"
)

func runInUserNamespaceIfNeeded(options option.Options, optionsList []*OptionsEntry) error {
	if !common.Any(options.NetworkNamespaces, func(namespace option.NetworkNamespace) bool {
		return namespace.Type == C.NetNsTypeUnshare
	}) {
		return nil
	}
	var header unix.CapUserHeader
	header.Version = unix.LINUX_CAPABILITY_VERSION_3
	var data [2]unix.CapUserData
	err := unix.Capget(&header, &data[0])
	if err != nil {
		return E.Cause(err, "get capabilities")
	}
	if data[0].Effective&(1<<unix.CAP_SYS_ADMIN) != 0 {
		return nil
	}
	command := exec.Command("/proc/self/exe", os.Args[1:]...)
	command.Args = os.Args
	command.Stdin = os.Stdin
	for _, entry := range optionsList {
		if entry.path == "stdin" {
			command.Stdin = bytes.NewReader(entry.content)
		}
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: os.Geteuid(), HostID: os.Geteuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: os.Getegid(), HostID: os.Getegid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_NET_ADMIN, unix.CAP_NET_RAW},
		Setpgid:                    true,
		Pdeathsig:                  syscall.SIGKILL,
	}
	err = command.Start()
	if err != nil {
		return E.Cause(err, "create user namespace for unshare network namespace (is unprivileged user namespace creation allowed by the kernel?)")
	}
	signalChannel := make(chan os.Signal, 4)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for receivedSignal := range signalChannel {
			command.Process.Signal(receivedSignal)
		}
	}()
	err = command.Wait()
	exitError, isExitError := E.Cast[*exec.ExitError](err)
	if isExitError {
		os.Exit(exitError.ExitCode())
	}
	if err != nil {
		return E.Cause(err, "wait user namespace child")
	}
	os.Exit(0)
	return nil
}
