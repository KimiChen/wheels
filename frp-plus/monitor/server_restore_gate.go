package monitor

import (
	"errors"
	"os"
)

// A restored controller may contain stale operation intents. Observation and
// recovery remain available, but new configuration writes require an explicit
// offline acknowledgement and a subsequent process restart.
var ErrServerRestorePending = errors.New("server restore requires offline confirmation")

const serverRestoreGateSuffix = ".restore-gate.json"

// This is a startup latch, not an authorization document parser. Any marker,
// including an unreadable or malformed one, closes the write gate. Removing it
// while this process is running cannot reopen the gate. The offline restore
// tool validates the private document and recovery conditions before removal.
func serverRestoreGateAtStart(databaseFile string) bool {
	if databaseFile == "" {
		return false
	}
	_, err := os.Lstat(databaseFile + serverRestoreGateSuffix)
	return !errors.Is(err, os.ErrNotExist)
}

func serverRestoreActionBlocked(pending bool, action string) bool {
	if !pending {
		return false
	}
	switch action {
	case "prepare", "apply", "rollback":
		return true
	default:
		// inspect/query are observations. Cancellation only releases a prepared
		// operation; the Agent rejects cancellation of an applied operation.
		// Restore acknowledgement uses the separate authenticated restore lane.
		return false
	}
}
