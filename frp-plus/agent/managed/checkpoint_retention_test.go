//go:build linux || darwin

package managed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckpointExpiredJournalOmitsResiduesAndCannotResurrect(t *testing.T) {
	opts, capture, _, out := restoreFixtureV2(t)
	e, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	op := prepare(t, e, request("expired-checkpoint"))
	crashEngine(e)
	prefix := filepath.Join(opts.Root, "operations", idHash(op.ID))
	data, err := os.ReadFile(prefix + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if json.Unmarshal(data, &r) != nil {
		t.Fatal("record")
	}
	now := time.Now().UTC()
	r.Version = 2
	r.State = Cancelled
	r.NeedsRuntime = false
	r.TerminalAt = &now
	r.MaterialsState = MaterialsExpired
	r.MaterialsExpiredAt = &now
	r.MaterialsExpiryReason = "ttl"
	data, _ = json.Marshal(r)
	if err = os.WriteFile(prefix+".json", data, 0600); err != nil {
		t.Fatal(err)
	}
	summary, err := ExportCheckpointV2(context.Background(), opts, capture, out)
	if err != nil {
		t.Fatal(err)
	}
	root, err := openExistingPrivateDir(out)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	m, payload, err := readCheckpointV2(root, summary.ManifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	journalFound := false
	for _, file := range payload {
		if strings.HasPrefix(file.entry.Path, "managed/operations/") {
			if !strings.HasSuffix(file.entry.Path, ".json") {
				t.Fatal("expired private material archived")
			}
			journalFound = true
		}
	}
	if !journalFound {
		t.Fatal("idempotency tombstone omitted")
	}
	variants, err := variantsFromPayload(payload)
	if err != nil || len(variants) != 1 {
		t.Fatal("expired variants restored", err)
	}
	// Offline capture does not perform GC, recovery, or deletion on the live root.
	if _, err = os.Stat(prefix + ".old"); err != nil {
		t.Fatal("backup deleted residue")
	}
	if _, err = os.Stat(prefix + ".new"); err != nil {
		t.Fatal("backup deleted residue")
	}
	residual, err := os.ReadFile(prefix + ".new")
	if err != nil {
		t.Fatal(err)
	}
	injected := append(append([]checkpointPayload{}, payload...), checkpointPayload{entry: CheckpointFile{Path: "managed/operations/" + idHash(op.ID) + ".new"}, data: residual})
	if validateManagedCheckpointPayload(m.CheckpointManifest, injected) == nil {
		t.Fatal("sealed expired snapshot accepted")
	}
	if _, err = variantsFromPayload(injected); err == nil {
		t.Fatal("expired candidate became a runtime variant")
	}
	lease, err := OpenOfflineLease(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	variants, _, err = lease.variantsForContext()
	if err != nil || len(variants) != 1 {
		t.Fatal("confirm captured expired snapshots", err)
	}
}
