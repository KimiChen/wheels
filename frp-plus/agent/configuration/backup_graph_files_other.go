//go:build !linux && !darwin

package configuration

import (
	"context"
	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
)

type backupDiskView struct{}

func newBackupDiskView(BackupGraphPolicy) (*backupDiskView, error) {
	return nil, backupErr("platform_unsupported")
}
func (*backupDiskView) close()                            {}
func (*backupDiskView) check() error                      { return backupErr("platform_unsupported") }
func (*backupDiskView) sameIdentity(*backupDiskView) bool { return false }
func (*backupDiskView) read(context.Context, string) (backupSourceFile, error) {
	return backupSourceFile{}, backupErr("platform_unsupported")
}
func (*backupDiskView) directory(context.Context, string) ([]backupmanifest.Entry, error) {
	return nil, backupErr("platform_unsupported")
}
