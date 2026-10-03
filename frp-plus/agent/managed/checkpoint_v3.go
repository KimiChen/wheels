package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ContextSourceV3 struct {
	Path    string `json:"path"`
	Payload string `json:"payload"`
}
type ContextSnapshotV3 struct {
	ContextSnapshotV2
	PolicyBytes []byte
}
type ContextCaptureV3 func(context.Context, []StoreVariant) (ContextSnapshotV3, error)
type ContextValidatorV3 func(context.Context, CheckpointManifestV2, []ContextFile, []StoreVariant, []byte, []byte) error

// Context paths are private metadata. Payload names are fixed opaque indices;
// neither capture nor an archive authorizes any destination path.
func captureContextV3(ctx context.Context, root string, capture ContextCaptureV3, variants []StoreVariant) (ContextSnapshotV3, []checkpointPayload, []ContextSourceV3, error) {
	var out ContextSnapshotV3
	if capture == nil {
		return out, nil, nil, ErrInvalid
	}
	out, err := capture(ctx, variants)
	if err != nil {
		return out, nil, nil, err
	}
	installation := filepath.Dir(root)
	if out.ConfigFile != filepath.Join(installation, "agent.toml") || out.WorkingDir != installation || !digestString(out.Revision) || len(out.GraphBytes) == 0 || len(out.PolicyBytes) == 0 || len(out.GraphBytes) > int(MaxCheckpointManifestBytes) || len(out.PolicyBytes) > int(MaxCheckpointManifestBytes) || len(out.Files) < 2 || len(out.Files) > 128 {
		return out, nil, nil, ErrRecovery
	}
	files := append([]ContextFile(nil), out.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	payload := []checkpointPayload{}
	sources := []ContextSourceV3{}
	seen := map[string]bool{}
	stamp := int64(0)
	for index, file := range files {
		if !canonicalAbsolute(file.Path) || seen[file.Path] || len(file.Bytes) > int(MaxCheckpointFileBytes) || file.ModifiedNS <= 0 {
			return out, nil, nil, ErrRecovery
		}
		seen[file.Path] = true
		name := fmt.Sprintf("context/f%06d", index+1)
		data := append([]byte(nil), file.Bytes...)
		sources = append(sources, ContextSourceV3{file.Path, name})
		payload = append(payload, checkpointPayload{CheckpointFile{Path: name, Size: int64(len(data)), SHA256: checkpointHash(data), ModifiedNS: file.ModifiedNS}, data})
		if file.Path == out.ConfigFile {
			stamp = file.ModifiedNS
		}
	}
	if stamp == 0 || !seen[filepath.Join(installation, "installation.json")] {
		return out, nil, nil, ErrRecovery
	}
	for _, meta := range []struct {
		name string
		data []byte
	}{{"GRAPH.json", out.GraphBytes}, {"POLICY.json", out.PolicyBytes}} {
		data := append([]byte(nil), meta.data...)
		payload = append(payload, checkpointPayload{CheckpointFile{Path: meta.name, Size: int64(len(data)), SHA256: checkpointHash(data), ModifiedNS: stamp}, data})
	}
	return out, payload, sources, nil
}

func ExportCheckpointV3(ctx context.Context, options Options, capture ContextCaptureV3, output string) (CheckpointSummary, error) {
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
	before, contextFiles, sources, err := captureContextV3(ctx, options.Root, capture, variants)
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
	m := CheckpointManifestV2{CheckpointManifest: CheckpointManifest{Kind: "frp-managed-checkpoint", Version: 3, ID: id, CreatedAtMS: time.Now().UnixMilli(), Root: options.Root, StoreName: "store.json", ConfigFile: before.ConfigFile, WorkingDir: before.WorkingDir, ContextRevision: before.Revision, ServiceID: identity, StoreExists: store.Exists, StoreDigest: Digest(store), Files: []CheckpointFile{}}, GraphSHA256: checkpointHash(before.GraphBytes), PolicySHA256: checkpointHash(before.PolicyBytes), ContextSources: sources}
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
	after, afterFiles, _, err := captureContextV3(ctx, options.Root, capture, variants)
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

func validateV3Sources(m CheckpointManifestV2, payload []checkpointPayload) error {
	if m.Version != 3 {
		if m.PolicySHA256 != "" || len(m.ContextSources) != 0 {
			return ErrRecovery
		}
		return nil
	}
	if !digestString(m.PolicySHA256) || len(m.ContextSources) < 2 || len(m.ContextSources) > 128 {
		return ErrRecovery
	}
	paths, entries := map[string]bool{}, map[string]bool{}
	byPath := map[string]checkpointPayload{}
	for _, file := range payload {
		byPath[file.entry.Path] = file
	}
	for index, source := range m.ContextSources {
		file, ok := byPath[source.Payload]
		if !ok || source.Payload != fmt.Sprintf("context/f%06d", index+1) || !canonicalAbsolute(source.Path) || paths[source.Path] || entries[source.Payload] || len(file.data) > int(MaxCheckpointFileBytes) {
			return ErrRecovery
		}
		paths[source.Path], entries[source.Payload] = true, true
	}
	for path := range byPath {
		if strings.HasPrefix(path, "context/") && !entries[path] {
			return ErrRecovery
		}
	}
	if !paths[m.ConfigFile] || !paths[filepath.Join(m.WorkingDir, "installation.json")] || checkpointHash(byPath["POLICY.json"].data) != m.PolicySHA256 {
		return ErrRecovery
	}
	return nil
}
func v3Policy(payload []checkpointPayload) []byte {
	for _, file := range payload {
		if file.entry.Path == "POLICY.json" {
			return append([]byte(nil), file.data...)
		}
	}
	return nil
}
func ValidateCheckpointV3(ctx context.Context, path, digest string, validate ContextValidatorV3) (CheckpointManifestV2, error) {
	var m CheckpointManifestV2
	root, err := openExistingPrivateDir(path)
	if err != nil {
		return m, err
	}
	defer root.close()
	m, payload, err := readCheckpointV2(root, digest)
	if err != nil || m.Version != 3 {
		return m, ErrRecovery
	}
	variants, err := variantsFromPayload(payload)
	if err != nil {
		return m, err
	}
	files, graph := v2ContextFiles(m, payload)
	if validate == nil {
		return m, ErrInvalid
	}
	if err = validate(ctx, m, files, variants, graph, v3Policy(payload)); err != nil {
		return m, err
	}
	return m, nil
}

// A relocator is supplied by trusted local native configuration code. It must
// validate the source closure, rewrite only declared native path fields, and
// revalidate the complete target closure without filesystem writes.
type RelocationContextV3 struct {
	Context     ContextSnapshotV3
	Store       StoreSnapshot
	TargetRoots []string
	TargetFiles []string
}
type ContextRelocatorV3 func(context.Context, CheckpointManifestV2, []ContextFile, []StoreVariant, []byte, []byte) (RelocationContextV3, error)
