//go:build !linux && !darwin

package store

import "errors"

// The project ships Linux amd64/arm64 and runs its native tests on macOS.
// Unknown memory limits must disable optional history instead of risking a VM fatal exit.
func platformMemoryLimit() (uint64, error) { return 0, errors.New("history memory limit unavailable") }
