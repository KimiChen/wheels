package managed

import (
	"encoding/json"
	"time"
)

const (
	MaxManagedOperations = 1024
	MaterialsRetained    = "retained"
	MaterialsExpired     = "expired"
)

type snapshotPolicy uint8

const (
	snapshotsRequired snapshotPolicy = iota
	snapshotsExpired
)

// The same strict record decoder is used by live loading and sealed checkpoint
// validation. Only live loading may tolerate already-expired residual files.
func decodeManagedRecord(data []byte, name string) (record, snapshotPolicy, error) {
	var r record
	if decodeCheckpointJSON(data, &r) != nil || !safeID.MatchString(r.ID) || name != idHash(r.ID)+".json" || !knownState(r.State) || !digestString(r.BaseRevision) || !digestString(r.ContextRevision) || !digestString(r.RequestDigest) || !digestString(r.OldDigest) || !digestString(r.NewDigest) || !digestString(r.KeyDigest) || !digestString(r.Fingerprint) || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.Deadline.IsZero() {
		return r, 0, ErrRecovery
	}
	switch r.Version {
	case 1:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return r, 0, ErrRecovery
		}
		for _, key := range []string{"materials_state", "materials_expired_at", "materials_expiry_reason", "terminal_at"} {
			if _, exists := fields[key]; exists {
				return r, 0, ErrRecovery
			}
		}
		if r.MaterialsState != "" || r.MaterialsExpiredAt != nil || r.MaterialsExpiryReason != "" || r.TerminalAt != nil {
			return r, 0, ErrRecovery
		}
		return r, snapshotsRequired, nil
	case 2:
		if r.TerminalAt != nil && r.TerminalAt.IsZero() {
			return r, 0, ErrRecovery
		}
		if terminal(r.State) && r.TerminalAt == nil {
			return r, 0, ErrRecovery
		}
		switch r.MaterialsState {
		case MaterialsRetained:
			if r.MaterialsExpiredAt != nil || r.MaterialsExpiryReason != "" {
				return r, 0, ErrRecovery
			}
			return r, snapshotsRequired, nil
		case MaterialsExpired:
			if !terminal(r.State) || r.NeedsRuntime || r.TerminalAt == nil || r.MaterialsExpiredAt == nil || r.MaterialsExpiredAt.IsZero() || r.MaterialsExpiredAt.Before(*r.TerminalAt) || r.MaterialsExpiryReason != "ttl" {
				return r, 0, ErrRecovery
			}
			return r, snapshotsExpired, nil
		}
	}
	return r, 0, ErrRecovery
}

func visibleOperation(r *record) Operation {
	op := r.Operation
	if r.contextChanged && op.MaterialsState != MaterialsExpired {
		op.MaterialsState = MaterialsContextChanged
	}
	if op.MaterialsState == "" {
		op.MaterialsState = MaterialsRetained
	}
	if op.MaterialsExpiredAt != nil {
		at := *op.MaterialsExpiredAt
		op.MaterialsExpiredAt = &at
	}
	return op
}
func normalizeRecordRetention(r *record, now time.Time) {
	if r.Version == 1 {
		r.Version = 2
		r.MaterialsState = MaterialsRetained
	}
	if terminal(r.State) && r.TerminalAt == nil {
		at := now.UTC()
		r.TerminalAt = &at
	}
}
func (e *Engine) retentionNow() time.Time {
	if e.testRetentionNow != nil {
		return e.testRetentionNow().UTC()
	}
	return time.Now().UTC()
}
