//go:build !linux

package collect

import "errors"

const nativeSupported = false

func nativeStatvfs(string) (diskStat, error) {
	return diskStat{}, errors.New("Linux collection is unsupported on this platform")
}
