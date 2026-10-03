package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
)

// BackupGraphPolicy is trusted local authorization, never taken from an
// archive. Roots authorize subtrees; Mappings authorize single external files.
// All paths are original absolute paths. This increment does not relocate,
// restore, lock a running Agent, or export an Engine checkpoint.
type BackupGraphPolicy struct {
	ConfigFile string
	WorkingDir string
	StoreFile  string
	Roots      []BackupRoot
	Mappings   []BackupMapping
	Limits     BackupGraphLimits
}
type BackupRoot struct{ ID, Path string }
type BackupMapping struct{ ID, Path string }
type BackupGraphLimits struct {
	Files, Directories, Entries, Edges int
	FileBytes, TotalBytes              int64
}
type BackupGraphFile struct {
	ID    string
	Bytes []byte
}

// BackupGraph is private. ManifestBytes has a caller-bound digest; neither
// the manifest nor file payloads may be added to telemetry or safe summaries.
type BackupGraph struct {
	Manifest       backupmanifest.Manifest
	ManifestBytes  []byte
	ManifestDigest string
	Files          []BackupGraphFile
}

var backupIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func backupErr(code string) error { return failure("backup_" + code) }
func backupPath(path string) bool {
	return path != "" && len(path) <= 4096 && utf8.ValidString(path) && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}
func backupWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func normalizeBackupPolicy(in BackupGraphPolicy) (BackupGraphPolicy, string, error) {
	p := in
	p.Roots = append([]BackupRoot(nil), in.Roots...)
	p.Mappings = append([]BackupMapping(nil), in.Mappings...)
	if !backupPath(p.ConfigFile) || !backupPath(p.WorkingDir) || !backupPath(p.StoreFile) || p.ConfigFile == p.StoreFile || len(p.Roots) == 0 || len(p.Roots)+len(p.Mappings) > backupmanifest.MaxFiles {
		return p, "", backupErr("policy_invalid")
	}
	ids, paths := map[string]bool{}, map[string]bool{}
	for _, root := range p.Roots {
		if !backupIdentifier.MatchString(root.ID) || ids[root.ID] || !backupPath(root.Path) || filepath.Dir(root.Path) == root.Path || paths[root.Path] {
			return p, "", backupErr("policy_invalid")
		}
		ids[root.ID], paths[root.Path] = true, true
	}
	for i, root := range p.Roots {
		for j, other := range p.Roots {
			if i != j && backupWithin(root.Path, other.Path) {
				return p, "", backupErr("policy_invalid")
			}
		}
	}
	for _, mapping := range p.Mappings {
		if !backupIdentifier.MatchString(mapping.ID) || ids[mapping.ID] || !backupPath(mapping.Path) || paths[mapping.Path] {
			return p, "", backupErr("policy_invalid")
		}
		for _, root := range p.Roots {
			if backupWithin(mapping.Path, root.Path) {
				return p, "", backupErr("policy_invalid")
			}
		}
		ids[mapping.ID], paths[mapping.Path] = true, true
	}
	sort.Slice(p.Roots, func(i, j int) bool { return p.Roots[i].ID < p.Roots[j].ID })
	sort.Slice(p.Mappings, func(i, j int) bool { return p.Mappings[i].ID < p.Mappings[j].ID })
	max := BackupGraphLimits{backupmanifest.MaxFiles, backupmanifest.MaxDirectories, backupmanifest.MaxEntries, backupmanifest.MaxEdges, backupmanifest.MaxFileBytes, backupmanifest.MaxTotalBytes}
	for _, pair := range [][2]*int{{&p.Limits.Files, &max.Files}, {&p.Limits.Directories, &max.Directories}, {&p.Limits.Entries, &max.Entries}, {&p.Limits.Edges, &max.Edges}} {
		if *pair[0] == 0 {
			*pair[0] = *pair[1]
		}
		if *pair[0] < 1 || *pair[0] > *pair[1] {
			return p, "", backupErr("policy_invalid")
		}
	}
	for _, pair := range [][2]*int64{{&p.Limits.FileBytes, &max.FileBytes}, {&p.Limits.TotalBytes, &max.TotalBytes}} {
		if *pair[0] == 0 {
			*pair[0] = *pair[1]
		}
		if *pair[0] < 1 || *pair[0] > *pair[1] {
			return p, "", backupErr("policy_invalid")
		}
	}
	if _, err := p.logical(p.ConfigFile, false); err != nil {
		return p, "", err
	}
	if _, err := p.logical(p.StoreFile, false); err != nil {
		return p, "", err
	}
	if _, err := p.logical(p.WorkingDir, true); err != nil {
		return p, "", err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return p, "", backupErr("policy_invalid")
	}
	return p, backupmanifest.Digest(data), nil
}

func (p BackupGraphPolicy) logical(path string, directory bool) (string, error) {
	if !backupPath(path) {
		return "", backupErr("path_unauthorized")
	}
	for _, root := range p.Roots {
		if backupWithin(path, root.Path) {
			rel, _ := filepath.Rel(root.Path, path)
			if !directory && rel == "." {
				return "", backupErr("path_unauthorized")
			}
			return "roots/" + root.ID + "/" + filepath.ToSlash(rel), nil
		}
	}
	if !directory {
		for _, mapping := range p.Mappings {
			if path == mapping.Path {
				return "files/" + mapping.ID, nil
			}
		}
	}
	return "", backupErr("path_unauthorized")
}

// Source views have no fallback. A sealed view can read only supplied files
// and inventories; native parsing is shared with live bounded collection.
type backupSourceView interface {
	read(context.Context, string) (backupSourceFile, error)
	directory(context.Context, string) ([]backupmanifest.Entry, error)
}
type backupSourceFile struct {
	data     []byte
	exists   bool
	mode     uint32
	modified int64
}

// CaptureBackupGraph checks the entire closure twice, including full include
// directory inventories. Callers must separately hold their offline lease.
// It detects observed drift; it is not an atomic filesystem snapshot and
// cannot freeze independent writers. It captures disk inputs, not a running
// Service's in-memory sources, Engine identity, journals or secret vault.
func CaptureBackupGraph(ctx context.Context, policy BackupGraphPolicy) (*BackupGraph, error) {
	return captureBackupGraph(ctx, policy, nil)
}
func captureBackupGraph(ctx context.Context, policy BackupGraphPolicy, between func()) (*BackupGraph, error) {
	p, digest, err := normalizeBackupPolicy(policy)
	if err != nil {
		return nil, err
	}
	firstView, err := newBackupDiskView(p)
	if err != nil {
		return nil, err
	}
	defer firstView.close()
	first, err := buildBackupGraph(ctx, p, digest, firstView)
	if err != nil {
		return nil, err
	}
	if firstView.check() != nil {
		return nil, backupErr("source_changed")
	}
	if between != nil {
		between()
	}
	secondView, err := newBackupDiskView(p)
	if err != nil {
		return nil, err
	}
	defer secondView.close()
	second, err := buildBackupGraph(ctx, p, digest, secondView)
	if err != nil {
		return nil, err
	}
	if firstView.check() != nil || secondView.check() != nil || first.ManifestDigest != second.ManifestDigest || !firstView.sameIdentity(secondView) {
		return nil, backupErr("source_changed")
	}
	return first, nil
}

// ValidateBackupGraph requires a digest held by a trusted caller. Recomputing
// that expected value from an untrusted archive is not authenticity checking.
// No filesystem or process environment reads occur on this path.
func ValidateBackupGraph(ctx context.Context, policy BackupGraphPolicy, manifestBytes []byte, files []BackupGraphFile, expectedManifestDigest string) (*BackupGraph, error) {
	p, digest, err := normalizeBackupPolicy(policy)
	if err != nil {
		return nil, err
	}
	if !backupmanifest.ValidDigest(expectedManifestDigest) || backupmanifest.Digest(manifestBytes) != expectedManifestDigest {
		return nil, backupErr("manifest_mismatch")
	}
	m, err := backupmanifest.Decode(manifestBytes)
	if err != nil {
		return nil, backupErr("manifest_invalid")
	}
	if m.PolicyDigest != digest || m.ConfigFile != p.ConfigFile || m.WorkingDir != p.WorkingDir || m.StoreFile != p.StoreFile {
		return nil, backupErr("policy_mismatch")
	}
	view, err := newBackupSealedView(p, m, files)
	if err != nil {
		return nil, err
	}
	rebuilt, err := buildBackupGraph(ctx, p, digest, view)
	if err != nil {
		return nil, err
	}
	canonical, err := backupmanifest.Encode(m)
	if err != nil || !bytes.Equal(canonical, rebuilt.ManifestBytes) || len(view.usedFiles) != len(view.files) || len(view.usedDirectories) != len(view.directories) {
		return nil, backupErr("closure_mismatch")
	}
	// Keep the caller-bound original encoding and clone all returned buffers.
	rebuilt.ManifestBytes = append([]byte(nil), manifestBytes...)
	rebuilt.ManifestDigest = expectedManifestDigest
	return rebuilt, nil
}

func CheckBackupGraph(ctx context.Context, policy BackupGraphPolicy, graph *BackupGraph) error {
	if graph == nil {
		return backupErr("manifest_invalid")
	}
	validated, err := ValidateBackupGraph(ctx, policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest)
	if err != nil {
		return err
	}
	current, err := CaptureBackupGraph(ctx, policy)
	if err != nil {
		return err
	}
	canonical, err := backupmanifest.Encode(validated.Manifest)
	if err != nil || !bytes.Equal(current.ManifestBytes, canonical) {
		return backupErr("source_changed")
	}
	return nil
}

type backupGraphBuilder struct {
	ctx         context.Context
	policy      BackupGraphPolicy
	view        backupSourceView
	manifest    backupmanifest.Manifest
	files       map[string]backupSourceFile
	fileKinds   map[string]string
	directories map[string][]backupmanifest.Entry
	total       int64
	entries     int
}

func (b *backupGraphBuilder) resolve(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(b.policy.WorkingDir, path)
}
func (b *backupGraphBuilder) read(path, kind string, missing bool) (backupSourceFile, error) {
	if err := b.ctx.Err(); err != nil {
		return backupSourceFile{}, err
	}
	if _, err := b.policy.logical(path, false); err != nil {
		return backupSourceFile{}, err
	}
	if prior, ok := b.files[path]; ok {
		if b.fileKinds[path] != kind || (!prior.exists && !missing) {
			return backupSourceFile{}, backupErr("ambiguous_dependency")
		}
		return prior, nil
	}
	if len(b.files) >= b.policy.Limits.Files {
		return backupSourceFile{}, backupErr("limit_exceeded")
	}
	file, err := b.view.read(b.ctx, path)
	if err != nil {
		return backupSourceFile{}, err
	}
	if !file.exists && !missing {
		return backupSourceFile{}, backupErr("dependency_missing")
	}
	if int64(len(file.data)) > b.policy.Limits.FileBytes || b.total+int64(len(file.data)) > b.policy.Limits.TotalBytes {
		return backupSourceFile{}, backupErr("limit_exceeded")
	}
	b.total += int64(len(file.data))
	b.files[path], b.fileKinds[path] = file, kind
	return file, nil
}
func (b *backupGraphBuilder) reference(from, field, to, kind string, missing bool) (backupSourceFile, error) {
	if len(b.manifest.References) >= b.policy.Limits.Edges {
		return backupSourceFile{}, backupErr("limit_exceeded")
	}
	b.manifest.References = append(b.manifest.References, backupmanifest.Reference{From: from, Field: field, To: to})
	return b.read(to, kind, missing)
}
func (b *backupGraphBuilder) list(path string) ([]backupmanifest.Entry, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := b.policy.logical(path, true); err != nil {
		return nil, err
	}
	if entries, ok := b.directories[path]; ok {
		return entries, nil
	}
	if len(b.directories) >= b.policy.Limits.Directories {
		return nil, backupErr("limit_exceeded")
	}
	entries, err := b.view.directory(b.ctx, path)
	if err != nil {
		return nil, err
	}
	if b.entries+len(entries) > b.policy.Limits.Entries {
		return nil, backupErr("limit_exceeded")
	}
	b.entries += len(entries)
	b.directories[path] = entries
	return entries, nil
}
func buildBackupGraph(ctx context.Context, p BackupGraphPolicy, digest string, view backupSourceView) (*BackupGraph, error) {
	b := &backupGraphBuilder{ctx: ctx, policy: p, view: view, files: map[string]backupSourceFile{}, fileKinds: map[string]string{}, directories: map[string][]backupmanifest.Entry{}, manifest: backupmanifest.Manifest{Version: backupmanifest.Version, Kind: backupmanifest.Kind, PolicyDigest: digest, ConfigFile: p.ConfigFile, WorkingDir: p.WorkingDir, StoreFile: p.StoreFile, Files: []backupmanifest.File{}, Directories: []backupmanifest.Directory{}, Includes: []backupmanifest.Include{}, References: []backupmanifest.Reference{}}}
	if err := b.collect(); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(b.files))
	for path := range b.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := &BackupGraph{Files: []BackupGraphFile{}}
	for i, path := range paths {
		file := b.files[path]
		logical, _ := p.logical(path, false)
		id := fmt.Sprintf("f%06d", i+1)
		entry := backupmanifest.File{ID: id, Path: path, LogicalPath: logical, Kind: b.fileKinds[path], Exists: file.exists, Size: int64(len(file.data)), Mode: file.mode, ModifiedNS: file.modified}
		if file.exists {
			entry.SHA256 = backupmanifest.Digest(file.data)
			out.Files = append(out.Files, BackupGraphFile{ID: id, Bytes: append([]byte(nil), file.data...)})
		}
		b.manifest.Files = append(b.manifest.Files, entry)
	}
	paths = paths[:0]
	for path := range b.directories {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		b.manifest.Directories = append(b.manifest.Directories, backupmanifest.Directory{Path: path, Entries: append([]backupmanifest.Entry{}, b.directories[path]...)})
	}
	data, err := backupmanifest.Encode(b.manifest)
	if err != nil {
		return nil, backupErr("limit_exceeded")
	}
	out.Manifest, out.ManifestBytes, out.ManifestDigest = b.manifest, data, backupmanifest.Digest(data)
	return out, nil
}

// Only live collection checks ambient defaults. A sealed graph neither reads
// an environment nor promises that a future service has the same environment.
func backupAmbientProxyUnsupported(view backupSourceView, explicit string) bool {
	_, live := view.(*backupDiskView)
	return live && explicit == "" && os.Getenv("http_proxy") != ""
}
