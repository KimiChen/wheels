//go:build linux || darwin

package managed

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
)

func restoreFixtureV2(t *testing.T) (Options, ContextCaptureV2, ContextValidatorV2, string) {
	t.Helper()
	opts, base, _, _ := restoreFixture(t)
	path := filepath.Join(filepath.Dir(opts.Root), "credentials", "plugin.key")
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("synthetic-private-plugin-key"), 0600); err != nil {
		t.Fatal(err)
	}
	capture := func(ctx context.Context, _ []StoreVariant) (ContextSnapshotV2, error) {
		old, err := base(ctx)
		if err != nil {
			return ContextSnapshotV2{}, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return ContextSnapshotV2{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return ContextSnapshotV2{}, err
		}
		old.Files = append(old.Files, ContextFile{Path: path, Bytes: data, ModifiedNS: info.ModTime().UnixNano()})
		graph, err := backupmanifest.Encode(backupmanifest.Manifest{Version: 2, Kind: backupmanifest.Kind, PolicyDigest: idHash("fixture-policy"), ConfigFile: old.ConfigFile, WorkingDir: old.WorkingDir, StoreFile: opts.StorePath, Files: []backupmanifest.File{{Path: old.ConfigFile}}, Directories: []backupmanifest.Directory{}, Includes: []backupmanifest.Include{}, References: []backupmanifest.Reference{}})
		return ContextSnapshotV2{ConfigFile: old.ConfigFile, WorkingDir: old.WorkingDir, Revision: old.Revision, GraphBytes: graph, Files: old.Files}, err
	}
	validate := func(context.Context, CheckpointManifestV2, []ContextFile, []StoreVariant, []byte) error { return nil }
	outputParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(outputParent, 0700); err != nil {
		t.Fatal(err)
	}
	return opts, capture, validate, filepath.Join(outputParent, "checkpoint")
}
func initializedCheckpointV2(t *testing.T) (Options, ContextCaptureV2, ContextValidatorV2, string, CheckpointSummary) {
	t.Helper()
	o, c, v, out := restoreFixtureV2(t)
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.PutSecret(context.Background(), "11111111-1111-4111-8111-111111111111", "synthetic-vault-secret"); err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := ExportCheckpointV2(context.Background(), o, c, out)
	if err != nil {
		t.Fatal(err)
	}
	return o, c, v, out, summary
}

func TestCheckpointV2LifecycleAndPrivateSourceSeal(t *testing.T) {
	o, capture, validate, out, summary := initializedCheckpointV2(t)
	ctx := context.Background()
	if version, err := CheckpointVersion(out, summary.ManifestDigest); err != nil || version != 2 {
		t.Fatal("version", version, err)
	}
	if _, err := ValidateCheckpointV2(ctx, out, summary.ManifestDigest, validate); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(filepath.Dir(o.Root), "credentials", "plugin.key")
	before, _ := os.Stat(key)
	want, _ := os.ReadFile(key)
	lockBefore, _ := os.Stat(filepath.Join(o.Root, ".lock"))
	if err := os.WriteFile(key, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	installed, err := InstallCheckpointV2(ctx, out, o.Root, summary.ManifestDigest, capture, validate)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(key)
	got, _ := os.ReadFile(key)
	lockAfter, _ := os.Stat(filepath.Join(o.Root, ".lock"))
	if !bytesEqual(want, got) || before.ModTime() != after.ModTime() || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("restore lost source metadata or lock")
	}
	again, err := InstallCheckpointV2(ctx, out, o.Root, summary.ManifestDigest, capture, validate)
	if err != nil || again != installed {
		t.Fatal("retry changed epoch", err)
	}
	if _, err = ConfirmRestorationV2(ctx, o, installed.Epoch, summary.ManifestDigest, capture); err == nil {
		t.Fatal("unverified confirmation")
	}
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &memoryRuntime{current: oldStore}
	if err = e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return e.RestoreStatus().State == "verified" })
	status := e.RestoreStatus()
	if err = os.WriteFile(key, []byte("drift-after-native-verification"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AcknowledgeRestore(ctx, status.Epoch, status.ManifestDigest, status.ContextRevision, status.StoreDigest, "33333333-3333-4333-8333-333333333333"); !errors.Is(err, ErrConflict) {
		t.Fatal("source seal did not reject ack", err)
	}
	if err = os.WriteFile(key, want, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(key, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AcknowledgeRestore(ctx, status.Epoch, status.ManifestDigest, status.ContextRevision, status.StoreDigest, "33333333-3333-4333-8333-333333333333"); err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = ConfirmRestorationV2(ctx, o, installed.Epoch, summary.ManifestDigest, capture); err != nil {
		t.Fatal(err)
	}
	e, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if !e.ManagementWriteBlocked() {
		t.Fatal("restart verification skipped")
	}
	if err = e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return !e.ManagementWriteBlocked() })
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := ExportCheckpointV2(ctx, o, capture, out+"-second")
	if err != nil {
		t.Fatal("rebackup", err)
	}
	if _, err = ValidateCheckpointV2(ctx, out+"-second", next.ManifestDigest, validate); err != nil {
		t.Fatal("rebackup validation", err)
	}
}

func TestCheckpointV2InterruptedInstallCAS(t *testing.T) {
	for _, stage := range []string{"plan_persisted", "marker_persisted", "target:credentials/plugin.key:after_file_sync", "target:credentials/plugin.key:after_rename", "before_pending"} {
		t.Run(stage, func(t *testing.T) {
			o, c, v, out, summary := initializedCheckpointV2(t)
			ctx := context.Background()
			key := filepath.Join(filepath.Dir(o.Root), "credentials", "plugin.key")
			if err := os.WriteFile(key, []byte("old-target"), 0600); err != nil {
				t.Fatal(err)
			}
			fired := false
			_, err := installCheckpointV2(ctx, out, o.Root, summary.ManifestDigest, c, v, func(at string) error {
				if at == stage {
					fired = true
					return ErrStorage
				}
				return nil
			})
			if err == nil || !fired {
				t.Fatal("fault missed", err)
			}
			if engine, err := Open(o); err == nil {
				engine.Close()
				t.Fatal("incomplete install started")
			}
			root, err := openExistingPrivateDir(o.Root)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := readRestorePlan(root, "")
			root.close()
			if err != nil {
				t.Fatal(err)
			}
			installed, err := InstallCheckpointV2(ctx, out, o.Root, summary.ManifestDigest, c, v)
			if err != nil {
				t.Fatal("retry", err)
			}
			if installed.Epoch != plan.Epoch || installed.ServiceID != plan.ServiceID {
				t.Fatal("retry changed identity")
			}
		})
	}
}

func TestCheckpointV2InterruptedInstallExternalDrift(t *testing.T) {
	o, c, v, out, summary := initializedCheckpointV2(t)
	ctx := context.Background()
	_, err := installCheckpointV2(ctx, out, o.Root, summary.ManifestDigest, c, v, func(at string) error {
		if at == "marker_persisted" {
			return ErrStorage
		}
		return nil
	})
	if err == nil {
		t.Fatal("no interruption")
	}
	key := filepath.Join(filepath.Dir(o.Root), "credentials", "plugin.key")
	foreign := []byte("foreign-do-not-overwrite")
	if err = os.WriteFile(key, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = InstallCheckpointV2(ctx, out, o.Root, summary.ManifestDigest, c, v); !errors.Is(err, ErrConflict) {
		t.Fatal("drift", err)
	}
	got, _ := os.ReadFile(key)
	if !bytesEqual(got, foreign) {
		t.Fatal("overwrote drift")
	}
}

func TestRestoreV1MarkerEncodingUnchanged(t *testing.T) {
	marker := restoreMarker{Version: 1, RestoreStatus: RestoreStatus{State: "pending"}}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		t.Fatal("decode")
	}
	if fields["checkpoint_version"] != nil || fields["plan_digest"] != nil {
		t.Fatal("v1 marker changed")
	}
}

func TestCheckpointV2OrphanPlanDoesNotCreateIdentity(t *testing.T) {
	o, c, v, out, summary := initializedCheckpointV2(t)
	identity := filepath.Join(o.Root, "identity.json")
	if err := os.Remove(identity); err != nil {
		t.Fatal(err)
	}
	_, err := installCheckpointV2(context.Background(), out, o.Root, summary.ManifestDigest, c, v, func(stage string) error {
		if stage == "plan_persisted" {
			return ErrStorage
		}
		return nil
	})
	if err == nil {
		t.Fatal("interruption missed")
	}
	if engine, err := Open(o); err == nil {
		engine.Close()
		t.Fatal("orphan plan started")
	}
	if _, err := os.Stat(identity); !os.IsNotExist(err) {
		t.Fatal("failed startup created identity")
	}
	if _, err := InstallCheckpointV2(context.Background(), out, o.Root, summary.ManifestDigest, c, v); err != nil {
		t.Fatal("retry", err)
	}
}
