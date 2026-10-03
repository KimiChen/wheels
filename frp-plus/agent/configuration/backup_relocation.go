package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// PrepareRelocationV3 is a pure, sealed transformation. The archive supplies
// material, never path authorization. Source and destination policies/Envs are
// independent explicit local inputs. Only native path fields may change.
func PrepareRelocationV3(sourcePolicy, targetPolicy BackupGraphPolicy) managed.ContextRelocatorV3 {
	return func(ctx context.Context, manifest managed.CheckpointManifestV2, files []managed.ContextFile, variants []managed.StoreVariant, graph, policyJSON []byte) (managed.RelocationContextV3, error) {
		var out managed.RelocationContextV3
		source, _, err := normalizeBackupPolicy(sourcePolicy)
		if err != nil {
			return out, err
		}
		target, targetDigest, err := normalizeBackupPolicy(targetPolicy)
		if err != nil {
			return out, err
		}
		mapping, err := newBackupRelocationMap(source, target)
		if err != nil {
			return out, err
		}
		if err = ValidateCheckpointContextV3(source)(ctx, manifest, files, variants, graph, policyJSON); err != nil {
			return out, err
		}
		m, err := backupmanifest.Decode(graph)
		if err != nil {
			return out, backupErr("manifest_invalid")
		}
		kinds := map[string]string{}
		for _, entry := range m.Files {
			kinds[entry.Path] = entry.Kind
		}
		view := &backupSealedView{files: map[string]backupSourceFile{}, directories: map[string][]backupmanifest.Entry{}, usedFiles: map[string]bool{}, usedDirectories: map[string]bool{}}
		material := map[string]managed.ContextFile{}
		for _, file := range files {
			path, err := mapping.path(file.Path, false)
			if err != nil {
				return out, err
			}
			data := append([]byte(nil), file.Bytes...)
			if kinds[file.Path] == "main" || kinds[file.Path] == "include" {
				data, err = relocateBackupConfig(ctx, source, target, mapping, file.Path, path, data, kinds[file.Path] == "main")
				if err != nil {
					return out, err
				}
			}
			if _, exists := material[path]; exists {
				return out, backupErr("ambiguous_dependency")
			}
			material[path] = managed.ContextFile{Path: path, Bytes: data, ModifiedNS: file.ModifiedNS}
			view.files[path] = backupSourceFile{data: data, exists: true, mode: 0600, modified: file.ModifiedNS}
		}
		var store managed.StoreSnapshot
		found := false
		for _, variant := range variants {
			if variant.Path == "managed/store.json" {
				if found {
					return out, backupErr("source_invalid")
				}
				found = true
				store = variant.Snapshot
				store.Bytes = append([]byte(nil), store.Bytes...)
			}
		}
		if !found {
			return out, backupErr("source_invalid")
		}
		if store.Exists {
			store.Bytes, err = relocateBackupStore(source, target, mapping, store.Bytes)
			if err != nil {
				return out, err
			}
		}
		stamp := int64(0)
		for _, entry := range m.Files {
			if entry.Kind == "store" {
				stamp = entry.ModifiedNS
			}
		}
		view.files[target.StoreFile] = backupSourceFile{data: store.Bytes, exists: store.Exists, mode: 0600, modified: stamp}
		if !store.Exists {
			view.files[target.StoreFile] = backupSourceFile{}
		}
		for _, directory := range m.Directories {
			path, err := mapping.path(directory.Path, true)
			if err != nil {
				return out, err
			}
			if _, exists := view.directories[path]; exists {
				return out, backupErr("ambiguous_dependency")
			}
			view.directories[path] = append([]backupmanifest.Entry{}, directory.Entries...)
		}
		rebuilt, err := buildBackupGraph(ctx, target, targetDigest, view)
		if err != nil {
			return out, err
		}
		// Only executable closure plus generated installation metadata is installed.
		// Historical operation snapshots remain original managed material; they
		// cannot authorize copying old-context certificates into the new context.
		needed := map[string]bool{filepath.Join(target.WorkingDir, "installation.json"): true}
		for path := range view.usedFiles {
			if path != target.StoreFile {
				needed[path] = true
			}
		}
		collectTargetFiles := func() ([]managed.ContextFile, error) {
			out := []managed.ContextFile{}
			for path := range needed {
				file, ok := material[path]
				if !ok {
					return nil, backupErr("dependency_missing")
				}
				out = append(out, file)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
			return out, nil
		}
		targetFiles, err := collectTargetFiles()
		if err != nil {
			return out, err
		}
		targetVariants := []managed.StoreVariant{{Path: "managed/store.json", Snapshot: store}}
		revision, err := validateCheckpointSourcesV3(ctx, target, rebuilt.ManifestBytes, targetFiles, targetVariants)
		if err != nil {
			return out, err
		}
		targetPolicyJSON, err := EncodeBackupPolicy(target)
		if err != nil {
			return out, err
		}
		changed := revision != manifest.ContextRevision || !bytes.Equal(targetPolicyJSON, policyJSON)
		for _, variant := range variants {
			if variant.Path != "managed/store.json" {
				if changed {
					variant.Historical = true
				}
				targetVariants = append(targetVariants, variant)
			}
		}
		if !changed {
			// Same-context restoration must keep old/new runtime dependencies
			// needed by unfinished operations. They cannot be discarded merely
			// because current Store does not reference them.
			extra, err := checkpointVariantDependencies(target, targetVariants)
			if err != nil {
				return out, err
			}
			for _, path := range extra {
				needed[path] = true
			}
			targetFiles, err = collectTargetFiles()
			if err != nil {
				return out, err
			}
		}
		revision, err = validateCheckpointSourcesV3(ctx, target, rebuilt.ManifestBytes, targetFiles, targetVariants)
		if err != nil {
			return out, err
		}
		out.Context = managed.ContextSnapshotV3{ContextSnapshotV2: managed.ContextSnapshotV2{ConfigFile: target.ConfigFile, WorkingDir: target.WorkingDir, Revision: revision, GraphBytes: rebuilt.ManifestBytes, Files: targetFiles}, PolicyBytes: targetPolicyJSON}
		out.Store = store
		for _, root := range target.Roots {
			out.TargetRoots = append(out.TargetRoots, root.Path)
		}
		for _, file := range target.Mappings {
			out.TargetFiles = append(out.TargetFiles, file.Path)
		}
		return out, nil
	}
}

type backupRelocationMap struct {
	source, target BackupGraphPolicy
	roots, files   map[string]string
}

func newBackupRelocationMap(source, target BackupGraphPolicy) (backupRelocationMap, error) {
	out := backupRelocationMap{source: source, target: target, roots: map[string]string{}, files: map[string]string{}}
	if len(source.Roots) != len(target.Roots) || len(source.Mappings) != len(target.Mappings) {
		return out, backupErr("policy_mismatch")
	}
	for i, root := range source.Roots {
		if root.ID != target.Roots[i].ID {
			return out, backupErr("policy_mismatch")
		}
		out.roots[root.Path] = target.Roots[i].Path
	}
	for i, file := range source.Mappings {
		if file.ID != target.Mappings[i].ID {
			return out, backupErr("policy_mismatch")
		}
		out.files[file.Path] = target.Mappings[i].Path
	}
	for _, pair := range [][2]string{{source.ConfigFile, target.ConfigFile}, {source.WorkingDir, target.WorkingDir}, {source.StoreFile, target.StoreFile}} {
		mapped, err := out.path(pair[0], pair[0] == source.WorkingDir)
		if err != nil || mapped != pair[1] {
			return out, backupErr("policy_mismatch")
		}
	}
	// Mapping into a different source subtree risks overwriting live source
	// material during publication; same-path mappings themselves are allowed.
	for old, next := range out.roots {
		for _, root := range source.Roots {
			if old != next && (backupWithin(next, root.Path) || backupWithin(root.Path, next)) {
				return out, backupErr("policy_mismatch")
			}
		}
		for _, file := range source.Mappings {
			if old != next && backupWithin(file.Path, next) {
				return out, backupErr("policy_mismatch")
			}
		}
	}
	for old, next := range out.files {
		if old == next {
			continue
		}
		for _, root := range source.Roots {
			if backupWithin(next, root.Path) {
				return out, backupErr("policy_mismatch")
			}
		}
		for _, file := range source.Mappings {
			if next == file.Path {
				return out, backupErr("policy_mismatch")
			}
		}
	}
	return out, nil
}
func (m backupRelocationMap) path(path string, directory bool) (string, error) {
	if !backupPath(path) {
		return "", backupErr("path_unauthorized")
	}
	if !directory {
		if next, ok := m.files[path]; ok {
			return next, nil
		}
	}
	for old, next := range m.roots {
		if backupWithin(path, old) {
			rel, _ := filepath.Rel(old, path)
			return filepath.Join(next, rel), nil
		}
	}
	return "", backupErr("path_unauthorized")
}
func relocationAbsolute(value, base string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(base, value)
}

var relocationCommonPaths = []string{"transport.tls.certFile", "transport.tls.keyFile", "transport.tls.trustedCaFile", "webServer.tls.certFile", "webServer.tls.keyFile", "webServer.tls.trustedCaFile", "auth.oidc.trustedCaFile", "auth.tokenSource.file.path", "auth.oidc.tokenSource.file.path", "telemetry.tokenFile", "telemetry.caFile", "telemetry.configManagement.root"}

// relocatePathFields modifies a generic, lossless parsed configuration. The
// visitor receives the source path resolution base (Store is config-relative;
// every other FRP dependency is working-directory-relative).
func relocatePathFields(raw map[string]any, policy BackupGraphPolicy, main bool, visit func(string, string) (string, error)) error {
	paths := []string{}
	if main {
		paths = append(paths, relocationCommonPaths...)
		paths = append(paths, "store.path")
	}
	for _, path := range paths {
		parent, key, ok := relocationField(raw, path)
		if !ok {
			continue
		}
		value, ok := parent[key].(string)
		if !ok {
			return backupErr("source_invalid")
		}
		if value == "" {
			continue
		}
		base := policy.WorkingDir
		if path == "store.path" {
			base = filepath.Dir(policy.ConfigFile)
		}
		next, err := visit(value, base)
		if err != nil {
			return err
		}
		parent[key] = next
	}
	if main {
		if value, ok := raw["includes"]; ok {
			entries, ok := value.([]any)
			if !ok {
				return backupErr("source_invalid")
			}
			for i, entry := range entries {
				pattern, ok := entry.(string)
				if !ok {
					return backupErr("source_invalid")
				}
				next, err := visit(pattern, policy.WorkingDir)
				if err != nil {
					return err
				}
				entries[i] = next
			}
		}
	}
	for _, kind := range []string{"proxies", "visitors"} {
		value, ok := raw[kind]
		if !ok {
			continue
		}
		entries, ok := value.([]any)
		if !ok {
			return backupErr("source_invalid")
		}
		for _, entry := range entries {
			object, ok := entry.(map[string]any)
			if !ok {
				return backupErr("source_invalid")
			}
			for _, path := range []string{"plugin.crtPath", "plugin.keyPath"} {
				parent, key, ok := relocationField(object, path)
				if !ok {
					continue
				}
				value, ok := parent[key].(string)
				if !ok {
					return backupErr("source_invalid")
				}
				if value == "" {
					continue
				}
				next, err := visit(value, policy.WorkingDir)
				if err != nil {
					return err
				}
				parent[key] = next
			}
		}
	}
	return nil
}
func relocationField(raw map[string]any, path string) (map[string]any, string, bool) {
	parts := strings.Split(path, ".")
	current := raw
	for _, part := range parts[:len(parts)-1] {
		child, ok := current[part].(map[string]any)
		if !ok {
			return nil, "", false
		}
		current = child
	}
	key := parts[len(parts)-1]
	_, ok := current[key]
	return current, key, ok
}
func relocationDecode(path string, data []byte) (map[string]any, error) {
	var out map[string]any
	var err error
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		err = decoder.Decode(&out)
	case ".toml":
		err = toml.Unmarshal(data, &out)
	case ".yaml", ".yml":
		err = yaml.Unmarshal(data, &out)
	default:
		return nil, backupErr("unsupported_format")
	}
	if err != nil || out == nil {
		return nil, backupErr("relocation_template_unsupported")
	}
	// Normalize TOML's []map and numeric concrete types without losing fields.
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, backupErr("source_invalid")
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	if dec.Decode(&out) != nil {
		return nil, backupErr("source_invalid")
	}
	return out, nil
}
func relocationEncode(path string, raw map[string]any) ([]byte, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return json.MarshalIndent(raw, "", "  ")
	case ".toml":
		keys := make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var out strings.Builder
		for _, key := range keys {
			name, _ := json.Marshal(key)
			value, err := relocationTOML(raw[key])
			if err != nil {
				return nil, err
			}
			out.Write(name)
			out.WriteString(" = ")
			out.WriteString(value)
			out.WriteByte('\n')
		}
		return []byte(out.String()), nil
	case ".yaml", ".yml":
		node, err := relocationYAML(raw)
		if err != nil {
			return nil, err
		}
		return yaml.Marshal(node)
	}
	return nil, backupErr("unsupported_format")
}
func relocationYAML(value any) (*yaml.Node, error) {
	switch v := value.(type) {
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, err := relocationYAML(v[key])
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, Style: yaml.DoubleQuotedStyle}, child)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range v {
			child, err := relocationYAML(item)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, child)
		}
		return n, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(string(v), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(v)}, nil
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(v)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	default:
		return nil, backupErr("source_invalid")
	}
}

func relocationTOML(value any) (string, error) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := []string{}
		for _, key := range keys {
			name, _ := json.Marshal(key)
			text, err := relocationTOML(v[key])
			if err != nil {
				return "", err
			}
			parts = append(parts, string(name)+" = "+text)
		}
		return "{ " + strings.Join(parts, ", ") + " }", nil
	case []any:
		parts := []string{}
		for _, item := range v {
			text, err := relocationTOML(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case json.Number, string, bool:
		data, err := json.Marshal(v)
		if err != nil {
			return "", backupErr("source_invalid")
		}
		return string(data), nil
	default:
		return "", backupErr("source_invalid")
	}
}
func relocationComparable(path string, data []byte, policy BackupGraphPolicy, main bool, mapper *backupRelocationMap) (map[string]any, error) {
	raw, err := relocationDecode(path, data)
	if err != nil {
		return nil, err
	}
	err = relocatePathFields(raw, policy, main, func(value, base string) (string, error) {
		absolute := relocationAbsolute(value, base)
		if mapper != nil {
			return mapper.path(absolute, false)
		}
		return absolute, nil
	})
	return raw, err
}
func relocateBackupConfig(ctx context.Context, source, target BackupGraphPolicy, mapping backupRelocationMap, oldPath, newPath string, data []byte, main bool) ([]byte, error) {
	before, _, err := RenderBackupTemplate(ctx, data, source.TemplateEnv)
	if err != nil {
		return nil, err
	}
	expected, err := relocationComparable(oldPath, before, source, main, &mapping)
	if err != nil {
		return nil, err
	}
	matches := func(candidate []byte) bool {
		rendered, _, err := RenderBackupTemplate(ctx, candidate, target.TemplateEnv)
		if err != nil {
			return false
		}
		if _, _, err = decodeBackupRendered(newPath, rendered); err != nil {
			return false
		}
		actual, err := relocationComparable(newPath, rendered, target, main, nil)
		return err == nil && reflect.DeepEqual(expected, actual)
	}
	if matches(data) {
		return append([]byte(nil), data...), nil
	}
	symbolic, actions, err := relocationSymbolize(oldPath, data)
	if err != nil {
		return nil, err
	}
	raw, err := relocationDecode(oldPath, symbolic)
	if err != nil {
		return nil, err
	}
	err = relocatePathFields(raw, source, main, func(value, base string) (string, error) {
		if strings.Contains(value, relocationTemplatePrefix) {
			// A full variable is reproduced by target Envs; an absolute literal
			// prefix can itself be mapped without touching the expression.
			if !filepath.IsAbs(value) {
				return value, nil
			}
			return mapping.path(filepath.Clean(value), false)
		}
		return mapping.path(relocationAbsolute(value, base), false)
	})
	if err != nil {
		return nil, err
	}
	candidate, err := relocationEncode(newPath, raw)
	if err != nil {
		return nil, backupErr("source_invalid")
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
			return nil, backupErr("relocation_template_unsupported")
		}
		candidate = bytes.Replace(candidate, []byte(needle), action.raw, 1)
	}
	if !matches(candidate) {
		return nil, backupErr("relocation_semantics_changed")
	}
	return candidate, nil
}
func relocateBackupStore(source, target BackupGraphPolicy, mapping backupRelocationMap, data []byte) ([]byte, error) {
	if _, err := parseStore(data); err != nil {
		return nil, backupErr("source_invalid")
	}
	raw, err := relocationDecode("store.json", data)
	if err != nil {
		return nil, err
	}
	expected, err := relocationComparable("store.json", data, source, false, &mapping)
	if err != nil {
		return nil, err
	}
	unchanged, err := relocationComparable("store.json", data, target, false, nil)
	if err == nil && reflect.DeepEqual(expected, unchanged) {
		return append([]byte(nil), data...), nil
	}
	err = relocatePathFields(raw, source, false, func(value, base string) (string, error) { return mapping.path(relocationAbsolute(value, base), false) })
	if err != nil {
		return nil, err
	}
	candidate, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, backupErr("source_invalid")
	}
	if _, err = parseStore(candidate); err != nil {
		return nil, backupErr("source_invalid")
	}
	return candidate, nil
}

const relocationTemplatePrefix = "FRPPLUSBACKUPTEMPLATE"

type relocationAction struct {
	marker string
	raw    []byte
	quoted bool
}

// Symbolization preserves exact scalar template expressions, not rendered
// values. Control-flow templates can be kept byte-for-byte by the fast path;
// rewriting them is explicitly unsupported instead of guessing their syntax.
func relocationSymbolize(path string, data []byte) ([]byte, []relocationAction, error) {
	if bytes.Contains(data, []byte(relocationTemplatePrefix)) {
		return nil, nil, backupErr("relocation_template_unsupported")
	}
	var out bytes.Buffer
	actions := []relocationAction{}
	quote := byte(0)
	triple := false
	comment := false
	format := strings.ToLower(filepath.Ext(path))
	for i := 0; i < len(data); {
		if i+1 < len(data) && data[i] == '{' && data[i+1] == '{' {
			end, err := relocationActionEnd(data, i+2)
			if err != nil {
				return nil, nil, err
			}
			raw := data[i:end]
			noop := func(...any) any { return nil }
			parsed, err := template.New("scalar").Funcs(template.FuncMap{"parseNumberRange": noop, "parseNumberRangePair": noop}).Parse(string(raw))
			if err != nil || len(parsed.Tree.Root.Nodes) != 1 {
				return nil, nil, backupErr("relocation_template_unsupported")
			}
			node, ok := parsed.Tree.Root.Nodes[0].(*parse.ActionNode)
			if !ok || len(node.Pipe.Decl) != 0 {
				return nil, nil, backupErr("relocation_template_unsupported")
			}
			marker := fmt.Sprintf("%s%06dZ", relocationTemplatePrefix, len(actions)+1)
			inside := quote != 0
			if !inside && format != ".yaml" && format != ".yml" {
				out.WriteByte('"')
			}
			out.WriteString(marker)
			if !inside && format != ".yaml" && format != ".yml" {
				out.WriteByte('"')
			}
			actions = append(actions, relocationAction{marker, append([]byte(nil), raw...), inside})
			i = end
			continue
		}
		ch := data[i]
		if ch == '\n' {
			comment = false
		}
		if !comment {
			if quote != 0 {
				if ch == '\\' && quote == '"' && i+1 < len(data) {
					out.Write(data[i : i+2])
					i += 2
					continue
				}
				if ch == quote {
					if triple {
						if i+2 < len(data) && data[i+1] == quote && data[i+2] == quote {
							out.Write(data[i : i+3])
							i += 3
							quote = 0
							triple = false
							continue
						}
					} else {
						quote = 0
					}
				}
			} else if ch == '#' && format != ".json" {
				comment = true
			} else if ch == '"' || ch == '\'' && format != ".json" {
				quote = ch
				if format == ".toml" && i+2 < len(data) && data[i+1] == ch && data[i+2] == ch {
					triple = true
					out.Write(data[i : i+3])
					i += 3
					continue
				}
			}
		}
		out.WriteByte(ch)
		i++
	}
	return out.Bytes(), actions, nil
}
func relocationActionEnd(data []byte, start int) (int, error) {
	quote := byte(0)
	for i := start; i < len(data); i++ {
		ch := data[i]
		if quote != 0 {
			if ch == '\\' && quote != '`' {
				i++
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' || ch == '`' {
			quote = ch
			continue
		}
		if ch == '}' && i+1 < len(data) && data[i+1] == '}' {
			return i + 2, nil
		}
	}
	return 0, backupErr("relocation_template_unsupported")
}
