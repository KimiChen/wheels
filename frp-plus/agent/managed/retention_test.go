//go:build linux || darwin

package managed

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func retentionEngine(t *testing.T) (*Engine, *atomic.Int64, Options) {
	opts := fixture(t)
	e := opened(t, opts)
	clock := new(atomic.Int64)
	clock.Store(time.Now().UTC().UnixNano())
	e.testRetentionNow = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	if err := e.AttachRuntime((&memoryRuntime{current: oldStore}).hooks()); err != nil {
		t.Fatal(err)
	}
	return e, clock, opts
}
func confirmedRetention(t *testing.T, e *Engine, id string) (Operation, Request) {
	req := request(id)
	prepare(t, e, req)
	op, err := e.Apply(context.Background(), id)
	if err != nil || op.State != Confirmed {
		t.Fatal(op, err)
	}
	return op, req
}
func TestSnapshotRetentionKeepsFactsAndIdempotencyAfterRestart(t *testing.T) {
	e, clock, opts := retentionEngine(t)
	confirmed, req := confirmedRetention(t, e, "retained")
	clock.Add(int64(31 * 24 * time.Hour))
	r, err := e.PruneSnapshots(context.Background())
	if err != nil || r.Expired != 1 || r.RemovedBytes != int64(len(oldStore.Bytes)+len(newStore.Bytes)) {
		t.Fatal(r, err)
	}
	op, err := e.Query(context.Background(), req.ID)
	if err != nil || op.State != Confirmed || op.MaterialsState != MaterialsExpired || op.MaterialsExpiryReason != "ttl" || !op.UpdatedAt.Equal(confirmed.UpdatedAt) || op.Verification != confirmed.Verification {
		t.Fatal(op, err)
	}
	disk(t, opts.StorePath, newStore)
	for _, suffix := range []string{".old", ".new"} {
		if _, err = os.Stat(filepath.Join(opts.Root, "operations", idHash(req.ID)+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private snapshot retained", suffix, err)
		}
	}
	for _, action := range []func(context.Context, string) (Operation, error){e.Apply, e.Rollback} {
		got, err := action(context.Background(), req.ID)
		if !errors.Is(err, ErrNotFound) || got.State != Confirmed || got.MaterialsState != MaterialsExpired {
			t.Fatal(got, err)
		}
	}
	replay, err := e.Prepare(context.Background(), req)
	if err != nil || replay.MaterialsState != MaterialsExpired {
		t.Fatal(replay, err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e = opened(t, opts)
	replay, err = e.Prepare(context.Background(), req)
	if err != nil || replay.MaterialsState != MaterialsExpired {
		t.Fatal(replay, err)
	}
	next := request("next")
	next.ExpectedDigest = Digest(newStore)
	next.Candidate = oldStore.Bytes
	if _, err = e.Prepare(context.Background(), next); err != nil {
		t.Fatal("expired historical rollback retained lease", err)
	}
}
func TestSnapshotRetentionCrashBoundariesNeverLoseRecoveryMaterial(t *testing.T) {
	for _, stage := range []string{"journal:confirmed:before_write", "journal:confirmed:after_rename", "gc:.old:before_rename", "gc:.old:after_rename", "gc:.new:after_dir_sync"} {
		t.Run(stage, func(t *testing.T) {
			e, clock, opts := retentionEngine(t)
			_, req := confirmedRetention(t, e, "boundary")
			clock.Add(int64(31 * 24 * time.Hour))
			e.mu.Lock()
			e.testHook = func(at string) error {
				if at == stage {
					return ErrStorage
				}
				return nil
			}
			e.mu.Unlock()
			if _, err := e.PruneSnapshots(context.Background()); err == nil {
				t.Fatal("injected boundary not reached")
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e = opened(t, opts)
			op, err := e.Query(context.Background(), req.ID)
			if err != nil || op.State != Confirmed {
				t.Fatal(op, err)
			}
			want := MaterialsExpired
			if stage == "journal:confirmed:before_write" {
				want = MaterialsRetained
			}
			if op.MaterialsState != want {
				t.Fatal("wrong durable boundary", op.MaterialsState)
			}
			if want == MaterialsRetained {
				if _, _, err = e.snapshots(e.records[req.ID]); err != nil {
					t.Fatal("deleted before durable tombstone", err)
				}
			} else {
				if _, err = e.PruneSnapshots(context.Background()); err != nil {
					t.Fatal(err)
				}
				if got, err := e.Rollback(context.Background(), req.ID); !errors.Is(err, ErrNotFound) || got.State != Confirmed {
					t.Fatal(got, err)
				}
			}
			disk(t, opts.StorePath, newStore)
		})
	}
}
func TestSnapshotRetentionProtectsActiveWorkAndDoesNotStarveLaterRecords(t *testing.T) {
	e, clock, _ := retentionEngine(t)
	// Cancelled operations retain their exact private candidate until TTL too.
	for i := 0; i < 12; i++ {
		id := strings.Repeat("x", i+1)
		prepare(t, e, request(id))
		if _, err := e.Rollback(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	clock.Add(int64(31 * 24 * time.Hour))
	prepare(t, e, request("active"))
	if _, err := e.PruneSnapshots(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("active material eligible", err)
	}
	if _, err := e.Rollback(context.Background(), "active"); err != nil {
		t.Fatal(err)
	}
	a, err := e.PruneSnapshots(context.Background())
	if err != nil || a.Expired != 8 {
		t.Fatal(a, err)
	}
	b, err := e.PruneSnapshots(context.Background())
	if err != nil || b.Expired != 4 {
		t.Fatal("old tombstones starved later records", b, err)
	}
}
func TestSnapshotRetentionLegacyAdoptionAndStrictRecordPolicy(t *testing.T) {
	e, clock, opts := retentionEngine(t)
	_, req := confirmedRetention(t, e, "legacy")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.Root, "operations", idHash(req.ID)+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recordFields map[string]any
	if json.Unmarshal(raw, &recordFields) != nil {
		t.Fatal("journal")
	}
	recordFields["version"] = 1
	for _, key := range []string{"materials_state", "materials_expired_at", "materials_expiry_reason", "terminal_at"} {
		delete(recordFields, key)
	}
	raw, _ = json.Marshal(recordFields)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, policy, err := decodeManagedRecord(raw, filepath.Base(path)); err != nil || policy != snapshotsRequired {
		t.Fatal(policy, err)
	}
	recordFields["materials_state"] = ""
	bad, _ := json.Marshal(recordFields)
	if _, _, err := decodeManagedRecord(bad, filepath.Base(path)); err == nil {
		t.Fatal("legacy accepted future fields")
	}
	e = opened(t, opts)
	e.testRetentionNow = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	clock.Add(int64(100 * 24 * time.Hour))
	result, err := e.PruneSnapshots(context.Background())
	if err != nil || result.Expired != 0 {
		t.Fatal("legacy deleted without grace", result, err)
	}
	clock.Add(int64(30 * 24 * time.Hour))
	if result, err = e.PruneSnapshots(context.Background()); err != nil || result.Expired != 0 {
		t.Fatal("TTL shortened", result, err)
	}
	clock.Add(int64(time.Second))
	if result, err = e.PruneSnapshots(context.Background()); err != nil || result.Expired != 1 {
		t.Fatal(result, err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, policy, err := decodeManagedRecord(raw, filepath.Base(path))
	if err != nil || policy != snapshotsExpired {
		t.Fatal(r, policy, err)
	}
	for _, state := range []string{Prepared, Applying, Verifying, OutcomeUnknown, RollingBack, RollbackFailed} {
		r.State = state
		bad, _ := json.Marshal(r)
		if _, _, err := decodeManagedRecord(bad, filepath.Base(path)); err == nil {
			t.Fatal("unsafe tombstone", state)
		}
	}
}
func TestSnapshotCapacityBeforeWritingAndMetadataLimitAfterRestart(t *testing.T) {
	opts := fixture(t)
	opts.MaxBytes = 1 << 20
	opts.MaxSnapshotBytes = 1 << 20
	opts.MaxOperations = 2
	e := opened(t, opts)
	req := request("large")
	req.Candidate = []byte(strings.Repeat("x", 700000))
	prepare(t, e, req)
	if _, err := e.Rollback(context.Background(), req.ID); err != nil {
		t.Fatal(err)
	}
	next := request("over-budget")
	next.Candidate = []byte(strings.Repeat("y", 400000))
	if _, err := e.Prepare(context.Background(), next); !errors.Is(err, ErrCapacity) {
		t.Fatal("byte capacity not enforced", err)
	}
	if _, err := os.Stat(filepath.Join(opts.Root, "operations", idHash(next.ID)+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed admission wrote journal", err)
	}
	small := request("small")
	prepare(t, e, small)
	if _, err := e.Rollback(context.Background(), small.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	opts.MaxOperations = 1
	e = opened(t, opts)
	if _, err := e.Prepare(context.Background(), request("full")); !errors.Is(err, ErrCapacity) {
		t.Fatal("metadata capacity not enforced", err)
	}
	if op, err := e.Query(context.Background(), small.ID); err != nil || op.State != Cancelled {
		t.Fatal("lower admission limit broke existing records", op, err)
	}
}
