//go:build linux || darwin

package managed

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The lifecycle suite establishes this durable marker via both confirmations.
// Here a valid confirmed marker isolates restart's two read-only Check calls.
func restoreCheckEngine(t *testing.T, timeout time.Duration) *Engine {
	t.Helper()
	opts, _, _, _ := restoreFixture(t)
	opts.RollbackTimeout = timeout
	e, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	e.restore = &restoreMarker{Version: 1, ServiceID: e.ServiceID(), ConfigFile: filepath.Join(filepath.Dir(opts.Root), "agent.toml"), WorkingDir: filepath.Dir(opts.Root), CreatedAtMS: time.Now().UnixMilli(), VerifiedAtMS: time.Now().UnixMilli(), RestoreStatus: RestoreStatus{
		State: "confirmed", Epoch: "11111111-1111-4111-8111-111111111111", BackupServiceID: "22222222-2222-4222-8222-222222222222", AcknowledgementID: "33333333-3333-4333-8333-333333333333", ManifestDigest: idHash("backup"), ContextRevision: idHash("context"), StoreDigest: Digest(oldStore), RuntimeLoaded: true, ResourcesReady: true,
	}}
	if err = writeRestoreMarker(e.root, e.restore); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRestoreVerificationRetriesOnlyBusyChecks(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			e := restoreCheckEngine(t, time.Second)
			var checks, verifies, applies atomic.Int32
			runtime := Runtime{
				Check: func(context.Context, Operation, string) error {
					if (phase == "before" && verifies.Load() == 0) || (phase == "after" && verifies.Load() > 0) {
						if checks.Add(1) <= 2 {
							return errors.Join(ErrBusy, errors.New("temporary native lock contention"))
						}
					}
					return nil
				},
				Apply: func(context.Context, StoreSnapshot) error { applies.Add(1); return nil },
				Verify: func(context.Context, StoreSnapshot) (Verification, error) {
					verifies.Add(1)
					return Verification{RuntimeLoaded: true, ResourcesReady: true}, nil
				},
			}
			if err := e.AttachRuntime(runtime); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool { return !e.ManagementWriteBlocked() })
			if checks.Load() != 3 || verifies.Load() != 1 || applies.Load() != 0 {
				t.Fatal("restore retry repeated verification or configuration writes", checks.Load(), verifies.Load(), applies.Load())
			}
		})
	}
}

func TestRestoreVerificationCheckFailureRemainsClosed(t *testing.T) {
	for _, mode := range []string{"busy_deadline", "busy_deadline_after", "busy_cancel", "conflict", "panic"} {
		t.Run(mode, func(t *testing.T) {
			e := restoreCheckEngine(t, 180*time.Millisecond)
			var checks, verifies, applies atomic.Int32
			entered := make(chan struct{}, 1)
			runtime := Runtime{
				Check: func(context.Context, Operation, string) error {
					if mode == "busy_deadline_after" && verifies.Load() == 0 {
						return nil
					}
					checks.Add(1)
					select {
					case entered <- struct{}{}:
					default:
					}
					if mode == "panic" {
						panic("synthetic check failure")
					}
					if mode == "conflict" {
						return ErrConflict
					}
					return ErrBusy
				},
				Apply: func(context.Context, StoreSnapshot) error { applies.Add(1); return nil },
				Verify: func(context.Context, StoreSnapshot) (Verification, error) {
					verifies.Add(1)
					return Verification{RuntimeLoaded: true, ResourcesReady: true}, nil
				},
			}
			if err := e.AttachRuntime(runtime); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("restore check did not start")
			}
			if mode == "busy_cancel" {
				start := time.Now()
				if err := e.Close(); err != nil || time.Since(start) > 500*time.Millisecond {
					t.Fatal("restore cancellation did not terminate bounded wait", err)
				}
			} else {
				eventually(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return !e.running })
			}
			wantVerifies := int32(0)
			if mode == "busy_deadline_after" {
				wantVerifies = 1
			}
			if !e.ManagementWriteBlocked() || verifies.Load() != wantVerifies || applies.Load() != 0 {
				t.Fatal("failed check opened gate or invoked later callbacks")
			}
			if (mode == "conflict" || mode == "panic" || mode == "busy_cancel") && checks.Load() != 1 {
				t.Fatal("nonretryable or cancelled check retried", checks.Load())
			}
			if (mode == "busy_deadline" || mode == "busy_deadline_after") && (checks.Load() < 1 || checks.Load() > 5) {
				t.Fatal("busy retry was absent or exceeded deadline", checks.Load())
			}
			if mode != "busy_cancel" {
				marker, err := readRestoreMarker(e.root)
				if err != nil || marker.Activated {
					t.Fatal("failed check changed durable activation")
				}
			}
		})
	}
}
