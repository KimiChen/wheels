package configuration

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

const MaxMigrationStoreBytes = 1 << 20

var migrationStoreName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type MigrationSelection struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type MigrationRequest struct {
	ExpectedRevision string
	TargetRoot       string
	TargetStore      string
	Select           []MigrationSelection
}
type MigrationObject struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Source  int    `json:"source"`
	Ordinal int    `json:"ordinal"`
	SHA256  string `json:"sha256"`
}
type MigrationFile struct {
	Source       int               `json:"source"`
	CommonSHA256 string            `json:"common_sha256"`
	Objects      []MigrationObject `json:"objects"`
}
type MigrationInclude struct {
	Pattern string   `json:"pattern"`
	Files   []string `json:"files"`
}

// MigrationPlan is a PRIVATE manifest. Original common fields and credentials
// remain in private source backup files; the manifest holds opaque digests.
// Candidate and Source.Data are never encoded by an ordinary JSON marshal.
type MigrationPlan struct {
	Version          int                `json:"version"`
	Revision         string             `json:"revision"`
	ConfigFile       string             `json:"config_file"`
	WorkingDir       string             `json:"working_dir"`
	TargetRoot       string             `json:"target_root"`
	TargetStore      string             `json:"target_store"`
	CommonSHA256     string             `json:"common_sha256"`
	EffectiveSHA256  string             `json:"effective_sha256"`
	AllObjectsSHA256 string             `json:"all_objects_sha256"`
	CandidateSHA256  string             `json:"candidate_sha256"`
	UnsafeFeatures   []string           `json:"unsafe_features"`
	Sources          []MigrationSource  `json:"sources"`
	Files            []MigrationFile    `json:"files"`
	Includes         []MigrationInclude `json:"includes"`
	Remove           []MigrationObject  `json:"remove"`
	Candidate        []byte             `json:"-"`
}

func migrationCommon(common any) string { data, _ := json.Marshal(common); return hash(data) }
func migrationStableCommon(s *snapshot) (string, error) {
	common, err := commonCopy(s.in.ReloadCommon)
	if err != nil {
		return "", err
	}
	common.Store.Path = ""
	common.Telemetry.ConfigManagement = nil
	return migrationCommon(common), nil
}

func migrationTarget(root, store string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || filepath.Dir(root) == root || !filepath.IsAbs(store) || filepath.Clean(store) != store || filepath.Dir(store) != root {
		return failure("migration_target_unsafe")
	}
	name := filepath.Base(store)
	reserved := strings.ToLower(name)
	if !migrationStoreName.MatchString(strings.TrimSuffix(name, ".json")) || strings.HasPrefix(name, ".") || reserved == "operations" || reserved == "secrets" || reserved == "identity.json" || reserved == "restore.json" {
		return failure("migration_target_unsafe")
	}
	for cursor := root; ; cursor = filepath.Dir(cursor) {
		info, err := os.Lstat(cursor)
		if err != nil && !(cursor == root && os.IsNotExist(err)) {
			return failure("migration_target_unsafe")
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (cursor == root && (info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid())))) {
			return failure("migration_target_unsafe")
		}
		if filepath.Dir(cursor) == cursor {
			break
		}
	}
	if info, err := os.Lstat(store); err == nil {
		stat := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
			return failure("migration_target_unsafe")
		}
	} else if !os.IsNotExist(err) {
		return failure("migration_target_unsafe")
	}
	return nil
}

func migrationSnapshot(input Input) (*snapshot, []migrationFile, error) {
	files, err := migrationFiles(input.ConfigFile, input.WorkingDir)
	if err != nil {
		return nil, nil, err
	}
	s, err := takeSnapshot(input)
	if err != nil {
		return nil, nil, err
	}
	if len(s.issues) != 0 {
		return nil, nil, failure(s.issues[0].Code)
	}
	for _, dep := range s.deps {
		if dep.State != "ready" {
			continue
		}
		info, err := os.Lstat(dep.Path)
		if err != nil || info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, failure("migration_source_unavailable")
		}
	}
	for _, values := range [][]object{s.files, s.store} {
		for _, o := range values {
			if hasPlugin(o) {
				return nil, nil, failure("migration_plugin_unsupported")
			}
		}
	}
	for _, file := range files {
		found := false
		for _, dep := range s.deps {
			if dep.Path == file.source.Path && dep.State == "ready" && hash(dep.Data) == file.source.SHA256 {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, failure("source_changed")
		}
	}
	return s, files, nil
}

// PlanMigration changes no source or Store. It proves equivalence of the full
// native object set after removing only the explicitly selected file objects.
func PlanMigration(input Input, request MigrationRequest) (*MigrationPlan, error) {
	if err := migrationTarget(request.TargetRoot, request.TargetStore); err != nil {
		return nil, err
	}
	if input.ReloadCommon == nil || input.ReloadCommon.Telemetry.ConfigManagement != nil && input.ReloadCommon.Telemetry.ConfigManagement.Enabled {
		return nil, failure("migration_already_managed")
	}
	if !input.ReloadCommon.Telemetry.Enabled {
		return nil, failure("migration_telemetry_required")
	}
	if len(request.Select) == 0 || len(request.Select) > MaxChanges {
		return nil, failure("invalid_change")
	}
	entries, err := os.ReadDir(request.TargetRoot)
	if err != nil && !os.IsNotExist(err) {
		return nil, failure("migration_target_unsafe")
	}
	for _, entry := range entries {
		if filepath.Join(request.TargetRoot, entry.Name()) != input.StoreFile || input.ReloadCommon.Store.Path == "" || entry.IsDir() {
			return nil, failure("migration_target_exists")
		}
	}
	s, files, err := migrationSnapshot(input)
	if err != nil {
		return nil, err
	}
	if request.ExpectedRevision != "" && request.ExpectedRevision != s.revision {
		return nil, failure("revision_conflict")
	}
	selected := map[string]bool{}
	for _, item := range request.Select {
		if !identifier(item.Name) || item.Kind != "proxy" && item.Kind != "visitor" || selected[item.Kind+"/"+item.Name] {
			return nil, failure("invalid_change")
		}
		selected[item.Kind+"/"+item.Name] = true
	}
	plan := &MigrationPlan{Version: 1, Revision: s.revision, ConfigFile: s.resolve(input.ConfigFile), WorkingDir: s.in.WorkingDir,
		TargetRoot: request.TargetRoot, TargetStore: request.TargetStore, UnsafeFeatures: append([]string{}, s.in.UnsafeFeatures...), Sources: []MigrationSource{}, Files: []MigrationFile{}, Remove: []MigrationObject{}}
	plan.Includes = []MigrationInclude{}
	for _, pattern := range s.patterns {
		plan.Includes = append(plan.Includes, MigrationInclude{Pattern: pattern.Pattern, Files: append([]string{}, pattern.Files...)})
	}
	plan.CommonSHA256, err = migrationStableCommon(s)
	if err != nil {
		return nil, err
	}
	plan.AllObjectsSHA256, _ = fingerprint(append(append([]object{}, s.files...), s.store...))
	index := map[string]int{}
	for _, dep := range s.deps {
		index[dep.Path] = len(plan.Sources)
		plan.Sources = append(plan.Sources, MigrationSource{Path: dep.Path, Kind: dep.Kind, SHA256: hash(dep.Data), Mode: dep.Mode & 0777, Exists: dep.State == "ready", Data: append([]byte(nil), dep.Data...)})
	}
	candidate, remaining := append([]object{}, s.store...), []object{}
	for _, file := range files {
		fileView := MigrationFile{Source: index[file.source.Path], CommonSHA256: migrationCommon(file.common), Objects: []MigrationObject{}}
		for ordinal, o := range file.objects {
			view := MigrationObject{Kind: o.kind, Name: o.name, Type: o.typ, Source: fileView.Source, Ordinal: ordinal, SHA256: migrationObjectHash(o)}
			if selected[o.key()] {
				plan.Remove = append(plan.Remove, view)
				candidate = append(candidate, o)
				delete(selected, o.key())
			} else {
				fileView.Objects = append(fileView.Objects, view)
				remaining = append(remaining, o)
			}
		}
		plan.Files = append(plan.Files, fileView)
	}
	if len(selected) != 0 {
		return nil, failure("not_found")
	}
	before, _, err := s.validateCandidate(s.store)
	if err != nil {
		return nil, err
	}
	s.files = remaining
	after, _, err := s.validateCandidate(candidate)
	if err != nil || !sameJSON(before, after) {
		// Native arrays may be reordered by moving objects into Store. Compare
		// the keyed complete configurations, never a lossy allowlisted preview.
		if err != nil {
			return nil, err
		}
		b, _ := fromNative(before, "file")
		a, _ := fromNative(after, "file")
		bh, _ := fingerprint(b)
		ah, _ := fingerprint(a)
		if bh != ah {
			return nil, failure("migration_not_equivalent")
		}
	}
	all, _ := fromNative(before, "file")
	plan.EffectiveSHA256, _ = fingerprint(all)
	plan.Candidate, err = encodeStore(candidate)
	if err != nil {
		return nil, err
	}
	if len(plan.Candidate) > MaxMigrationStoreBytes {
		return nil, failure("limit_exceeded")
	}
	plan.CandidateSHA256 = hash(plan.Candidate)
	last, _, err := migrationSnapshot(input)
	if err != nil || last.revision != plan.Revision {
		return nil, failure("source_changed")
	}
	if err := ValidateMigrationPlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// CheckMigration validates the installed disk configuration only. offline is
// an explicit maintenance acknowledgement, not a claim to detect live frpc.
func CheckMigration(plan *MigrationPlan, input Input, offline bool) error {
	if !offline {
		return failure("migration_offline_required")
	}
	if err := ValidateMigrationPlan(plan); err != nil {
		return err
	}
	if input.ReloadCommon == nil {
		return failure("migration_context_changed")
	}
	if filepath.Clean(input.ConfigFile) != plan.ConfigFile || input.WorkingDir != plan.WorkingDir || input.StoreFile != plan.TargetStore {
		return failure("migration_context_changed")
	}
	if err := migrationTarget(plan.TargetRoot, plan.TargetStore); err != nil {
		return err
	}
	opt := input.ReloadCommon.Telemetry.ConfigManagement
	if opt == nil || !opt.Enabled || opt.Root != plan.TargetRoot || input.ReloadCommon.Store.Path != plan.TargetStore {
		return failure("migration_not_enabled")
	}
	s, files, err := migrationSnapshot(input)
	if err != nil {
		return err
	}
	common, _ := migrationStableCommon(s)
	if common != plan.CommonSHA256 || hash(s.originalStore) != plan.CandidateSHA256 || !s.originalStoreExists || !sameJSON(append([]string{}, s.in.UnsafeFeatures...), plan.UnsafeFeatures) {
		return failure("migration_context_changed")
	}
	allHash, _ := fingerprint(append(append([]object{}, s.files...), s.store...))
	if allHash != plan.AllObjectsSHA256 {
		return failure("migration_not_equivalent")
	}
	if len(files) != len(plan.Files) {
		return failure("migration_sources_changed")
	}
	for i, file := range files {
		expected := plan.Files[i]
		if file.source.Path != plan.Sources[expected.Source].Path {
			return failure("migration_sources_changed")
		}
		if i != 0 && migrationCommon(file.common) != expected.CommonSHA256 {
			return failure("migration_context_changed")
		}
		actual, wanted := map[string]string{}, map[string]string{}
		for _, o := range file.objects {
			actual[o.key()] = migrationObjectHash(o)
		}
		for _, o := range expected.Objects {
			wanted[o.Kind+"/"+o.Name] = o.SHA256
		}
		if !sameJSON(actual, wanted) {
			return failure("migration_removal_incomplete")
		}
	}
	for _, source := range plan.Sources {
		if source.Kind != "local_file" {
			continue
		}
		found := false
		for _, dep := range s.deps {
			if dep.Path == source.Path && dep.State == "ready" && hash(dep.Data) == source.SHA256 {
				found = true
				break
			}
		}
		if !found {
			return failure("migration_context_changed")
		}
	}
	effective, _, err := s.validateCandidate(s.store)
	if err != nil {
		return err
	}
	values, _ := fromNative(effective, "file")
	fingerprint, _ := fingerprint(values)
	if fingerprint != plan.EffectiveSHA256 {
		return failure("migration_not_equivalent")
	}
	last, _, err := migrationSnapshot(input)
	if err != nil || last.revision != s.revision {
		return failure("source_changed")
	}
	return nil
}

func DecodeMigrationManifest(data []byte) (*MigrationPlan, error) {
	if len(data) > MaxMigrationStoreBytes || strictJSON(data) != nil {
		return nil, failure("migration_bundle_invalid")
	}
	var plan MigrationPlan
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&plan) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, failure("migration_bundle_invalid")
	}
	return &plan, nil
}

// ValidateMigrationPlan is also called after loading a private bundle. It
// validates its bounded fixed schema; source paths are only recovery metadata,
// never instructions for writing arbitrary files.
func ValidateMigrationPlan(plan *MigrationPlan) error {
	if plan == nil || plan.Version != 1 || len(plan.Sources) == 0 || len(plan.Sources) > MaxDependencies || len(plan.Files) == 0 || len(plan.Files) > MaxDependencies || len(plan.Remove) == 0 || len(plan.Remove) > MaxChanges || len(plan.Candidate) > MaxMigrationStoreBytes || hash(plan.Candidate) != plan.CandidateSHA256 {
		return failure("migration_bundle_invalid")
	}
	if migrationTarget(plan.TargetRoot, plan.TargetStore) != nil || !filepath.IsAbs(plan.ConfigFile) || !filepath.IsAbs(plan.WorkingDir) {
		return failure("migration_bundle_invalid")
	}
	seen := map[string]bool{}
	total := 0
	for _, source := range plan.Sources {
		total += len(source.Data)
		if !filepath.IsAbs(source.Path) || filepath.Clean(source.Path) != source.Path || seen[source.Path] || hash(source.Data) != source.SHA256 || (!source.Exists && len(source.Data) != 0) || total > MaxInputBytes {
			return failure("migration_bundle_invalid")
		}
		seen[source.Path] = true
	}
	if _, err := parseStore(plan.Candidate); err != nil {
		return failure("migration_bundle_invalid")
	}
	count := len(plan.Remove)
	for _, file := range plan.Files {
		if file.Source < 0 || file.Source >= len(plan.Sources) {
			return failure("migration_bundle_invalid")
		}
		count += len(file.Objects)
		for _, o := range file.Objects {
			if o.Source != file.Source {
				return failure("migration_bundle_invalid")
			}
		}
	}
	if count > MaxObjects {
		return failure("migration_bundle_invalid")
	}
	for _, o := range plan.Remove {
		if o.Source < 0 || o.Source >= len(plan.Sources) || !identifier(o.Name) || o.Kind != "proxy" && o.Kind != "visitor" {
			return failure("migration_bundle_invalid")
		}
	}
	if !sort.StringsAreSorted(plan.UnsafeFeatures) {
		return failure("migration_bundle_invalid")
	}
	if err := validateMigrationRecovery(plan); err != nil {
		return err
	}
	return validateMigrationClosure(plan)
}

// Derive the selection, remaining objects and candidate again from the private
// original bytes. Digests in a manifest alone are not evidence of preservation.
func validateMigrationRecovery(plan *MigrationPlan) error {
	invalid := failure("migration_bundle_invalid")
	removed := map[string]MigrationObject{}
	for _, o := range plan.Remove {
		key := o.Kind + "/" + o.Name
		if _, ok := removed[key]; ok {
			return invalid
		}
		removed[key] = o
	}
	all, candidate := []object{}, []object{}
	owners := map[string]bool{}
	files := map[int]bool{}
	for i, expected := range plan.Files {
		source := plan.Sources[expected.Source]
		if files[expected.Source] || !source.Exists || i == 0 && (source.Kind != "file" || source.Path != plan.ConfigFile) || i != 0 && source.Kind != "include" {
			return invalid
		}
		files[expected.Source] = true
		file, err := decodeMigrationFile(source.Path, source.Kind, source.Data)
		if err != nil || migrationCommon(file.common) != expected.CommonSHA256 {
			return invalid
		}
		remaining := []MigrationObject{}
		for ordinal, o := range file.objects {
			if owners[o.key()] || hasPlugin(o) {
				return invalid
			}
			owners[o.key()] = true
			all = append(all, o)
			view := MigrationObject{Kind: o.kind, Name: o.name, Type: o.typ, Source: expected.Source, Ordinal: ordinal, SHA256: migrationObjectHash(o)}
			if selected, ok := removed[o.key()]; ok {
				if selected != view {
					return invalid
				}
				candidate = append(candidate, o)
				delete(removed, o.key())
			} else {
				remaining = append(remaining, view)
			}
		}
		if !sameJSON(remaining, expected.Objects) {
			return invalid
		}
	}
	stores := 0
	for i, source := range plan.Sources {
		switch source.Kind {
		case "file", "include":
			if !files[i] {
				return invalid
			}
		case "local_file":
			if !source.Exists {
				return invalid
			}
		case "store":
			stores++
			if source.Exists {
				values, err := parseStore(source.Data)
				if err != nil {
					return invalid
				}
				for _, o := range values {
					if owners[o.key()] || hasPlugin(o) {
						return invalid
					}
					owners[o.key()] = true
				}
				all = append(all, values...)
				candidate = append(candidate, values...)
			}
		default:
			return invalid
		}
	}
	if stores != 1 || len(removed) != 0 {
		return invalid
	}
	data, err := encodeStore(candidate)
	allHash, _ := fingerprint(all)
	if err != nil || hash(data) != plan.CandidateSHA256 || allHash != plan.AllObjectsSHA256 {
		return invalid
	}
	return nil
}
