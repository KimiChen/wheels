package managed

import (
	"context"
	"path/filepath"
	"strings"
)

func InstallCheckpointV3(ctx context.Context, checkpoint, rootPath, digest string, capture ContextCaptureV3, relocate ContextRelocatorV3) (RestoreSummary, error) {
	return installCheckpointV3(ctx, checkpoint, rootPath, digest, capture, relocate, nil)
}
func installCheckpointV3(ctx context.Context, checkpoint, rootPath, digest string, capture ContextCaptureV3, relocate ContextRelocatorV3, hook func(string) error) (RestoreSummary, error) {
	var out RestoreSummary
	source, err := openExistingPrivateDir(checkpoint)
	if err != nil {
		return out, err
	}
	defer source.close()
	original, sourcePayload, err := readCheckpointV2(source, digest)
	if err != nil || original.Version != 3 {
		return out, ErrRecovery
	}
	sourceManifest, _, err := source.readMetadata("CHECKPOINT.json", MaxCheckpointManifestBytes)
	if err != nil || checkpointHash(sourceManifest.Bytes) != digest {
		return out, ErrRecovery
	}
	m, payload, target, err := prepareV3Payload(ctx, original, sourcePayload, sourceManifest.Bytes, rootPath, relocate)
	if err != nil {
		return out, err
	}
	if capture == nil || checkpoint == m.WorkingDir || strings.HasPrefix(checkpoint, m.WorkingDir+string(filepath.Separator)) {
		return out, ErrUnsafePath
	}
	root, err := openExistingPrivateDir(rootPath)
	if err != nil {
		return out, err
	}
	defer root.close()
	lock, err := root.lock()
	if err != nil {
		return out, err
	}
	defer lock.Close()
	installation, err := openExistingPrivateDir(m.WorkingDir)
	if err != nil {
		return out, err
	}
	defer installation.close()
	variants, err := variantsFromPayload(payload)
	if err != nil {
		return out, err
	}
	_, graph := v2ContextFiles(m, payload)
	authorization := restoreInstallPlan{Version: 2, TargetRoots: target.TargetRoots, TargetFiles: target.TargetFiles}
	names, err := root.names()
	if err != nil {
		return out, err
	}
	for _, name := range names {
		switch name {
		case ".lock", "store.json", "identity.json", "operations", "secrets", "restore.json", restorePlanName, contextHistoryName:
		default:
			return out, ErrRecovery
		}
	}
	prior, err := readRestoreMarker(root)
	if err != nil {
		return out, err
	}
	plan, err := readRestorePlan(root, "")
	if err != nil {
		return out, err
	}
	if err = checkV3IncludeTargets(authorization, installation, graph); err != nil {
		return out, err
	}
	if prior != nil && prior.State == "pending" && prior.CheckpointVersion == 3 && prior.ManifestDigest == digest {
		if !planMatchesMarker(plan, prior) || plan.ContextRevision != m.ContextRevision || !planAuthorizationMatches(*plan, target) {
			return out, ErrConflict
		}
		if err = validateInstallPlanTargetsV3(m, payload, installation, *plan); err != nil {
			return out, err
		}
		for _, entry := range plan.Entries {
			value, stamp, readErr := planReadTarget(installation, *plan, entry.Target, v2TargetLimit(entry))
			if readErr != nil || Digest(value) != entry.NewDigest || stamp != entry.NewModifiedNS {
				return out, ErrConflict
			}
		}
		if err = checkRestoreV2Sources(root, prior); err != nil {
			return out, err
		}
		id, _, err := root.readMetadata("identity.json", 1024)
		if err != nil || !bytesEqual(id.Bytes, v2Identity(prior.ServiceID)) {
			return out, ErrConflict
		}
		return RestoreSummary{Code: "ok", Epoch: prior.Epoch, ServiceID: prior.ServiceID, ManifestDigest: digest, State: "pending"}, nil
	}
	relocated := rootPath != original.Root || m.ContextRevision != original.ContextRevision || !bytesEqual(target.Context.PolicyBytes, v3Policy(sourcePayload))
	if relocated && (prior == nil || prior.Activated) {
		if err = checkRelocationTargetHistory(root); err != nil {
			return out, err
		}
	}
	if prior != nil && !prior.Activated && (prior.State != "installing" || prior.CheckpointVersion != 3 || prior.ManifestDigest != digest) {
		return out, ErrConflict
	}
	// Reuse a plan written before its marker or after an interrupted install.
	// An activated old receipt may be replaced only while its old plan matches.
	reuse := plan != nil && plan.ManifestDigest == digest && plan.ContextRevision == m.ContextRevision && planAuthorizationMatches(*plan, target) && (prior == nil || prior.State == "installing" || prior.Activated && prior.ServiceID == plan.ReplacedServiceID)
	if !reuse {
		if plan != nil && (prior == nil || !prior.Activated || !planMatchesMarker(plan, prior)) {
			return out, ErrConflict
		}
		created, err := newInstallPlanV3(m, payload, installation, root, digest, target)
		if err != nil {
			return out, err
		}
		plan = &created
	}
	seal, planBytes, err := planDigest(*plan)
	if err != nil {
		return out, err
	}
	if reuse && prior != nil && prior.State == "installing" && (prior.PlanDigest != seal || !planMatchesMarker(plan, prior)) {
		return out, ErrConflict
	}
	if err = validateInstallPlanTargetsV3(m, payload, installation, *plan); err != nil {
		return out, err
	}
	if !reuse {
		old, _, err := root.readMetadata(restorePlanName, MaxCheckpointManifestBytes)
		if err != nil {
			return out, err
		}
		if err = root.replace(restorePlanName, StoreSnapshot{Exists: true, Bytes: planBytes}, Digest(old), MaxCheckpointManifestBytes, nil); err != nil {
			return out, err
		}
	}
	call := func(stage string) error {
		if hook != nil {
			return hook(stage)
		}
		return nil
	}
	if err = call("plan_persisted"); err != nil {
		return out, err
	}
	count := 0
	for _, file := range payload {
		if strings.HasPrefix(file.entry.Path, "managed/operations/") && strings.HasSuffix(file.entry.Path, ".json") {
			count++
		}
	}
	marker := planMarker(*plan, seal, count)
	marker.CheckpointVersion = 3
	marker.ConfigFile, marker.WorkingDir = m.ConfigFile, m.WorkingDir
	if err = writeRestoreMarker(root, &marker); err != nil {
		return out, err
	}
	if err = call("marker_persisted"); err != nil {
		return out, err
	}
	byPath := map[string]checkpointPayload{}
	for _, file := range payload {
		byPath[file.entry.Path] = file
	}
	for _, entry := range plan.Entries {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		var desired StoreSnapshot
		if entry.NewExists {
			if entry.Payload == "@identity" {
				desired = StoreSnapshot{Exists: true, Bytes: v2Identity(plan.ServiceID)}
			} else {
				file, ok := byPath[entry.Payload]
				if !ok {
					return out, ErrRecovery
				}
				desired = StoreSnapshot{Exists: true, Bytes: file.data}
			}
		}
		if Digest(desired) != entry.NewDigest {
			return out, ErrRecovery
		}
		current, stamp, err := planReadTarget(installation, *plan, entry.Target, v2TargetLimit(entry))
		if err != nil {
			return out, err
		}
		if Digest(current) == entry.NewDigest && stamp == entry.NewModifiedNS {
			continue
		}
		if Digest(current) != entry.OldDigest || stamp != entry.OldModifiedNS {
			return out, ErrConflict
		}
		dir, name, close, err := planWalk(installation, *plan, entry.Target, true)
		if err != nil {
			close()
			return out, err
		}
		err = replaceV2Timed(dir, name, desired, entry.NewModifiedNS, entry.OldDigest, entry.OldModifiedNS, v2TargetLimit(entry), func(stage string) error { return call("target:" + entry.Target + ":" + stage) })
		close()
		if err != nil {
			return out, err
		}
	}
	if err = checkRestoreV2Sources(root, &marker); err != nil {
		return out, err
	}
	if err = checkV3IncludeTargets(authorization, installation, graph); err != nil {
		return out, err
	}
	variants, err = variantsFromPayload(payload)
	if err != nil {
		return out, err
	}
	actual, _, _, err := captureContextV3(ctx, rootPath, capture, variants)
	if err != nil || actual.Revision != m.ContextRevision {
		return out, ErrConflict
	}
	again, againPayload, err := readCheckpointV2(source, digest)
	if err != nil || again.ID != m.ID || !sameCheckpointPayload(sourcePayload, againPayload) {
		return out, ErrConflict
	}
	if err = call("before_pending"); err != nil {
		return out, err
	}
	marker.State = "pending"
	if err = writeRestoreMarker(root, &marker); err != nil {
		return out, err
	}
	return RestoreSummary{Code: "ok", Epoch: marker.Epoch, ServiceID: marker.ServiceID, ManifestDigest: digest, State: "pending"}, nil
}
