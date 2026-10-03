package managed

import (
	"context"
	"time"
)

func checkSafely(callback func(context.Context, Operation, string) error, ctx context.Context, operation Operation, phase string) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrConflict
		}
	}()
	if callback(ctx, operation, phase) != nil || ctx.Err() != nil {
		return ErrConflict
	}
	return nil
}

func applySafely(callback func(context.Context, StoreSnapshot) error, ctx context.Context, value StoreSnapshot) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrRuntime
		}
	}()
	if err = callback(ctx, cloneSnapshot(value)); err != nil {
		return ErrRuntime
	}
	return nil
}

func verifySafely(callback func(context.Context, StoreSnapshot) (Verification, error), ctx context.Context, value StoreSnapshot) (result Verification, err error) {
	defer func() {
		if recover() != nil {
			result, err = Verification{}, ErrRuntime
		}
	}()
	result, err = callback(ctx, cloneSnapshot(value))
	if err != nil {
		return Verification{}, ErrRuntime
	}
	return result, nil
}

// A deadline watcher records uncertainty even if a hook ignores cancellation.
// It never invokes another hook. The existing worker is the only executor and
// must receive the result before a restore can begin.
func (e *Engine) phaseContext(id string, deadline time.Time, timeoutCode string) (context.Context, func()) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	finished := make(chan struct{})
	e.mu.Lock()
	e.phase++
	phase := e.phase
	e.cancel = cancel
	if e.abortCode != "" && timeoutCode == "deadline_expired" {
		cancel()
	}
	e.mu.Unlock()
	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		select {
		case <-finished:
			return
		default:
		}
		if e.running && e.active == id && e.phase == phase {
			code := timeoutCode
			if e.abortCode != "" {
				code = e.abortCode
			}
			_ = e.state(e.records[id], OutcomeUnknown, code)
		}
	}()
	return ctx, func() {
		close(finished)
		cancel()
		e.mu.Lock()
		if e.phase == phase {
			e.cancel = nil
		}
		e.mu.Unlock()
	}
}

func (e *Engine) applyWorker(id string) {
	e.mu.Lock()
	operation := e.records[id].Operation
	e.mu.Unlock()
	checkContext, finishCheck := e.phaseContext(id, operation.Deadline, "deadline_expired")
	checkErr := checkSafely(e.runtime.Check, checkContext, operation, "apply")
	checkCancelled := checkContext.Err() != nil
	finishCheck()
	e.mu.Lock()
	r := e.records[id]
	if checkErr != nil {
		if checkCancelled {
			_ = e.state(r, Cancelled, "deadline_expired")
		} else {
			_ = e.state(r, Conflict, "source_drift")
		}
		e.mu.Unlock()
		return
	}
	_, desired, err := e.snapshots(r)
	if err != nil {
		_ = e.state(r, OutcomeUnknown, "snapshot_invalid")
		e.mu.Unlock()
		return
	}
	if !time.Now().Before(r.Deadline) || e.abortCode != "" {
		code := "deadline_expired"
		if e.abortCode != "" {
			code = e.abortCode
		}
		_ = e.state(r, Cancelled, code)
		e.mu.Unlock()
		return
	}
	if err := e.writeStore(desired, r.OldDigest); err != nil {
		if isConflict(err) {
			_ = e.state(r, Conflict, "store_drift")
			e.mu.Unlock()
			return
		}
		_ = e.state(r, OutcomeUnknown, "store_write_failed")
		e.mu.Unlock()
		e.rollbackWorker(id, false)
		return
	}
	r.StorePersisted = true
	if err := e.save(r); err != nil {
		_ = e.state(r, OutcomeUnknown, "journal_failed")
		e.mu.Unlock()
		e.rollbackWorker(id, false)
		return
	}
	if !time.Now().Before(r.Deadline) || e.abortCode != "" {
		_ = e.state(r, OutcomeUnknown, "deadline_expired")
		e.mu.Unlock()
		e.rollbackWorker(id, false)
		return
	}
	deadline := r.Deadline
	e.mu.Unlock()
	ctx, finish := e.phaseContext(id, deadline, "deadline_expired")
	err = applySafely(e.runtime.Apply, ctx, desired)
	cancelled := ctx.Err() != nil
	finish()
	e.mu.Lock()
	if err != nil || cancelled || e.abortCode != "" {
		code := "runtime_apply_failed"
		if cancelled {
			code = "deadline_expired"
		}
		if e.abortCode != "" {
			code = e.abortCode
		}
		_ = e.state(r, OutcomeUnknown, code)
		e.mu.Unlock()
		e.rollbackWorker(id, false)
		return
	}
	r.RuntimeApplied = true
	if err := e.state(r, Verifying, ""); err != nil {
		e.mu.Unlock()
		e.rollbackWorker(id, false)
		return
	}
	e.mu.Unlock()
	ctx, finish = e.phaseContext(id, deadline, "deadline_expired")
	verified, err := verifySafely(e.runtime.Verify, ctx, desired)
	cancelled = ctx.Err() != nil
	finish()
	e.mu.Lock()
	if err != nil || !verified.RuntimeLoaded || !verified.ResourcesReady || cancelled || e.abortCode != "" {
		code := "verification_failed"
		if cancelled {
			code = "deadline_expired"
		}
		if e.abortCode != "" {
			code = e.abortCode
		}
		_ = e.state(r, OutcomeUnknown, code)
		e.mu.Unlock()
		e.rollbackWorker(id, false)
		return
	}
	current, readErr := e.readStore()
	if readErr != nil || Digest(current) != r.NewDigest {
		_ = e.state(r, OutcomeUnknown, "store_drift")
		e.mu.Unlock()
		e.rollbackWorker(id, false)
		return
	}
	r.Verification = verified
	err = e.state(r, Confirmed, "")
	e.mu.Unlock()
	if err != nil {
		e.rollbackWorker(id, false)
	}
}

func (e *Engine) rollbackWorker(id string, startup bool) {
	e.mu.Lock()
	e.restoring = true
	operation := e.records[id].Operation
	e.mu.Unlock()
	phase := "rollback"
	if startup {
		phase = "recovery"
	}
	checkContext, finishCheck := e.phaseContext(id, time.Now().Add(e.opts.RollbackTimeout), "rollback_timeout")
	checkErr := checkSafely(e.runtime.Check, checkContext, operation, phase)
	checkCancelled := checkContext.Err() != nil
	finishCheck()
	e.mu.Lock()
	r := e.records[id]
	if checkErr != nil {
		code := "source_drift"
		if checkCancelled {
			code = "rollback_timeout"
		}
		_ = e.state(r, RollbackFailed, code)
		e.mu.Unlock()
		return
	}
	old, _, err := e.snapshots(r)
	if err != nil {
		// A preparation that never completed its recovery snapshots must not
		// manufacture an invalid durable journal merely because rollback was
		// requested. It remains gated in this process; orphan files alone carry
		// no recovery authority on the next Open.
		journal, journalErr := e.operations.read(idHash(r.ID)+".json", journalLimit)
		if journalErr == nil && !journal.Exists {
			r.State, r.ErrorCode = RollbackFailed, "snapshot_invalid"
		} else {
			_ = e.state(r, RollbackFailed, "snapshot_invalid")
		}
		e.mu.Unlock()
		return
	}
	current, err := e.readStore()
	if err != nil || (Digest(current) != r.OldDigest && Digest(current) != r.NewDigest) {
		_ = e.state(r, RollbackFailed, "store_drift")
		e.mu.Unlock()
		return
	}
	code := r.ErrorCode
	// A durable applying/outcome_unknown journal already contains all recovery
	// material if this phase update fails. Still attempt the old disk/runtime;
	// a final journal failure prevents a false success and keeps the gate shut.
	_ = e.state(r, RollingBack, code)
	if err := e.writeStore(old, Digest(current)); err != nil {
		_ = e.state(r, RollbackFailed, "rollback_store_failed")
		e.mu.Unlock()
		return
	}
	r.StorePersisted = false
	if err := e.save(r); err != nil {
		r.ErrorCode = "journal_failed"
	}
	e.mu.Unlock()
	if !startup {
		ctx, finish := e.phaseContext(id, time.Now().Add(e.opts.RollbackTimeout), "rollback_timeout")
		err = applySafely(e.runtime.Apply, ctx, old)
		finish()
		if err != nil {
			e.mu.Lock()
			_ = e.state(r, RollbackFailed, "rollback_apply_failed")
			e.mu.Unlock()
			return
		}
	}
	// A late Apply return still requires a fresh explicit observation. This is
	// a verification-only phase, never a second application of the same set.
	ctx, finish := e.phaseContext(id, time.Now().Add(e.opts.RollbackTimeout), "rollback_timeout")
	verified, err := verifySafely(e.runtime.Verify, ctx, old)
	cancelled := ctx.Err() != nil
	finish()
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancelled {
		_ = e.state(r, RollbackFailed, "rollback_timeout")
		return
	}
	if err != nil || !verified.RuntimeLoaded || !verified.ResourcesReady {
		_ = e.state(r, RollbackFailed, "rollback_verification_failed")
		return
	}
	current, err = e.readStore()
	if err != nil || Digest(current) != r.OldDigest {
		_ = e.state(r, RollbackFailed, "store_drift")
		return
	}
	r.RuntimeApplied, r.NeedsRuntime, r.Verification = false, false, verified
	_ = e.state(r, RolledBack, code)
}
