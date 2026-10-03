package managed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const restorePlanName = "restore-install.json"

type restorePlanEntry struct {
	Target        string `json:"target"`
	Payload       string `json:"payload"`
	OldDigest     string `json:"old_digest"`
	OldModifiedNS int64  `json:"old_mtime_ns"`
	NewDigest     string `json:"new_digest"`
	NewModifiedNS int64  `json:"new_mtime_ns"`
	NewExists     bool   `json:"new_exists"`
}
type restoreInstallPlan struct {
	Version           int                `json:"version"`
	ManifestDigest    string             `json:"manifest_digest"`
	ContextRevision   string             `json:"context_revision"`
	Epoch             string             `json:"epoch"`
	ServiceID         string             `json:"service_id"`
	BackupServiceID   string             `json:"backup_service_id"`
	ReplacedServiceID string             `json:"replaced_service_id"`
	CreatedAtMS       int64              `json:"created_at_ms"`
	Entries           []restorePlanEntry `json:"entries"`
}

func validPlanTarget(path string) bool {
	if !validV2Relative(path) {
		return false
	}
	if strings.HasPrefix(path, "managed/") {
		return path != "managed/restore.json" && path != "managed/restore-install.json" && validCheckpointPath(path)
	}
	return path != "managed"
}
func (p restoreInstallPlan) valid() bool {
	if p.Version != 1 || !digestString(p.ManifestDigest) || !digestString(p.ContextRevision) || !serviceIdentity.MatchString(p.Epoch) || !serviceIdentity.MatchString(p.ServiceID) || !serviceIdentity.MatchString(p.BackupServiceID) || p.ReplacedServiceID != "" && !serviceIdentity.MatchString(p.ReplacedServiceID) || p.CreatedAtMS <= 0 || len(p.Entries) < 3 || len(p.Entries) > MaxCheckpointFiles {
		return false
	}
	seen := map[string]bool{}
	for _, entry := range p.Entries {
		if !validPlanTarget(entry.Target) || seen[entry.Target] || !digestString(entry.OldDigest) || !digestString(entry.NewDigest) || entry.OldModifiedNS < 0 || entry.NewExists && entry.NewModifiedNS <= 0 || !entry.NewExists && (entry.NewModifiedNS != 0 || entry.Payload != "" || entry.NewDigest != Digest(StoreSnapshot{})) {
			return false
		}
		if entry.NewExists && entry.Target != "managed/identity.json" && !validV2Payload(entry.Payload) {
			return false
		}
		if entry.NewExists && entry.Target == "managed/identity.json" && entry.Payload != "@identity" {
			return false
		}
		seen[entry.Target] = true
	}
	return seen["agent.toml"] && seen["installation.json"] && seen["managed/identity.json"] && seen["managed/store.json"]
}
func readRestorePlan(root *privateDir, expected string) (*restoreInstallPlan, error) {
	file, _, err := root.readMetadata(restorePlanName, MaxCheckpointManifestBytes)
	if err != nil {
		return nil, err
	}
	if !file.Exists {
		return nil, nil
	}
	if expected != "" && checkpointHash(file.Bytes) != expected {
		return nil, ErrConflict
	}
	var plan restoreInstallPlan
	if decodeCheckpointJSON(file.Bytes, &plan) != nil || !plan.valid() {
		return nil, ErrRecovery
	}
	return &plan, nil
}
func planDigest(plan restoreInstallPlan) (string, []byte, error) {
	if !plan.valid() {
		return "", nil, ErrRecovery
	}
	data, err := json.Marshal(plan)
	if err != nil || int64(len(data)) > MaxCheckpointManifestBytes {
		return "", nil, ErrInvalid
	}
	return checkpointHash(data), data, nil
}

// Validate the durable installation gate before Open can create identity or
// operation files. An interrupted plan must remain retryable even if identity
// was absent when offline installation began.
func checkRestorePlanStartup(root *privateDir, marker *restoreMarker) error {
	if marker != nil && marker.CheckpointVersion == 2 {
		plan, err := readRestorePlan(root, marker.PlanDigest)
		if err != nil || !planMatchesMarker(plan, marker) {
			return ErrRecovery
		}
		return nil
	}
	file, err := root.read(restorePlanName, MaxCheckpointManifestBytes)
	if err != nil || file.Exists {
		return ErrRecovery
	}
	return nil
}

func v2Identity(service string) []byte {
	data, _ := json.Marshal(struct {
		Version   int    `json:"version"`
		ServiceID string `json:"service_id"`
		StoreName string `json:"store_name"`
	}{1, service, "store.json"})
	return data
}
func planMarker(plan restoreInstallPlan, digest string, operations int) restoreMarker {
	return restoreMarker{Version: 1, CheckpointVersion: 2, PlanDigest: digest, RestoreStatus: RestoreStatus{State: "installing", Epoch: plan.Epoch, BackupServiceID: plan.BackupServiceID, ReplacedServiceID: plan.ReplacedServiceID, ManifestDigest: plan.ManifestDigest, ContextRevision: plan.ContextRevision, OperationsCount: operations}, ServiceID: plan.ServiceID, ConfigFile: "", CreatedAtMS: plan.CreatedAtMS}
}
func planMatchesMarker(plan *restoreInstallPlan, marker *restoreMarker) bool {
	return plan != nil && marker != nil && plan.Epoch == marker.Epoch && plan.ServiceID == marker.ServiceID && plan.ManifestDigest == marker.ManifestDigest && plan.ContextRevision == marker.ContextRevision && plan.BackupServiceID == marker.BackupServiceID && plan.ReplacedServiceID == marker.ReplacedServiceID
}
func v2Walk(installation *privateDir, path string, create bool) (*privateDir, string, func(), error) {
	if !validV2Relative(path) {
		return nil, "", func() {}, ErrUnsafePath
	}
	parts := strings.Split(path, "/")
	dir := installation
	opened := []*privateDir{}
	close := func() {
		for i := len(opened) - 1; i >= 0; i-- {
			opened[i].close()
		}
	}
	for _, part := range parts[:len(parts)-1] {
		var next *privateDir
		var err error
		if create {
			next, err = dir.subdir(part)
		} else {
			next, err = dir.optionalSubdir(part)
		}
		if err != nil || next == nil {
			close()
			if err != nil {
				return nil, "", func() {}, err
			}
			return nil, parts[len(parts)-1], func() {}, nil
		}
		opened = append(opened, next)
		dir = next
	}
	return dir, parts[len(parts)-1], close, nil
}
func v2ReadTarget(installation *privateDir, path string, limit int64) (StoreSnapshot, int64, error) {
	dir, name, close, err := v2Walk(installation, path, false)
	defer close()
	if err != nil {
		return StoreSnapshot{}, 0, err
	}
	if dir == nil {
		return StoreSnapshot{}, 0, nil
	}
	return dir.readMetadata(name, limit)
}
func checkRestoreV2Sources(root *privateDir, marker *restoreMarker) error {
	if marker == nil || marker.CheckpointVersion != 2 || marker.Activated {
		return nil
	}
	plan, err := readRestorePlan(root, marker.PlanDigest)
	if err != nil || !planMatchesMarker(plan, marker) {
		return ErrRecovery
	}
	installation, err := openExistingPrivateDir(filepath.Dir(root.path))
	if err != nil {
		return err
	}
	defer installation.close()
	for _, entry := range plan.Entries {
		if strings.HasPrefix(entry.Target, "managed/") {
			continue
		}
		file, stamp, err := v2ReadTarget(installation, entry.Target, MaxCheckpointFileBytes)
		if err != nil || Digest(file) != entry.NewDigest || stamp != entry.NewModifiedNS {
			return ErrConflict
		}
	}
	return nil
}

func (l *OfflineLease) variantsForContext() ([]StoreVariant, *Engine, error) {
	e := &Engine{opts: l.options, root: l.root, operations: l.operations, storeName: "store.json", records: map[string]*record{}, keys: map[string]string{}}
	if err := e.load(); err != nil {
		return nil, nil, err
	}
	store, err := e.readStore()
	if err != nil {
		return nil, nil, err
	}
	out := []StoreVariant{{Path: "managed/store.json", Snapshot: store}}
	for _, r := range e.records {
		if r.MaterialsState == MaterialsExpired {
			continue
		} // e.load already applied the strict record policy.
		for _, suffix := range []string{".old", ".new"} {
			name := idHash(r.ID) + suffix
			data, _, err := l.operations.readMetadata(name, l.options.MaxBytes)
			if err != nil || !data.Exists {
				return nil, nil, ErrRecovery
			}
			value := StoreSnapshot{Exists: true, Bytes: data.Bytes}
			expected := r.NewDigest
			if suffix == ".old" {
				value.Exists = r.OldExists
				expected = r.OldDigest
			}
			if Digest(value) != expected {
				return nil, nil, ErrRecovery
			}
			out = append(out, StoreVariant{Path: "managed/operations/" + name, Snapshot: value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, e, nil
}
func ConfirmRestorationV2(ctx context.Context, options Options, epoch, digest string, capture ContextCaptureV2) (RestoreSummary, error) {
	var out RestoreSummary
	l, err := OpenOfflineLease(options)
	if err != nil {
		return out, err
	}
	defer l.Close()
	m, err := readRestoreMarker(l.root)
	if err != nil || m == nil || m.CheckpointVersion != 2 {
		return out, ErrRecovery
	}
	if m.State != "acknowledged" || m.Activated || m.Epoch != epoch || m.ManifestDigest != digest || !m.RuntimeLoaded || !m.ResourcesReady {
		return out, ErrConflict
	}
	if err = checkRestoreV2Sources(l.root, m); err != nil {
		return out, err
	}
	variants, engine, err := l.variantsForContext()
	if err != nil || engine.active != "" {
		return out, ErrRecovery
	}
	var current StoreSnapshot
	for _, v := range variants {
		if v.Path == "managed/store.json" {
			current = v.Snapshot
		}
	}
	if Digest(current) != m.StoreDigest {
		return out, ErrConflict
	}
	context, _, err := captureContextV2(ctx, options.Root, capture, variants)
	if err != nil || context.Revision != m.ContextRevision {
		return out, ErrConflict
	}
	identity, _, err := l.root.readMetadata("identity.json", 1024)
	if err != nil || !bytesEqual(identity.Bytes, v2Identity(m.ServiceID)) {
		return out, ErrConflict
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err = checkRestoreV2Sources(l.root, m); err != nil {
		return out, err
	}
	m.State = "confirmed"
	if err = writeRestoreMarker(l.root, m); err != nil {
		return out, err
	}
	return RestoreSummary{Code: "ok", Epoch: m.Epoch, ServiceID: m.ServiceID, ManifestDigest: m.ManifestDigest, State: "confirmed"}, nil
}
func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

func RestoreCheckpointVersion(rootPath string) (int, error) {
	root, err := openExistingPrivateDir(rootPath)
	if err != nil {
		return 0, err
	}
	defer root.close()
	marker, err := readRestoreMarker(root)
	if err != nil || marker == nil {
		return 0, ErrRecovery
	}
	if marker.CheckpointVersion == 2 {
		return 2, nil
	}
	return 1, nil
}

func newInstallPlan(m CheckpointManifestV2, payload []checkpointPayload, installation *privateDir, root *privateDir, digest string) (restoreInstallPlan, error) {
	plan := restoreInstallPlan{Version: 1, ManifestDigest: digest, ContextRevision: m.ContextRevision, BackupServiceID: m.ServiceID, CreatedAtMS: time.Now().UnixMilli(), Entries: []restorePlanEntry{}}
	var err error
	plan.Epoch, err = newPrivateID()
	if err != nil {
		return plan, err
	}
	plan.ServiceID, err = newPrivateID()
	if err != nil {
		return plan, err
	}
	id, _, err := root.readMetadata("identity.json", 1024)
	if err != nil {
		return plan, err
	}
	if id.Exists {
		var value struct {
			Version   int    `json:"version"`
			ServiceID string `json:"service_id"`
			StoreName string `json:"store_name"`
		}
		if decodeCheckpointJSON(id.Bytes, &value) != nil || value.Version != 1 || !serviceIdentity.MatchString(value.ServiceID) || value.StoreName != "store.json" {
			return plan, ErrRecovery
		}
		plan.ReplacedServiceID = value.ServiceID
	}
	desired := map[string]checkpointPayload{}
	for _, file := range payload {
		if file.entry.Path == "GRAPH.json" || file.entry.Path == "managed/restore.json" || file.entry.Path == "managed/restore-install.json" {
			continue
		}
		target := file.entry.Path
		if strings.HasPrefix(target, "context/") {
			target = strings.TrimPrefix(target, "context/")
		}
		desired[target] = file
	}
	if !m.StoreExists {
		desired["managed/store.json"] = checkpointPayload{}
	}
	for _, sub := range []string{"operations", "secrets"} {
		dir, err := root.optionalSubdir(sub)
		if err != nil {
			return plan, err
		}
		if dir == nil {
			continue
		}
		names, err := dir.names()
		dir.close()
		if err != nil {
			return plan, err
		}
		for _, name := range names {
			target := "managed/" + sub + "/" + name
			if !validCheckpointPath(target) {
				return plan, ErrRecovery
			}
			if _, ok := desired[target]; !ok {
				desired[target] = checkpointPayload{}
			}
		}
	}
	paths := make([]string, 0, len(desired))
	for path := range desired {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, target := range paths {
		file := desired[target]
		old, stamp, err := v2ReadTarget(installation, target, v2EntryLimit(file.entry.Path))
		if err != nil {
			return plan, err
		}
		entry := restorePlanEntry{Target: target, Payload: file.entry.Path, OldDigest: Digest(old), OldModifiedNS: stamp}
		value := StoreSnapshot{Exists: file.entry.Path != "", Bytes: file.data}
		if target == "managed/identity.json" {
			value = StoreSnapshot{Exists: true, Bytes: v2Identity(plan.ServiceID)}
			entry.Payload = "@identity"
			entry.NewModifiedNS = time.Now().UnixNano()
		} else if value.Exists {
			entry.NewModifiedNS = file.entry.ModifiedNS
		}
		entry.NewExists, entry.NewDigest = value.Exists, Digest(value)
		plan.Entries = append(plan.Entries, entry)
	}
	if !plan.valid() {
		return plan, ErrRecovery
	}
	return plan, nil
}
