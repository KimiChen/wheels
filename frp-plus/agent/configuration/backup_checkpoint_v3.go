package configuration

import (
	"context"
	"encoding/json"
	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	"os"
	"path/filepath"
)

func validateCheckpointSourcesV3(ctx context.Context, policy BackupGraphPolicy, graphBytes []byte, files []managed.ContextFile, variants []managed.StoreVariant) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	m, err := backupmanifest.Decode(graphBytes)
	if err != nil {
		return "", backupErr("manifest_invalid")
	}
	material := map[string]dependency{}
	if len(files) > backupmanifest.MaxFiles {
		return "", backupErr("limit_exceeded")
	}
	total := 0
	for _, file := range files {
		total += len(file.Bytes)
		if total > MaxInputBytes {
			return "", backupErr("limit_exceeded")
		}
		if _, err := policy.logical(file.Path, false); err != nil || backupWithin(file.Path, filepath.Dir(policy.StoreFile)) || material[file.Path].Path != "" || file.ModifiedNS <= 0 || len(file.Bytes) > backupmanifest.MaxFileBytes {
			return "", backupErr("payload_invalid")
		}
		material[file.Path] = dependency{Path: file.Path, State: "ready", Data: append([]byte(nil), file.Bytes...), Mode: 0600, Modified: file.ModifiedNS}
	}
	var current managed.StoreSnapshot
	for _, variant := range variants {
		if variant.Path == "managed/store.json" {
			current = variant.Snapshot
		}
	}
	payload := []BackupGraphFile{}
	required := map[string]bool{}
	for _, entry := range m.Files {
		if entry.Kind == "store" {
			if entry.Path != policy.StoreFile || entry.Exists != current.Exists || entry.Exists && backupmanifest.Digest(current.Bytes) != entry.SHA256 {
				return "", backupErr("store_mismatch")
			}
			if current.Exists {
				payload = append(payload, BackupGraphFile{ID: entry.ID, Bytes: append([]byte(nil), current.Bytes...)})
			}
			continue
		}
		file, exists := material[entry.Path]
		if !exists || entry.Mode != 0600 || entry.ModifiedNS != file.Modified {
			return "", backupErr("dependency_missing")
		}
		payload = append(payload, BackupGraphFile{ID: entry.ID, Bytes: file.Data})
		required[entry.Path] = true
	}
	if _, err = ValidateBackupGraph(ctx, policy, graphBytes, payload, backupmanifest.Digest(graphBytes)); err != nil {
		return "", err
	}
	extra, err := checkpointVariantDependencies(policy, variants)
	if err != nil {
		return "", err
	}
	for _, path := range extra {
		required[path] = true
	}
	metaPath := filepath.Join(policy.WorkingDir, "installation.json")
	required[metaPath] = true
	if len(required) != len(material) {
		return "", backupErr("closure_mismatch")
	}
	for path := range required {
		if material[path].Path == "" {
			return "", backupErr("dependency_missing")
		}
	}
	var metadata struct {
		Format  int      `json:"format"`
		Roles   []string `json:"roles"`
		Managed struct {
			Version int    `json:"version"`
			Root    string `json:"root"`
			Store   string `json:"store"`
		} `json:"managed"`
	}
	if strictJSON(material[metaPath].Data) != nil || json.Unmarshal(material[metaPath].Data, &metadata) != nil || metadata.Format != 2 || len(metadata.Roles) != 1 || metadata.Roles[0] != "agent" || metadata.Managed.Version != 1 || metadata.Managed.Root != "managed" || metadata.Managed.Store != "store.json" {
		return "", backupErr("unsupported_dependency")
	}
	rendered, _, err := RenderBackupTemplate(ctx, material[policy.ConfigFile].Data, policy.TemplateEnv)
	if err != nil {
		return "", err
	}
	common, objects, err := decodeBackupRendered(policy.ConfigFile, rendered)
	if err != nil {
		return "", err
	}
	if common.Telemetry.ConfigManagement == nil || !common.Telemetry.ConfigManagement.Enabled || common.Telemetry.ConfigManagement.Root != filepath.Dir(policy.StoreFile) || !common.Telemetry.Enabled {
		return "", backupErr("unsupported_dependency")
	}
	sealed := map[string][]string{}
	for _, include := range m.Includes {
		sealed[include.Pattern] = append([]string{}, include.Files...)
		for _, path := range include.Files {
			rendered, _, err := RenderBackupTemplate(ctx, material[path].Data, policy.TemplateEnv)
			if err != nil {
				return "", err
			}
			_, values, err := decodeBackupRendered(path, rendered)
			if err != nil {
				return "", err
			}
			objects = append(objects, values...)
		}
	}
	proxy := common.Transport.ProxyURL
	if common.Complete() != nil {
		return "", backupErr("source_invalid")
	}
	common.Transport.ProxyURL = proxy
	input := Input{ConfigFile: policy.ConfigFile, StoreFile: policy.StoreFile, WorkingDir: policy.WorkingDir, StartupCommon: common, ReloadCommon: common, FileMemory: native(objects), TemplateEnv: policy.TemplateEnv, UnsafeFeatures: []string{}}
	snapshot, err := takeSnapshotSealed(input, material, sealed, true)
	if err != nil || len(snapshot.issues) != 0 {
		return "", backupErr("source_invalid")
	}
	return snapshot.contextRevision, nil
}

func CheckpointCaptureV3(policy BackupGraphPolicy) managed.ContextCaptureV3 {
	return func(ctx context.Context, variants []managed.StoreVariant) (managed.ContextSnapshotV3, error) {
		var out managed.ContextSnapshotV3
		if os.Getenv("http_proxy") != "" {
			return out, backupErr("unsupported_dependency")
		}
		graph, err := CaptureBackupGraph(ctx, policy)
		if err != nil {
			return out, err
		}
		byID := map[string][]byte{}
		for _, file := range graph.Files {
			byID[file.ID] = file.Bytes
		}
		files := []managed.ContextFile{}
		seen := map[string]bool{}
		for _, entry := range graph.Manifest.Files {
			if entry.Kind == "store" {
				continue
			}
			if !entry.Exists || entry.Mode != 0600 || backupWithin(entry.Path, filepath.Dir(policy.StoreFile)) {
				return out, backupErr("unsupported_dependency")
			}
			files = append(files, managed.ContextFile{Path: entry.Path, Bytes: append([]byte(nil), byID[entry.ID]...), ModifiedNS: entry.ModifiedNS})
			seen[entry.Path] = true
		}
		extra, err := checkpointVariantDependencies(policy, variants)
		if err != nil {
			return out, err
		}
		extra = append(extra, filepath.Join(policy.WorkingDir, "installation.json"))
		for _, path := range extra {
			if seen[path] {
				continue
			}
			if _, err = policy.logical(path, false); err != nil {
				return out, err
			}
			file, err := maintenancePrivate(path)
			if err != nil {
				return out, err
			}
			files = append(files, file)
			seen[path] = true
		}
		revision, err := validateCheckpointSourcesV3(ctx, policy, graph.ManifestBytes, files, variants)
		if err != nil {
			return out, err
		}
		for _, file := range files {
			again, err := maintenancePrivate(file.Path)
			if err != nil || again.ModifiedNS != file.ModifiedNS || !bytesEqual(again.Bytes, file.Bytes) {
				return out, backupErr("source_changed")
			}
		}
		if err = CheckBackupGraph(ctx, policy, graph); err != nil {
			return out, err
		}
		encoded, err := EncodeBackupPolicy(policy)
		if err != nil {
			return out, err
		}
		return managed.ContextSnapshotV3{ContextSnapshotV2: managed.ContextSnapshotV2{ConfigFile: policy.ConfigFile, WorkingDir: policy.WorkingDir, Revision: revision, GraphBytes: graph.ManifestBytes, Files: files}, PolicyBytes: encoded}, nil
	}
}
func ValidateCheckpointContextV3(policy BackupGraphPolicy) managed.ContextValidatorV3 {
	return func(ctx context.Context, m managed.CheckpointManifestV2, files []managed.ContextFile, variants []managed.StoreVariant, graph, rawPolicy []byte) error {
		expected, err := EncodeBackupPolicy(policy)
		if err != nil || !bytesEqual(expected, rawPolicy) || m.Root != filepath.Dir(policy.StoreFile) || m.WorkingDir != policy.WorkingDir || m.ConfigFile != policy.ConfigFile || backupmanifest.Digest(graph) != m.GraphSHA256 {
			return backupErr("policy_mismatch")
		}
		revision, err := validateCheckpointSourcesV3(ctx, policy, graph, files, variants)
		if err != nil {
			return err
		}
		if revision != m.ContextRevision {
			return managed.ErrConflict
		}
		return nil
	}
}
func bytesEqual(a, b []byte) bool { return string(a) == string(b) }
