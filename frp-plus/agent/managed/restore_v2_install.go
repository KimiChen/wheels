package managed

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
)

func checkV2IncludeTargets(installation *privateDir, graphBytes []byte) error {
	graph, err := backupmanifest.Decode(graphBytes)
	if err != nil {
		return ErrRecovery
	}
	for _, include := range graph.Includes {
		rel, err := filepath.Rel(installation.path, filepath.Dir(include.Pattern))
		if err != nil {
			return ErrRecovery
		}
		path := filepath.ToSlash(filepath.Join(rel, "placeholder"))
		dir, _, close, err := v2Walk(installation, path, false)
		if err != nil {
			close()
			return err
		}
		if dir == nil {
			close()
			continue
		}
		names, err := v2MatchingFiles(dir, filepath.Base(include.Pattern))
		close()
		if err != nil {
			return err
		}
		expected := map[string]bool{}
		for _, path := range include.Files {
			expected[filepath.Base(path)] = true
		}
		for _, name := range names {
			if !expected[name] {
				return ErrConflict
			}
		}
	}
	return nil
}

func InstallCheckpointV2(ctx context.Context, checkpoint, rootPath, digest string, capture ContextCaptureV2, validate ContextValidatorV2) (RestoreSummary, error) {
	return installCheckpointV2(ctx, checkpoint, rootPath, digest, capture, validate, nil)
}
func installCheckpointV2(ctx context.Context, checkpoint, rootPath, digest string, capture ContextCaptureV2, validate ContextValidatorV2, hook func(string) error) (RestoreSummary, error) {
	var out RestoreSummary
	source, err := openExistingPrivateDir(checkpoint)
	if err != nil {
		return out, err
	}
	defer source.close()
	m, payload, err := readCheckpointV2(source, digest)
	if err != nil {
		return out, err
	}
	if rootPath != m.Root || capture == nil || validate == nil || checkpoint == m.WorkingDir || strings.HasPrefix(checkpoint, m.WorkingDir+string(filepath.Separator)) {
		return out, ErrUnsafePath
	}
	variants, err := variantsFromPayload(payload)
	if err != nil {
		return out, err
	}
	contextFiles, graph := v2ContextFiles(m, payload)
	if err = validate(ctx, m, contextFiles, variants, graph); err != nil {
		return out, ErrRecovery
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
	if err = checkV2IncludeTargets(installation, graph); err != nil {
		return out, err
	}
	if prior != nil && prior.State == "pending" && prior.CheckpointVersion == 2 && prior.ManifestDigest == digest {
		if !planMatchesMarker(plan, prior) {
			return out, ErrConflict
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
	if prior != nil && !prior.Activated && (prior.State != "installing" || prior.CheckpointVersion != 2 || prior.ManifestDigest != digest) {
		return out, ErrConflict
	}
	// Reuse a plan written before its marker or after an interrupted install.
	// An activated old receipt may be replaced only while its old plan matches.
	reuse := plan != nil && plan.ManifestDigest == digest && plan.ContextRevision == m.ContextRevision && (prior == nil || prior.State == "installing" || prior.Activated && prior.ServiceID == plan.ReplacedServiceID)
	if !reuse {
		if plan != nil && (prior == nil || !prior.Activated || !planMatchesMarker(plan, prior)) {
			return out, ErrConflict
		}
		created, err := newInstallPlan(m, payload, installation, root, digest)
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
	if err = validateInstallPlanTargets(m, payload, installation, *plan); err != nil {
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
		current, stamp, err := v2ReadTarget(installation, entry.Target, v2TargetLimit(entry))
		if err != nil {
			return out, err
		}
		if Digest(current) == entry.NewDigest && stamp == entry.NewModifiedNS {
			continue
		}
		if Digest(current) != entry.OldDigest || stamp != entry.OldModifiedNS {
			return out, ErrConflict
		}
		dir, name, close, err := v2Walk(installation, entry.Target, true)
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
	if err = checkV2IncludeTargets(installation, graph); err != nil {
		return out, err
	}
	actual, _, err := captureContextV2(ctx, rootPath, capture, variants)
	if err != nil || actual.Revision != m.ContextRevision {
		return out, ErrConflict
	}
	again, againPayload, err := readCheckpointV2(source, digest)
	if err != nil || again.ID != m.ID || !sameCheckpointPayload(payload, againPayload) {
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

func validateInstallPlanTargets(m CheckpointManifestV2, payload []checkpointPayload, installation *privateDir, plan restoreInstallPlan) error {
	expected := map[string]checkpointPayload{}
	for _, file := range payload {
		if file.entry.Path == "GRAPH.json" || file.entry.Path == "managed/restore.json" || file.entry.Path == "managed/"+restorePlanName {
			continue
		}
		target := strings.TrimPrefix(file.entry.Path, "context/")
		expected[target] = file
	}
	if !m.StoreExists {
		expected["managed/store.json"] = checkpointPayload{}
	}
	for _, entry := range plan.Entries {
		file, exists := expected[entry.Target]
		if exists {
			var desired StoreSnapshot
			payloadName := file.entry.Path
			stamp := file.entry.ModifiedNS
			if entry.Target == "managed/identity.json" {
				desired = StoreSnapshot{Exists: true, Bytes: v2Identity(plan.ServiceID)}
				payloadName = "@identity"
				stamp = entry.NewModifiedNS
			} else if file.entry.Path != "" {
				desired = StoreSnapshot{Exists: true, Bytes: file.data}
			}
			if entry.Payload != payloadName || entry.NewExists != desired.Exists || entry.NewDigest != Digest(desired) || entry.NewModifiedNS != stamp {
				return ErrRecovery
			}
			delete(expected, entry.Target)
		} else if entry.NewExists || (entry.Target != "managed/"+contextHistoryName && !strings.HasPrefix(entry.Target, "managed/operations/") && !strings.HasPrefix(entry.Target, "managed/secrets/")) {
			return ErrRecovery
		}
		current, stamp, err := v2ReadTarget(installation, entry.Target, v2TargetLimit(entry))
		if err != nil {
			return err
		}
		if !(Digest(current) == entry.OldDigest && stamp == entry.OldModifiedNS) && !(Digest(current) == entry.NewDigest && stamp == entry.NewModifiedNS) {
			return ErrConflict
		}
	}
	if len(expected) != 0 {
		return ErrRecovery
	}
	return nil
}
