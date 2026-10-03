package managed

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const journalLimit = 16 * 1024

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var serviceIdentity = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type record struct {
	Version int `json:"version"`
	Operation
	KeyDigest    string     `json:"key_digest"`
	Fingerprint  string     `json:"fingerprint"`
	OldExists    bool       `json:"old_exists"`
	NeedsRuntime bool       `json:"needs_runtime"`
	TerminalAt   *time.Time `json:"terminal_at,omitempty"`
}

type workerResult struct {
	done      chan struct{}
	operation Operation
	err       error
}

type Engine struct {
	mu               sync.Mutex
	opts             Options
	root, operations *privateDir
	lock             *os.File
	storeName        string
	serviceID        string
	records          map[string]*record
	keys             map[string]string
	active           string
	runtime          Runtime
	attached         bool
	running          bool
	restoring        bool
	cancel           context.CancelFunc
	abortCode        string
	phase            uint64
	worker           *workerResult
	closing          bool
	closed           chan struct{}
	closeOnce        sync.Once
	recoveryGate     bool
	restore          *restoreMarker
	restoreVerified  bool
	// Tests inject persistence failures/crashes at named boundaries only. No
	// production option enables this hook or exposes private data to it.
	testRetentionNow func() time.Time
	testHook         func(string) error
}

// Open must run before the native Store loader. An interrupted transaction is
// restored to its old disk snapshot only when disk equals a recorded version.
// Runtime recovery remains visibly rolling_back until AttachRuntime verifies
// the freshly started native service; Open alone never claims runtime health.
func Open(options Options) (_ *Engine, resultErr error) {
	if options.SnapshotRetentionDays == 0 {
		options.SnapshotRetentionDays = 30
	}
	if options.MaxSnapshotBytes == 0 {
		options.MaxSnapshotBytes = 32 << 20
	}
	if options.SnapshotRetentionDays < 1 || options.SnapshotRetentionDays > 366 || options.MaxSnapshotBytes < 1<<20 || options.MaxSnapshotBytes > 1<<30 {
		return nil, ErrInvalid
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 1024 * 1024
	}
	if options.MaxOperations == 0 {
		options.MaxOperations = 1024
	}
	if options.RollbackTimeout == 0 {
		options.RollbackTimeout = 30 * time.Second
	}
	if options.CloseTimeout == 0 {
		options.CloseTimeout = time.Second
	}
	if options.MaxBytes < 1 || options.MaxBytes > 16*1024*1024 || options.MaxOperations < 1 || options.MaxOperations > MaxManagedOperations || options.RollbackTimeout <= 0 || options.CloseTimeout <= 0 {
		return nil, ErrInvalid
	}
	if !filepath.IsAbs(options.Root) || filepath.Clean(options.Root) != options.Root || !filepath.IsAbs(options.StorePath) || filepath.Clean(options.StorePath) != options.StorePath || filepath.Dir(options.StorePath) != options.Root {
		return nil, ErrUnsafePath
	}
	name := filepath.Base(options.StorePath)
	reserved := strings.ToLower(name)
	if !safeID.MatchString(strings.TrimSuffix(name, ".json")) || strings.HasPrefix(name, ".") || reserved == "operations" || reserved == "secrets" || reserved == "identity.json" || reserved == "restore.json" || reserved == restorePlanName {
		return nil, ErrUnsafePath
	}
	root, err := openPrivateDir(options.Root)
	if err != nil {
		return nil, err
	}
	e := &Engine{opts: options, root: root, storeName: name, records: map[string]*record{}, keys: map[string]string{}, closed: make(chan struct{})}
	defer func() {
		if resultErr != nil {
			e.release()
		}
	}()
	e.lock, err = root.lock()
	if err != nil {
		return nil, err
	}
	marker, markerErr := readRestoreMarker(root)
	if markerErr != nil || (marker != nil && marker.State == "installing") {
		return nil, ErrRecovery
	}
	if err := checkRestorePlanStartup(root, marker); err != nil {
		return nil, err
	}
	e.operations, err = root.subdir("operations")
	if err != nil {
		return nil, err
	}
	if err := e.loadIdentity(); err != nil {
		return nil, err
	}
	if err := e.loadRestore(); err != nil {
		return nil, err
	}
	if _, err := e.readStore(); err != nil {
		return nil, err
	}
	if err := e.load(); err != nil {
		return nil, err
	}
	if e.active != "" {
		r := e.records[e.active]
		if options.RecoveryCheck == nil {
			return nil, ErrRecovery
		}
		ctx, cancel := context.WithTimeout(context.Background(), options.RollbackTimeout)
		checkErr := checkSafely(func(ctx context.Context, op Operation, _ string) error { return options.RecoveryCheck(ctx, op) }, ctx, r.Operation, "recovery")
		cancel()
		if checkErr != nil {
			return nil, ErrRecovery
		}
		old, _, err := e.snapshots(r)
		if err != nil {
			return nil, ErrRecovery
		}
		current, err := e.readStore()
		if err != nil || (Digest(current) != r.OldDigest && Digest(current) != r.NewDigest) {
			return nil, ErrRecovery
		}
		r.State, r.ErrorCode, r.NeedsRuntime = RollingBack, "startup_recovery", true
		r.RuntimeApplied, r.Verification = false, Verification{}
		if err := e.save(r); err != nil {
			return nil, ErrRecovery
		}
		if err := e.writeStore(old, Digest(current)); err != nil {
			return nil, ErrRecovery
		}
		r.StorePersisted = false
		if err := e.save(r); err != nil {
			return nil, ErrRecovery
		}
	}
	return e, nil
}

// ServiceID is the persistent local managed identity, not a claimed FRP user or
// remote client ID. Restore tooling must explicitly rotate identity on restore.
func (e *Engine) ServiceID() string { return e.serviceID }

func (e *Engine) loadIdentity() error {
	file, err := e.root.read("identity.json", 1024)
	if err != nil {
		return err
	}
	identity := struct {
		Version   int    `json:"version"`
		ServiceID string `json:"service_id"`
		StoreName string `json:"store_name"`
	}{}
	if file.Exists {
		decoder := json.NewDecoder(bytes.NewReader(file.Bytes))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&identity) != nil || decoder.Decode(new(any)) != io.EOF || identity.Version != 1 || identity.StoreName != e.storeName || !serviceIdentity.MatchString(identity.ServiceID) {
			return ErrRecovery
		}
	} else {
		names, err := e.operations.names()
		if err != nil || len(names) != 0 {
			return ErrRecovery
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return ErrStorage
		}
		random[6], random[8] = random[6]&0x0f|0x40, random[8]&0x3f|0x80
		identity.Version, identity.StoreName = 1, e.storeName
		identity.ServiceID = fmt.Sprintf("%x-%x-%x-%x-%x", random[0:4], random[4:6], random[6:8], random[8:10], random[10:16])
		data, err := json.Marshal(identity)
		if err != nil {
			return ErrStorage
		}
		if err := e.root.replace("identity.json", StoreSnapshot{Exists: true, Bytes: data}, Digest(file), 1024, e.hook("identity")); err != nil {
			return err
		}
	}
	e.serviceID = identity.ServiceID
	return nil
}

func idHash(id string) string { sum := sha256.Sum256([]byte(id)); return hex.EncodeToString(sum[:]) }
func digestString(s string) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == 32 && strings.ToLower(s) == s
}
func knownState(s string) bool {
	switch s {
	case Prepared, Applying, Verifying, Confirmed, OutcomeUnknown, RollingBack, RolledBack, RollbackFailed, Conflict, Cancelled:
		return true
	}
	return false
}

func (e *Engine) readStore() (StoreSnapshot, error) { return e.root.read(e.storeName, e.opts.MaxBytes) }
func (e *Engine) hook(prefix string) func(string) error {
	return func(stage string) error {
		if e.testHook != nil {
			return e.testHook(prefix + ":" + stage)
		}
		return nil
	}
}
func (e *Engine) writeStore(value StoreSnapshot, expected string) error {
	return e.root.replace(e.storeName, value, expected, e.opts.MaxBytes, e.hook("store"))
}
func (e *Engine) save(r *record) error {
	r.UpdatedAt = time.Now().UTC()
	return e.persist(r)
}

// persist preserves the exact historical operation when a rollback CAS lost
// before replacing the Store and only its intent journal needs cancellation.
func (e *Engine) persist(r *record) error {
	normalizeRecordRetention(r, e.retentionNow())
	data, err := json.Marshal(r)
	if err != nil || len(data) > journalLimit {
		return ErrStorage
	}
	if err := e.operations.replace(idHash(r.ID)+".json", StoreSnapshot{Exists: true, Bytes: data}, "", journalLimit, e.hook("journal:"+r.State)); err != nil {
		return ErrStorage
	}
	return nil
}
func (e *Engine) state(r *record, state, code string) error {
	if terminal(state) && !terminal(r.State) {
		at := e.retentionNow()
		r.TerminalAt = &at
	}
	r.State, r.ErrorCode = state, code
	if err := e.save(r); err != nil {
		r.State, r.ErrorCode = OutcomeUnknown, "journal_failed"
		e.active = r.ID
		return err
	}
	if terminal(state) && !e.running && e.active == r.ID {
		e.active = ""
	}
	return nil
}

func (e *Engine) load() error {
	names, err := e.operations.names()
	if err != nil || len(names) > MaxManagedOperations*4+32 {
		return ErrRecovery
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if len(e.records) >= MaxManagedOperations {
			return ErrRecovery
		}
		file, err := e.operations.read(name, journalLimit)
		if err != nil || !file.Exists {
			return ErrRecovery
		}
		r, policy, err := decodeManagedRecord(file.Bytes, name)
		if err != nil {
			return ErrRecovery
		}
		if _, found := e.keys[r.KeyDigest]; found {
			return ErrRecovery
		}
		if policy == snapshotsRequired {
			if _, _, err := e.snapshots(&r); err != nil {
				return ErrRecovery
			}
		} else if err := e.checkExpiredResidues(&r); err != nil {
			return ErrRecovery
		}
		e.records[r.ID], e.keys[r.KeyDigest] = &r, r.ID
		if !terminal(r.State) {
			if e.active != "" {
				return ErrRecovery
			}
			e.active = r.ID
		}
	}
	return nil
}

func (e *Engine) snapshots(r *record) (StoreSnapshot, StoreSnapshot, error) {
	if r.MaterialsState == MaterialsExpired {
		return StoreSnapshot{}, StoreSnapshot{}, ErrNotFound
	}
	old, err := e.operations.read(idHash(r.ID)+".old", e.opts.MaxBytes)
	if err != nil || !old.Exists {
		return StoreSnapshot{}, StoreSnapshot{}, ErrRecovery
	}
	old.Exists = r.OldExists
	if !old.Exists && len(old.Bytes) != 0 {
		return StoreSnapshot{}, StoreSnapshot{}, ErrRecovery
	}
	newValue, err := e.operations.read(idHash(r.ID)+".new", e.opts.MaxBytes)
	if err != nil || !newValue.Exists || Digest(old) != r.OldDigest || Digest(newValue) != r.NewDigest {
		return StoreSnapshot{}, StoreSnapshot{}, ErrRecovery
	}
	return old, newValue, nil
}

func (e *Engine) Prepare(ctx context.Context, request Request) (Operation, error) {
	if ctx.Err() != nil {
		return Operation{}, ctx.Err()
	}
	if !safeID.MatchString(request.ID) || !safeID.MatchString(request.IdempotencyKey) || !digestString(request.BaseRevision) || !digestString(request.ContextRevision) || !digestString(request.RequestDigest) || !digestString(request.ExpectedDigest) || len(request.Candidate) == 0 || int64(len(request.Candidate)) > e.opts.MaxBytes || request.Deadline.IsZero() {
		return Operation{}, ErrInvalid
	}
	newValue := StoreSnapshot{Exists: true, Bytes: append([]byte(nil), request.Candidate...)}
	key := idHash(request.IdempotencyKey)
	fingerprint := idHash(request.BaseRevision + "\x00" + request.ContextRevision + "\x00" + request.RequestDigest + "\x00" + request.ExpectedDigest + "\x00" + Digest(newValue) + "\x00" + request.Deadline.UTC().Format(time.RFC3339Nano))
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return Operation{}, ErrClosed
	}
	if e.recoveryGate || e.restoreBlocked() {
		return Operation{}, ErrRecovery
	}
	if r, ok := e.records[request.ID]; ok {
		if r.KeyDigest != key || r.Fingerprint != fingerprint {
			return visibleOperation(r), ErrConflict
		}
		return visibleOperation(r), nil
	}
	if _, used := e.keys[key]; used {
		return Operation{}, ErrConflict
	}
	if e.active != "" || e.running {
		return Operation{}, ErrBusy
	}
	if len(e.records) >= e.opts.MaxOperations {
		return Operation{}, ErrCapacity
	}
	now := time.Now().UTC()
	if !request.Deadline.After(now) {
		return Operation{}, ErrExpired
	}
	if request.Deadline.After(now.Add(24 * time.Hour)) {
		return Operation{}, ErrInvalid
	}
	old, err := e.readStore()
	if err != nil {
		return Operation{}, err
	}
	if Digest(old) != request.ExpectedDigest {
		return Operation{}, ErrConflict
	}
	usage, err := e.snapshotUsageLocked()
	if err != nil {
		return Operation{}, err
	}
	if usage+int64(len(old.Bytes))+int64(len(newValue.Bytes)) > e.opts.MaxSnapshotBytes {
		return Operation{}, ErrCapacity
	}
	journal, err := e.operations.read(idHash(request.ID)+".json", journalLimit)
	if err != nil || journal.Exists {
		e.recoveryGate = true
		return Operation{}, ErrRecovery
	}
	r := &record{Version: 2, KeyDigest: key, Fingerprint: fingerprint, OldExists: old.Exists,
		Operation: Operation{MaterialsState: MaterialsRetained, ID: request.ID, BaseRevision: request.BaseRevision, ContextRevision: request.ContextRevision, RequestDigest: request.RequestDigest, OldDigest: Digest(old), NewDigest: Digest(newValue), State: Prepared, CreatedAt: now, UpdatedAt: now, Deadline: request.Deadline.UTC()}}
	// Reserve the ID and gate before the first recovery write. A failed fsync
	// may still have installed a journal; no second transaction may assume it
	// was absent or overwrite its snapshots with a different request.
	e.records[r.ID], e.keys[key], e.active = r, r.ID, r.ID
	for _, file := range []struct {
		name  string
		value StoreSnapshot
	}{{".old", StoreSnapshot{Exists: true, Bytes: old.Bytes}}, {".new", newValue}} {
		if err := e.operations.replace(idHash(r.ID)+file.name, file.value, "", e.opts.MaxBytes, e.hook("snapshot")); err != nil {
			// Orphan snapshots without a journal never authorize native loading
			// or replay. This process remains gated; Open ignores such orphans.
			r.State, r.ErrorCode = OutcomeUnknown, "snapshot_write_failed"
			return visibleOperation(r), ErrStorage
		}
	}
	if err := e.save(r); err != nil {
		r.State, r.ErrorCode = OutcomeUnknown, "journal_failed"
		return visibleOperation(r), ErrStorage
	}
	go e.expirePrepared(r.ID, r.Deadline)
	return visibleOperation(r), nil
}

func (e *Engine) expirePrepared(id string, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-e.closed:
		return
	case <-timer.C:
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closing && e.records[id].State == Prepared {
		_ = e.state(e.records[id], Cancelled, "deadline_expired")
	}
}

func (e *Engine) Query(ctx context.Context, id string) (Operation, error) {
	if ctx.Err() != nil {
		return Operation{}, ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return Operation{}, ErrClosed
	}
	r, ok := e.records[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return visibleOperation(r), nil
}

// AttachRuntime never treats startup as confirmation. Pending recovery is
// independently verified by a local worker before another Prepare is allowed.
func (e *Engine) AttachRuntime(runtime Runtime) error {
	if runtime.Check == nil || runtime.Apply == nil || runtime.Verify == nil {
		return ErrInvalid
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return ErrClosed
	}
	if e.attached {
		return ErrConflict
	}
	e.runtime, e.attached = runtime, true
	go e.retentionLoop()
	if e.active != "" && e.records[e.active].NeedsRuntime {
		r := e.records[e.active]
		e.launch(r, func() error { e.rollbackWorker(r.ID, true); return nil })
	}
	e.scheduleRestoreVerification()
	return nil
}

func (e *Engine) launch(r *record, work func() error) {
	e.running, e.active, e.abortCode = true, r.ID, ""
	e.restoring = false
	worker := &workerResult{done: make(chan struct{})}
	e.worker = worker
	go func() {
		err := work()
		e.mu.Lock()
		defer e.mu.Unlock()
		worker.err = err
		worker.operation = visibleOperation(e.records[r.ID])
		e.running, e.cancel = false, nil
		if terminal(worker.operation.State) {
			e.active = ""
		}
		close(worker.done)
		if e.closing {
			e.release()
		}
	}()
}

func (e *Engine) wait(ctx context.Context, id string, worker *workerResult) (Operation, error) {
	select {
	case <-ctx.Done():
		e.mu.Lock()
		defer e.mu.Unlock()
		return visibleOperation(e.records[id]), ctx.Err()
	case <-worker.done:
		if worker.err != nil {
			return worker.operation, worker.err
		}
		if worker.operation.State == OutcomeUnknown || worker.operation.State == RollbackFailed {
			return worker.operation, ErrOutcomeUnknown
		}
		if worker.operation.State == Conflict {
			return worker.operation, ErrConflict
		}
		return worker.operation, nil
	}
}

func (e *Engine) Apply(ctx context.Context, id string) (Operation, error) {
	if ctx.Err() != nil {
		return Operation{}, ctx.Err()
	}
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return Operation{}, ErrClosed
	}
	if e.recoveryGate || e.restoreBlocked() {
		e.mu.Unlock()
		return Operation{}, ErrRecovery
	}
	r, ok := e.records[id]
	if !ok {
		e.mu.Unlock()
		return Operation{}, ErrNotFound
	}
	if r.MaterialsState == MaterialsExpired {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, ErrNotFound
	}
	if e.running && e.active == id {
		worker := e.worker
		e.mu.Unlock()
		return e.wait(ctx, id, worker)
	}
	if r.State != Prepared {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, nil
	}
	if !e.attached {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, ErrRuntime
	}
	if !time.Now().Before(r.Deadline) {
		err := e.state(r, Cancelled, "deadline_expired")
		result := visibleOperation(r)
		e.mu.Unlock()
		if err != nil {
			return result, err
		}
		return result, ErrExpired
	}
	current, err := e.readStore()
	if err != nil || Digest(current) != r.OldDigest {
		_ = e.state(r, Conflict, "store_drift")
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, ErrConflict
	}
	if err := e.state(r, Applying, ""); err != nil {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, err
	}
	e.launch(r, func() error { e.applyWorker(id); return nil })
	worker := e.worker
	e.mu.Unlock()
	return e.wait(ctx, id, worker)
}

// Rollback during an uncooperative callback only requests cancellation. The
// same worker must finish that call before restoring disk/runtime serially.
func (e *Engine) Rollback(ctx context.Context, id string) (Operation, error) {
	if ctx.Err() != nil {
		return Operation{}, ctx.Err()
	}
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return Operation{}, ErrClosed
	}
	if e.recoveryGate || e.restoreBlocked() {
		e.mu.Unlock()
		return Operation{}, ErrRecovery
	}
	r, ok := e.records[id]
	if !ok {
		e.mu.Unlock()
		return Operation{}, ErrNotFound
	}
	if r.MaterialsState == MaterialsExpired {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, ErrNotFound
	}
	if e.running {
		if e.active != id {
			result := visibleOperation(r)
			e.mu.Unlock()
			return result, ErrBusy
		}
		if !e.restoring {
			e.abortCode = "rollback_requested"
			if e.cancel != nil {
				e.cancel()
			}
		}
		worker := e.worker
		e.mu.Unlock()
		return e.wait(ctx, id, worker)
	}
	if r.State == Prepared {
		err := e.state(r, Cancelled, "cancelled_before_apply")
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, err
	}
	if r.State == RolledBack || r.State == Cancelled || r.State == Conflict {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, nil
	}
	if e.active != "" && e.active != id {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, ErrBusy
	}
	// A historical confirmed operation has no unfinished work to recover. An
	// obsolete undo request must not turn it into an incomplete transaction.
	// This is byte/presence CAS, not a history generation: identical restored
	// bytes (ABA) plus unchanged context remain eligible for an explicit undo.
	if r.State == Confirmed {
		if !e.attached {
			result := visibleOperation(r)
			e.mu.Unlock()
			return result, ErrRuntime
		}
		current, err := e.readStore()
		if err != nil || Digest(current) != r.NewDigest {
			result := visibleOperation(r)
			e.mu.Unlock()
			return result, ErrConflict
		}
		if _, _, err = e.snapshots(r); err != nil {
			result := visibleOperation(r)
			e.mu.Unlock()
			return result, ErrStorage
		}
		original := r.Operation
		e.launch(r, func() error { return e.rollbackConfirmed(original) })
		e.restoring = true // duplicate rollback calls join this read-only preflight
		worker := e.worker
		e.mu.Unlock()
		return e.wait(ctx, id, worker)
	}

	if _, _, snapshotErr := e.snapshots(r); snapshotErr != nil {
		journal, journalErr := e.operations.read(idHash(r.ID)+".json", journalLimit)
		if journalErr == nil && !journal.Exists {
			r.State, r.ErrorCode = RollbackFailed, "snapshot_invalid"
			result := visibleOperation(r)
			e.mu.Unlock()
			return result, ErrOutcomeUnknown
		}
	}
	if !e.attached {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, ErrRuntime
	}
	if err := e.state(r, RollingBack, "rollback_requested"); err != nil {
		result := visibleOperation(r)
		e.mu.Unlock()
		return result, err
	}
	e.launch(r, func() error { e.rollbackWorker(id, false); return nil })
	worker := e.worker
	e.mu.Unlock()
	return e.wait(ctx, id, worker)
}

func (e *Engine) release() {
	e.closeOnce.Do(func() {
		e.operations.close()
		if e.lock != nil {
			e.lock.Close()
		}
		e.root.close()
		close(e.closed)
	})
}

// Close is bounded while a runtime hook is stuck. On timeout the process lock
// stays held until the worker returns; returning an error never permits another
// engine to race an outstanding native callback.
func (e *Engine) Close() error {
	e.mu.Lock()
	if !e.closing {
		e.closing = true
		if e.running {
			e.abortCode = "engine_closing"
			if e.cancel != nil {
				e.cancel()
			}
			if r := e.records[e.active]; r != nil && !terminal(r.State) {
				_ = e.state(r, OutcomeUnknown, "engine_closing")
			}
		} else {
			if e.active != "" && e.records[e.active].State == Prepared {
				_ = e.state(e.records[e.active], Cancelled, "cancelled_before_apply")
			}
			e.release()
		}
	}
	e.mu.Unlock()
	timer := time.NewTimer(e.opts.CloseTimeout)
	defer timer.Stop()
	select {
	case <-e.closed:
		return nil
	case <-timer.C:
		return ErrOutcomeUnknown
	}
}

// Keep errors.Is usable for callers without exposing underlying disk paths.
func isConflict(err error) bool { return errors.Is(err, ErrConflict) }
