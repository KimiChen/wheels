package managed

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
)

func prepareV3Payload(ctx context.Context, source CheckpointManifestV2, sourcePayload []checkpointPayload, sourceManifest []byte, root string, relocate ContextRelocatorV3) (CheckpointManifestV2, []checkpointPayload, RelocationContextV3, error) {
	var target RelocationContextV3
	if relocate == nil || !canonicalAbsolute(root) || filepath.Base(root) != "managed" {
		return source, nil, target, ErrInvalid
	}
	variants, err := variantsFromPayload(sourcePayload)
	if err != nil {
		return source, nil, target, err
	}
	files, graph := v2ContextFiles(source, sourcePayload)
	target, err = relocate(ctx, source, files, variants, graph, v3Policy(sourcePayload))
	if err != nil {
		return source, nil, target, err
	}
	capture := func(context.Context, []StoreVariant) (ContextSnapshotV3, error) { return target.Context, nil }
	_, contextPayload, sources, err := captureContextV3(ctx, root, capture, variants)
	if err != nil {
		return source, nil, target, err
	}
	changed := root != source.Root || target.Context.Revision != source.ContextRevision || !bytesEqual(target.Context.PolicyBytes, v3Policy(sourcePayload))
	history, err := contextHistoryPayload(sourcePayload, changed)
	if err != nil {
		return source, nil, target, err
	}
	if changed {
		history, err = preserveRelocationOrigin(history, sourceManifest, sourcePayload)
		if err != nil {
			return source, nil, target, err
		}
	}
	payload := append([]checkpointPayload{}, contextPayload...)
	for _, file := range sourcePayload {
		if !strings.HasPrefix(file.entry.Path, "managed/") || file.entry.Path == "managed/restore.json" || file.entry.Path == "managed/"+restorePlanName || file.entry.Path == "managed/"+contextHistoryName || file.entry.Path == "managed/store.json" {
			continue
		}
		payload = append(payload, file)
	}
	stamp := int64(0)
	for _, file := range contextPayload {
		if file.entry.Path == "GRAPH.json" {
			stamp = file.entry.ModifiedNS
		}
	}
	if target.Store.Exists {
		payload = append(payload, checkpointPayload{CheckpointFile{Path: "managed/store.json", Size: int64(len(target.Store.Bytes)), SHA256: checkpointHash(target.Store.Bytes), ModifiedNS: stamp}, append([]byte(nil), target.Store.Bytes...)})
	}
	if history != nil {
		payload = append(payload, checkpointPayload{CheckpointFile{Path: "managed/" + contextHistoryName, Size: int64(len(history)), SHA256: checkpointHash(history), ModifiedNS: stamp}, history})
	}
	sort.Slice(payload, func(i, j int) bool { return payload[i].entry.Path < payload[j].entry.Path })
	m := source
	m.Root = root
	m.ConfigFile = target.Context.ConfigFile
	m.WorkingDir = target.Context.WorkingDir
	m.ContextRevision = target.Context.Revision
	m.StoreExists = target.Store.Exists
	m.StoreDigest = Digest(target.Store)
	m.GraphSHA256 = checkpointHash(target.Context.GraphBytes)
	m.PolicySHA256 = checkpointHash(target.Context.PolicyBytes)
	m.ContextSources = sources
	m.Files = []CheckpointFile{}
	var total int64
	for _, file := range payload {
		m.Files = append(m.Files, file.entry)
		total += file.entry.Size
	}
	if len(payload) > MaxCheckpointFiles || total > MaxCheckpointBytes {
		return m, nil, target, ErrInvalid
	}
	if err = validateManagedCheckpointPayload(m.CheckpointManifest, payload); err != nil {
		return m, nil, target, err
	}
	return m, payload, target, nil
}

func newInstallPlanV3(m CheckpointManifestV2, payload []checkpointPayload, installation, root *privateDir, digest string, target RelocationContextV3) (restoreInstallPlan, error) {
	// Use the v2 planner for managed ownership, identity rotation, stale-file
	// removal and original target CAS; context paths are explicitly substituted.
	local := m
	local.ContextSources = nil
	flat := []checkpointPayload{}
	context := []checkpointPayload{}
	for _, file := range payload {
		if strings.HasPrefix(file.entry.Path, "context/") {
			context = append(context, file)
		} else if file.entry.Path != "POLICY.json" {
			flat = append(flat, file)
		}
	}
	for _, source := range m.ContextSources {
		if source.Path == m.ConfigFile || source.Path == filepath.Join(m.WorkingDir, "installation.json") {
			for _, file := range context {
				if file.entry.Path == source.Payload {
					copy := file
					copy.entry.Path = "context/" + filepath.Base(source.Path)
					flat = append(flat, copy)
				}
			}
		}
	}
	plan, err := newInstallPlan(local, flat, installation, root, digest)
	if err != nil {
		return plan, err
	}
	plan.Version = 2
	plan.TargetRoots = append([]string(nil), target.TargetRoots...)
	plan.TargetFiles = append([]string(nil), target.TargetFiles...)
	entries := plan.Entries[:0]
	for _, entry := range plan.Entries {
		if strings.HasPrefix(entry.Target, "managed/") {
			entries = append(entries, entry)
		}
	}
	plan.Entries = entries
	byPayload := map[string]string{}
	for _, source := range m.ContextSources {
		byPayload[source.Payload] = source.Path
	}
	for _, file := range context {
		path := byPayload[file.entry.Path]
		if pathWithin(path, root.path) || !plan.validTarget(path) {
			return plan, ErrUnsafePath
		}
		old, stamp, err := planReadTarget(installation, plan, path, MaxCheckpointFileBytes)
		if err != nil {
			return plan, err
		}
		plan.Entries = append(plan.Entries, restorePlanEntry{Target: path, Payload: file.entry.Path, OldDigest: Digest(old), OldModifiedNS: stamp, NewDigest: Digest(StoreSnapshot{Exists: true, Bytes: file.data}), NewModifiedNS: file.entry.ModifiedNS, NewExists: true})
	}
	sort.Slice(plan.Entries, func(i, j int) bool { return plan.Entries[i].Target < plan.Entries[j].Target })
	if !plan.valid() {
		return plan, ErrRecovery
	}
	return plan, nil
}

func checkV3IncludeTargets(plan restoreInstallPlan, installation *privateDir, graph []byte) error {
	// Native live context verification repeats complete includes; preflight below
	// additionally refuses any target match absent from the sealed graph.
	return checkMappedIncludeTargets(plan, installation, graph)
}

func validateInstallPlanTargetsV3(m CheckpointManifestV2, payload []checkpointPayload, installation *privateDir, plan restoreInstallPlan) error {
	expected := map[string]checkpointPayload{}
	sources := map[string]string{}
	for _, v := range m.ContextSources {
		sources[v.Payload] = v.Path
	}
	for _, file := range payload {
		if file.entry.Path == "GRAPH.json" || file.entry.Path == "POLICY.json" {
			continue
		}
		target := file.entry.Path
		if v, ok := sources[target]; ok {
			target = v
		}
		expected[target] = file
	}
	if !m.StoreExists {
		expected["managed/store.json"] = checkpointPayload{}
	}
	seen := map[string]bool{}
	for _, entry := range plan.Entries {
		file, ok := expected[entry.Target]
		if !ok {
			if entry.Target != "managed/"+contextHistoryName && !strings.HasPrefix(entry.Target, "managed/operations/") && !strings.HasPrefix(entry.Target, "managed/secrets/") {
				return ErrRecovery
			}
			if entry.NewExists {
				return ErrRecovery
			}
		} else {
			want := StoreSnapshot{Exists: file.entry.Path != "", Bytes: file.data}
			if entry.Target == "managed/identity.json" {
				want = StoreSnapshot{Exists: true, Bytes: v2Identity(plan.ServiceID)}
			}
			if Digest(want) != entry.NewDigest || entry.NewExists != want.Exists || entry.Payload != file.entry.Path && entry.Target != "managed/identity.json" {
				return ErrRecovery
			}
		}
		if seen[entry.Target] {
			return ErrRecovery
		}
		seen[entry.Target] = true
		current, stamp, err := planReadTarget(installation, plan, entry.Target, v2TargetLimit(entry))
		if err != nil {
			return err
		}
		if (Digest(current) != entry.OldDigest || stamp != entry.OldModifiedNS) && (Digest(current) != entry.NewDigest || stamp != entry.NewModifiedNS) {
			return ErrConflict
		}
	}
	for target := range expected {
		if !seen[target] {
			return ErrRecovery
		}
	}
	return nil
}

// ConfirmRestorationV3 preserves the existing two-party receipt and final
// runtime gate. The supplied capture uses administrator-authorized target roots.
func ConfirmRestorationV3(ctx context.Context, options Options, epoch, digest string, capture ContextCaptureV3) (RestoreSummary, error) {
	var out RestoreSummary
	lease, err := OpenOfflineLease(options)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	marker, err := readRestoreMarker(lease.root)
	if err != nil || marker == nil || marker.CheckpointVersion != 3 || marker.State != "acknowledged" || marker.Activated || marker.Epoch != epoch || marker.ManifestDigest != digest || !marker.RuntimeLoaded || !marker.ResourcesReady {
		return out, ErrConflict
	}
	if err = checkRestoreV2Sources(lease.root, marker); err != nil {
		return out, err
	}
	variants, engine, err := lease.variantsForContext()
	if err != nil || engine.active != "" {
		return out, ErrRecovery
	}
	actual, _, _, err := captureContextV3(ctx, options.Root, capture, variants)
	if err != nil || actual.Revision != marker.ContextRevision {
		return out, ErrConflict
	}
	current, err := engine.readStore()
	if err != nil || Digest(current) != marker.StoreDigest {
		return out, ErrConflict
	}
	id, err := lease.root.read("identity.json", 1024)
	if err != nil || !bytesEqual(id.Bytes, v2Identity(marker.ServiceID)) {
		return out, ErrConflict
	}
	if err = checkRestoreV2Sources(lease.root, marker); err != nil {
		return out, err
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	marker.State = "confirmed"
	if err = writeRestoreMarker(lease.root, marker); err != nil {
		return out, err
	}
	return RestoreSummary{Code: "ok", Epoch: marker.Epoch, ServiceID: marker.ServiceID, ManifestDigest: digest, State: "confirmed"}, nil
}
