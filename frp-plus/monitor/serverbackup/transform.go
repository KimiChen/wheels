package serverbackup

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
)

// Transform is pure: neither the source nor target installation is read. The
// verified sealed snapshot supplies bytes; only caller-local policies authorize
// paths. Database/TSDB content is never examined or rewritten here.
func Transform(ctx context.Context, source Snapshot, target Policy, sourceEnv, targetEnv map[string]string, forbidden []string) (Snapshot, error) {
	target, e := normalize(target)
	if e != nil {
		return Snapshot{}, e
	}
	mapPath, e := mapping(source.Policy, target)
	if e != nil {
		return Snapshot{}, e
	}
	original := source.payload[source.Policy.ConfigFile]
	raw, _, _, e := decodeConfig(ctx, source.Policy, original.data, sourceEnv)
	if e != nil {
		return Snapshot{}, e
	}
	if e = changePaths(raw, source.Policy.WorkingDir, mapPath); e != nil {
		return Snapshot{}, e
	}
	expected := raw
	matches := func(data []byte) bool {
		actual, _, _, e := decodeConfig(ctx, target, data, targetEnv)
		if e != nil {
			return false
		}
		e = changePaths(actual, target.WorkingDir, func(p string, dir bool) (string, error) {
			if !target.authorized(p, dir) {
				return "", failure("path_unauthorized")
			}
			return p, nil
		})
		return e == nil && reflect.DeepEqual(expected, actual)
	}
	candidate := append([]byte(nil), original.data...)
	if !matches(candidate) {
		symbolic, actions, e := relocationSymbolize(source.Policy.ConfigFile, original.data)
		if e != nil {
			return Snapshot{}, e
		}
		raw, e := decodeRaw(source.Policy.ConfigFile, symbolic)
		if e != nil {
			return Snapshot{}, e
		}
		for _, path := range pathFields {
			m, k, ok := field(raw, path)
			if !ok {
				continue
			}
			value, ok := m[k].(string)
			if !ok {
				return Snapshot{}, failure("config_invalid")
			}
			if value == "" || path == "log.to" && value == "console" {
				continue
			}
			if strings.Contains(value, relocationTemplatePrefix) && !filepath.IsAbs(value) {
				continue
			}
			v, e := mapPath(resolve(value, source.Policy.WorkingDir), path == "monitor.historyDataPath")
			if e != nil {
				return Snapshot{}, e
			}
			m[k] = v
		}
		candidate, e = relocationEncode(target.ConfigFile, raw)
		if e != nil {
			return Snapshot{}, e
		}
		for _, action := range actions {
			needle := action.marker
			if !action.quoted {
				if bytes.Contains(candidate, []byte(`"`+needle+`"`)) {
					needle = `"` + needle + `"`
				} else if bytes.Contains(candidate, []byte("'"+needle+"'")) {
					needle = "'" + needle + "'"
				}
			}
			if bytes.Count(candidate, []byte(needle)) != 1 {
				return Snapshot{}, failure("relocation_template_unsupported")
			}
			candidate = bytes.Replace(candidate, []byte(needle), action.raw, 1)
		}
		if !matches(candidate) {
			return Snapshot{}, failure("relocation_semantics_changed")
		}
	}
	materials := map[string]material{}
	for path, m := range source.payload {
		targetPath, e := mapPath(path, false)
		if e != nil {
			return Snapshot{}, e
		}
		if _, ok := materials[targetPath]; ok {
			return Snapshot{}, failure("dependency_conflict")
		}
		if path == source.Policy.ConfigFile {
			m.data = candidate
		}
		materials[targetPath] = m
	}
	result, e := build(ctx, target, targetEnv, func(path string) (material, error) {
		m, ok := materials[path]
		if !ok {
			return m, failure("dependency_missing")
		}
		return m, nil
	})
	if e != nil {
		return Snapshot{}, e
	}
	if len(result.payload) != len(materials) {
		return Snapshot{}, failure("dependency_conflict")
	}
	if e = result.protect(forbidden); e != nil {
		return Snapshot{}, e
	}
	return result, nil
}
