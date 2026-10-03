//go:build linux || darwin

package configuration

import (
	"os"
	"syscall"
)

func backupOwnerAndLinksForCheckpoint(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
