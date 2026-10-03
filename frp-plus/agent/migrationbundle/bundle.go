// Package migrationbundle persists private, bounded offline migration material.
// It never writes the source paths named in its manifest and has no restore API.
package migrationbundle

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fatedier/frp/extension/frpmonitor/agent/configuration"
)

var ErrBundle = errors.New("migration_bundle_unavailable")

const manifestName = "manifest.private.json"
const candidateName = "store.candidate.json"
const instructionsName = "MAINTENANCE.txt"
const instructions = `This directory contains PRIVATE native configuration and recovery material.
No source, native Store, service identity, or running process was changed by plan.
The manifest remove list identifies source file indexes, object kinds/names and
zero-based ordinals in each source's proxies-then-visitors list. Remove only those
objects manually; retain source files and include patterns, even when empty.

Maintenance sequence:
1. Stop the old frpc and ensure any earlier managed transactions were resolved.
2. Review manifest.private.json and preserve this entire private bundle.
3. Remove precisely the listed objects from their original file/include sources.
4. Install store.candidate.json at the manifest target_store with mode 0600 in
   target_root (0700). Set store.path to that absolute path, enable the local
   telemetry.configManagement flag and set its root. Keep other common settings.
5. Run config-migrate check --offline with the same config, working directory,
   unsafe-feature settings and this bundle. disk_verified only validates disk;
   --offline acknowledges your stop procedure, it does not detect live processes.
6. Start frpc and verify actual Store ownership, resource readiness and business
   forwarding separately. A successful plan/check never claims runtime health.

If maintenance fails, keep frpc stopped. Restore original source bytes from
source-NNNN.original using the private manifest path mapping and original modes;
restore the original Store existence state and bytes. An absent original means
absence, not an empty Store. No automatic arbitrary-path restore is provided.
Do not overwrite existing service identity, secrets or transaction journals.
Never publish this bundle or its contents. stdout contains no private paths.
`

func sourceName(index int) string { return fmt.Sprintf("source-%04d.original", index) }

// Write creates a new private bundle exclusively. A partial failure leaves a
// private incomplete directory; Read rejects it. Existing bundles are not replaced.
func Write(path string, plan *configuration.MigrationPlan) error {
	if configuration.ValidateMigrationPlan(plan) != nil {
		return ErrBundle
	}
	manifest, err := json.MarshalIndent(plan, "", "  ")
	if err != nil || len(manifest) > configuration.MaxMigrationStoreBytes {
		return ErrBundle
	}
	dir, err := createDirectory(path)
	if err != nil {
		return ErrBundle
	}
	defer dir.close()
	if dir.write(candidateName, plan.Candidate) != nil || dir.write(instructionsName, []byte(instructions)) != nil {
		return ErrBundle
	}
	for i, source := range plan.Sources {
		if dir.write(sourceName(i), source.Data) != nil {
			return ErrBundle
		}
	}
	// The manifest is the final completion marker, after all material was synced.
	if dir.sync() != nil || dir.write(manifestName, append(manifest, '\n')) != nil || dir.sync() != nil {
		return ErrBundle
	}
	return nil
}

func Read(path string) (*configuration.MigrationPlan, error) {
	dir, err := openDirectory(path)
	if err != nil {
		return nil, ErrBundle
	}
	defer dir.close()
	data, err := dir.read(manifestName, configuration.MaxMigrationStoreBytes)
	if err != nil {
		return nil, ErrBundle
	}
	plan, err := configuration.DecodeMigrationManifest(data)
	if err != nil || len(plan.Sources) == 0 || len(plan.Sources) > configuration.MaxDependencies {
		return nil, ErrBundle
	}
	plan.Candidate, err = dir.read(candidateName, configuration.MaxMigrationStoreBytes)
	if err != nil {
		return nil, ErrBundle
	}
	allowed := map[string]bool{manifestName: true, candidateName: true, instructionsName: true}
	total := 0
	for i := range plan.Sources {
		name := sourceName(i)
		allowed[name] = true
		plan.Sources[i].Data, err = dir.read(name, configuration.MaxInputBytes-total)
		if err != nil {
			return nil, ErrBundle
		}
		total += len(plan.Sources[i].Data)
	}
	data, err = dir.read(instructionsName, len(instructions))
	if err != nil || string(data) != instructions || dir.only(allowed) != nil || configuration.ValidateMigrationPlan(plan) != nil || dir.check() != nil {
		return nil, ErrBundle
	}
	return plan, nil
}
