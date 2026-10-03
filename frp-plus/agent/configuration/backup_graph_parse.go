package configuration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	nativeconfig "github.com/fatedier/frp/pkg/config"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/pelletier/go-toml/v2"
)

// Native decoding is strict and non-executing. Format is checked before the
// native parser's format fallbacks, and templates are rejected before render.
func decodeBackupFile(path string, data []byte) (*v1.ClientCommonConfig, []object, error) {
	if bytes.Contains(data, []byte("{{")) {
		return nil, nil, backupErr("unsupported_dependency")
	}
	return decodeBackupRendered(path, data)
}

func decodeBackupRendered(path string, data []byte) (*v1.ClientCommonConfig, []object, error) {
	if nativeconfig.DetectLegacyINIFormat(data) {
		return nil, nil, backupErr("unsupported_dependency")
	}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if format == "yml" {
		format = "yaml"
	}
	switch format {
	case "json":
		if strictJSON(data) != nil {
			return nil, nil, backupErr("source_invalid")
		}
	case "toml":
		var raw map[string]any
		if toml.Unmarshal(data, &raw) != nil {
			return nil, nil, backupErr("source_invalid")
		}
	case "yaml":
		// Includes duplicate keys, aliases/anchors and native dot-key removal.
		if migrationStrict(data, "yaml") != nil {
			return nil, nil, backupErr("source_invalid")
		}
	default:
		return nil, nil, backupErr("unsupported_format")
	}
	var cfg v1.ClientConfig
	if nativeconfig.LoadConfigure(data, &cfg, true, format) != nil {
		return nil, nil, backupErr("source_invalid")
	}
	values := Objects{}
	for _, p := range cfg.Proxies {
		values.Proxies = append(values.Proxies, p.ProxyConfigurer)
	}
	for _, v := range cfg.Visitors {
		values.Visitors = append(values.Visitors, v.VisitorConfigurer)
	}
	objects, err := fromNative(values, "file")
	if err != nil {
		return nil, nil, backupErr("source_invalid")
	}
	return &cfg.ClientCommonConfig, objects, nil
}
func backupRaw(value any) map[string]json.RawMessage {
	data, _ := json.Marshal(value)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	return raw
}
func backupString(raw map[string]json.RawMessage, path string) (string, error) {
	data, ok := getPath(raw, path)
	if !ok {
		return "", nil
	}
	var value string
	if json.Unmarshal(data, &value) != nil {
		return "", backupErr("source_invalid")
	}
	return value, nil
}

func (b *backupGraphBuilder) collect() error {
	main, err := b.read(b.policy.ConfigFile, "main", false)
	if err != nil {
		return err
	}
	common, objects, err := b.decode(b.policy.ConfigFile, main.data)
	if err != nil {
		return err
	}
	if backupAmbientProxyUnsupported(b.view, common.Transport.ProxyURL) {
		return backupErr("unsupported_dependency")
	}
	if common.Store.Path == "" {
		return backupErr("store_unconfigured")
	}
	storePath := common.Store.Path
	if !filepath.IsAbs(storePath) {
		storePath = filepath.Join(filepath.Dir(b.policy.ConfigFile), storePath)
	} else {
		storePath = filepath.Clean(storePath)
	}
	if storePath != b.policy.StoreFile {
		return backupErr("store_mismatch")
	}
	if len(common.IncludeConfigFiles) > b.policy.Limits.Directories {
		return backupErr("limit_exceeded")
	}
	if err := b.commonDependencies(b.policy.ConfigFile, common); err != nil {
		return err
	}
	seen := map[string]bool{}
	if err := b.objectDependencies(b.policy.ConfigFile, objects, seen); err != nil {
		return err
	}
	seenFiles := map[string]bool{b.policy.ConfigFile: true}
	for index, pattern := range common.IncludeConfigFiles {
		if pattern == "" || strings.ContainsAny(pattern, "\x00\r\n") {
			return backupErr("source_invalid")
		}
		pattern = b.resolve(pattern)
		if _, err := filepath.Match(filepath.Base(pattern), ""); err != nil {
			return backupErr("source_invalid")
		}
		entries, err := b.list(filepath.Dir(pattern))
		if err != nil {
			return err
		}
		include := backupmanifest.Include{Pattern: pattern, Files: []string{}}
		for _, entry := range entries {
			if entry.Kind == "directory" {
				continue
			}
			matches, err := filepath.Match(filepath.Base(pattern), entry.Name)
			if err != nil {
				return backupErr("source_invalid")
			}
			if !matches {
				continue
			}
			path := filepath.Join(filepath.Dir(pattern), entry.Name)
			if entry.Kind != "file" {
				return backupErr("source_unsafe")
			}
			// Native loads every pattern in order. Reject duplicate physical
			// inputs even if an empty include would otherwise hide the overlap.
			if seenFiles[path] {
				return backupErr("duplicate_source")
			}
			seenFiles[path] = true
			file, err := b.reference(b.policy.ConfigFile, fmt.Sprintf("includes[%d]", index), path, "include", false)
			if err != nil {
				return err
			}
			_, included, err := b.decode(path, file.data)
			if err != nil {
				return err
			}
			// Include common fields (including nested includes) are discarded
			// by native FRP. Only their Proxy/Visitor objects contribute.
			if err := b.objectDependencies(path, included, seen); err != nil {
				return err
			}
			include.Files = append(include.Files, path)
		}
		b.manifest.Includes = append(b.manifest.Includes, include)
	}
	store, err := b.reference(b.policy.ConfigFile, "store.path", storePath, "store", true)
	if err != nil {
		return err
	}
	if store.exists {
		values, err := parseStore(store.data)
		if err != nil {
			return backupErr("source_invalid")
		}
		if err := b.objectDependencies(storePath, values, seen); err != nil {
			return err
		}
	}
	return nil
}

func (b *backupGraphBuilder) commonDependencies(from string, common *v1.ClientCommonConfig) error {
	raw := backupRaw(common)
	for _, path := range []string{"auth.tokenSource.type", "auth.oidc.tokenSource.type"} {
		value, err := backupString(raw, path)
		if err != nil {
			return err
		}
		if value != "" && value != "file" {
			return backupErr("unsupported_dependency")
		}
	}
	assets, err := backupString(raw, "webServer.assetsDir")
	if err != nil {
		return err
	}
	if assets != "" {
		return backupErr("unsupported_dependency")
	}
	for _, path := range []string{"transport.tls.certFile", "transport.tls.keyFile", "transport.tls.trustedCaFile", "webServer.tls.certFile", "webServer.tls.keyFile", "webServer.tls.trustedCaFile", "auth.oidc.trustedCaFile", "auth.tokenSource.file.path", "auth.oidc.tokenSource.file.path", "telemetry.tokenFile", "telemetry.caFile"} {
		value, err := backupString(raw, path)
		if err != nil {
			return err
		}
		if value == "" {
			continue
		}
		if _, err := b.reference(from, path, b.resolve(value), "local_file", false); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{{"auth.tokenSource.type", "auth.tokenSource.file.path"}, {"auth.oidc.tokenSource.type", "auth.oidc.tokenSource.file.path"}} {
		kind, _ := backupString(raw, pair[0])
		path, _ := backupString(raw, pair[1])
		if kind == "file" && path == "" {
			return backupErr("source_invalid")
		}
	}
	return nil
}
func (b *backupGraphBuilder) objectDependencies(from string, objects []object, seen map[string]bool) error {
	for _, o := range objects {
		if len(seen) >= MaxObjects {
			return backupErr("limit_exceeded")
		}
		if seen[o.key()] {
			return backupErr("duplicate_name")
		}
		seen[o.key()] = true
		plugin, err := backupString(o.raw, "plugin.type")
		if err != nil {
			return err
		}
		if plugin == "static_file" || plugin == "unix_domain_socket" {
			return backupErr("unsupported_dependency")
		}
		for _, path := range []string{"plugin.localPath", "plugin.unixPath"} {
			value, err := backupString(o.raw, path)
			if err != nil {
				return err
			}
			if value != "" {
				return backupErr("unsupported_dependency")
			}
		}
		for _, path := range []string{"plugin.crtPath", "plugin.keyPath"} {
			value, err := backupString(o.raw, path)
			if err != nil {
				return err
			}
			if value == "" {
				continue
			}
			field := o.kind + "[" + o.name + "]." + path
			if _, err := b.reference(from, field, b.resolve(value), "local_file", false); err != nil {
				return err
			}
		}
	}
	return nil
}
