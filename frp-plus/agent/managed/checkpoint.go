package managed

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	MaxCheckpointBytes         int64 = 64 << 20
	MaxCheckpointFiles               = 8192
	MaxCheckpointManifestBytes int64 = 1 << 20
	MaxCheckpointFileBytes     int64 = 1 << 20
)

// ContextSnapshot is private, locally captured native input. It is never a wire
// DTO. Capture must prove the restricted generated installation's closure.
type ContextSnapshot struct {
	ConfigFile string
	WorkingDir string
	Revision   string
	Files      []ContextFile
}
type ContextFile struct {
	Path       string
	Bytes      []byte
	ModifiedNS int64
}
type ContextCapture func(context.Context) (ContextSnapshot, error)

// ContextValidator proves native dependency closure from private staged bytes.
type ContextValidator func(context.Context, CheckpointManifest, []ContextFile) error

type CheckpointFile struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	ModifiedNS int64  `json:"mtime_ns"`
}
type CheckpointManifest struct {
	Kind            string           `json:"kind"`
	Version         int              `json:"version"`
	ID              string           `json:"id"`
	CreatedAtMS     int64            `json:"created_at_ms"`
	Root            string           `json:"root"`
	StoreName       string           `json:"store_name"`
	ConfigFile      string           `json:"config_file"`
	WorkingDir      string           `json:"cwd"`
	ContextRevision string           `json:"context_revision"`
	ServiceID       string           `json:"service_id"`
	StoreExists     bool             `json:"store_exists"`
	StoreDigest     string           `json:"store_digest"`
	Files           []CheckpointFile `json:"files"`
}

// CheckpointSummary is safe command output; private filenames and per-file
// secret digests remain exclusively in the private manifest.
type CheckpointSummary struct {
	Code           string `json:"code"`
	CheckpointID   string `json:"checkpoint_id"`
	ManifestDigest string `json:"manifest_digest"`
	FileCount      int    `json:"file_count"`
	TotalBytes     int64  `json:"total_bytes"`
}
type checkpointPayload struct {
	entry CheckpointFile
	data  []byte
}

// OfflineLease never invokes Engine.Open/Close: both can mutate recovery state.
// It excludes the native engine through the exact same lifetime lock inode.
type OfflineLease struct {
	root, operations, secrets *privateDir
	lock                      *os.File
	options                   Options
	closed                    bool
}

func OpenOfflineLease(options Options) (*OfflineLease, error) {
	if !filepath.IsAbs(options.Root) || filepath.Clean(options.Root) != options.Root || filepath.Dir(options.StorePath) != options.Root || filepath.Clean(options.StorePath) != options.StorePath || filepath.Base(options.Root) != "managed" {
		return nil, ErrUnsafePath
	}
	name := filepath.Base(options.StorePath)
	if name != "store.json" {
		return nil, ErrUnsafePath
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = MaxCheckpointFileBytes
	}
	if options.MaxOperations == 0 {
		options.MaxOperations = 1024
	}
	if options.MaxBytes < 1 || options.MaxBytes > MaxCheckpointFileBytes || options.MaxOperations < 1 || options.MaxOperations > 1024 {
		return nil, ErrInvalid
	}
	root, err := openExistingPrivateDir(options.Root)
	if err != nil {
		return nil, err
	}
	lease := &OfflineLease{root: root, options: options}
	fail := func(e error) (*OfflineLease, error) { lease.Close(); return nil, e }
	lease.lock, err = root.lock()
	if err != nil {
		return fail(err)
	}
	lease.operations, err = root.existingSubdir("operations")
	if err != nil {
		return fail(err)
	}
	lease.secrets, err = root.optionalSubdir("secrets")
	if err != nil {
		return fail(err)
	}
	return lease, nil
}
func (l *OfflineLease) Close() {
	if l == nil || l.closed {
		return
	}
	l.closed = true
	l.operations.close()
	l.secrets.close()
	if l.lock != nil {
		l.lock.Close()
	}
	l.root.close()
}
func newPrivateID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", ErrStorage
	}
	raw[6] = raw[6]&15 | 64
	raw[8] = raw[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}
func checkpointHash(data []byte) string { return idHash(string(data)) }

// Reject duplicate keys as well as unknown fields. Recovery documents must not
// have multiple interpretations across Go and the packaging tool.
func decodeCheckpointJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 16 {
			return ErrRecovery
		}
		token, err := decoder.Token()
		if err != nil {
			return ErrRecovery
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || seen[key] || key == "" || strings.ToLower(key) != key {
					return ErrRecovery
				}
				seen[key] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrRecovery
		}
		_, err = decoder.Token()
		if err != nil {
			return ErrRecovery
		}
		return nil
	}
	if walk(0) != nil {
		return ErrRecovery
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrRecovery
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrRecovery
	}
	return nil
}
func checkpointContextName(name string) bool {
	switch name {
	case "installation.json", "agent.toml", "agent.token", "frp.token", "ca.crt", "tls.crt", "tls.key", "local.crt", "local.key":
		return true
	}
	return false
}
func captureCheckpointContext(ctx context.Context, root string, capture ContextCapture) (ContextSnapshot, []checkpointPayload, error) {
	if capture == nil {
		return ContextSnapshot{}, nil, ErrInvalid
	}
	snapshot, err := capture(ctx)
	if err != nil {
		return snapshot, nil, ErrRecovery
	}
	installation := filepath.Dir(root)
	if snapshot.ConfigFile != filepath.Join(installation, "agent.toml") || snapshot.WorkingDir != installation || !digestString(snapshot.Revision) || len(snapshot.Files) < 3 || len(snapshot.Files) > 16 {
		return snapshot, nil, ErrRecovery
	}
	payload := []checkpointPayload{}
	seen := map[string]bool{}
	for _, file := range snapshot.Files {
		name := filepath.Base(file.Path)
		if filepath.Dir(file.Path) != installation || !checkpointContextName(name) || seen[name] || len(file.Bytes) > int(MaxCheckpointFileBytes) || file.ModifiedNS <= 0 {
			return snapshot, nil, ErrRecovery
		}
		seen[name] = true
		data := append([]byte(nil), file.Bytes...)
		payload = append(payload, checkpointPayload{CheckpointFile{Path: "context/" + name, Size: int64(len(data)), SHA256: checkpointHash(data), ModifiedNS: file.ModifiedNS}, data})
	}
	if !seen["agent.toml"] || !seen["installation.json"] || !seen["agent.token"] {
		return snapshot, nil, ErrRecovery
	}
	return snapshot, payload, nil
}
func (l *OfflineLease) payload(ctx context.Context) ([]checkpointPayload, string, StoreSnapshot, error) {
	if l == nil || l.closed {
		return nil, "", StoreSnapshot{}, ErrClosed
	}
	roots, err := l.root.names()
	if err != nil {
		return nil, "", StoreSnapshot{}, err
	}
	for _, name := range roots {
		switch name {
		case ".lock", "store.json", "identity.json", "operations", "secrets", "restore.json":
		default:
			return nil, "", StoreSnapshot{}, ErrRecovery
		}
	}
	marker, markerErr := readRestoreMarker(l.root)
	if markerErr != nil || (marker != nil && (marker.State != "confirmed" || !marker.Activated)) {
		return nil, "", StoreSnapshot{}, ErrRecovery
	}
	identity, _, err := l.root.readMetadata("identity.json", 1024)
	if err != nil || !identity.Exists {
		return nil, "", StoreSnapshot{}, ErrRecovery
	}
	var id struct {
		Version   int    `json:"version"`
		ServiceID string `json:"service_id"`
		StoreName string `json:"store_name"`
	}
	if decodeCheckpointJSON(identity.Bytes, &id) != nil || id.Version != 1 || !serviceIdentity.MatchString(id.ServiceID) || id.StoreName != "store.json" {
		return nil, "", StoreSnapshot{}, ErrRecovery
	}
	names, err := l.operations.names()
	if err != nil || len(names) > 3*l.options.MaxOperations {
		return nil, "", StoreSnapshot{}, ErrRecovery
	}
	expected := map[string]bool{}
	engine := &Engine{opts: l.options, root: l.root, operations: l.operations, storeName: id.StoreName, records: map[string]*record{}, keys: map[string]string{}}
	for _, name := range names {
		if len(name) < 65 || !digestString(name[:64]) {
			return nil, "", StoreSnapshot{}, ErrRecovery
		}
		suffix := name[64:]
		if suffix != ".json" && suffix != ".old" && suffix != ".new" {
			return nil, "", StoreSnapshot{}, ErrRecovery
		}
		if suffix == ".json" {
			data, _, err := l.operations.readMetadata(name, journalLimit)
			var record record
			if err != nil || decodeCheckpointJSON(data.Bytes, &record) != nil {
				return nil, "", StoreSnapshot{}, ErrRecovery
			}
			for _, suffix := range []string{".json", ".old", ".new"} {
				expected[name[:64]+suffix] = true
			}
		}
	}
	if len(expected) != len(names) {
		return nil, "", StoreSnapshot{}, ErrRecovery
	}
	for _, name := range names {
		if !expected[name] {
			return nil, "", StoreSnapshot{}, ErrRecovery
		}
	}
	if engine.load() != nil {
		return nil, "", StoreSnapshot{}, ErrRecovery
	}
	store, err := engine.readStore()
	if err != nil {
		return nil, "", StoreSnapshot{}, err
	}
	if engine.active != "" {
		r := engine.records[engine.active]
		digest := Digest(store)
		if digest != r.OldDigest && digest != r.NewDigest {
			return nil, "", StoreSnapshot{}, ErrConflict
		}
	}
	payload := []checkpointPayload{}
	var total int64
	add := func(dir *privateDir, name, prefix string, limit int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, mtime, err := dir.readMetadata(name, limit)
		if err != nil {
			return err
		}
		if !data.Exists {
			return nil
		}
		total += int64(len(data.Bytes))
		if total > MaxCheckpointBytes || len(payload) >= MaxCheckpointFiles {
			return ErrInvalid
		}
		payload = append(payload, checkpointPayload{CheckpointFile{Path: prefix + name, Size: int64(len(data.Bytes)), SHA256: checkpointHash(data.Bytes), ModifiedNS: mtime}, data.Bytes})
		return nil
	}
	for _, name := range []string{"store.json", "identity.json", "restore.json"} {
		limit := l.options.MaxBytes
		if name == "restore.json" {
			limit = restoreMarkerLimit
		}
		if err = add(l.root, name, "managed/", limit); err != nil {
			return nil, "", StoreSnapshot{}, err
		}
	}
	for _, name := range names {
		limit := l.options.MaxBytes
		if strings.HasSuffix(name, ".json") {
			limit = journalLimit
		}
		if err = add(l.operations, name, "managed/operations/", limit); err != nil {
			return nil, "", StoreSnapshot{}, err
		}
	}
	if l.secrets != nil {
		names, err := l.secrets.names()
		if err != nil || len(names) > MaxLocalSecrets {
			return nil, "", StoreSnapshot{}, ErrRecovery
		}
		for _, name := range names {
			if !strings.HasSuffix(name, ".secret") || !serviceIdentity.MatchString(strings.TrimSuffix(name, ".secret")) {
				return nil, "", StoreSnapshot{}, ErrRecovery
			}
			value, _, err := l.secrets.readMetadata(name, MaxSecretBytes)
			if err != nil || !validSecretValue(string(value.Bytes)) {
				return nil, "", StoreSnapshot{}, ErrRecovery
			}
			if err = add(l.secrets, name, "managed/secrets/", MaxSecretBytes); err != nil {
				return nil, "", StoreSnapshot{}, err
			}
		}
	}
	return payload, id.ServiceID, store, nil
}
func sameCheckpointPayload(a, b []checkpointPayload) bool {
	sort.Slice(a, func(i, j int) bool { return a[i].entry.Path < a[j].entry.Path })
	sort.Slice(b, func(i, j int) bool { return b[i].entry.Path < b[j].entry.Path })
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].entry != b[i].entry || !bytes.Equal(a[i].data, b[i].data) {
			return false
		}
	}
	return true
}

// ExportCheckpoint snapshots only a stopped, initialized generated Agent. The
// caller owns the fresh output directory and must remove it if export fails.
func ExportCheckpoint(ctx context.Context, options Options, capture ContextCapture, output string) (CheckpointSummary, error) {
	summary := CheckpointSummary{}
	lease, err := OpenOfflineLease(options)
	if err != nil {
		return summary, err
	}
	defer lease.Close()
	contextBefore, contextFiles, err := captureCheckpointContext(ctx, options.Root, capture)
	if err != nil {
		return summary, err
	}
	files, identity, store, err := lease.payload(ctx)
	if err != nil {
		return summary, err
	}
	// A pending journal must match the captured restart context exactly.
	engine := &Engine{opts: lease.options, root: lease.root, operations: lease.operations, storeName: "store.json", records: map[string]*record{}, keys: map[string]string{}}
	if engine.load() != nil {
		return summary, ErrRecovery
	}
	if engine.active != "" && engine.records[engine.active].ContextRevision != contextBefore.Revision {
		return summary, ErrConflict
	}
	all := append(append([]checkpointPayload{}, contextFiles...), files...)
	sort.Slice(all, func(i, j int) bool { return all[i].entry.Path < all[j].entry.Path })
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
	if !filepath.IsAbs(output) || filepath.Clean(output) != output || output == options.Root || strings.HasPrefix(output, options.Root+string(filepath.Separator)) {
		return summary, ErrUnsafePath
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
	manifest := CheckpointManifest{Kind: "frp-managed-checkpoint", Version: 1, ID: id, CreatedAtMS: time.Now().UnixMilli(), Root: options.Root, StoreName: "store.json", ConfigFile: contextBefore.ConfigFile, WorkingDir: contextBefore.WorkingDir, ContextRevision: contextBefore.Revision, ServiceID: identity, StoreExists: store.Exists, StoreDigest: Digest(store), Files: []CheckpointFile{}}
	for _, file := range all {
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}
		if err = writeCheckpointPayload(target, file); err != nil {
			return summary, err
		}
		manifest.Files = append(manifest.Files, file.entry)
	}
	contextAfter, afterContextFiles, err := captureCheckpointContext(ctx, options.Root, capture)
	if err != nil || contextBefore.Revision != contextAfter.Revision || !sameCheckpointPayload(contextFiles, afterContextFiles) {
		return summary, ErrConflict
	}
	afterFiles, afterIdentity, afterStore, err := lease.payload(ctx)
	if err != nil || afterIdentity != identity || Digest(afterStore) != Digest(store) || !sameCheckpointPayload(files, afterFiles) {
		return summary, ErrConflict
	}
	data, err := json.Marshal(manifest)
	if err != nil || int64(len(data)) > MaxCheckpointManifestBytes {
		return summary, ErrInvalid
	}
	if err = target.replace("CHECKPOINT.json", StoreSnapshot{Exists: true, Bytes: data}, Digest(StoreSnapshot{}), MaxCheckpointManifestBytes, nil); err != nil {
		return summary, err
	}
	return CheckpointSummary{Code: "ok", CheckpointID: id, ManifestDigest: checkpointHash(data), FileCount: len(all), TotalBytes: total}, nil
}
func writeCheckpointPayload(root *privateDir, payload checkpointPayload) error {
	parts := strings.Split(payload.entry.Path, "/")
	dir := root
	var opened []*privateDir
	defer func() {
		for _, d := range opened {
			d.close()
		}
	}()
	for _, name := range parts[:len(parts)-1] {
		next, err := dir.subdir(name)
		if err != nil {
			return err
		}
		opened = append(opened, next)
		dir = next
	}
	return dir.replace(parts[len(parts)-1], StoreSnapshot{Exists: true, Bytes: payload.data}, Digest(StoreSnapshot{}), MaxCheckpointFileBytes, nil)
}
