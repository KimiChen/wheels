package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
)

func checkpointPolicy(configFile string) (BackupGraphPolicy, error) {
	if !backupPath(configFile) || filepath.Base(configFile) != "agent.toml" {
		return BackupGraphPolicy{}, backupErr("policy_invalid")
	}
	parent := filepath.Dir(configFile)
	return BackupGraphPolicy{ConfigFile: configFile, WorkingDir: parent, StoreFile: filepath.Join(parent, "managed", "store.json"), Roots: []BackupRoot{{ID: "installation", Path: parent}}}, nil
}

// CheckpointCaptureV2 is called only while managed holds the offline lease.
// Graph collection remains read-only; all current/old/new variants are bound
// to already verified native journal material supplied by that lease.
func CheckpointCaptureV2(configFile string) managed.ContextCaptureV2 {
	return func(ctx context.Context, variants []managed.StoreVariant) (managed.ContextSnapshotV2, error) {
		var result managed.ContextSnapshotV2
		policy, err := checkpointPolicy(configFile)
		if err != nil {
			return result, err
		}
		if os.Getenv("http_proxy") != "" {
			return result, backupErr("unsupported_dependency")
		}
		graph, err := CaptureBackupGraph(ctx, policy)
		if err != nil {
			return result, err
		}
		material := map[string]dependency{}
		payload := map[string][]byte{}
		for _, file := range graph.Files {
			payload[file.ID] = file.Bytes
		}
		for _, file := range graph.Manifest.Files {
			if file.Kind == "store" {
				continue
			}
			if !file.Exists || file.Mode != 0600 || backupWithin(file.Path, filepath.Dir(policy.StoreFile)) {
				return result, backupErr("unsupported_dependency")
			}
			material[file.Path] = dependency{Path: file.Path, State: "ready", Data: payload[file.ID], Mode: file.Mode, Modified: file.ModifiedNS}
		}
		required, err := checkpointVariantDependencies(policy, variants)
		if err != nil {
			return result, err
		}
		required = append(required, filepath.Join(policy.WorkingDir, "installation.json"))
		for _, path := range required {
			if _, exists := material[path]; exists {
				continue
			}
			file, err := maintenancePrivate(path)
			if err != nil {
				return result, backupErr("dependency_missing")
			}
			material[path] = dependency{Path: path, State: "ready", Data: file.Bytes, Mode: 0600, Modified: file.ModifiedNS}
		}
		files := []managed.ContextFile{}
		paths := make([]string, 0, len(material))
		for path := range material {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		if len(paths) > backupmanifest.MaxFiles {
			return result, backupErr("limit_exceeded")
		}
		var total int
		for _, path := range paths {
			for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
				info, err := os.Lstat(dir)
				if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !backupOwnerAndLinksForCheckpoint(info) {
					return result, backupErr("source_unsafe")
				}
				if dir == policy.WorkingDir {
					break
				}
			}
			file := material[path]
			total += len(file.Data)
			if total > MaxInputBytes {
				return result, backupErr("limit_exceeded")
			}
			files = append(files, managed.ContextFile{Path: path, Bytes: append([]byte(nil), file.Data...), ModifiedNS: file.Modified})
		}
		revision, err := validateCheckpointSources(ctx, policy, graph.ManifestBytes, files, variants)
		if err != nil {
			return result, err
		}
		// Re-read every extra dependency too. The baseline graph already checks
		// its closure twice; operation-only certificates must not escape CAS.
		for _, path := range paths {
			file, err := maintenancePrivate(path)
			prior := material[path]
			if err != nil || file.ModifiedNS != prior.Modified || !bytes.Equal(file.Bytes, prior.Data) {
				return result, backupErr("source_changed")
			}
		}
		if err = CheckBackupGraph(ctx, policy, graph); err != nil {
			return result, err
		}
		return managed.ContextSnapshotV2{ConfigFile: configFile, WorkingDir: policy.WorkingDir, Revision: revision, GraphBytes: append([]byte(nil), graph.ManifestBytes...), Files: files}, nil
	}
}

func checkpointVariantDependencies(policy BackupGraphPolicy, variants []managed.StoreVariant) ([]string, error) {
	if len(variants) == 0 || len(variants) > 2049 {
		return nil, backupErr("limit_exceeded")
	}
	seen, paths := map[string]bool{}, map[string]bool{}
	current := false
	for _, variant := range variants {
		if len(variant.Snapshot.Bytes) > int(managed.MaxCheckpointFileBytes) {
			return nil, backupErr("limit_exceeded")
		}
		if seen[variant.Path] {
			return nil, backupErr("source_invalid")
		}
		seen[variant.Path] = true
		if variant.Path == "managed/store.json" {
			current = true
		} else if !strings.HasPrefix(variant.Path, "managed/operations/") || (!strings.HasSuffix(variant.Path, ".old") && !strings.HasSuffix(variant.Path, ".new")) {
			return nil, backupErr("source_invalid")
		}
		if !variant.Snapshot.Exists {
			if len(variant.Snapshot.Bytes) != 0 {
				return nil, backupErr("source_invalid")
			}
			continue
		}
		values, err := parseStore(variant.Snapshot.Bytes)
		if err != nil {
			return nil, backupErr("source_invalid")
		}
		for _, object := range values {
			if maintenanceDynamicPlugin(object) {
				return nil, backupErr("unsupported_dependency")
			}
			for _, field := range []string{"plugin.localPath", "plugin.unixPath"} {
				value, err := backupString(object.raw, field)
				if err != nil || value != "" {
					return nil, backupErr("unsupported_dependency")
				}
			}
			for _, field := range []string{"plugin.crtPath", "plugin.keyPath"} {
				path, err := backupString(object.raw, field)
				if err != nil {
					return nil, err
				}
				if path == "" {
					continue
				}
				if !filepath.IsAbs(path) {
					path = filepath.Join(policy.WorkingDir, path)
				} else {
					path = filepath.Clean(path)
				}
				if _, err := policy.logical(path, false); err != nil || backupWithin(path, filepath.Dir(policy.StoreFile)) {
					return nil, backupErr("path_unauthorized")
				}
				paths[path] = true
			}
		}
	}
	if !current {
		return nil, backupErr("source_invalid")
	}
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

func ValidateCheckpointContextV2(ctx context.Context, manifest managed.CheckpointManifestV2, files []managed.ContextFile, variants []managed.StoreVariant, graph []byte) error {
	policy, err := checkpointPolicy(manifest.ConfigFile)
	if err != nil {
		return err
	}
	if manifest.Root != filepath.Dir(policy.StoreFile) || manifest.WorkingDir != policy.WorkingDir || backupmanifest.Digest(graph) != manifest.GraphSHA256 {
		return backupErr("policy_mismatch")
	}
	revision, err := validateCheckpointSources(ctx, policy, graph, files, variants)
	if err != nil {
		return err
	}
	if revision != manifest.ContextRevision {
		return managed.ErrConflict
	}
	return nil
}

func validateCheckpointSources(ctx context.Context, policy BackupGraphPolicy, graphBytes []byte, files []managed.ContextFile, variants []managed.StoreVariant) (string, error) {
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
	common, objects, err := decodeBackupFile(policy.ConfigFile, material[policy.ConfigFile].Data)
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
			_, values, err := decodeBackupFile(path, material[path].Data)
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
	input := Input{ConfigFile: policy.ConfigFile, StoreFile: policy.StoreFile, WorkingDir: policy.WorkingDir, StartupCommon: common, ReloadCommon: common, FileMemory: native(objects), TemplateEnv: map[string]string{}, UnsafeFeatures: []string{}}
	snapshot, err := takeSnapshotSealed(input, material, sealed, true)
	if err != nil || len(snapshot.issues) != 0 || len(snapshot.referencedEnv) != 0 {
		return "", backupErr("source_invalid")
	}
	return snapshot.contextRevision, nil
}
