package managed

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
)

func validateManagedCheckpointPayload(manifest CheckpointManifest, payload []checkpointPayload) error {
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
		return ErrRecovery
	}
	if old, ok := dataByPath["managed/restore.json"]; ok {
		var marker restoreMarker
		if decodeCheckpointJSON(old, &marker) != nil || !marker.valid(manifest.Root) || marker.State != "confirmed" || !marker.Activated || marker.ServiceID != manifest.ServiceID {
			return ErrRecovery
		}
		planBytes, exists := dataByPath["managed/"+restorePlanName]
		if marker.CheckpointVersion == 2 {
			var plan restoreInstallPlan
			if !exists || checkpointHash(planBytes) != marker.PlanDigest || decodeCheckpointJSON(planBytes, &plan) != nil || !plan.valid() || !planMatchesMarker(&plan, &marker) {
				return ErrRecovery
			}
		} else if exists {
			return ErrRecovery
		}
	} else if _, exists := dataByPath["managed/"+restorePlanName]; exists {
		return ErrRecovery
	}
	records := map[string]record{}
	keys := map[string]bool{}
	active := 0
	for _, file := range payload {
		if !strings.HasPrefix(file.entry.Path, "managed/operations/") || !strings.HasSuffix(file.entry.Path, ".json") {
			continue
		}
		r, policy, err := decodeManagedRecord(file.data, filepath.Base(file.entry.Path))
		if err != nil || keys[r.KeyDigest] {
			return ErrRecovery
		}
		keys[r.KeyDigest] = true
		records[idHash(r.ID)] = r
		if !terminal(r.State) {
			active++
			if active > 1 || r.ContextRevision != manifest.ContextRevision || (manifest.StoreDigest != r.OldDigest && manifest.StoreDigest != r.NewDigest) {
				return ErrRecovery
			}
		}
		prefix := "managed/operations/" + idHash(r.ID)
		if policy == snapshotsExpired {
			if _, ok := dataByPath[prefix+".old"]; ok {
				return ErrRecovery
			}
			if _, ok := dataByPath[prefix+".new"]; ok {
				return ErrRecovery
			}
			continue
		}
		old, ok := dataByPath[prefix+".old"]
		if !ok || (!r.OldExists && len(old) != 0) || Digest(StoreSnapshot{Exists: r.OldExists, Bytes: old}) != r.OldDigest {
			return ErrRecovery
		}
		next, ok := dataByPath[prefix+".new"]
		if !ok || Digest(StoreSnapshot{Exists: true, Bytes: next}) != r.NewDigest {
			return ErrRecovery
		}
	}
	if len(records) > 1024 {
		return ErrRecovery
	}
	for _, file := range payload {
		if strings.HasPrefix(file.entry.Path, "managed/operations/") {
			name := filepath.Base(file.entry.Path)
			if _, ok := records[name[:64]]; !ok {
				return ErrRecovery
			}
		}
		if strings.HasPrefix(file.entry.Path, "managed/secrets/") && !validSecretValue(string(file.data)) {
			return ErrRecovery
		}
	}
	return nil
}

func readV2Payload(root *privateDir, path string, limit int64) (StoreSnapshot, error) {
	dir := root
	opened := []*privateDir{}
	defer func() {
		for _, child := range opened {
			child.close()
		}
	}()
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := dir.existingSubdir(part)
		if err != nil {
			return StoreSnapshot{}, err
		}
		opened = append(opened, next)
		dir = next
	}
	file, _, err := dir.readMetadata(parts[len(parts)-1], limit)
	return file, err
}
func readCheckpointV2(root *privateDir, expected string) (CheckpointManifestV2, []checkpointPayload, error) {
	var m CheckpointManifestV2
	file, _, err := root.readMetadata("CHECKPOINT.json", MaxCheckpointManifestBytes)
	if err != nil || !file.Exists || !digestString(expected) || checkpointHash(file.Bytes) != expected || decodeCheckpointJSON(file.Bytes, &m) != nil {
		return m, nil, ErrRecovery
	}
	if m.Version != 2 || m.Kind != "frp-managed-checkpoint" || !serviceIdentity.MatchString(m.ID) || !serviceIdentity.MatchString(m.ServiceID) || m.CreatedAtMS <= 0 || !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root || filepath.Base(m.Root) != "managed" || m.StoreName != "store.json" || m.ConfigFile != filepath.Join(filepath.Dir(m.Root), "agent.toml") || m.WorkingDir != filepath.Dir(m.Root) || !digestString(m.ContextRevision) || !digestString(m.StoreDigest) || !digestString(m.GraphSHA256) || len(m.Files) < 4 || len(m.Files) > MaxCheckpointFiles {
		return m, nil, ErrRecovery
	}
	allowed := map[string]bool{"CHECKPOINT.json": true}
	payload := []checkpointPayload{}
	var total int64
	for _, entry := range m.Files {
		if !validV2Payload(entry.Path) || allowed[entry.Path] || entry.Size < 0 || entry.Size > v2EntryLimit(entry.Path) || !digestString(entry.SHA256) || entry.ModifiedNS <= 0 {
			return m, nil, ErrRecovery
		}
		allowed[entry.Path] = true
		total += entry.Size
		if total > MaxCheckpointBytes {
			return m, nil, ErrRecovery
		}
		file, err := readV2Payload(root, entry.Path, v2EntryLimit(entry.Path))
		if err != nil || !file.Exists || int64(len(file.Bytes)) != entry.Size || checkpointHash(file.Bytes) != entry.SHA256 {
			return m, nil, ErrRecovery
		}
		payload = append(payload, checkpointPayload{entry, file.Bytes})
	}
	if !allowed["GRAPH.json"] || !allowed["context/agent.toml"] || !allowed["context/installation.json"] || !allowed["managed/identity.json"] || allowed["managed/store.json"] != m.StoreExists {
		return m, nil, ErrRecovery
	}
	if err = checkpointTreeExact(root, "", allowed); err != nil {
		return m, nil, err
	}
	if err = validateManagedCheckpointPayload(m.CheckpointManifest, payload); err != nil {
		return m, nil, err
	}
	for _, file := range payload {
		if file.entry.Path == "GRAPH.json" && checkpointHash(file.data) != m.GraphSHA256 {
			return m, nil, ErrRecovery
		}
	}
	sort.Slice(payload, func(i, j int) bool { return payload[i].entry.Path < payload[j].entry.Path })
	return m, payload, nil
}
func v2ContextFiles(m CheckpointManifestV2, payload []checkpointPayload) ([]ContextFile, []byte) {
	files := []ContextFile{}
	var graph []byte
	for _, file := range payload {
		if file.entry.Path == "GRAPH.json" {
			graph = append([]byte(nil), file.data...)
		}
		if strings.HasPrefix(file.entry.Path, "context/") {
			files = append(files, ContextFile{Path: filepath.Join(m.WorkingDir, filepath.FromSlash(strings.TrimPrefix(file.entry.Path, "context/"))), Bytes: append([]byte(nil), file.data...), ModifiedNS: file.entry.ModifiedNS})
		}
	}
	return files, graph
}
func ValidateCheckpointV2(ctx context.Context, checkpoint, digest string, validate ContextValidatorV2) (CheckpointManifestV2, error) {
	var m CheckpointManifestV2
	root, err := openExistingPrivateDir(checkpoint)
	if err != nil {
		return m, err
	}
	defer root.close()
	m, payload, err := readCheckpointV2(root, digest)
	if err != nil {
		return m, err
	}
	if ctx.Err() != nil {
		return m, ctx.Err()
	}
	if validate == nil {
		return m, ErrInvalid
	}
	variants, err := variantsFromPayload(payload)
	if err != nil {
		return m, err
	}
	files, graph := v2ContextFiles(m, payload)
	if err = validate(ctx, m, files, variants, graph); err != nil {
		return m, ErrRecovery
	}
	return m, nil
}

// CheckpointVersion validates the exact caller-bound manifest before dispatch.
// It never treats a future version or malformed envelope as version one.
func CheckpointVersion(checkpoint, digest string) (int, error) {
	root, err := openExistingPrivateDir(checkpoint)
	if err != nil {
		return 0, err
	}
	defer root.close()
	file, _, err := root.readMetadata("CHECKPOINT.json", MaxCheckpointManifestBytes)
	if err != nil || !file.Exists || !digestString(digest) || checkpointHash(file.Bytes) != digest {
		return 0, ErrRecovery
	}
	var m CheckpointManifestV2
	if decodeCheckpointJSON(file.Bytes, &m) != nil || m.Kind != "frp-managed-checkpoint" || m.Version != 1 && m.Version != 2 {
		return 0, ErrRecovery
	}
	if m.Version == 1 && m.GraphSHA256 != "" {
		return 0, ErrRecovery
	}
	return m.Version, nil
}
