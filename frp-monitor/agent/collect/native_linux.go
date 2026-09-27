//go:build linux

package collect

import "syscall"

const nativeSupported = true

func nativeStatvfs(path string) (diskStat, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return diskStat{}, err
	}
	var block, fragment uint64
	if stat.Bsize > 0 {
		block = uint64(stat.Bsize)
	}
	if stat.Frsize > 0 {
		fragment = uint64(stat.Frsize)
	}
	return diskStat{Blocks: stat.Blocks, Bfree: stat.Bfree, Frsize: fragment, Bsize: block}, nil
}
