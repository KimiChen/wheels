package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StoreVariant is private material already validated against its operation
// journal. Empty old snapshots preserve absence rather than an empty Store.
type StoreVariant struct {
	Path       string
	Snapshot   StoreSnapshot
	Historical bool
}
type ContextSnapshotV2 struct {
	ConfigFile, WorkingDir, Revision string
	GraphBytes                       []byte
	Files                            []ContextFile
}
type ContextCaptureV2 func(context.Context, []StoreVariant) (ContextSnapshotV2, error)
type ContextValidatorV2 func(context.Context, CheckpointManifestV2, []ContextFile, []StoreVariant, []byte) error
type CheckpointManifestV2 struct {
	CheckpointManifest
	GraphSHA256    string            `json:"graph_sha256"`
	PolicySHA256   string            `json:"policy_sha256,omitempty"`
	ContextSources []ContextSourceV3 `json:"context_sources,omitempty"`
}

func variantsFromPayload(payload []checkpointPayload) ([]StoreVariant, error) {
	byPath := map[string][]byte{}
	for _, file := range payload {
		byPath[file.entry.Path] = file.data
	}
	store, exists := byPath["managed/store.json"]
	out := []StoreVariant{{Path: "managed/store.json", Snapshot: StoreSnapshot{Exists: exists, Bytes: append([]byte(nil), store...)}}}
	for _, file := range payload {
		if !strings.HasPrefix(file.entry.Path, "managed/operations/") || !strings.HasSuffix(file.entry.Path, ".json") {
			continue
		}
		r, policy, err := decodeManagedRecord(file.data, filepath.Base(file.entry.Path))
		if err != nil {
			return nil, ErrRecovery
		}
		prefix := strings.TrimSuffix(file.entry.Path, ".json")
		if policy == snapshotsExpired {
			if _, ok := byPath[prefix+".old"]; ok {
				return nil, ErrRecovery
			}
			if _, ok := byPath[prefix+".new"]; ok {
				return nil, ErrRecovery
			}
			continue
		}
		old, ok := byPath[prefix+".old"]
		if !ok || Digest(StoreSnapshot{Exists: r.OldExists, Bytes: old}) != r.OldDigest {
			return nil, ErrRecovery
		}
		next, ok := byPath[prefix+".new"]
		if !ok || Digest(StoreSnapshot{Exists: true, Bytes: next}) != r.NewDigest {
			return nil, ErrRecovery
		}
		out = append(out, StoreVariant{Path: prefix + ".old", Snapshot: StoreSnapshot{Exists: r.OldExists, Bytes: append([]byte(nil), old...)}}, StoreVariant{Path: prefix + ".new", Snapshot: StoreSnapshot{Exists: true, Bytes: append([]byte(nil), next...)}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	for _, file := range payload {
		if file.entry.Path == "managed/"+contextHistoryName {
			var h contextHistory
			if decodeCheckpointJSON(file.data, &h) != nil {
				return nil, ErrRecovery
			}
			for _, entry := range h.Entries {
				prefix := "managed/operations/" + idHash(entry.ID)
				for i := range out {
					if out[i].Path == prefix+".old" || out[i].Path == prefix+".new" {
						out[i].Historical = true
					}
				}
			}
		}
	}
	return out, nil
}

func validV2Relative(path string) bool {
	if path == "" || len(path) > 4096 || strings.ContainsAny(path, "\\\x00\r\n") || strings.HasPrefix(path, "/") || filepath.ToSlash(filepath.Clean(path)) != path {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) > 32 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 255 || strings.HasPrefix(part, ".txn-") {
			return false
		}
	}
	return true
}
func validV2Context(path string) bool {
	if !strings.HasPrefix(path, "context/") {
		return false
	}
	rel := strings.TrimPrefix(path, "context/")
	return validV2Relative(rel) && rel != "managed" && !strings.HasPrefix(rel, "managed/")
}
func validV2Payload(path string) bool {
	return path == "POLICY.json" || path == "GRAPH.json" || validV2Context(path) || path == "managed/restore-install.json" || validCheckpointPath(path) && strings.HasPrefix(path, "managed/")
}
func v2EntryLimit(path string) int64 {
	if path == "managed/"+contextHistoryName {
		return contextHistoryLimit
	}
	if path == "POLICY.json" || path == "GRAPH.json" || path == "managed/restore-install.json" {
		return MaxCheckpointManifestBytes
	}
	if strings.HasPrefix(path, "context/") {
		return MaxCheckpointFileBytes
	}
	return checkpointEntryLimit(path)
}
func captureContextV2(ctx context.Context, root string, capture ContextCaptureV2, variants []StoreVariant) (ContextSnapshotV2, []checkpointPayload, error) {
	var out ContextSnapshotV2
	if capture == nil {
		return out, nil, ErrInvalid
	}
	out, err := capture(ctx, variants)
	if err != nil {
		return out, nil, ErrRecovery
	}
	installation := filepath.Dir(root)
	if out.ConfigFile != filepath.Join(installation, "agent.toml") || out.WorkingDir != installation || !digestString(out.Revision) || len(out.GraphBytes) == 0 || int64(len(out.GraphBytes)) > MaxCheckpointManifestBytes || len(out.Files) < 2 || len(out.Files) > 128 {
		return out, nil, ErrRecovery
	}
	files := []checkpointPayload{}
	seen := map[string]bool{}
	for _, file := range out.Files {
		rel, err := filepath.Rel(installation, file.Path)
		path := "context/" + filepath.ToSlash(rel)
		if err != nil || !validV2Context(path) || seen[path] || len(file.Bytes) > int(MaxCheckpointFileBytes) || file.ModifiedNS <= 0 {
			return out, nil, ErrRecovery
		}
		seen[path] = true
		data := append([]byte(nil), file.Bytes...)
		files = append(files, checkpointPayload{CheckpointFile{Path: path, Size: int64(len(data)), SHA256: checkpointHash(data), ModifiedNS: file.ModifiedNS}, data})
	}
	if !seen["context/agent.toml"] || !seen["context/installation.json"] {
		return out, nil, ErrRecovery
	}
	// Graph bytes are metadata, not a live source; use the main file's stable
	// mtime so before/after capture comparison has no wall-clock dependency.
	stamp := int64(0)
	for _, file := range files {
		if file.entry.Path == "context/agent.toml" {
			stamp = file.entry.ModifiedNS
		}
	}
	graph := append([]byte(nil), out.GraphBytes...)
	files = append(files, checkpointPayload{CheckpointFile{Path: "GRAPH.json", Size: int64(len(graph)), SHA256: checkpointHash(graph), ModifiedNS: stamp}, graph})
	return out, files, nil
}

func ExportCheckpointV2(ctx context.Context, options Options, capture ContextCaptureV2, output string) (CheckpointSummary, error) {
	var summary CheckpointSummary
	lease, err := OpenOfflineLease(options)
	if err != nil {
		return summary, err
	}
	defer lease.Close()
	managedFiles, identity, store, err := lease.payload(ctx)
	if err != nil {
		return summary, err
	}
	variants, err := variantsFromPayload(managedFiles)
	if err != nil {
		return summary, err
	}
	before, contextFiles, err := captureContextV2(ctx, options.Root, capture, variants)
	if err != nil {
		return summary, err
	}
	all := append(append([]checkpointPayload{}, managedFiles...), contextFiles...)
	engine := &Engine{opts: lease.options, root: lease.root, operations: lease.operations, storeName: "store.json", records: map[string]*record{}, keys: map[string]string{}}
	if engine.load() != nil || engine.active != "" && engine.records[engine.active].ContextRevision != before.Revision {
		return summary, ErrConflict
	}
	if !filepath.IsAbs(output) || filepath.Clean(output) != output || output == filepath.Dir(options.Root) || strings.HasPrefix(output, filepath.Dir(options.Root)+string(filepath.Separator)) {
		return summary, ErrUnsafePath
	}
	if len(all) > MaxCheckpointFiles {
		return summary, ErrInvalid
	}
	var total int64
	for _, file := range all {
		total += file.entry.Size
	}
	if total > MaxCheckpointBytes {
		return summary, ErrInvalid
	}
	target, err := createPrivateDirectory(output)
	if err != nil {
		return summary, err
	}
	defer target.close()
	id, err := newPrivateID()
	if err != nil {
		return summary, err
	}
	m := CheckpointManifestV2{CheckpointManifest: CheckpointManifest{Kind: "frp-managed-checkpoint", Version: 2, ID: id, CreatedAtMS: time.Now().UnixMilli(), Root: options.Root, StoreName: "store.json", ConfigFile: before.ConfigFile, WorkingDir: before.WorkingDir, ContextRevision: before.Revision, ServiceID: identity, StoreExists: store.Exists, StoreDigest: Digest(store), Files: []CheckpointFile{}}, GraphSHA256: checkpointHash(before.GraphBytes)}
	sort.Slice(all, func(i, j int) bool { return all[i].entry.Path < all[j].entry.Path })
	for _, file := range all {
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}
		if err = writeCheckpointPayload(target, file); err != nil {
			return summary, err
		}
		m.Files = append(m.Files, file.entry)
	}
	afterManaged, afterID, afterStore, err := lease.payload(ctx)
	if err != nil || afterID != identity || Digest(afterStore) != Digest(store) || !sameCheckpointPayload(managedFiles, afterManaged) {
		return summary, ErrConflict
	}
	after, afterFiles, err := captureContextV2(ctx, options.Root, capture, variants)
	if err != nil || before.Revision != after.Revision || !bytes.Equal(before.GraphBytes, after.GraphBytes) || !sameCheckpointPayload(contextFiles, afterFiles) {
		return summary, ErrConflict
	}
	data, err := json.Marshal(m)
	if err != nil || int64(len(data)) > MaxCheckpointManifestBytes {
		return summary, ErrInvalid
	}
	if err = target.replace("CHECKPOINT.json", StoreSnapshot{Exists: true, Bytes: data}, Digest(StoreSnapshot{}), MaxCheckpointManifestBytes, nil); err != nil {
		return summary, err
	}
	return CheckpointSummary{Code: "ok", CheckpointID: id, ManifestDigest: checkpointHash(data), FileCount: len(all), TotalBytes: total}, nil
}
