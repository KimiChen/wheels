//go:build darwin

package store

import "golang.org/x/sys/unix"

func platformMemoryLimit() (uint64, error) { return unix.SysctlUint64("hw.memsize") }
