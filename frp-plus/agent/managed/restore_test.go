//go:build linux || darwin

package managed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func restoreFixture(t *testing.T) (Options, ContextCapture, ContextValidator, string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "managed")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"agent.toml": "synthetic config", "installation.json": "{}", "agent.token": "synthetic secret"} {
		if err = os.WriteFile(filepath.Join(parent, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{Root: root, StorePath: filepath.Join(root, "store.json"), RollbackTimeout: time.Second, CloseTimeout: time.Second, RecoveryCheck: func(context.Context, Operation) error { return nil }, RestoreCheck: func(context.Context, string) error { return nil }}
	if err = os.WriteFile(opts.StorePath, oldStore.Bytes, 0600); err != nil {
		t.Fatal(err)
	}
	capture := func(ctx context.Context) (ContextSnapshot, error) {
		result := ContextSnapshot{ConfigFile: filepath.Join(parent, "agent.toml"), WorkingDir: parent, Revision: idHash("context")}
		for _, name := range []string{"agent.toml", "installation.json", "agent.token"} {
			path := filepath.Join(parent, name)
			data, err := os.ReadFile(path)
			if err != nil {
				return result, err
			}
			info, err := os.Stat(path)
			if err != nil {
				return result, err
			}
			result.Files = append(result.Files, ContextFile{Path: path, Bytes: data, ModifiedNS: info.ModTime().UnixNano()})
		}
		return result, ctx.Err()
	}
	validate := func(context.Context, CheckpointManifest, []ContextFile) error { return nil }
	return opts, capture, validate, filepath.Join(parent, "checkpoint")
}
func crashEngine(e *Engine) { e.mu.Lock(); e.release(); e.mu.Unlock() }

func TestRestoreOfflineLifecycleRetainsLockAndRequiresTwoConfirmations(t *testing.T) {
	o, capture, validate, out := restoreFixture(t)
	ctx := context.Background()
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	oldID := e.ServiceID()
	if err = e.PutSecret(ctx, "11111111-1111-4111-8111-111111111111", "synthetic-vault-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err = ExportCheckpoint(ctx, o, capture, out); !errors.Is(err, ErrLocked) {
		t.Fatalf("live export: %v", err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(filepath.Join(o.Root, ".lock"))
	summary, err := ExportCheckpoint(ctx, o, capture, out)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Code != "ok" || summary.FileCount < 5 || summary.TotalBytes == 0 {
		t.Fatal("incomplete checkpoint")
	}
	if _, err = ValidateCheckpoint(ctx, out, summary.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(o.StorePath, newStore.Bytes, 0600); err != nil {
		t.Fatal(err)
	}
	install, err := InstallCheckpoint(ctx, out, o.Root, summary.ManifestDigest, capture, validate)
	if err != nil {
		t.Fatal(err)
	}
	if install.State != "pending" || install.ServiceID == oldID {
		t.Fatal("restore identity not rotated")
	}
	after, _ := os.Stat(filepath.Join(o.Root, ".lock"))
	if !os.SameFile(before, after) {
		t.Fatal("lifetime lock inode replaced")
	}
	disk(t, o.StorePath, oldStore)
	if _, err = ConfirmRestoration(ctx, o, install.Epoch, summary.ManifestDigest, capture); err == nil {
		t.Fatal("confirmed without native verification or remote acknowledgement")
	}
	e, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if !e.ManagementWriteBlocked() {
		t.Fatal("pending restore allowed writes")
	}
	if err = e.PutSecret(ctx, "22222222-2222-4222-8222-222222222222", "secret"); err == nil {
		t.Fatal("pending restore allowed secret write")
	}
	if _, err = e.Prepare(ctx, request("restore-denied")); err == nil {
		t.Fatal("pending restore allowed prepare")
	}
	runtime := &memoryRuntime{current: oldStore}
	if err = e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return e.RestoreStatus().State == "verified" })
	status := e.RestoreStatus()
	ack := "33333333-3333-4333-8333-333333333333"
	if _, err = e.AcknowledgeRestore(ctx, status.Epoch, status.ManifestDigest, status.ContextRevision, idHash("wrong"), ack); !errors.Is(err, ErrConflict) {
		t.Fatal("ack ignored store CAS")
	}
	if _, err = e.AcknowledgeRestore(ctx, status.Epoch, status.ManifestDigest, status.ContextRevision, status.StoreDigest, ack); err != nil {
		t.Fatal(err)
	}
	if !e.ManagementWriteBlocked() {
		t.Fatal("remote acknowledgement opened gate")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	confirmed, err := ConfirmRestoration(ctx, o, install.Epoch, summary.ManifestDigest, capture)
	if err != nil || confirmed.State != "confirmed" {
		t.Fatalf("offline confirmation failed: %v", err)
	}
	e, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if !e.ManagementWriteBlocked() {
		t.Fatal("offline confirmation skipped restart verification")
	}
	if current := e.RestoreStatus(); current.State != "pending" || current.RuntimeLoaded || current.ResourcesReady || current.StoreDigest != "" || current.AcknowledgementID != "" {
		t.Fatal("new boot exposed historical restoration facts as fresh")
	}
	if _, err = e.AcknowledgeRestore(ctx, status.Epoch, status.ManifestDigest, status.ContextRevision, status.StoreDigest, ack); !errors.Is(err, ErrBusy) {
		t.Fatal("new boot accepted stale verification receipt")
	}

	if err = e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return !e.ManagementWriteBlocked() })
	if err = e.PutSecret(ctx, "22222222-2222-4222-8222-222222222222", "secret"); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreCheckpointPreservesUnfinishedJournal(t *testing.T) {
	o, capture, validate, out := restoreFixture(t)
	ctx := context.Background()
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	op := prepare(t, e, request("unfinished"))
	crashEngine(e)
	summary, err := ExportCheckpoint(ctx, o, capture, out)
	if err != nil {
		t.Fatal(err)
	}
	_, err = InstallCheckpoint(ctx, out, o.Root, summary.ManifestDigest, capture, validate)
	if err != nil {
		t.Fatal(err)
	}
	e, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	recovered, err := e.Query(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != RollingBack {
		t.Fatalf("prepared recovery wrong: %s", recovered.State)
	}
	runtime := &memoryRuntime{current: oldStore}
	if err = e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return e.RestoreStatus().State == "verified" })
	if e.RestoreStatus().OperationsCount != 1 {
		t.Fatal("journal inventory lost")
	}
}

func TestRestoreRejectsInvalidClosureBeforeMutation(t *testing.T) {
	o, capture, _, out := restoreFixture(t)
	ctx := context.Background()
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	id := e.ServiceID()
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := ExportCheckpoint(ctx, o, capture, out)
	if err != nil {
		t.Fatal(err)
	}
	reject := func(context.Context, CheckpointManifest, []ContextFile) error { return ErrInvalid }
	if _, err = InstallCheckpoint(ctx, out, o.Root, summary.ManifestDigest, capture, reject); err == nil {
		t.Fatal("unproven dependency accepted")
	}
	if _, err = os.Stat(filepath.Join(o.Root, "restore.json")); !os.IsNotExist(err) {
		t.Fatal("invalid closure mutated target")
	}
	e, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.ServiceID() != id {
		t.Fatal("invalid restore rotated identity")
	}
}

func TestRestorePrivateJSONRejectsCaseAliases(t *testing.T) {
	for _, data := range []string{`{"root":"a","Root":"b"}`, `{"state":"pending","STATE":"confirmed"}`, `{"Version":1}`} {
		var value map[string]any
		if decodeCheckpointJSON([]byte(data), &value) == nil {
			t.Fatal("case alias accepted")
		}
	}
}

func TestRestoreInstallReplyRetryAndVerifyContextDrift(t *testing.T) {
	o, capture, validate, out := restoreFixture(t)
	ctx := context.Background()
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := ExportCheckpoint(ctx, o, capture, out)
	if err != nil {
		t.Fatal(err)
	}
	first, err := InstallCheckpoint(ctx, out, o.Root, backup.ManifestDigest, capture, validate)
	if err != nil {
		t.Fatal(err)
	}
	again, err := InstallCheckpoint(ctx, out, o.Root, backup.ManifestDigest, capture, validate)
	if err != nil || first != again {
		t.Fatal("lost installation reply rotated identity")
	}
	e, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	checks := 0
	verified := make(chan struct{})
	hooks := Runtime{Check: func(context.Context, Operation, string) error {
		checks++
		if checks > 1 {
			return ErrConflict
		}
		return nil
	}, Apply: func(context.Context, StoreSnapshot) error { return nil }, Verify: func(context.Context, StoreSnapshot) (Verification, error) {
		close(verified)
		return Verification{RuntimeLoaded: true, ResourcesReady: true}, nil
	}}
	if err = e.AttachRuntime(hooks); err != nil {
		t.Fatal(err)
	}
	<-verified
	eventually(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return !e.running })
	if e.RestoreStatus().State != "pending" || !e.ManagementWriteBlocked() {
		t.Fatal("context drift during verification opened gate")
	}
}

func TestCheckpointRejectsUnfinishedStoreDrift(t *testing.T) {
	o, capture, _, out := restoreFixture(t)
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("drift"))
	crashEngine(e)
	if err = os.WriteFile(o.StorePath, []byte(`{"proxies":[],"visitors":[],"unexpected":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ExportCheckpoint(context.Background(), o, capture, out); err == nil {
		t.Fatal("unrecoverable Store drift exported")
	}
}
