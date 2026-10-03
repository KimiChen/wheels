package managed

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
)

const contextHistoryName = "context-history.json"
const contextHistoryLimit int64 = 4 << 20
const MaterialsContextChanged = "context_changed"

var ErrContextChanged = errors.New("managed original operation context changed")

type contextHistoryEntry struct {
	ID              string `json:"id"`
	Fingerprint     string `json:"fingerprint"`
	ContextRevision string `json:"context_revision"`
	OldDigest       string `json:"old_digest"`
	NewDigest       string `json:"new_digest"`
}
type contextHistoryOrigin struct {
	Manifest []byte `json:"manifest"`
	Digest   string `json:"manifest_digest"`
	Restore  []byte `json:"restore,omitempty"`
	Plan     []byte `json:"plan,omitempty"`
}
type contextHistory struct {
	Version int                    `json:"version"`
	Entries []contextHistoryEntry  `json:"entries"`
	Origins []contextHistoryOrigin `json:"origins,omitempty"`
}

func historyEntry(r record) contextHistoryEntry {
	return contextHistoryEntry{r.ID, r.Fingerprint, r.ContextRevision, r.OldDigest, r.NewDigest}
}
func decodeContextHistory(data []byte, records map[string]*record) (map[string]bool, error) {
	out := map[string]bool{}
	if data == nil {
		return out, nil
	}
	var h contextHistory
	if len(data) > int(contextHistoryLimit) || decodeCheckpointJSON(data, &h) != nil || h.Version != 1 || h.Entries == nil || len(h.Entries) > MaxManagedOperations || len(h.Origins) > 128 {
		return nil, ErrRecovery
	}
	for _, origin := range h.Origins {
		var m CheckpointManifestV2
		if len(origin.Manifest) == 0 || len(origin.Manifest) > int(MaxCheckpointManifestBytes) || !digestString(origin.Digest) || checkpointHash(origin.Manifest) != origin.Digest || decodeCheckpointJSON(origin.Manifest, &m) != nil || m.Kind != "frp-managed-checkpoint" || m.Version != 3 || !canonicalAbsolute(m.Root) || filepath.Base(m.Root) != "managed" || !serviceIdentity.MatchString(m.ServiceID) || !digestString(m.ContextRevision) {
			return nil, ErrRecovery
		}
		if origin.Restore != nil {
			var marker restoreMarker
			if decodeCheckpointJSON(origin.Restore, &marker) != nil || !marker.valid(m.Root) || marker.ServiceID != m.ServiceID || !marker.Activated {
				return nil, ErrRecovery
			}
			if (marker.CheckpointVersion == 2 || marker.CheckpointVersion == 3) && origin.Plan == nil {
				return nil, ErrRecovery
			}
			if origin.Plan != nil {
				var plan restoreInstallPlan
				if decodeCheckpointJSON(origin.Plan, &plan) != nil || checkpointHash(origin.Plan) != marker.PlanDigest || !plan.valid() || !planMatchesMarker(&plan, &marker) {
					return nil, ErrRecovery
				}
			}
		} else if origin.Plan != nil {
			return nil, ErrRecovery
		}
	}
	for _, entry := range h.Entries {
		r := records[entry.ID]
		if r == nil || !terminal(r.State) || r.NeedsRuntime || out[entry.ID] || entry != historyEntry(*r) {
			return nil, ErrRecovery
		}
		out[entry.ID] = true
	}
	return out, nil
}
func (e *Engine) loadContextHistory() error {
	file, err := e.root.read(contextHistoryName, contextHistoryLimit)
	if err != nil {
		return err
	}
	if file.Exists && len(file.Bytes) == 0 {
		return ErrRecovery
	}
	plan, err := readRestorePlan(e.root, "")
	if err != nil {
		return err
	}
	if plan != nil {
		for _, entry := range plan.Entries {
			if entry.Target == "managed/"+contextHistoryName && Digest(file) != entry.NewDigest {
				return ErrRecovery
			}
		}
	}
	ids, err := decodeContextHistory(file.Bytes, e.records)
	if err != nil {
		return err
	}
	for id := range ids {
		e.records[id].contextChanged = true
	}
	return nil
}
func contextHistoryPayload(payload []checkpointPayload, relocated bool) ([]byte, error) {
	records := map[string]*record{}
	var original []byte
	for _, file := range payload {
		if file.entry.Path == "managed/"+contextHistoryName {
			if len(file.data) == 0 {
				return nil, ErrRecovery
			}
			original = file.data
		}
		if filepath.Dir(file.entry.Path) == "managed/operations" && filepath.Ext(file.entry.Path) == ".json" {
			r, _, err := decodeManagedRecord(file.data, filepath.Base(file.entry.Path))
			if err != nil {
				return nil, err
			}
			records[r.ID] = &r
		}
	}
	for _, file := range payload {
		if file.entry.Path == "managed/"+restorePlanName {
			var plan restoreInstallPlan
			if decodeCheckpointJSON(file.data, &plan) != nil {
				return nil, ErrRecovery
			}
			for _, entry := range plan.Entries {
				if entry.Target == "managed/"+contextHistoryName && Digest(StoreSnapshot{Exists: original != nil, Bytes: original}) != entry.NewDigest {
					return nil, ErrRecovery
				}
			}
		}
	}
	ids, err := decodeContextHistory(original, records)
	if err != nil {
		return nil, err
	}
	if !relocated {
		return original, nil
	}
	for id, r := range records {
		if !terminal(r.State) || r.NeedsRuntime {
			return nil, ErrBusy
		}
		ids[id] = true
	}
	h := contextHistory{Version: 1, Entries: []contextHistoryEntry{}}
	if original != nil {
		var prior contextHistory
		if decodeCheckpointJSON(original, &prior) != nil {
			return nil, ErrRecovery
		}
		h.Origins = prior.Origins
	}
	for id := range ids {
		h.Entries = append(h.Entries, historyEntry(*records[id]))
	}
	sort.Slice(h.Entries, func(i, j int) bool { return h.Entries[i].ID < h.Entries[j].ID })
	data, err := json.Marshal(h)
	if err != nil || len(data) > int(contextHistoryLimit) {
		return nil, ErrRecovery
	}
	return data, nil
}

func preserveRelocationOrigin(history, manifest []byte, payload []checkpointPayload) ([]byte, error) {
	if history == nil {
		return nil, nil
	}
	var h contextHistory
	if decodeCheckpointJSON(history, &h) != nil {
		return nil, ErrRecovery
	}
	digest := checkpointHash(manifest)
	for _, origin := range h.Origins {
		if checkpointHash(origin.Manifest) == digest {
			return history, nil
		}
	}
	origin := contextHistoryOrigin{Manifest: append([]byte(nil), manifest...), Digest: digest}
	for _, file := range payload {
		if file.entry.Path == "managed/restore.json" {
			origin.Restore = append([]byte(nil), file.data...)
		}
		if file.entry.Path == "managed/"+restorePlanName {
			origin.Plan = append([]byte(nil), file.data...)
		}
	}
	h.Origins = append(h.Origins, origin)
	data, err := json.Marshal(h)
	if err != nil || len(data) > int(contextHistoryLimit) {
		return nil, ErrRecovery
	}
	return data, nil
}

func hasContextHistory(payload []checkpointPayload) bool {
	for _, file := range payload {
		if file.entry.Path == "managed/"+contextHistoryName {
			return true
		}
	}
	return false
}
