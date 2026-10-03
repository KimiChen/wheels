package managed

import (
	"context"
	"sort"
	"strings"
	"time"
)

type SnapshotGCResult struct {
	Expired      int
	RemovedBytes int64
}

func (e *Engine) checkExpiredResidues(r *record) error {
	for _, suffix := range []string{".old", ".new"} {
		if _, err := e.operations.read(idHash(r.ID)+suffix, e.opts.MaxBytes); err != nil {
			return err
		}
	}
	return nil
}

// Count actual private snapshot bytes, including interrupted cleanup/orphans.
// Reaching the admission line is enough: no expensive scan of excess material
// is required to prove that another candidate cannot be admitted.
func (e *Engine) snapshotUsageLocked() (int64, error) {
	names, err := e.operations.names()
	if err != nil {
		return 0, err
	}
	var used int64
	for _, name := range names {
		if !strings.HasSuffix(name, ".old") && !strings.HasSuffix(name, ".new") {
			continue
		}
		value, err := e.operations.read(name, e.opts.MaxBytes)
		if err != nil {
			return 0, err
		}
		used += int64(len(value.Bytes))
		if used > e.opts.MaxSnapshotBytes {
			return used, nil
		}
	}
	return used, nil
}

// PruneSnapshots holds only the managed transaction lock, never native reload
// locks. Existing active or recovery work always takes precedence.
func (e *Engine) PruneSnapshots(ctx context.Context) (SnapshotGCResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := SnapshotGCResult{}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if e.closing {
		return out, ErrClosed
	}
	if e.active != "" || e.running || e.recoveryGate || e.restoreBlocked() {
		return out, ErrBusy
	}
	ids := make([]string, 0, len(e.records))
	for id := range e.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := e.retentionNow()
	cutoff := now.Add(-time.Duration(e.opts.SnapshotRetentionDays) * 24 * time.Hour)
	touched := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if touched >= 8 {
			break
		}
		r := e.records[id]
		if !terminal(r.State) || r.NeedsRuntime {
			continue
		}
		if r.Version == 1 {
			// Historical UpdatedAt is not evidence of a full retention interval under
			// this new policy. Adopt once durably and start its conservative clock now.
			normalizeRecordRetention(r, now)
			if err := e.persist(r); err != nil {
				e.recoveryGate = true
				return out, err
			}
			touched++
			continue
		}
		changed := false
		if r.MaterialsState != MaterialsExpired {
			if r.TerminalAt == nil || !r.TerminalAt.Before(cutoff) {
				continue
			}
			r.MaterialsState, r.MaterialsExpiryReason = MaterialsExpired, "ttl"
			at := now
			r.MaterialsExpiredAt = &at
			// Do not alter the operation's last runtime observation timestamp/state.
			if err := e.persist(r); err != nil {
				e.recoveryGate = true
				return out, err
			}
			out.Expired++
			changed = true
		}
		for _, suffix := range []string{".old", ".new"} {
			name := idHash(id) + suffix
			value, err := e.operations.read(name, e.opts.MaxBytes)
			if err != nil {
				return out, err
			}
			if !value.Exists {
				continue
			}
			if err = e.operations.replace(name, StoreSnapshot{}, Digest(value), e.opts.MaxBytes, e.hook("gc:"+suffix)); err != nil {
				return out, err
			}
			out.RemovedBytes += int64(len(value.Bytes))
			changed = true
		}
		if changed {
			touched++
		}
	}
	return out, nil
}
func (e *Engine) retentionLoop() {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-e.closed:
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = e.PruneSnapshots(ctx)
		cancel()
		timer.Reset(time.Minute)
	}
}
