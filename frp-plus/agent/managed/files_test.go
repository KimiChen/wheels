//go:build linux || darwin

package managed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRejectUnsafePathsPermissionsAndObjects(t *testing.T) {
	for _, kind := range []string{"outside", "nested", "reserved", "root-mode", "store-mode", "store-symlink", "store-directory", "store-fifo", "store-hardlink", "oversize", "ancestor-symlink", "lock-symlink", "identity-symlink"} {
		t.Run(kind, func(t *testing.T) {
			o := fixture(t)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "outside":
				o.StorePath = filepath.Join(filepath.Dir(o.Root), "outside.json")
			case "nested":
				o.StorePath = filepath.Join(o.Root, "nested", "store.json")
			case "reserved":
				o.StorePath = filepath.Join(o.Root, "identity.json")
			case "root-mode":
				must(os.Chmod(o.Root, 0755))
			case "store-mode":
				must(os.Chmod(o.StorePath, 0644))
			case "store-symlink":
				must(os.Remove(o.StorePath))
				must(os.Symlink("missing.json", o.StorePath))
			case "store-directory":
				must(os.Remove(o.StorePath))
				must(os.Mkdir(o.StorePath, 0700))
			case "store-fifo":
				must(os.Remove(o.StorePath))
				must(unix.Mkfifo(o.StorePath, 0600))
			case "store-hardlink":
				must(os.Link(o.StorePath, filepath.Join(o.Root, "alias.json")))
			case "oversize":
				must(os.WriteFile(o.StorePath, []byte(strings.Repeat("x", 4097)), 0600))
			case "ancestor-symlink":
				link := o.Root + "-alias"
				must(os.Symlink(o.Root, link))
				t.Cleanup(func() { os.Remove(link) })
				o.Root = filepath.Join(link, "managed")
				o.StorePath = filepath.Join(o.Root, "store.json")
			case "lock-symlink":
				must(os.Symlink(o.StorePath, filepath.Join(o.Root, ".lock")))
			case "identity-symlink":
				must(os.Symlink(o.StorePath, filepath.Join(o.Root, "identity.json")))
			}
			if e, err := Open(o); !errors.Is(err, ErrUnsafePath) {
				if e != nil {
					e.Close()
				}
				t.Fatalf("unsafe path accepted: %v", err)
			}
		})
	}
}

func TestMissingStoreCanApplyAndRollbackToAbsence(t *testing.T) {
	o := fixture(t)
	if err := os.Remove(o.StorePath); err != nil {
		t.Fatal(err)
	}
	e := opened(t, o)
	runtime := &memoryRuntime{}
	if err := e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	req := request("missing")
	req.ExpectedDigest = Digest(StoreSnapshot{})
	prepare(t, e, req)
	if op, err := e.Apply(context.Background(), req.ID); err != nil || op.State != Confirmed {
		t.Fatalf("apply: %#v %v", op, err)
	}
	if op, err := e.Rollback(context.Background(), req.ID); err != nil || op.State != RolledBack {
		t.Fatalf("rollback: %#v %v", op, err)
	}
	disk(t, o.StorePath, StoreSnapshot{})
	if Digest(StoreSnapshot{}) == Digest(StoreSnapshot{Exists: true}) {
		t.Fatal("missing and empty Store have equal digest")
	}
}

func TestCandidateSizeAndIdentityBinding(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	req := request("large")
	req.Candidate = make([]byte, o.MaxBytes+1)
	if _, err := e.Prepare(context.Background(), req); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversize candidate accepted")
	}
	disk(t, o.StorePath, oldStore)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	changed := o
	changed.StorePath = filepath.Join(o.Root, "another.json")
	if next, err := Open(changed); !errors.Is(err, ErrRecovery) {
		if next != nil {
			next.Close()
		}
		t.Fatal("same identity rebound to another Store")
	}
}

func TestRootReplacementCannotBeConfirmed(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore)}
	hooks := runtime.hooks()
	verify := hooks.Verify
	moved := o.Root + "-moved"
	t.Cleanup(func() { os.RemoveAll(moved) })
	hooks.Verify = func(ctx context.Context, value StoreSnapshot) (Verification, error) {
		if err := os.Rename(o.Root, moved); err != nil {
			return Verification{}, err
		}
		if err := os.Mkdir(o.Root, 0700); err != nil {
			return Verification{}, err
		}
		if err := os.WriteFile(o.StorePath, []byte("external replacement"), 0600); err != nil {
			return Verification{}, err
		}
		return verify(ctx, value)
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("replace-root"))
	result, err := e.Apply(context.Background(), "replace-root")
	if err == nil || result.State == Confirmed || result.State == RolledBack {
		t.Fatalf("detached root falsely confirmed: %#v %v", result, err)
	}
	disk(t, o.StorePath, StoreSnapshot{Exists: true, Bytes: []byte("external replacement")})
}

func TestRollbackDoesNotAcceptLateVerification(t *testing.T) {
	o := fixture(t)
	o.RollbackTimeout = 40 * time.Millisecond
	e := opened(t, o)
	runtime := &memoryRuntime{current: cloneSnapshot(oldStore), failNew: true}
	hooks := runtime.hooks()
	verify := hooks.Verify
	hooks.Verify = func(ctx context.Context, value StoreSnapshot) (Verification, error) {
		if Digest(value) == Digest(oldStore) {
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
		}
		return verify(ctx, value)
	}
	if err := e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("late-verify"))
	result, err := e.Apply(context.Background(), "late-verify")
	if !errors.Is(err, ErrOutcomeUnknown) || result.State != RollbackFailed || result.ErrorCode != "rollback_timeout" {
		t.Fatalf("late health accepted: %#v %v", result, err)
	}
}

func TestSnapshotCorruptionAndMissingIdentityDoNotLoseHistory(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	prepare(t, e, request("history"))
	if _, err := e.Rollback(context.Background(), "history"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(o.Root, "identity.json")); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(o); !errors.Is(err, ErrRecovery) {
		if other != nil {
			other.Close()
		}
		t.Fatal("lost identity silently recreated over recovery history")
	}
}
