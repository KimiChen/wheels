package managed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const restoreMarkerLimit = 16 * 1024

// RestoreStatus contains safe recovery facts only. Native paths remain in the
// private marker, never in the management protocol or operation audit.
type RestoreStatus struct {
	State             string `json:"state"`
	Epoch             string `json:"epoch"`
	BackupServiceID   string `json:"backup_service_id"`
	ReplacedServiceID string `json:"replaced_service_id"`
	ManifestDigest    string `json:"manifest_digest"`
	ContextRevision   string `json:"context_revision"`
	StoreDigest       string `json:"store_digest"`
	AcknowledgementID string `json:"acknowledgement_id"`
	RuntimeLoaded     bool   `json:"runtime_loaded"`
	ResourcesReady    bool   `json:"resources_ready"`
	OperationsCount   int    `json:"operations_count"`
}
type restoreMarker struct {
	Version int `json:"version"`
	RestoreStatus
	ServiceID    string `json:"service_id"`
	ConfigFile   string `json:"config_file"`
	WorkingDir   string `json:"cwd"`
	CreatedAtMS  int64  `json:"created_at_ms"`
	VerifiedAtMS int64  `json:"verified_at_ms"`
	// confirmed first requires one more runtime verification on restart. Once
	// activated, normal later Store changes do not become restoration drift.
	Activated bool `json:"activated"`
}
type RestoreSummary struct {
	Code           string `json:"code"`
	Epoch          string `json:"epoch"`
	ServiceID      string `json:"service_id"`
	ManifestDigest string `json:"manifest_digest"`
	State          string `json:"state"`
}

func (m restoreMarker) valid(root string) bool {
	if m.Version != 1 || !serviceIdentity.MatchString(m.Epoch) || !serviceIdentity.MatchString(m.ServiceID) || !serviceIdentity.MatchString(m.BackupServiceID) || (m.ReplacedServiceID != "" && !serviceIdentity.MatchString(m.ReplacedServiceID)) || !digestString(m.ManifestDigest) || !digestString(m.ContextRevision) || m.ConfigFile != filepath.Join(filepath.Dir(root), "agent.toml") || m.WorkingDir != filepath.Dir(root) || m.CreatedAtMS <= 0 || m.OperationsCount < 0 || m.OperationsCount > 1024 {
		return false
	}
	switch m.State {
	case "installing", "pending":
		return m.StoreDigest == "" && m.AcknowledgementID == "" && !m.RuntimeLoaded && !m.ResourcesReady && !m.Activated
	case "verified":
		return digestString(m.StoreDigest) && m.RuntimeLoaded && m.ResourcesReady && m.AcknowledgementID == "" && !m.Activated && m.VerifiedAtMS > 0
	case "acknowledged", "confirmed":
		return digestString(m.StoreDigest) && m.RuntimeLoaded && m.ResourcesReady && serviceIdentity.MatchString(m.AcknowledgementID) && m.VerifiedAtMS > 0 && (m.State == "confirmed" || !m.Activated)
	}
	return false
}
func readRestoreMarker(root *privateDir) (*restoreMarker, error) {
	file, err := root.read("restore.json", restoreMarkerLimit)
	if err != nil {
		return nil, err
	}
	if !file.Exists {
		return nil, nil
	}
	var marker restoreMarker
	if decodeCheckpointJSON(file.Bytes, &marker) != nil || !marker.valid(root.path) {
		return nil, ErrRecovery
	}
	return &marker, nil
}
func writeRestoreMarker(root *privateDir, marker *restoreMarker) error {
	if marker == nil || !marker.valid(root.path) {
		return ErrRecovery
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return ErrStorage
	}
	return root.replace("restore.json", StoreSnapshot{Exists: true, Bytes: data}, "", restoreMarkerLimit, nil)
}

func (e *Engine) loadRestore() error {
	marker, err := readRestoreMarker(e.root)
	if err != nil {
		return err
	}
	if marker == nil {
		return nil
	}
	if marker.State == "installing" {
		return ErrRecovery
	}
	if marker.ServiceID != e.serviceID {
		return ErrRecovery
	}
	if !marker.Activated {
		if e.opts.RestoreCheck == nil {
			return ErrRecovery
		}
		ctx, cancel := context.WithTimeout(context.Background(), e.opts.RollbackTimeout)
		defer cancel()
		if checkSafely(func(ctx context.Context, _ Operation, _ string) error {
			return e.opts.RestoreCheck(ctx, marker.ContextRevision)
		}, ctx, Operation{}, "restore") != nil {
			return ErrRecovery
		}
	}
	e.restore = marker
	return nil
}
func (e *Engine) restoreBlocked() bool { return e.restore != nil && !e.restore.Activated }
func (e *Engine) ManagementWriteBlocked() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.restoreBlocked()
}
func (e *Engine) RestoreStatus() RestoreStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.restore == nil {
		return RestoreStatus{State: "none"}
	}
	return e.restoreStatusLocked()
}
func (e *Engine) restoreStatusLocked() RestoreStatus {
	status := e.restore.RestoreStatus
	if e.restoreBlocked() && !e.restoreVerified {
		status.State = "pending"
		status.StoreDigest = ""
		status.AcknowledgementID = ""
		status.RuntimeLoaded = false
		status.ResourcesReady = false
	}
	return status
}

// Begin after the ordinary startup rollback worker. Writes stay blocked while
// verification runs; at most this one additional worker exists, and Close keeps
// the same lifecycle lock until its native callback actually returns.
func (e *Engine) scheduleRestoreVerification() {
	if !e.restoreBlocked() {
		return
	}
	var previous <-chan struct{}
	if e.running && e.worker != nil {
		previous = e.worker.done
	}
	go func() {
		if previous != nil {
			<-previous
		}
		e.mu.Lock()
		if e.closing || e.running || e.active != "" {
			e.mu.Unlock()
			return
		}
		e.running = true
		e.mu.Unlock()
		e.verifyRestoration()
	}()
}
func (e *Engine) verifyRestoration() {
	e.mu.Lock()
	marker := *e.restore
	runtime := e.runtime
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.RollbackTimeout)
	e.mu.Lock()
	e.cancel = cancel
	if e.closing {
		cancel()
	}
	e.mu.Unlock()
	current, err := e.readStore()
	if err == nil {
		err = checkSafely(runtime.Check, ctx, Operation{ID: marker.Epoch, ContextRevision: marker.ContextRevision}, "recovery")
	}
	var verified Verification
	if err == nil {
		verified, err = verifySafely(runtime.Verify, ctx, current)
	}
	if err == nil {
		err = checkSafely(runtime.Check, ctx, Operation{ID: marker.Epoch, ContextRevision: marker.ContextRevision}, "recovery")
	}
	if err == nil {
		after, readErr := e.readStore()
		if readErr != nil || Digest(current) != Digest(after) {
			err = ErrConflict
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	contextErr := ctx.Err()
	cancel()
	e.cancel = nil
	e.running = false
	if err == nil && contextErr != nil {
		err = ErrRecovery
	}
	if err == nil && verified.RuntimeLoaded && verified.ResourcesReady && !e.closing {
		if marker.State == "confirmed" {
			if marker.StoreDigest != Digest(current) {
				err = ErrConflict
			} else {
				marker.Activated = true
			}
		} else {
			// Acknowledgement is a CAS over this exact verified baseline. Startup
			// re-verification may not silently refresh an acknowledged Store digest.
			if marker.State == "acknowledged" && marker.StoreDigest != Digest(current) {
				err = ErrConflict
			} else if marker.State != "acknowledged" {
				marker.State = "verified"
			}
		}
		if err == nil {
			marker.StoreDigest = Digest(current)
			marker.RuntimeLoaded = true
			marker.ResourcesReady = true
			marker.VerifiedAtMS = time.Now().UnixMilli()
			if writeRestoreMarker(e.root, &marker) == nil {
				e.restore = &marker
				e.restoreVerified = true
			}
		}
	}
	if e.closing {
		e.release()
	}
}
func (e *Engine) AcknowledgeRestore(ctx context.Context, epoch, manifest, contextRevision, storeDigest, ackID string) (RestoreStatus, error) {
	if ctx.Err() != nil {
		return RestoreStatus{}, ctx.Err()
	}
	if !serviceIdentity.MatchString(ackID) {
		return RestoreStatus{}, ErrInvalid
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return RestoreStatus{}, ErrClosed
	}
	if e.restore == nil {
		return RestoreStatus{}, ErrNotFound
	}
	m := *e.restore
	if e.restoreBlocked() && !e.restoreVerified {
		return e.restoreStatusLocked(), ErrBusy
	}
	if m.Epoch != epoch || m.ManifestDigest != manifest || m.ContextRevision != contextRevision || m.StoreDigest != storeDigest {
		return m.RestoreStatus, ErrConflict
	}
	if m.State == "acknowledged" || m.State == "confirmed" {
		if m.AcknowledgementID != ackID {
			return m.RestoreStatus, ErrConflict
		}
		return m.RestoreStatus, nil
	}
	if m.State != "verified" || e.running || e.active != "" || !m.RuntimeLoaded || !m.ResourcesReady {
		return m.RestoreStatus, ErrBusy
	}
	if e.opts.RestoreCheck == nil || checkSafely(func(ctx context.Context, _ Operation, _ string) error {
		return e.opts.RestoreCheck(ctx, m.ContextRevision)
	}, ctx, Operation{}, "restore") != nil {
		return m.RestoreStatus, ErrConflict
	}
	current, err := e.readStore()
	if err != nil || Digest(current) != m.StoreDigest {
		return m.RestoreStatus, ErrConflict
	}
	m.State, m.AcknowledgementID = "acknowledged", ackID
	if err = writeRestoreMarker(e.root, &m); err != nil {
		return e.restore.RestoreStatus, err
	}
	e.restore = &m
	return m.RestoreStatus, nil
}

func checkpointEntryLimit(path string) int64 {
	if path == "managed/identity.json" {
		return 1024
	}
	if strings.HasPrefix(path, "managed/operations/") && strings.HasSuffix(path, ".json") {
		return journalLimit
	}
	if strings.HasPrefix(path, "managed/secrets/") {
		return MaxSecretBytes
	}
	if path == "managed/restore.json" {
		return restoreMarkerLimit
	}
	return MaxCheckpointFileBytes
}
func validCheckpointPath(path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[0] == "context" {
		return checkpointContextName(parts[1])
	}
	if len(parts) == 2 && parts[0] == "managed" {
		return parts[1] == "store.json" || parts[1] == "identity.json" || parts[1] == "restore.json"
	}
	if len(parts) != 3 || parts[0] != "managed" {
		return false
	}
	if parts[1] == "secrets" {
		return strings.HasSuffix(parts[2], ".secret") && serviceIdentity.MatchString(strings.TrimSuffix(parts[2], ".secret"))
	}
	if parts[1] == "operations" && len(parts[2]) > 64 && digestString(parts[2][:64]) {
		suffix := parts[2][64:]
		return suffix == ".json" || suffix == ".old" || suffix == ".new"
	}
	return false
}
func readCheckpoint(root *privateDir, expected string) (CheckpointManifest, []checkpointPayload, error) {
	var manifest CheckpointManifest
	file, _, err := root.readMetadata("CHECKPOINT.json", MaxCheckpointManifestBytes)
	if err != nil || !file.Exists || !digestString(expected) || checkpointHash(file.Bytes) != expected || decodeCheckpointJSON(file.Bytes, &manifest) != nil {
		return manifest, nil, ErrRecovery
	}
	if manifest.Kind != "frp-managed-checkpoint" || manifest.Version != 1 || !serviceIdentity.MatchString(manifest.ID) || manifest.CreatedAtMS <= 0 || !filepath.IsAbs(manifest.Root) || filepath.Clean(manifest.Root) != manifest.Root || filepath.Base(manifest.Root) != "managed" || manifest.StoreName != "store.json" || manifest.ConfigFile != filepath.Join(filepath.Dir(manifest.Root), "agent.toml") || manifest.WorkingDir != filepath.Dir(manifest.Root) || !digestString(manifest.ContextRevision) || !serviceIdentity.MatchString(manifest.ServiceID) || !digestString(manifest.StoreDigest) || len(manifest.Files) < 4 || len(manifest.Files) > MaxCheckpointFiles {
		return manifest, nil, ErrRecovery
	}
	allowed := map[string]bool{"CHECKPOINT.json": true}
	payload := []checkpointPayload{}
	var total int64
	for _, entry := range manifest.Files {
		if !validCheckpointPath(entry.Path) || allowed[entry.Path] || entry.Size < 0 || entry.Size > checkpointEntryLimit(entry.Path) || !digestString(entry.SHA256) || entry.ModifiedNS <= 0 {
			return manifest, nil, ErrRecovery
		}
		allowed[entry.Path] = true
		total += entry.Size
		if total > MaxCheckpointBytes {
			return manifest, nil, ErrRecovery
		}
		dir := root
		var opened []*privateDir
		parts := strings.Split(entry.Path, "/")
		for _, name := range parts[:len(parts)-1] {
			next, e := dir.existingSubdir(name)
			if e != nil {
				for _, d := range opened {
					d.close()
				}
				return manifest, nil, e
			}
			opened = append(opened, next)
			dir = next
		}
		data, _, e := dir.readMetadata(parts[len(parts)-1], checkpointEntryLimit(entry.Path))
		for _, d := range opened {
			d.close()
		}
		if e != nil || !data.Exists || int64(len(data.Bytes)) != entry.Size || checkpointHash(data.Bytes) != entry.SHA256 {
			return manifest, nil, ErrRecovery
		}
		payload = append(payload, checkpointPayload{entry, data.Bytes})
	}
	if !allowed["context/agent.toml"] || !allowed["context/installation.json"] || !allowed["context/agent.token"] || !allowed["managed/identity.json"] || allowed["managed/store.json"] != manifest.StoreExists {
		return manifest, nil, ErrRecovery
	}
	if err = checkpointTreeExact(root, "", allowed); err != nil {
		return manifest, nil, err
	}
	// Validate identity, complete triples and Store digest through the same
	// read-only validator used at export, using its payload directory as Root.
	dataByPath := map[string][]byte{}
	for _, file := range payload {
		dataByPath[file.entry.Path] = file.data
	}
	var identity struct {
		Version   int    `json:"version"`
		ServiceID string `json:"service_id"`
		StoreName string `json:"store_name"`
	}
	if decodeCheckpointJSON(dataByPath["managed/identity.json"], &identity) != nil || identity.Version != 1 || identity.ServiceID != manifest.ServiceID || identity.StoreName != manifest.StoreName || Digest(StoreSnapshot{Exists: manifest.StoreExists, Bytes: dataByPath["managed/store.json"]}) != manifest.StoreDigest {
		return manifest, nil, ErrRecovery
	}
	if old, ok := dataByPath["managed/restore.json"]; ok {
		var marker restoreMarker
		if decodeCheckpointJSON(old, &marker) != nil || !marker.valid(manifest.Root) || marker.State != "confirmed" || !marker.Activated || marker.ServiceID != manifest.ServiceID {
			return manifest, nil, ErrRecovery
		}
	}
	records := map[string]record{}
	keys := map[string]bool{}
	active := 0
	for _, file := range payload {
		if !strings.HasPrefix(file.entry.Path, "managed/operations/") || !strings.HasSuffix(file.entry.Path, ".json") {
			continue
		}
		var r record
		if decodeCheckpointJSON(file.data, &r) != nil || r.Version != 1 || !safeID.MatchString(r.ID) || file.entry.Path != "managed/operations/"+idHash(r.ID)+".json" || !knownState(r.State) || !digestString(r.BaseRevision) || !digestString(r.ContextRevision) || !digestString(r.RequestDigest) || !digestString(r.KeyDigest) || keys[r.KeyDigest] || !digestString(r.Fingerprint) || r.CreatedAt.IsZero() || r.Deadline.IsZero() {
			return manifest, nil, ErrRecovery
		}
		keys[r.KeyDigest] = true
		records[idHash(r.ID)] = r
		if !terminal(r.State) {
			active++
			if active > 1 || r.ContextRevision != manifest.ContextRevision || (manifest.StoreDigest != r.OldDigest && manifest.StoreDigest != r.NewDigest) {
				return manifest, nil, ErrRecovery
			}
		}
		prefix := "managed/operations/" + idHash(r.ID)
		old, ok := dataByPath[prefix+".old"]
		if !ok || (!r.OldExists && len(old) != 0) || Digest(StoreSnapshot{Exists: r.OldExists, Bytes: old}) != r.OldDigest {
			return manifest, nil, ErrRecovery
		}
		next, ok := dataByPath[prefix+".new"]
		if !ok || Digest(StoreSnapshot{Exists: true, Bytes: next}) != r.NewDigest {
			return manifest, nil, ErrRecovery
		}
	}
	if len(records) > 1024 {
		return manifest, nil, ErrRecovery
	}
	for _, file := range payload {
		if strings.HasPrefix(file.entry.Path, "managed/operations/") {
			name := filepath.Base(file.entry.Path)
			if _, ok := records[name[:64]]; !ok {
				return manifest, nil, ErrRecovery
			}
		}
		if strings.HasPrefix(file.entry.Path, "managed/secrets/") && !validSecretValue(string(file.data)) {
			return manifest, nil, ErrRecovery
		}
	}
	sort.Slice(payload, func(i, j int) bool { return payload[i].entry.Path < payload[j].entry.Path })
	return manifest, payload, nil
}
func checkpointTreeExact(root *privateDir, prefix string, allowed map[string]bool) error {
	names, err := root.names()
	if err != nil {
		return err
	}
	for _, name := range names {
		path := prefix + name
		if allowed[path] {
			continue
		}
		isDir := false
		for known := range allowed {
			if strings.HasPrefix(known, path+"/") {
				isDir = true
				break
			}
		}
		if !isDir {
			return ErrRecovery
		}
		child, err := root.existingSubdir(name)
		if err != nil {
			return err
		}
		err = checkpointTreeExact(child, path+"/", allowed)
		child.close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ValidateCheckpoint verifies the full private payload without acquiring or
// mutating a runtime root. Packaging code can use this before installation.
func ValidateCheckpoint(ctx context.Context, checkpoint, manifestDigest string) (CheckpointManifest, error) {
	root, err := openExistingPrivateDir(checkpoint)
	if err != nil {
		return CheckpointManifest{}, err
	}
	defer root.close()
	if ctx.Err() != nil {
		return CheckpointManifest{}, ctx.Err()
	}
	manifest, _, err := readCheckpoint(root, manifestDigest)
	return manifest, err
}

// InstallCheckpoint is an offline, same-path replacement. The target root and
// .lock inode stay in place. Every failure after installing the marker remains
// closed until this exact checkpoint is successfully reinstalled.
func InstallCheckpoint(ctx context.Context, checkpoint, rootPath, manifestDigest string, capture ContextCapture, validate ContextValidator) (RestoreSummary, error) {
	var summary RestoreSummary
	source, err := openExistingPrivateDir(checkpoint)
	if err != nil {
		return summary, err
	}
	defer source.close()
	manifest, payload, err := readCheckpoint(source, manifestDigest)
	if err != nil {
		return summary, err
	}
	if rootPath != manifest.Root || filepath.Dir(checkpoint) == rootPath || strings.HasPrefix(checkpoint, rootPath+string(filepath.Separator)) || capture == nil || validate == nil {
		return summary, ErrUnsafePath
	}
	stagedContext := []ContextFile{}
	for _, file := range payload {
		path := filepath.Join(filepath.Dir(manifest.Root), filepath.FromSlash(file.entry.Path))
		if strings.HasPrefix(file.entry.Path, "context/") {
			path = filepath.Join(filepath.Dir(manifest.Root), filepath.Base(file.entry.Path))
		}
		stagedContext = append(stagedContext, ContextFile{Path: path, Bytes: append([]byte(nil), file.data...), ModifiedNS: file.entry.ModifiedNS})
	}
	if err = validate(ctx, manifest, stagedContext); err != nil {
		return summary, ErrRecovery
	}
	root, err := openExistingPrivateDir(rootPath)
	if err != nil {
		return summary, err
	}
	defer root.close()
	lock, err := root.lock()
	if err != nil {
		return summary, err
	}
	defer lock.Close()
	installation, err := openExistingPrivateDir(filepath.Dir(rootPath))
	if err != nil {
		return summary, err
	}
	defer installation.close()
	names, err := root.names()
	if err != nil {
		return summary, err
	}
	for _, name := range names {
		switch name {
		case ".lock", "store.json", "identity.json", "operations", "secrets", "restore.json":
		default:
			return summary, ErrRecovery
		}
	}
	prior, err := readRestoreMarker(root)
	if err != nil {
		return summary, err
	}
	var marker restoreMarker
	if prior != nil && prior.State == "pending" && prior.ManifestDigest == manifestDigest && prior.ContextRevision == manifest.ContextRevision {
		// Lost install replies are retryable before first runtime verification.
		// Never reinstall a pending Store: startup may already have recovered an
		// unfinished transaction, and the exact restore epoch must survive.
		actual, _, captureErr := captureCheckpointContext(ctx, rootPath, capture)
		identity, _, identityErr := root.readMetadata("identity.json", 1024)
		var id struct {
			Version   int    `json:"version"`
			ServiceID string `json:"service_id"`
			StoreName string `json:"store_name"`
		}
		if captureErr != nil || actual.Revision != prior.ContextRevision || identityErr != nil || decodeCheckpointJSON(identity.Bytes, &id) != nil || id.Version != 1 || id.ServiceID != prior.ServiceID || id.StoreName != "store.json" {
			return summary, ErrConflict
		}
		return RestoreSummary{Code: "ok", Epoch: prior.Epoch, ServiceID: prior.ServiceID, ManifestDigest: manifestDigest, State: "pending"}, nil
	}
	if prior != nil && !prior.Activated {
		if prior.State != "installing" || prior.ManifestDigest != manifestDigest || prior.ContextRevision != manifest.ContextRevision {
			return summary, ErrConflict
		}
		marker = *prior
	} else {
		identity, _, err := root.readMetadata("identity.json", 1024)
		if err != nil {
			return summary, err
		}
		replaced := ""
		if identity.Exists {
			var id struct {
				Version   int    `json:"version"`
				ServiceID string `json:"service_id"`
				StoreName string `json:"store_name"`
			}
			if decodeCheckpointJSON(identity.Bytes, &id) != nil || id.Version != 1 || id.StoreName != manifest.StoreName || !serviceIdentity.MatchString(id.ServiceID) {
				return summary, ErrRecovery
			}
			replaced = id.ServiceID
		}
		epoch, err := newPrivateID()
		if err != nil {
			return summary, err
		}
		service, err := newPrivateID()
		if err != nil {
			return summary, err
		}
		operations := 0
		for _, file := range payload {
			if strings.HasPrefix(file.entry.Path, "managed/operations/") && strings.HasSuffix(file.entry.Path, ".json") {
				operations++
			}
		}
		marker = restoreMarker{Version: 1, RestoreStatus: RestoreStatus{State: "installing", Epoch: epoch, BackupServiceID: manifest.ServiceID, ReplacedServiceID: replaced, ManifestDigest: manifestDigest, ContextRevision: manifest.ContextRevision, OperationsCount: operations}, ServiceID: service, ConfigFile: manifest.ConfigFile, WorkingDir: manifest.WorkingDir, CreatedAtMS: time.Now().UnixMilli()}
	}
	if err = writeRestoreMarker(root, &marker); err != nil {
		return summary, err
	}
	if ctx.Err() != nil {
		return summary, ctx.Err()
	}
	operations, err := root.subdir("operations")
	if err != nil {
		return summary, err
	}
	defer operations.close()
	secrets, err := root.subdir("secrets")
	if err != nil {
		return summary, err
	}
	defer secrets.close()
	// Validate every existing name before deleting stale checkpoint members. No
	// recursive filesystem walk can erase user material or follow a link.
	desired := map[string]bool{}
	for _, file := range payload {
		desired[file.entry.Path] = true
	}
	for _, item := range []struct {
		dir    *privateDir
		prefix string
	}{{operations, "managed/operations/"}, {secrets, "managed/secrets/"}} {
		names, err := item.dir.names()
		if err != nil {
			return summary, err
		}
		for _, name := range names {
			path := item.prefix + name
			if !validCheckpointPath(path) {
				return summary, ErrRecovery
			}
			current, _, err := item.dir.readMetadata(name, checkpointEntryLimit(path))
			if err != nil || !current.Exists {
				return summary, ErrRecovery
			}
			if !desired[path] {
				if err = item.dir.replace(name, StoreSnapshot{}, Digest(current), checkpointEntryLimit(path), nil); err != nil {
					return summary, err
				}
			}
		}
	}
	for _, name := range []string{"installation.json", "agent.toml", "agent.token", "frp.token", "ca.crt", "tls.crt", "tls.key", "local.crt", "local.key"} {
		if desired["context/"+name] {
			continue
		}
		old, _, err := installation.readMetadata(name, MaxCheckpointFileBytes)
		if err != nil {
			return summary, err
		}
		if old.Exists {
			if err = installation.replace(name, StoreSnapshot{}, Digest(old), MaxCheckpointFileBytes, nil); err != nil {
				return summary, err
			}
		}
	}
	for _, file := range payload {
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}
		if file.entry.Path == "managed/identity.json" || file.entry.Path == "managed/restore.json" {
			continue
		}
		dir := root
		name := filepath.Base(file.entry.Path)
		switch {
		case strings.HasPrefix(file.entry.Path, "context/"):
			dir = installation
		case strings.HasPrefix(file.entry.Path, "managed/operations/"):
			dir = operations
		case strings.HasPrefix(file.entry.Path, "managed/secrets/"):
			dir = secrets
		}
		old, _, err := dir.readMetadata(name, checkpointEntryLimit(file.entry.Path))
		if err != nil {
			return summary, err
		}
		if err = dir.replace(name, StoreSnapshot{Exists: true, Bytes: file.data}, Digest(old), checkpointEntryLimit(file.entry.Path), nil); err != nil {
			return summary, err
		}
		if err = dir.restoreTime(name, file.entry.ModifiedNS); err != nil {
			return summary, err
		}
	}
	if !manifest.StoreExists {
		old, _, err := root.readMetadata("store.json", MaxCheckpointFileBytes)
		if err != nil {
			return summary, err
		}
		if err = root.replace("store.json", StoreSnapshot{}, Digest(old), MaxCheckpointFileBytes, nil); err != nil {
			return summary, err
		}
	}
	identityData, _ := json.Marshal(struct {
		Version   int    `json:"version"`
		ServiceID string `json:"service_id"`
		StoreName string `json:"store_name"`
	}{1, marker.ServiceID, "store.json"})
	if err = root.replace("identity.json", StoreSnapshot{Exists: true, Bytes: identityData}, "", 1024, nil); err != nil {
		return summary, err
	}
	// Capture rebuilds the actual native dependency closure, not a list supplied
	// by the archive. Missing or extra context payloads fail before activation.
	actual, contextFiles, err := captureCheckpointContext(ctx, rootPath, capture)
	if err != nil || actual.Revision != manifest.ContextRevision {
		return summary, ErrConflict
	}
	expectedContext := []checkpointPayload{}
	for _, file := range payload {
		if strings.HasPrefix(file.entry.Path, "context/") {
			expectedContext = append(expectedContext, file)
		}
	}
	if !sameCheckpointPayload(expectedContext, contextFiles) {
		return summary, ErrConflict
	}
	// Re-read source at the end to reject a replaced or edited staged checkpoint.
	again, againPayload, err := readCheckpoint(source, manifestDigest)
	if err != nil || again.ID != manifest.ID || !sameCheckpointPayload(payload, againPayload) {
		return summary, ErrConflict
	}
	marker.State = "pending"
	if err = writeRestoreMarker(root, &marker); err != nil {
		return summary, err
	}
	return RestoreSummary{Code: "ok", Epoch: marker.Epoch, ServiceID: marker.ServiceID, ManifestDigest: manifestDigest, State: "pending"}, nil
}

// ConfirmRestoration is the second offline step. A remote acknowledgement and
// actual native verification receipt are mandatory; this command never assumes
// that successfully copying files proved the running service healthy.
func ConfirmRestoration(ctx context.Context, options Options, epoch, manifestDigest string, capture ContextCapture) (RestoreSummary, error) {
	var summary RestoreSummary
	lease, err := OpenOfflineLease(options)
	if err != nil {
		return summary, err
	}
	defer lease.Close()
	marker, err := readRestoreMarker(lease.root)
	if err != nil || marker == nil {
		return summary, ErrRecovery
	}
	if marker.Epoch != epoch || marker.ManifestDigest != manifestDigest || marker.State != "acknowledged" || marker.Activated || !marker.RuntimeLoaded || !marker.ResourcesReady {
		return summary, ErrConflict
	}
	current, err := lease.root.read("store.json", MaxCheckpointFileBytes)
	if err != nil || Digest(current) != marker.StoreDigest {
		return summary, ErrConflict
	}
	snapshot, _, err := captureCheckpointContext(ctx, options.Root, capture)
	if err != nil || snapshot.Revision != marker.ContextRevision {
		return summary, ErrConflict
	}
	engine := &Engine{opts: lease.options, root: lease.root, operations: lease.operations, storeName: "store.json", records: map[string]*record{}, keys: map[string]string{}}
	if engine.load() != nil || engine.active != "" {
		return summary, ErrRecovery
	}
	identity, _, err := lease.root.readMetadata("identity.json", 1024)
	var id struct {
		Version   int    `json:"version"`
		ServiceID string `json:"service_id"`
		StoreName string `json:"store_name"`
	}
	if err != nil || decodeCheckpointJSON(identity.Bytes, &id) != nil || id.ServiceID != marker.ServiceID || id.StoreName != "store.json" {
		return summary, ErrConflict
	}
	if ctx.Err() != nil {
		return summary, ctx.Err()
	}
	marker.State = "confirmed"
	if err = writeRestoreMarker(lease.root, marker); err != nil {
		return summary, err
	}
	return RestoreSummary{Code: "ok", Epoch: marker.Epoch, ServiceID: marker.ServiceID, ManifestDigest: marker.ManifestDigest, State: "confirmed"}, nil
}
