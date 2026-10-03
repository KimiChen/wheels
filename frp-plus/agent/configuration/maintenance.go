package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	nativeconfig "github.com/fatedier/frp/pkg/config"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/pelletier/go-toml/v2"
)

var maintenanceNames = []string{"installation.json", "agent.toml", "agent.token", "frp.token", "ca.crt", "tls.crt", "tls.key", "local.crt", "local.key"}

func maintenancePrivate(path string) (managed.ContextFile, error) {
	data, info, err := regularNoLinks(path)
	if err != nil {
		return managed.ContextFile{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode().Perm() != 0600 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || int64(len(data)) > managed.MaxCheckpointFileBytes {
		return managed.ContextFile{}, failure("unsupported_dependency")
	}
	return managed.ContextFile{Path: path, Bytes: data, ModifiedNS: info.ModTime().UnixNano()}, nil
}

// MaintenanceCapture deliberately supports only the generated, single-agent
// installation profile. It does not execute native loaders, load Store into
// memory, open the Engine, or infer environment values from an archive.
func MaintenanceCapture(configFile string) managed.ContextCapture {
	return func(ctx context.Context) (managed.ContextSnapshot, error) {
		out := managed.ContextSnapshot{ConfigFile: configFile, WorkingDir: filepath.Dir(configFile)}
		if !filepath.IsAbs(configFile) || filepath.Clean(configFile) != configFile || filepath.Base(configFile) != "agent.toml" {
			return out, managed.ErrUnsafePath
		}
		info, err := os.Lstat(out.WorkingDir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&os.ModeSymlink != 0 {
			return out, managed.ErrUnsafePath
		}
		all := []managed.ContextFile{}
		var total int64
		for _, name := range maintenanceNames {
			file, err := maintenancePrivate(filepath.Join(out.WorkingDir, name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return out, managed.ErrUnsafePath
			}
			total += int64(len(file.Bytes))
			if total > managed.MaxCheckpointBytes {
				return out, managed.ErrInvalid
			}
			out.Files = append(out.Files, file)
			all = append(all, file)
		}
		root := filepath.Join(out.WorkingDir, "managed")
		store, err := maintenancePrivate(filepath.Join(root, "store.json"))
		if err == nil {
			total += int64(len(store.Bytes))
			if total > managed.MaxCheckpointBytes {
				return out, managed.ErrInvalid
			}
			all = append(all, store)
		} else if !os.IsNotExist(err) {
			return out, managed.ErrUnsafePath
		}
		names, err := os.ReadDir(filepath.Join(root, "operations"))
		if err != nil {
			return out, managed.ErrUnsafePath
		}
		if len(names) > managed.MaxCheckpointFiles {
			return out, managed.ErrInvalid
		}
		for _, name := range names {
			if strings.HasSuffix(name.Name(), ".old") || strings.HasSuffix(name.Name(), ".new") {
				file, err := maintenancePrivate(filepath.Join(root, "operations", name.Name()))
				if err != nil {
					return out, managed.ErrUnsafePath
				}
				total += int64(len(file.Bytes))
				if total > managed.MaxCheckpointBytes {
					return out, managed.ErrInvalid
				}
				all = append(all, file)
			}
		}
		revision, err := maintenanceValidate(ctx, configFile, all)
		if err != nil {
			return out, err
		}
		out.Revision = revision
		return out, nil
	}
}

// ValidateMaintenanceCheckpoint validates dependencies from staged bytes only.
// No current target file is used to satisfy a missing archive dependency.
func ValidateMaintenanceCheckpoint(ctx context.Context, manifest managed.CheckpointManifest, files []managed.ContextFile) error {
	revision, err := maintenanceValidate(ctx, manifest.ConfigFile, files)
	if err != nil {
		return err
	}
	if revision != manifest.ContextRevision {
		return managed.ErrConflict
	}
	return nil
}

func maintenanceValidate(ctx context.Context, configFile string, files []managed.ContextFile) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	parent := filepath.Dir(configFile)
	root := filepath.Join(parent, "managed")
	materials := map[string]dependency{}
	for _, file := range files {
		if _, ok := materials[file.Path]; ok {
			return "", managed.ErrInvalid
		}
		materials[file.Path] = dependency{Path: file.Path, State: "ready", Data: file.Bytes, Mode: 0600, Modified: file.ModifiedNS}
	}
	config, ok := materials[configFile]
	if !ok || bytes.Contains(config.Data, []byte("{{")) {
		return "", failure("unsupported_dependency")
	}
	metadata, ok := materials[filepath.Join(parent, "installation.json")]
	if !ok || strictJSON(metadata.Data) != nil {
		return "", failure("unsupported_dependency")
	}
	var install struct {
		Format  int      `json:"format"`
		Roles   []string `json:"roles"`
		Managed struct {
			Version int    `json:"version"`
			Root    string `json:"root"`
			Store   string `json:"store"`
		} `json:"managed"`
	}
	if json.Unmarshal(metadata.Data, &install) != nil || install.Format != 2 || len(install.Roles) != 1 || install.Roles[0] != "agent" || install.Managed.Version != 1 || install.Managed.Root != "managed" || install.Managed.Store != "store.json" {
		return "", failure("unsupported_dependency")
	}
	if _, ok := materials[filepath.Join(parent, "agent.token")]; !ok {
		return "", failure("unsupported_dependency")
	}
	var syntax map[string]any
	if toml.Unmarshal(config.Data, &syntax) != nil {
		return "", failure("unsupported_source")
	}
	cfg := v1.ClientConfig{}
	if nativeconfig.LoadConfigure(config.Data, &cfg, true, "toml") != nil {
		return "", failure("unsupported_source")
	}
	common := &cfg.ClientCommonConfig
	if len(common.IncludeConfigFiles) != 0 || common.Telemetry.ConfigManagement == nil || !common.Telemetry.ConfigManagement.Enabled || common.Telemetry.ConfigManagement.Root != root || common.Store.Path != filepath.Join(root, "store.json") {
		return "", failure("unsupported_dependency")
	}
	// Complete consults http_proxy. The generated profile cannot silently depend
	// on this process environment; require it absent even with an explicit URL.
	if os.Getenv("http_proxy") != "" {
		return "", failure("unsupported_dependency")
	}
	if common.Complete() != nil {
		return "", failure("invalid_source")
	}
	objects := Objects{Proxies: []v1.ProxyConfigurer{}, Visitors: []v1.VisitorConfigurer{}}
	for _, p := range cfg.Proxies {
		objects.Proxies = append(objects.Proxies, p.ProxyConfigurer)
	}
	for _, v := range cfg.Visitors {
		objects.Visitors = append(objects.Visitors, v.VisitorConfigurer)
	}
	input := Input{ConfigFile: configFile, StoreFile: filepath.Join(root, "store.json"), WorkingDir: parent, StartupCommon: common, ReloadCommon: common, FileMemory: objects, TemplateEnv: map[string]string{}, UnsafeFeatures: []string{}}
	snapshot, err := takeSnapshotMaterials(input, materials, true)
	if err != nil {
		return "", err
	}
	if len(snapshot.issues) != 0 || len(snapshot.referencedEnv) != 0 || len(snapshot.patterns) != 0 {
		return "", failure("unsupported_dependency")
	}
	allowed := map[string]bool{}
	for _, name := range maintenanceNames {
		allowed[filepath.Join(parent, name)] = true
	}
	for _, object := range snapshot.files {
		if maintenanceDynamicPlugin(object) {
			return "", failure("unsupported_dependency")
		}
	}
	for _, dep := range snapshot.deps {
		if !allowed[dep.Path] || dep.State != "ready" {
			return "", failure("unsupported_dependency")
		}
	}
	// Store snapshots can become the next runtime during recovery. Check every
	// old/new material as well as current Store; no external plugin resource may
	// hide in an inactive or historical candidate.
	paths := []string{}
	for path := range materials {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if path != input.StoreFile && !(filepath.Dir(path) == filepath.Join(root, "operations") && (strings.HasSuffix(path, ".old") || strings.HasSuffix(path, ".new"))) {
			continue
		}
		data := materials[path].Data
		if len(data) == 0 {
			continue
		} // absent old Store snapshots are zero-byte private files
		values, err := parseStore(data)
		if err != nil {
			return "", err
		}
		for _, object := range values {
			if maintenanceDynamicPlugin(object) {
				return "", failure("unsupported_dependency")
			}
			for _, field := range []string{"plugin.crtPath", "plugin.keyPath", "plugin.localPath", "plugin.unixPath"} {
				if raw, ok := getPath(object.raw, field); ok {
					var value string
					if json.Unmarshal(raw, &value) != nil || value != "" {
						return "", failure("unsupported_dependency")
					}
				}
			}
		}
	}
	return snapshot.contextRevision, nil
}

func maintenanceDynamicPlugin(object object) bool {
	raw, ok := getPath(object.raw, "plugin.type")
	if !ok {
		return false
	}
	var kind string
	if json.Unmarshal(raw, &kind) != nil {
		return true
	}
	return kind == "static_file" || kind == "unix_domain_socket"
}
