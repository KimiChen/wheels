package serverbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/agent/configuration"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

var pathFields = []string{"auth.tokenSource.file.path", "transport.tls.certFile", "transport.tls.keyFile", "transport.tls.trustedCaFile", "webServer.tls.certFile", "webServer.tls.keyFile", "webServer.tls.trustedCaFile", "custom404Page", "sshTunnelGateway.privateKeyFile", "sshTunnelGateway.autoGenPrivateKeyPath", "sshTunnelGateway.authorizedKeysFile", "monitor.certFile", "monitor.keyFile", "monitor.githubClientSecretFile", "monitor.databaseFile", "monitor.historyDataPath", "log.to"}

func uniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return failure("config_invalid")
		}
		v, e := d.Token()
		if e != nil {
			return e
		}
		switch v {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				s, ok := k.(string)
				if e != nil || !ok || seen[strings.ToLower(s)] {
					return failure("config_invalid")
				}
				seen[strings.ToLower(s)] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		case json.Delim('['):
			for d.More() {
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		}
		return nil
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return failure("config_invalid")
	}
	return nil
}
func yamlStrict(data []byte) error {
	var root yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(data))
	if d.Decode(&root) != nil || d.Decode(new(any)) != io.EOF {
		return failure("config_invalid")
	}
	var walk func(*yaml.Node, int) error
	walk = func(n *yaml.Node, depth int) error {
		if depth > 64 || n.Alias != nil || n.Anchor != "" {
			return failure("config_invalid")
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || strings.HasPrefix(k.Value, ".") || seen[strings.ToLower(k.Value)] {
					return failure("config_invalid")
				}
				seen[strings.ToLower(k.Value)] = true
			}
		}
		for _, c := range n.Content {
			if e := walk(c, depth+1); e != nil {
				return e
			}
		}
		return nil
	}
	return walk(&root, 0)
}
func decodeRaw(path string, data []byte) (map[string]any, error) {
	if len(data) > MaxFileBytes {
		return nil, failure("config_invalid")
	}
	var raw map[string]any
	var e error
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		e = uniqueJSON(data)
		if e == nil {
			d := json.NewDecoder(bytes.NewReader(data))
			d.UseNumber()
			e = d.Decode(&raw)
		}
	case ".toml":
		e = toml.Unmarshal(data, &raw)
	case ".yaml", ".yml":
		e = yamlStrict(data)
		if e == nil {
			e = yaml.Unmarshal(data, &raw)
		}
	default:
		return nil, failure("unsupported_format")
	}
	if e != nil || raw == nil {
		return nil, failure("config_invalid")
	}
	b, e := json.Marshal(raw)
	if e != nil {
		return nil, failure("config_invalid")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&raw) != nil {
		return nil, failure("config_invalid")
	}
	return raw, nil
}
func decodeConfig(ctx context.Context, p Policy, data []byte, env map[string]string) (map[string]any, *v1.ServerConfig, []TemplateRequirement, error) {
	rendered, names, e := configuration.RenderBackupTemplate(ctx, data, env)
	if e != nil {
		return nil, nil, nil, failure("template_invalid")
	}
	raw, e := decodeRaw(p.ConfigFile, rendered)
	if e != nil {
		return nil, nil, nil, e
	}
	b, e := json.Marshal(raw)
	var cfg v1.ServerConfig
	if e != nil || canonical(b, &cfg) != nil {
		return nil, nil, nil, failure("config_invalid")
	}
	if cfg.Auth.TokenSource != nil && cfg.Auth.TokenSource.Type != "file" {
		return nil, nil, nil, failure("unsupported_dependency")
	}
	if cfg.WebServer.AssetsDir != "" {
		return nil, nil, nil, failure("unsupported_dependency")
	}
	if cfg.Complete() != nil {
		return nil, nil, nil, failure("config_invalid")
	}
	if _, e = validation.NewConfigValidator(nil).ValidateServerConfig(&cfg); e != nil {
		return nil, nil, nil, failure("config_invalid")
	}
	if !cfg.Monitor.Enabled {
		return nil, nil, nil, failure("monitor_required")
	}
	requirements := []TemplateRequirement{}
	if bytes.Contains(data, []byte("{{")) {
		requirements = append(requirements, TemplateRequirement{p.ConfigFile, append([]string{}, names...), hash(rendered)})
	}
	return raw, &cfg, requirements, nil
}
func field(raw map[string]any, path string) (map[string]any, string, bool) {
	parts := strings.Split(path, ".")
	m := raw
	for _, part := range parts[:len(parts)-1] {
		next, ok := m[part].(map[string]any)
		if !ok {
			return nil, "", false
		}
		m = next
	}
	key := parts[len(parts)-1]
	_, ok := m[key]
	return m, key, ok
}
func changePaths(raw map[string]any, cwd string, change func(string, bool) (string, error)) error {
	for _, path := range pathFields {
		m, k, ok := field(raw, path)
		if !ok {
			continue
		}
		s, ok := m[k].(string)
		if !ok {
			return failure("config_invalid")
		}
		if s == "" || path == "log.to" && s == "console" {
			continue
		}
		v, e := change(resolve(s, cwd), path == "monitor.historyDataPath")
		if e != nil {
			return e
		}
		m[k] = v
	}
	return nil
}
func reserved(path, database string) bool {
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".frp-server-restore-") {
			return true
		}
	}
	return database != "" && (path == database+"-wal" || path == database+"-shm" || path == database+"-journal" || path == database+".restore-gate.json" || strings.HasPrefix(path, database+".restore-completed-"))
}
func dependencies(p Policy, c *v1.ServerConfig) ([]string, string, string, string, error) {
	values := []string{c.Transport.TLS.CertFile, c.Transport.TLS.KeyFile, c.Transport.TLS.TrustedCaFile, c.Custom404Page, c.Monitor.CertFile, c.Monitor.KeyFile, c.Monitor.GitHubClientSecretFile}
	if c.Auth.TokenSource != nil {
		values = append(values, c.Auth.TokenSource.File.Path)
	}
	if c.WebServer.TLS != nil {
		values = append(values, c.WebServer.TLS.CertFile, c.WebServer.TLS.KeyFile, c.WebServer.TLS.TrustedCaFile)
	}
	// The generated SSH key is identity material. Never regenerate it silently.
	if c.SSHTunnelGateway.PrivateKeyFile != "" {
		values = append(values, c.SSHTunnelGateway.PrivateKeyFile)
	} else if c.SSHTunnelGateway.BindPort > 0 {
		values = append(values, c.SSHTunnelGateway.AutoGenPrivateKeyPath)
	}
	values = append(values, c.SSHTunnelGateway.AuthorizedKeysFile)
	seen := map[string]bool{p.ConfigFile: true}
	for _, v := range values {
		if v != "" {
			v = resolve(v, p.WorkingDir)
			if !p.authorized(v, false) {
				return nil, "", "", "", failure("path_unauthorized")
			}
			seen[v] = true
		}
	}
	db := resolve(c.Monitor.DatabaseFile, p.WorkingDir)
	history, log := "", ""
	if c.Monitor.HistoryDataPath != "" {
		history = resolve(c.Monitor.HistoryDataPath, p.WorkingDir)
	}
	if c.Log.To != "console" {
		log = resolve(c.Log.To, p.WorkingDir)
	}
	if !p.authorized(db, false) || history != "" && !p.authorized(history, true) || log != "" && !p.authorized(log, false) {
		return nil, "", "", "", failure("path_unauthorized")
	}
	for file := range seen {
		if reserved(file, db) {
			return nil, "", "", "", failure("dependency_conflict")
		}
	}
	if reserved(db, "") || reserved(history, "") || reserved(log, "") || reserved(log, db) {
		return nil, "", "", "", failure("dependency_conflict")
	}
	dataPaths := []string{db, history, log, db + ".restore-gate.json"}
	for i, a := range dataPaths {
		if a == "" {
			continue
		}
		for _, b := range dataPaths[:i] {
			if b != "" && (within(a, b) || within(b, a)) {
				return nil, "", "", "", failure("dependency_conflict")
			}
		}
		for file := range seen {
			if within(file, a) || within(a, file) || file == db+"-wal" || file == db+"-shm" || file == db+"-journal" {
				return nil, "", "", "", failure("dependency_conflict")
			}
		}
	}
	out := []string{}
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, db, history, log, nil
}
