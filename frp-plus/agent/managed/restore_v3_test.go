//go:build linux || darwin

package managed

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureV3(t *testing.T, terminalRecord bool) (Options, ContextCaptureV3, string, CheckpointSummary, Operation) {
	t.Helper()
	o, base, _, out := restoreFixtureV2(t)
	capture := func(ctx context.Context, v []StoreVariant) (ContextSnapshotV3, error) {
		s, err := base(ctx, v)
		return ContextSnapshotV3{ContextSnapshotV2: s, PolicyBytes: []byte(`{"synthetic":"source-policy"}`)}, err
	}
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	var op Operation
	if terminalRecord {
		runtime := &memoryRuntime{current: oldStore}
		if err = e.AttachRuntime(runtime.hooks()); err != nil {
			t.Fatal(err)
		}
		op = confirmed(t, e, "source-operation", oldStore, newStore)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := ExportCheckpointV3(context.Background(), o, capture, out)
	if err != nil {
		t.Fatal(err)
	}
	return o, capture, out, summary, op
}
func relocatedFixtureV3(t *testing.T, source Options) (Options, ContextCaptureV3, ContextRelocatorV3) {
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
	target := source
	target.Root = root
	target.StorePath = filepath.Join(root, "store.json")
	var prepared ContextSnapshotV3
	relocate := func(ctx context.Context, m CheckpointManifestV2, files []ContextFile, variants []StoreVariant, graph, policy []byte) (RelocationContextV3, error) {
		prepared = ContextSnapshotV3{ContextSnapshotV2: ContextSnapshotV2{ConfigFile: filepath.Join(parent, "agent.toml"), WorkingDir: parent, Revision: idHash(parent), GraphBytes: append([]byte(nil), graph...)}, PolicyBytes: []byte(`{"synthetic":"target-policy"}`)}
		for _, file := range files {
			file.Path = filepath.Join(parent, strings.TrimPrefix(file.Path, filepath.Dir(source.Root)+string(filepath.Separator)))
			file.Bytes = append([]byte(nil), file.Bytes...)
			prepared.Files = append(prepared.Files, file)
		}
		var store StoreSnapshot
		for _, v := range variants {
			if v.Path == "managed/store.json" {
				store = cloneSnapshot(v.Snapshot)
			}
		}
		return RelocationContextV3{Context: prepared, Store: store, TargetRoots: []string{parent}}, ctx.Err()
	}
	capture := func(ctx context.Context, _ []StoreVariant) (ContextSnapshotV3, error) {
		out := prepared
		out.Files = nil
		for _, file := range prepared.Files {
			data, err := os.ReadFile(file.Path)
			if err != nil {
				return out, err
			}
			info, err := os.Stat(file.Path)
			if err != nil {
				return out, err
			}
			file.Bytes = data
			file.ModifiedNS = info.ModTime().UnixNano()
			out.Files = append(out.Files, file)
		}
		return out, ctx.Err()
	}
	return target, capture, relocate
}
func activateV3(t *testing.T, o Options, capture ContextCaptureV3, installed RestoreSummary, current StoreSnapshot) {
	t.Helper()
	ctx := context.Background()
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &memoryRuntime{current: current}
	if err = e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return e.RestoreStatus().State == "verified" })
	status := e.RestoreStatus()
	if _, err = e.AcknowledgeRestore(ctx, status.Epoch, status.ManifestDigest, status.ContextRevision, status.StoreDigest, "33333333-3333-4333-8333-333333333333"); err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if version, err := RestoreCheckpointVersion(o.Root); err != nil || version != 3 {
		t.Fatal("v3 dispatch", version, err)
	}
	if _, err = ConfirmRestorationV3(ctx, o, installed.Epoch, installed.ManifestDigest, capture); err != nil {
		t.Fatal(err)
	}
	e, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.AttachRuntime(runtime.hooks()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return !e.ManagementWriteBlocked() })
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestRelocationV3LifecycleSealedHistoryAndRebackup(t *testing.T) {
	ctx := context.Background()
	source, _, out, summary, op := fixtureV3(t, true)
	original, err := os.ReadFile(filepath.Join(source.Root, "operations", idHash(op.ID)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	target, capture, relocate := relocatedFixtureV3(t, source)
	installed, err := InstallCheckpointV3(ctx, out, target.Root, summary.ManifestDigest, capture, relocate)
	if err != nil {
		t.Fatal("install", err)
	}
	again, err := InstallCheckpointV3(ctx, out, target.Root, summary.ManifestDigest, capture, relocate)
	if err != nil || again != installed {
		t.Fatal("idempotency", err)
	}
	activateV3(t, target, capture, installed, newStore)
	e, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Query(ctx, op.ID)
	if err != nil || got.State != Confirmed || got.MaterialsState != MaterialsContextChanged {
		t.Fatal("history", got, err)
	}
	if _, err = e.Rollback(ctx, op.ID); !errors.Is(err, ErrContextChanged) {
		t.Fatal("rollback not refused", err)
	}
	if e.running || e.active != "" {
		t.Fatal("context refusal retained lease")
	}
	if data, _ := os.ReadFile(filepath.Join(target.Root, "operations", idHash(op.ID)+".json")); !bytesEqual(data, original) {
		t.Fatal("journal rewritten")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	next := out + "-target"
	second, err := ExportCheckpointV3(ctx, target, capture, next)
	if err != nil {
		t.Fatal("rebackup", err)
	}
	validator := func(context.Context, CheckpointManifestV2, []ContextFile, []StoreVariant, []byte, []byte) error {
		return nil
	}
	if _, err = ValidateCheckpointV3(ctx, next, second.ManifestDigest, validator); err != nil {
		t.Fatal("sealed target", err)
	}
	// Sidecar is exact proof material. It cannot disappear while its plan remains.
	if err = os.Remove(filepath.Join(target.Root, contextHistoryName)); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(target); err == nil {
		opened.Close()
		t.Fatal("missing provenance accepted")
	}
}
func TestRelocationV3CrashCASAndAuthorization(t *testing.T) {
	for _, mode := range []string{"plan_persisted", "marker_persisted", "before_pending", "external_write", "changed_policy"} {
		t.Run(mode, func(t *testing.T) {
			source, _, out, summary, _ := fixtureV3(t, false)
			target, capture, relocate := relocatedFixtureV3(t, source)
			ctx := context.Background()
			boom := errors.New("synthetic interruption")
			stop := mode
			if mode == "external_write" || mode == "changed_policy" {
				stop = "plan_persisted"
			}
			_, err := installCheckpointV3(ctx, out, target.Root, summary.ManifestDigest, capture, relocate, func(stage string) error {
				if stage == stop {
					return boom
				}
				return nil
			})
			if !errors.Is(err, boom) {
				t.Fatal("hook", err)
			}
			if mode == "external_write" {
				if err = os.WriteFile(filepath.Join(filepath.Dir(target.Root), "agent.toml"), []byte("concurrent change"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "changed_policy" {
				base := relocate
				relocate = func(ctx context.Context, m CheckpointManifestV2, f []ContextFile, v []StoreVariant, g, p []byte) (RelocationContextV3, error) {
					r, e := base(ctx, m, f, v, g, p)
					r.TargetRoots = append(r.TargetRoots, filepath.Join(filepath.Dir(target.Root), "extra"))
					return r, e
				}
			}
			_, err = InstallCheckpointV3(ctx, out, target.Root, summary.ManifestDigest, capture, relocate)
			if mode == "external_write" || mode == "changed_policy" {
				if err == nil {
					t.Fatal("changed CAS/authorization accepted")
				}
			} else if err != nil {
				t.Fatal("resume", err)
			}
		})
	}
}
func TestRelocationV3RejectsActiveSourceBeforeTargetWrites(t *testing.T) {
	source, base, _, _ := restoreFixtureV2(t)
	e, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	prepare(t, e, request("active"))
	crashEngine(e)
	capture := func(ctx context.Context, v []StoreVariant) (ContextSnapshotV3, error) {
		s, err := base(ctx, v)
		return ContextSnapshotV3{ContextSnapshotV2: s, PolicyBytes: []byte(`{}`)}, err
	}
	out := filepath.Join(filepath.Dir(source.Root), "..", filepath.Base(filepath.Dir(source.Root))+"-active-checkpoint")
	out = filepath.Clean(out)
	t.Cleanup(func() { os.RemoveAll(out) })
	summary, err := ExportCheckpointV3(context.Background(), source, capture, out)
	if err != nil {
		t.Fatal(err)
	}
	target, targetCapture, relocate := relocatedFixtureV3(t, source)
	_, err = InstallCheckpointV3(context.Background(), out, target.Root, summary.ManifestDigest, targetCapture, relocate)
	if !errors.Is(err, ErrBusy) {
		t.Fatal("active source", err)
	}
	names, _ := os.ReadDir(target.Root)
	if len(names) != 0 {
		t.Fatal("target mutated before source refusal")
	}
}
func TestContextHistoryRejectsEmptyOrAlteredAndKeepsTTLState(t *testing.T) {
	o := fixture(t)
	e := opened(t, o)
	rt := &memoryRuntime{current: oldStore}
	if err := e.AttachRuntime(rt.hooks()); err != nil {
		t.Fatal(err)
	}
	op := confirmed(t, e, "history", oldStore, newStore)
	r := e.records[op.ID]
	raw, _ := json.Marshal(contextHistory{Version: 1, Entries: []contextHistoryEntry{historyEntry(*r)}})
	for _, value := range [][]byte{[]byte{}, []byte(`{}`), []byte(`{"version":1,"entries":null}`)} {
		if _, err := decodeContextHistory(value, e.records); err == nil {
			t.Fatal("invalid provenance accepted")
		}
	}
	if _, err := decodeContextHistory(nil, e.records); err != nil {
		t.Fatal("absent provenance", err)
	}
	ids, err := decodeContextHistory(raw, e.records)
	if err != nil || !ids[op.ID] {
		t.Fatal("valid provenance", err)
	}
	altered := *r
	altered.NewDigest = idHash("altered")
	if _, err = decodeContextHistory(raw, map[string]*record{op.ID: &altered}); err == nil {
		t.Fatal("changed record accepted")
	}
	r.contextChanged = true
	r.MaterialsState = MaterialsExpired
	if visibleOperation(r).MaterialsState != MaterialsExpired {
		t.Fatal("real expiry hidden by relocation")
	}
}

func TestCheckpointV3SameContextKeepsPendingRecoveryMaterial(t *testing.T) {
	o, base, _, out := restoreFixtureV2(t)
	e, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	op := prepare(t, e, request("pending-source"))
	original := operationJournal(t, e, op.ID)
	crashEngine(e)
	capture := func(ctx context.Context, v []StoreVariant) (ContextSnapshotV3, error) {
		s, err := base(ctx, v)
		return ContextSnapshotV3{ContextSnapshotV2: s, PolicyBytes: []byte(`{}`)}, err
	}
	summary, err := ExportCheckpointV3(context.Background(), o, capture, out)
	if err != nil {
		t.Fatal(err)
	}
	relocate := func(ctx context.Context, m CheckpointManifestV2, files []ContextFile, variants []StoreVariant, graph, policy []byte) (RelocationContextV3, error) {
		var store StoreSnapshot
		for _, v := range variants {
			if v.Path == "managed/store.json" {
				store = v.Snapshot
			}
		}
		snapshot := ContextSnapshotV3{ContextSnapshotV2: ContextSnapshotV2{ConfigFile: m.ConfigFile, WorkingDir: m.WorkingDir, Revision: m.ContextRevision, Files: files, GraphBytes: graph}, PolicyBytes: policy}
		return RelocationContextV3{Context: snapshot, Store: store, TargetRoots: []string{m.WorkingDir}}, nil
	}
	restored, err := InstallCheckpointV3(context.Background(), out, o.Root, summary.ManifestDigest, capture, relocate)
	if err != nil || restored.State != "pending" {
		t.Fatal("same-context recovery", err)
	}
	got, err := os.ReadFile(filepath.Join(o.Root, "operations", idHash(op.ID)+".json"))
	if err != nil || !bytesEqual(got, original) {
		t.Fatal("pending recovery journal changed")
	}
	if _, err = os.Stat(filepath.Join(o.Root, contextHistoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("same-context recovery falsely marked relocated")
	}
}
