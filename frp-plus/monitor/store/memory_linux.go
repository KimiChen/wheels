//go:build linux

package store

import (
	"errors"
	"math"
	"syscall"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/cgroup"
)

func platformMemoryLimit() (uint64, error) {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0, errors.New("history memory limit unavailable")
	}
	unit := uint64(info.Unit)
	if unit == 0 || uint64(info.Totalram) > math.MaxUint64/unit {
		return 0, errors.New("history memory limit unavailable")
	}
	limit := uint64(info.Totalram) * unit
	// Match VM's physical/cgroup accounting, also honoring the parent cgroup.
	for _, n := range []int64{cgroup.GetMemoryLimit(), cgroup.GetHierarchicalMemoryLimit()} {
		if n > 0 && uint64(n) < limit {
			limit = uint64(n)
		}
	}
	return limit, nil
}
