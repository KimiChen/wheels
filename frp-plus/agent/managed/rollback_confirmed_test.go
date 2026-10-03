//go:build linux || darwin

package managed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Done is first consulted by Engine.wait, after the duplicate has captured the
// active worker. This makes joining the same preflight deterministic.
type observedWaitContext struct {
	context.Context
	reached chan struct{}
	once    sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.reached) })
	return c.Context.Done()
}

func confirmed(t *testing.T, e *Engine, id string, from, to StoreSnapshot) Operation {
	t.Helper()
	req := request(id)
	req.ExpectedDigest = Digest(from)
	req.Candidate = append([]byte(nil), to.Bytes...)
	prepare(t, e, req)
	op, err := e.Apply(context.Background(), id)
	if err != nil || op.State != Confirmed {
		t.Fatalf("confirmation failed: %#v %v", op, err)
	}
	return op
}
func unchangedConfirmed(t *testing.T, e *Engine, original Operation, journal []byte) {
	t.Helper()
	got, err := e.Query(context.Background(), original.ID)
	if err != nil || got != original {
		t.Fatalf("historical record changed: %#v %v", got, err)
	}
	current, err := os.ReadFile(filepath.Join(e.opts.Root, "operations", idHash(original.ID)+".json"))
	if err != nil || string(current) != string(journal) {
		t.Fatal("historical journal changed")
	}
	e.mu.Lock()
	busy := e.running || e.active != ""
	e.mu.Unlock()
	if busy {
		t.Fatal("read-only rollback refusal retained the transaction lease")
	}
}
func operationJournal(t *testing.T, e *Engine, id string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.opts.Root, "operations", idHash(id)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func prepareAfterRefusal(t *testing.T, e *Engine, id string, current StoreSnapshot) {
	t.Helper()
	req := request(id)
	req.ExpectedDigest = Digest(current)
	prepare(t, e, req)
	if _, err := e.Rollback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmedHistoricalRollbackPreservesLaterCommit(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	a := confirmed(t, e, "a", oldStore, newStore)
	journal := operationJournal(t, e, a.ID)
	bStore := StoreSnapshot{Exists: true, Bytes: []byte(`{"proxies":[],"visitors":[{"name":"later"}]}`)}
	b := confirmed(t, e, "b", newStore, bStore)
	result, err := e.Rollback(context.Background(), a.ID)
	if !errors.Is(err, ErrConflict) || result != a {
		t.Fatalf("obsolete rollback: %#v %v", result, err)
	}
	unchangedConfirmed(t, e, a, journal)
	disk(t, o.StorePath, bStore)
	if got, err := e.Query(context.Background(), b.ID); err != nil || got != b {
		t.Fatal("later operation changed")
	}
	if runtime.counts(Digest(oldStore)) != 0 || runtime.counts(Digest(bStore)) != 1 {
		t.Fatal("obsolete rollback changed runtime")
	}
	prepareAfterRefusal(t, e, "next", bStore)
}

func TestConfirmedRollbackContextCheckIsReadOnlyAndOutsideLock(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	hooks.Check = func(_ context.Context, op Operation, phase string) error {
		if phase == "rollback" {
			if got, err := e.Query(context.Background(), op.ID); err != nil || got.State != Confirmed {
				t.Error("preflight changed history or held engine lock")
			}
			return ErrConflict
		}
		return nil
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	a := confirmed(t, e, "a", oldStore, newStore)
	journal := operationJournal(t, e, a.ID)
	result, err := e.Rollback(context.Background(), a.ID)
	if !errors.Is(err, ErrConflict) || result != a {
		t.Fatalf("context refusal: %#v %v", result, err)
	}
	unchangedConfirmed(t, e, a, journal)
	disk(t, o.StorePath, newStore)
	if runtime.counts(Digest(oldStore)) != 0 {
		t.Fatal("context check changed runtime")
	}
	prepareAfterRefusal(t, e, "next", newStore)
}

func TestConfirmedRollbackPreflightSerializesAndSharesResult(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	started, release := make(chan struct{}), make(chan struct{})
	var checks atomic.Int32
	hooks.Check = func(_ context.Context, _ Operation, phase string) error {
		if phase == "rollback" {
			if checks.Add(1) == 1 {
				close(started)
			}
			<-release
			return ErrConflict
		}
		return nil
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	a := confirmed(t, e, "a", oldStore, newStore)
	journal := operationJournal(t, e, a.ID)
	results := make(chan error, 2)
	go func() { _, err := e.Rollback(context.Background(), a.ID); results <- err }()
	<-started
	if got, _ := e.Query(context.Background(), a.ID); got != a {
		t.Fatal("read-only check changed visible history")
	}
	if _, err := e.Prepare(context.Background(), request("competing")); !errors.Is(err, ErrBusy) {
		t.Fatal("preflight did not hold worker lease")
	}
	joined := &observedWaitContext{Context: context.Background(), reached: make(chan struct{})}
	go func() { _, err := e.Rollback(joined, a.ID); results <- err }()
	<-joined.reached
	close(release)
	for range 2 {
		if err := <-results; !errors.Is(err, ErrConflict) {
			t.Fatal("preflight refusal hidden", err)
		}
	}
	if checks.Load() != 1 {
		t.Fatal("duplicate rollback reran the preflight")
	}
	unchangedConfirmed(t, e, a, journal)
	disk(t, o.StorePath, newStore)
	if runtime.counts(Digest(oldStore)) != 0 {
		t.Fatal("duplicate preflight changed runtime")
	}
}

func TestConfirmedRollbackReadOnlyTimeoutKeepsHistory(t *testing.T) {
	o := fixture(t)
	o.RollbackTimeout = 25 * time.Millisecond
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	hooks.Check = func(ctx context.Context, _ Operation, phase string) error {
		if phase == "rollback" {
			<-ctx.Done()
		}
		return nil
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	a := confirmed(t, e, "a", oldStore, newStore)
	journal := operationJournal(t, e, a.ID)
	result, err := e.Rollback(context.Background(), a.ID)
	if !errors.Is(err, context.DeadlineExceeded) || result != a {
		t.Fatalf("readonly timeout: %#v %v", result, err)
	}
	unchangedConfirmed(t, e, a, journal)
	disk(t, o.StorePath, newStore)
	prepareAfterRefusal(t, e, "next", newStore)
}

func TestConfirmedRollbackCannotCancelOtherActiveOperation(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	started, release := make(chan struct{}), make(chan struct{})
	bStore := StoreSnapshot{Exists: true, Bytes: []byte(`{"proxies":[{"name":"later"}],"visitors":[]}`)}
	apply := hooks.Apply
	hooks.Apply = func(ctx context.Context, value StoreSnapshot) error {
		if Digest(value) == Digest(bStore) {
			close(started)
			<-release
		}
		return apply(ctx, value)
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	a := confirmed(t, e, "a", oldStore, newStore)
	journal := operationJournal(t, e, a.ID)
	req := request("b")
	req.ExpectedDigest = Digest(newStore)
	req.Candidate = bStore.Bytes
	prepare(t, e, req)
	if op, err := e.Rollback(context.Background(), a.ID); !errors.Is(err, ErrBusy) || op != a {
		t.Fatal("rollback ignored prepared successor")
	}
	result := make(chan error, 1)
	go func() { _, err := e.Apply(context.Background(), "b"); result <- err }()
	<-started
	if op, err := e.Rollback(context.Background(), a.ID); !errors.Is(err, ErrBusy) || op != a {
		t.Fatal("rollback cancelled a running successor")
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	unchangedConfirmed(t, e, a, journal)
	disk(t, o.StorePath, bStore)
}

func TestConfirmedRollbackExternalCASRacesRestoreHistory(t *testing.T) {
	for _, stage := range []string{"context_check", "store:before_rename"} {
		t.Run(stage, func(t *testing.T) {
			o := fixture(t)
			e := opened(t, o)
			runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
			hooks := runtime.hooks()
			external := StoreSnapshot{Exists: true, Bytes: []byte(`{"external":"concurrent-edit"}`)}
			if stage == "context_check" {
				hooks.Check = func(_ context.Context, _ Operation, phase string) error {
					if phase == "rollback" {
						return os.WriteFile(o.StorePath, external.Bytes, 0600)
					}
					return nil
				}
			}
			if err := e.AttachRuntime(hooks); err != nil {
				t.Fatal(err)
			}
			a := confirmed(t, e, "a", oldStore, newStore)
			journal := operationJournal(t, e, a.ID)
			if stage != "context_check" {
				e.testHook = func(got string) error {
					if got == stage {
						return os.WriteFile(o.StorePath, external.Bytes, 0600)
					}
					return nil
				}
			}
			result, err := e.Rollback(context.Background(), a.ID)
			if !errors.Is(err, ErrConflict) || result != a {
				t.Fatalf("external race: %#v %v", result, err)
			}
			e.testHook = nil
			unchangedConfirmed(t, e, a, journal)
			disk(t, o.StorePath, external)
			if runtime.counts(Digest(oldStore)) != 0 {
				t.Fatal("CAS race changed runtime")
			}
			prepareAfterRefusal(t, e, "next", external)
			if err = e.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := opened(t, o)
			if got, err := reopened.Query(context.Background(), a.ID); err != nil || got != a {
				t.Fatal("cancelled intent was not durable")
			}
		})
	}
}

func TestConfirmedRollbackJournalCancellationFailureRetainsGate(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	a := confirmed(t, e, "a", oldStore, newStore)
	external := StoreSnapshot{Exists: true, Bytes: []byte(`{"external":"concurrent-edit"}`)}
	e.testHook = func(stage string) error {
		if stage == "store:before_rename" {
			return os.WriteFile(o.StorePath, external.Bytes, 0600)
		}
		if stage == "journal:confirmed:before_write" {
			return ErrStorage
		}
		return nil
	}
	result, err := e.Rollback(context.Background(), a.ID)
	if !errors.Is(err, ErrStorage) || result.State != OutcomeUnknown {
		t.Fatalf("uncertain journal cancellation hidden: %#v %v", result, err)
	}
	e.testHook = nil
	disk(t, o.StorePath, external)
	if runtime.counts(Digest(oldStore)) != 0 {
		t.Fatal("CAS rejection changed runtime")
	}
	if _, err = e.Prepare(context.Background(), request("blocked")); !errors.Is(err, ErrBusy) {
		t.Fatal("uncertain durable intent released lease")
	}
}

func TestConfirmedRollbackDigestABAIsStateComparison(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	a := confirmed(t, e, "a", oldStore, newStore)
	other := StoreSnapshot{Exists: true, Bytes: []byte(`{"proxies":[],"later":true}`)}
	confirmed(t, e, "b", newStore, other)
	confirmed(t, e, "c", other, newStore)
	// No history-generation claim: identical bytes/presence and unchanged context
	// satisfy this contract even if another confirmed operation intervened.
	result, err := e.Rollback(context.Background(), a.ID)
	if err != nil || result.State != RolledBack {
		t.Fatalf("digest ABA boundary changed: %#v %v", result, err)
	}
	disk(t, o.StorePath, oldStore)
}

func TestConfirmedRollbackUncooperativeCheckKeepsOnlyTemporaryLease(t *testing.T) {
	o := fixture(t)
	o.RollbackTimeout = 25 * time.Millisecond
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	started, release := make(chan struct{}), make(chan struct{})
	hooks.Check = func(_ context.Context, _ Operation, phase string) error {
		if phase == "rollback" {
			close(started)
			<-release
		}
		return nil
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	a := confirmed(t, e, "a", oldStore, newStore)
	journal := operationJournal(t, e, a.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := e.Rollback(ctx, a.ID); result <- err }()
	<-started
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller did not observe timeout", err)
	}
	if op, _ := e.Query(context.Background(), a.ID); op != a {
		t.Fatal("read-only timeout rewrote journal")
	}
	if _, err := e.Prepare(context.Background(), request("too-early")); !errors.Is(err, ErrBusy) {
		t.Fatal("unreturned callback released lease")
	}
	close(release)
	eventually(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return !e.running })
	unchangedConfirmed(t, e, a, journal)
	disk(t, o.StorePath, newStore)
	if runtime.counts(Digest(oldStore)) != 0 {
		t.Fatal("late read-only callback mutated runtime")
	}
	prepareAfterRefusal(t, e, "next", newStore)
}
