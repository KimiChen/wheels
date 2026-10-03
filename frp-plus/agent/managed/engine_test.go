//go:build linux || darwin

package managed

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var oldStore = StoreSnapshot{Exists: true, Bytes: []byte(`{"proxies":[],"visitors":[]}`)}
var newStore = StoreSnapshot{Exists: true, Bytes: []byte(`{"proxies":[{"name":"private","secretKey":"synthetic-private-secret"}],"visitors":[]}`)}

func fixture(t *testing.T) Options {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "store.json")
	if err := os.WriteFile(path, oldStore.Bytes, 0600); err != nil {
		t.Fatal(err)
	}
	return Options{Root: root, StorePath: path, MaxBytes: 4096, MaxOperations: 32, RollbackTimeout: time.Second, CloseTimeout: 30 * time.Millisecond,
		RecoveryCheck: func(context.Context, Operation) error { return nil }}
}

func opened(t *testing.T, options Options) *Engine {
	t.Helper()
	e, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return e
}

func request(id string) Request {
	return Request{ID: id, IdempotencyKey: id, BaseRevision: idHash("base"), ContextRevision: idHash("context"), RequestDigest: idHash("instructions"), ExpectedDigest: Digest(oldStore), Candidate: append([]byte(nil), newStore.Bytes...), Deadline: time.Now().Add(5 * time.Second).UTC()}
}

func prepare(t *testing.T, e *Engine, req Request) Operation {
	t.Helper()
	op, err := e.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func disk(t *testing.T, path string, want StoreSnapshot) {
	t.Helper()
	data, err := os.ReadFile(path)
	if !want.Exists {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected absent Store, err=%v", err)
		}
		return
	}
	if err != nil || string(data) != string(want.Bytes) {
		t.Fatal("unexpected Store contents")
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for managed state")
}

type memoryRuntime struct {
	mu           sync.Mutex
	current      StoreSnapshot
	applied      []string
	failNew      bool
	failRollback bool
}

func (r *memoryRuntime) hooks() Runtime {
	return Runtime{
		Check: func(context.Context, Operation, string) error { return nil },
		Apply: func(_ context.Context, value StoreSnapshot) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.applied = append(r.applied, Digest(value))
			if r.failRollback && Digest(value) == Digest(oldStore) {
				return errors.New("private native error")
			}
			r.current = cloneSnapshot(value)
			return nil
		},
		Verify: func(_ context.Context, value StoreSnapshot) (Verification, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			ready := Digest(r.current) == Digest(value) && !(r.failNew && Digest(value) == Digest(newStore))
			return Verification{RuntimeLoaded: Digest(r.current) == Digest(value), ResourcesReady: ready}, nil
		},
	}
}

func (r *memoryRuntime) counts(digest string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, applied := range r.applied {
		if applied == digest {
			count++
		}
	}
	return count
}

func TestPrepareIsPrivateIdempotentAndDoesNotTouchStore(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	req := request("first")
	op := prepare(t, e, req)
	disk(t, o.StorePath, oldStore)
	if op.State != Prepared || op.StorePersisted || op.RuntimeApplied {
		t.Fatal("prepare claimed application")
	}
	again, err := e.Prepare(context.Background(), req)
	if err != nil || again != op {
		t.Fatal("idempotent prepare changed operation")
	}
	changed := req
	changed.Candidate = []byte(`{"proxies":[]}`)
	if _, err := e.Prepare(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed candidate reused operation")
	}
	changed = req
	changed.ContextRevision = idHash("different context")
	if _, err := e.Prepare(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed context reused operation")
	}
	changed = req
	changed.ID = "second"
	if _, err := e.Prepare(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatal("idempotency key reused")
	}
	if _, err := e.Prepare(context.Background(), request("other")); !errors.Is(err, ErrBusy) {
		t.Fatal("second transaction accepted")
	}
	data, err := json.Marshal(op)
	if err != nil || strings.Contains(string(data), "synthetic-private-secret") || strings.Contains(string(data), o.Root) {
		t.Fatal("private material escaped metadata")
	}
	entries, err := os.ReadDir(filepath.Join(o.Root, "operations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		st, _ := entry.Info()
		if st.Mode().Perm() != 0600 {
			t.Fatal("recovery file not private")
		}
	}
	if _, err := e.Rollback(context.Background(), req.ID); err != nil {
		t.Fatal(err)
	}
	disk(t, o.StorePath, oldStore)
}

func TestApplyOnceExplicitVerificationAndPersistentRollback(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	req := request("apply")
	prepare(t, e, req)
	result, err := e.Apply(context.Background(), req.ID)
	if err != nil || result.State != Confirmed || !result.StorePersisted || !result.RuntimeApplied || !result.Verification.ResourcesReady || result.Verification.BusinessChecked {
		t.Fatalf("not explicitly confirmed: %#v %v", result, err)
	}
	disk(t, o.StorePath, newStore)
	if replay, err := e.Prepare(context.Background(), req); err != nil || replay.State != Confirmed {
		t.Fatal("confirmed prepare replay did not return original metadata")
	}
	changed := req
	changed.RequestDigest = idHash("changed-instructions")
	if _, err := e.Prepare(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatal("different instruction digest reused a confirmed operation")
	}
	if _, err := e.Apply(context.Background(), req.ID); err != nil || runtime.counts(Digest(newStore)) != 1 {
		t.Fatal("duplicate runtime application")
	}
	identity := e.ServiceID()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = opened(t, o)
	if !serviceIdentity.MatchString(identity) || e.ServiceID() != identity {
		t.Fatal("identity changed across restart")
	}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	result, err = e.Rollback(context.Background(), req.ID)
	if err != nil || result.State != RolledBack || result.StorePersisted || result.RuntimeApplied || !result.Verification.ResourcesReady {
		t.Fatalf("rollback: %#v %v", result, err)
	}
	disk(t, o.StorePath, oldStore)
	if _, err := e.Rollback(context.Background(), req.ID); err != nil || runtime.counts(Digest(oldStore)) != 1 {
		t.Fatal("duplicate rollback application")
	}
}

func TestNilApplyAndNilVerifyDoNotImplyHealth(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore), failNew: true}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("unhealthy"))
	result, err := e.Apply(context.Background(), "unhealthy")
	if err != nil || result.State != RolledBack || result.ErrorCode != "verification_failed" {
		t.Fatalf("unhealthy candidate was not rolled back: %#v %v", result, err)
	}
	if runtime.counts(Digest(newStore)) != 1 || runtime.counts(Digest(oldStore)) != 1 {
		t.Fatal("whole-set apply/rollback counts wrong")
	}
	disk(t, o.StorePath, oldStore)
}

func TestRollbackFailureKeepsTransactionGateClosed(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore), failNew: true, failRollback: true}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("bad-rollback"))
	result, err := e.Apply(context.Background(), "bad-rollback")
	if !errors.Is(err, ErrOutcomeUnknown) || result.State != RollbackFailed {
		t.Fatalf("rollback failure hidden: %#v %v", result, err)
	}
	if _, err := e.Prepare(context.Background(), request("blocked")); !errors.Is(err, ErrBusy) {
		t.Fatal("uncertain runtime allowed a new transaction")
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "private native error") {
		t.Fatal("native error escaped")
	}
}

func TestRuntimeApplyFailureOrPanicRestoresBothDiskAndRuntime(t *testing.T) {
	for _, panicInstead := range []bool{false, true} {
		t.Run(fmt.Sprint(panicInstead), func(t *testing.T) {
			o := fixture(t)
			e := opened(t, o)
			runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
			hooks := runtime.hooks()
			apply := hooks.Apply
			hooks.Apply = func(ctx context.Context, value StoreSnapshot) error {
				if err := apply(ctx, value); err != nil {
					return err
				}
				if Digest(value) == Digest(newStore) {
					if panicInstead {
						panic("private native panic")
					}
					return errors.New("private native error")
				}
				return nil
			}
			if err := e.AttachRuntime(hooks); err != nil {
				t.Fatal(err)
			}
			prepare(t, e, request("native-failure"))
			result, err := e.Apply(context.Background(), "native-failure")
			if err != nil || result.State != RolledBack || result.ErrorCode != "runtime_apply_failed" {
				t.Fatalf("failure recovery: %#v %v", result, err)
			}
			disk(t, o.StorePath, oldStore)
			if runtime.counts(Digest(newStore)) != 1 || runtime.counts(Digest(oldStore)) != 1 {
				t.Fatal("runtime failure did not restore exactly once")
			}
		})
	}
}

func TestReadOnlyCheckTimeoutNeverAppliesCandidate(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	var checked atomic.Bool
	hooks.Check = func(ctx context.Context, _ Operation, _ string) error {
		checked.Store(true)
		<-ctx.Done()
		return nil
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	req := request("check-timeout")
	// Include the real Prepare journal fsyncs before exercising Check's timeout.
	req.Deadline = time.Now().Add(time.Second)
	prepare(t, e, req)
	result, err := e.Apply(context.Background(), req.ID)
	if err != nil || !checked.Load() || result.State != Cancelled || runtime.counts(Digest(newStore)) != 0 || runtime.counts(Digest(oldStore)) != 0 {
		t.Fatalf("read-only timeout mutated runtime: %#v %v", result, err)
	}
	disk(t, o.StorePath, oldStore)
}

func TestLocalDeadlineWaitsForCallbackBeforeRollback(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	started, release := make(chan struct{}), make(chan struct{})
	apply := hooks.Apply
	hooks.Apply = func(ctx context.Context, value StoreSnapshot) error {
		if Digest(value) == Digest(newStore) {
			close(started)
			<-release
		}
		return apply(ctx, value)
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	req := request("deadline")
	// Leave budget for the durable Prepare fsyncs before the callback starts.
	req.Deadline = time.Now().Add(time.Second)
	prepare(t, e, req)
	done := make(chan Operation, 1)
	go func() { op, _ := e.Apply(context.Background(), req.ID); done <- op }()
	select {
	case <-started:
	case op := <-done:
		t.Fatalf("operation completed before callback: %#v", op)
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not start")
	}
	eventually(t, func() bool { op, _ := e.Query(context.Background(), req.ID); return op.State == OutcomeUnknown })
	if runtime.counts(Digest(oldStore)) != 0 {
		t.Fatal("rollback raced a running apply")
	}
	if _, err := e.Prepare(context.Background(), request("too-early")); !errors.Is(err, ErrBusy) {
		t.Fatal("unknown outcome released gate")
	}
	close(release)
	select {
	case op := <-done:
		if op.State != RolledBack {
			t.Fatalf("late application was not rolled back: %#v", op)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("local recovery did not finish")
	}
	disk(t, o.StorePath, oldStore)
}

func TestRequestCancellationDoesNotCancelLocalTransaction(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	apply := hooks.Apply
	started, release := make(chan struct{}), make(chan struct{})
	hooks.Apply = func(ctx context.Context, value StoreSnapshot) error {
		close(started)
		<-release
		return apply(ctx, value)
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("request-cancel"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.Apply(ctx, "request-cancel"); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation not reported")
	}
	close(release)
	eventually(t, func() bool { op, _ := e.Query(context.Background(), "request-cancel"); return op.State == Confirmed })
	disk(t, o.StorePath, newStore)
}

func TestCloseIsBoundedAndKeepsLockUntilCallbackCompletes(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	apply := hooks.Apply
	started, release := make(chan struct{}), make(chan struct{})
	hooks.Apply = func(ctx context.Context, value StoreSnapshot) error {
		if Digest(value) == Digest(newStore) {
			close(started)
			<-release
		}
		return apply(ctx, value)
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("close"))
	go func() { _, _ = e.Apply(context.Background(), "close") }()
	<-started
	begin := time.Now()
	if err := e.Close(); !errors.Is(err, ErrOutcomeUnknown) || time.Since(begin) > time.Second {
		t.Fatal("Close did not bound a blocked hook")
	}
	if other, err := Open(o); !errors.Is(err, ErrLocked) {
		if other != nil {
			other.Close()
		}
		t.Fatal("Close released an active runtime owner")
	}
	close(release)
	select {
	case <-e.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not finish close")
	}
	disk(t, o.StorePath, oldStore)
}

func TestRawAndContextCASRejectDriftWithoutOverwrite(t *testing.T) {
	for _, kind := range []string{"raw", "context", "during-verify", "rollback-context"} {
		t.Run(kind, func(t *testing.T) {
			o := fixture(t)
			e := opened(t, o)
			runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
			hooks := runtime.hooks()
			foreign := StoreSnapshot{Exists: true, Bytes: []byte(`{"foreign":true}`)}
			if kind == "context" {
				hooks.Check = func(context.Context, Operation, string) error { return errors.New("private context path") }
			}
			if kind == "rollback-context" {
				runtime.failNew = true
				hooks.Check = func(_ context.Context, _ Operation, phase string) error {
					if phase == "rollback" {
						return ErrConflict
					}
					return nil
				}
			}
			if kind == "during-verify" {
				verify := hooks.Verify
				hooks.Verify = func(ctx context.Context, value StoreSnapshot) (Verification, error) {
					if err := os.WriteFile(o.StorePath, foreign.Bytes, 0600); err != nil {
						return Verification{}, err
					}
					return verify(ctx, value)
				}
			}
			if err := e.AttachRuntime(hooks); err != nil {
				t.Fatal(err)
			}
			prepare(t, e, request("drift"))
			if kind == "raw" {
				if err := os.WriteFile(o.StorePath, foreign.Bytes, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, _ := e.Apply(context.Background(), "drift")
			if kind == "raw" || kind == "context" {
				if result.State != Conflict || runtime.counts(Digest(newStore)) != 0 {
					t.Fatal("CAS rejection applied runtime")
				}
			} else if result.State != RollbackFailed {
				t.Fatalf("unsafe rollback accepted: %#v", result)
			}
			switch kind {
			case "raw", "during-verify":
				disk(t, o.StorePath, foreign)
			case "context":
				disk(t, o.StorePath, oldStore)
			case "rollback-context":
				disk(t, o.StorePath, newStore)
			}
		})
	}
}

func TestPersistenceFailuresCannotProduceFalseConfirmation(t *testing.T) {
	for _, stage := range []string{"journal:applying:before_write", "store:before_write", "store:after_file_sync", "store:after_rename", "store:after_dir_sync", "journal:confirmed:before_write"} {
		t.Run(stage, func(t *testing.T) {
			o := fixture(t)
			e := opened(t, o)
			runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
			if err := e.AttachRuntime(runtime.hooks()); err != nil {
				t.Fatal(err)
			}
			prepare(t, e, request("failure"))
			var fired bool
			e.testHook = func(got string) error {
				if got == stage && !fired {
					fired = true
					return errors.New("synthetic disk full")
				}
				return nil
			}
			result, _ := e.Apply(context.Background(), "failure")
			if !fired || result.State == Confirmed {
				t.Fatalf("persistence failure was hidden: %#v", result)
			}
			disk(t, o.StorePath, oldStore)
			if stage == "journal:applying:before_write" && runtime.counts(Digest(newStore)) != 0 {
				t.Fatal("mutation preceded durable journal")
			}
		})
	}
}

func TestPreparedJournalUncertainWriteKeepsIDAndGateAcrossRestart(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	req := request("uncertain-prepare")
	e.testHook = func(stage string) error {
		if stage == "journal:prepared:after_rename" {
			return ErrStorage
		}
		return nil
	}
	op, err := e.Prepare(context.Background(), req)
	if !errors.Is(err, ErrStorage) || op.State != OutcomeUnknown {
		t.Fatalf("uncertain prepare was dropped: %#v %v", op, err)
	}
	if current, err := e.Query(context.Background(), req.ID); err != nil || current.State != OutcomeUnknown {
		t.Fatal("uncertain ID was not queryable")
	}
	if replay, err := e.Prepare(context.Background(), req); err != nil || replay.State != OutcomeUnknown {
		t.Fatal("uncertain prepare was executed again")
	}
	changed := req
	changed.Candidate = []byte("another private candidate")
	if _, err := e.Prepare(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatal("uncertain snapshot overwritten")
	}
	if _, err := e.Prepare(context.Background(), request("second-operation")); !errors.Is(err, ErrBusy) {
		t.Fatal("uncertain prepared journal released lease")
	}
	disk(t, o.StorePath, oldStore)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = opened(t, o)
	disk(t, o.StorePath, oldStore)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { op, _ := e.Query(context.Background(), req.ID); return op.State == RolledBack })
}

func TestPartialSnapshotFailureKeepsProcessGateWithoutAuthorizingReplay(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	req := request("orphan")
	e.testHook = func(stage string) error {
		if stage == "snapshot:after_rename" {
			return ErrStorage
		}
		return nil
	}
	if op, err := e.Prepare(context.Background(), req); !errors.Is(err, ErrStorage) || op.State != OutcomeUnknown {
		t.Fatal("snapshot uncertainty hidden")
	}
	if _, err := e.Prepare(context.Background(), request("second")); !errors.Is(err, ErrBusy) {
		t.Fatal("orphan snapshot released process gate")
	}
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	if op, err := e.Rollback(context.Background(), req.ID); !errors.Is(err, ErrOutcomeUnknown) || op.State != RollbackFailed {
		t.Fatal("partial material falsely restored")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = opened(t, o)
	if _, err := e.Query(context.Background(), req.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("orphan snapshot authorized a transaction without journal")
	}
	disk(t, o.StorePath, oldStore)
}

func TestManagedLockHelper(t *testing.T) {
	root := os.Getenv("MANAGED_TEST_LOCK_ROOT")
	if root == "" {
		return
	}
	e, err := Open(Options{Root: root, StorePath: filepath.Join(root, "store.json")})
	if e != nil {
		e.Close()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second process acquired ownership: %v", err)
	}
}

func TestCrossProcessLockRejectsAnotherOwner(t *testing.T) {
	o := fixture(t)
	_ = opened(t, o)
	command := exec.Command(os.Args[0], "-test.run=^TestManagedLockHelper$", "-test.timeout=5s")
	command.Env = append(os.Environ(), "MANAGED_TEST_LOCK_ROOT="+o.Root)
	if err := command.Run(); err != nil {
		t.Fatal("cross-process lock was not exclusive")
	}
}

func TestConcurrentPrepareHasOneWinnerAndPreparedDeadlineCancels(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := request(fmt.Sprintf("parallel-%d", i))
			req.Deadline = time.Now().Add(150 * time.Millisecond)
			if _, err := e.Prepare(context.Background(), req); err == nil {
				count.Add(1)
			} else if !errors.Is(err, ErrBusy) {
				t.Errorf("prepare: %v", err)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatal("concurrent operations bypassed single writer")
	}
	eventually(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.active == "" })
	disk(t, o.StorePath, oldStore)
}

// The subprocess stops at a durable boundary and is actually killed by the
// parent. Restart recovery must precede native reading and must not replay new.
func TestManagedCrashHelper(t *testing.T) {
	root := os.Getenv("MANAGED_TEST_CRASH_ROOT")
	if root == "" {
		return
	}
	stage := os.Getenv("MANAGED_TEST_CRASH_STAGE")
	o := Options{Root: root, StorePath: filepath.Join(root, "store.json")}
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("killed"))
	e.testHook = func(got string) error {
		if got == stage {
			fmt.Println("crash-boundary-ready")
			select {}
		}
		return nil
	}
	_, _ = e.Apply(context.Background(), "killed")
	t.Fatal("crash boundary not reached")
}

func crashAt(t *testing.T, o Options, stage string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestManagedCrashHelper$", "-test.timeout=20s")
	command.Env = append(os.Environ(), "MANAGED_TEST_CRASH_ROOT="+o.Root, "MANAGED_TEST_CRASH_STAGE="+stage)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			command.Process.Kill()
			command.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "crash-boundary-ready" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("crash helper exited early")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("crash helper did not reach boundary")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("helper was not killed")
	}
}

func TestKillAndRestartRestoresBeforeRuntimeAndVerifiesOldOnly(t *testing.T) {
	for _, stage := range []string{"store:before_rename", "store:after_rename", "journal:verifying:after_dir_sync"} {
		t.Run(stage, func(t *testing.T) {
			o := fixture(t)
			crashAt(t, o, stage)
			e := opened(t, o)
			disk(t, o.StorePath, oldStore)
			op, err := e.Query(context.Background(), "killed")
			if err != nil || op.State != RollingBack {
				t.Fatal("disk recovery claimed runtime confirmation")
			}
			runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
			if err := e.AttachRuntime(runtime.hooks()); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { op, _ := e.Query(context.Background(), "killed"); return op.State == RolledBack })
			if runtime.counts(Digest(newStore)) != 0 || runtime.counts(Digest(oldStore)) != 0 {
				t.Fatal("startup replayed an application instead of verifying loaded old Store")
			}
		})
	}
}

func TestStartupDriftOrMissingContextCheckerBlocksWithoutWriting(t *testing.T) {
	for _, kind := range []string{"raw-drift", "context-drift", "missing-checker", "snapshot-corrupt"} {
		t.Run(kind, func(t *testing.T) {
			o := fixture(t)
			crashAt(t, o, "store:after_rename")
			want := newStore
			switch kind {
			case "raw-drift":
				want = StoreSnapshot{Exists: true, Bytes: []byte(`{"external":1}`)}
				if err := os.WriteFile(o.StorePath, want.Bytes, 0600); err != nil {
					t.Fatal(err)
				}
			case "context-drift":
				o.RecoveryCheck = func(context.Context, Operation) error { return ErrConflict }
			case "missing-checker":
				o.RecoveryCheck = nil
			case "snapshot-corrupt":
				if err := os.WriteFile(filepath.Join(o.Root, "operations", idHash("killed")+".old"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if e, err := Open(o); !errors.Is(err, ErrRecovery) {
				if e != nil {
					e.Close()
				}
				t.Fatalf("unsafe startup accepted: %v", err)
			}
			disk(t, o.StorePath, want)
		})
	}
}
