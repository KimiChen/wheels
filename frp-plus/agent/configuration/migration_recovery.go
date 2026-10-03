package configuration

import (
	"encoding/json"
	"path/filepath"
	"sort"
)

// Reconstruct the complete required backup path set from the original native
// main common configuration and recorded original include expansion. This is
// an integrity check, not a signature against an owner rewriting all material.
func validateMigrationClosure(plan *MigrationPlan) error {
	invalid := failure("migration_bundle_invalid")
	mainSource := plan.Sources[plan.Files[0].Source]
	main, err := decodeMigrationFile(mainSource.Path, "file", mainSource.Data)
	if err != nil {
		return invalid
	}
	common := main.common
	if common.Telemetry.ConfigManagement != nil && common.Telemetry.ConfigManagement.Enabled || !common.Telemetry.Enabled {
		return invalid
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Join(plan.WorkingDir, path)
	}
	required := map[string]string{plan.ConfigFile: "file"}
	if len(plan.Includes) != len(common.IncludeConfigFiles) {
		return invalid
	}
	expectedFiles := []string{plan.ConfigFile}
	for i, include := range plan.Includes {
		if include.Pattern != resolve(common.IncludeConfigFiles[i]) || !sort.StringsAreSorted(include.Files) || len(include.Files) > MaxDependencies {
			return invalid
		}
		for _, path := range include.Files {
			match, err := filepath.Match(filepath.Base(include.Pattern), filepath.Base(path))
			if err != nil || !match || filepath.Dir(path) != filepath.Dir(include.Pattern) || required[path] != "" {
				return invalid
			}
			required[path] = "include"
			expectedFiles = append(expectedFiles, path)
		}
	}
	if len(expectedFiles) != len(plan.Files) {
		return invalid
	}
	for i, file := range plan.Files {
		if plan.Sources[file.Source].Path != expectedFiles[i] {
			return invalid
		}
	}
	store := plan.TargetStore
	if common.Store.Path != "" {
		store = common.Store.Path
		if !filepath.IsAbs(store) {
			store = filepath.Join(filepath.Dir(plan.ConfigFile), store)
		}
	}
	if required[store] != "" {
		return invalid
	}
	required[store] = "store"
	data, _ := json.Marshal(common)
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return invalid
	}
	for _, path := range []string{"transport.tls.certFile", "transport.tls.keyFile", "transport.tls.trustedCaFile", "webServer.tls.certFile", "webServer.tls.keyFile", "webServer.tls.trustedCaFile", "auth.oidc.trustedCaFile", "auth.tokenSource.file.path", "auth.oidc.tokenSource.file.path", "telemetry.tokenFile", "telemetry.caFile"} {
		if value, ok := getPath(raw, path); ok {
			var filename string
			if json.Unmarshal(value, &filename) != nil {
				return invalid
			}
			if filename == "" {
				continue
			}
			filename = resolve(filename)
			if prior := required[filename]; prior != "" && prior != "local_file" {
				return invalid
			}
			required[filename] = "local_file"
		}
	}
	if len(required) != len(plan.Sources) {
		return invalid
	}
	for _, source := range plan.Sources {
		if required[source.Path] != source.Kind || source.Kind != "store" && !source.Exists || source.Kind == "store" && common.Store.Path == "" && source.Exists {
			return invalid
		}
	}
	return nil
}
