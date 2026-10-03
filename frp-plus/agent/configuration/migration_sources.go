package configuration

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	nativeconfig "github.com/fatedier/frp/pkg/config"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"gopkg.in/yaml.v3"
)

// MigrationSource is private recovery metadata. Data is written separately by
// migrationbundle; none of this structure belongs in telemetry or stdout.
type MigrationSource struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
	Exists bool   `json:"exists"`
	Data   []byte `json:"-"`
}

type migrationFile struct {
	source  MigrationSource
	common  *v1.ClientCommonConfig
	objects []object
}

func migrationStrict(data []byte, format string) error {
	if bytes.Contains(data, []byte("{{")) {
		return failure("migration_template_unsupported")
	}
	if nativeconfig.DetectLegacyINIFormat(data) {
		return failure("migration_legacy_unsupported")
	}
	if format == "json" || strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return strictJSON(data)
	}
	if format != "yaml" && format != "yml" {
		return nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if decoder.Decode(&root) != nil || decoder.Decode(new(yaml.Node)) != io.EOF {
		return failure("invalid_source")
	}
	var walk func(*yaml.Node, int) error
	walk = func(node *yaml.Node, depth int) error {
		if depth > 32 || node.Kind == yaml.AliasNode || node.Anchor != "" {
			return failure("migration_yaml_unsupported")
		}
		if node.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(node.Content); i += 2 {
				key := node.Content[i]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || strings.HasPrefix(key.Value, ".") || seen[key.Value] {
					return failure("migration_yaml_unsupported")
				}
				seen[key.Value] = true
			}
		}
		for _, child := range node.Content {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(&root, 0)
}

func parseMigrationFile(path, kind string) (migrationFile, error) {
	data, info, err := regularNoLinks(path)
	if err != nil {
		return migrationFile{}, failure("migration_source_unavailable")
	}
	file, err := decodeMigrationFile(path, kind, data)
	if err != nil {
		return migrationFile{}, err
	}
	file.source.Mode = uint32(info.Mode().Perm())
	return file, nil
}

func decodeMigrationFile(path, kind string, data []byte) (migrationFile, error) {
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if format != "json" && format != "toml" && format != "yaml" && format != "yml" {
		return migrationFile{}, failure("unsupported_source")
	}
	if err := migrationStrict(data, format); err != nil {
		return migrationFile{}, err
	}
	if format == "yml" {
		format = "yaml"
	}
	var cfg v1.ClientConfig
	// Native strict decoding happens before typed serialization: unknown members
	// must never disappear into a seemingly successful migration candidate.
	if nativeconfig.LoadConfigure(data, &cfg, true, format) != nil {
		return migrationFile{}, failure("unsupported_source")
	}
	values := Objects{}
	for _, p := range cfg.Proxies {
		values.Proxies = append(values.Proxies, p.ProxyConfigurer)
	}
	for _, v := range cfg.Visitors {
		values.Visitors = append(values.Visitors, v.VisitorConfigurer)
	}
	objects, err := fromNative(values, kind)
	if err != nil {
		return migrationFile{}, err
	}
	return migrationFile{source: MigrationSource{Path: path, Kind: kind, SHA256: hash(data), Exists: true, Data: data}, common: &cfg.ClientCommonConfig, objects: objects}, nil
}

func migrationFiles(configFile, cwd string) ([]migrationFile, error) {
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Join(cwd, path)
	}
	main, err := parseMigrationFile(resolve(configFile), "file")
	if err != nil {
		return nil, err
	}
	files := []migrationFile{main}
	seen := map[string]bool{main.source.Path: true}
	total := len(main.source.Data)
	for _, pattern := range main.common.IncludeConfigFiles {
		pattern = resolve(pattern)
		dir := filepath.Dir(pattern)
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil || resolved != dir {
			return nil, failure("unsupported_dependency")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, failure("unsupported_dependency")
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			match, err := filepath.Match(filepath.Base(pattern), entry.Name())
			if err != nil {
				return nil, failure("unsupported_dependency")
			}
			if !match {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if seen[path] {
				return nil, failure("duplicate_name")
			}
			seen[path] = true
			file, err := parseMigrationFile(path, "include")
			if err != nil {
				return nil, err
			}
			total += len(file.source.Data)
			if total > MaxInputBytes || len(files) >= MaxDependencies {
				return nil, failure("limit_exceeded")
			}
			files = append(files, file)
		}
	}
	return files, nil
}

// LoadMigrationInput loads only bounded regular native inputs, with templates
// and legacy syntax rejected before any template evaluation. It does not start
// FRP, open a managed Engine, execute token sources, or create an absent Store.
func LoadMigrationInput(configFile, cwd, emptyStorePath string, unsafe []string) (Input, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return Input{}, failure("migration_source_unavailable")
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return Input{}, failure("migration_source_unavailable")
	}
	files, err := migrationFiles(configFile, cwd)
	if err != nil {
		return Input{}, err
	}
	common, err := commonCopy(files[0].common)
	if err != nil || common.Complete() != nil {
		return Input{}, failure("invalid_source")
	}
	input := Input{ConfigFile: files[0].source.Path, WorkingDir: cwd, StartupCommon: common, ReloadCommon: common, TemplateEnv: map[string]string{}, HTTPProxy: os.Getenv("http_proxy"), UnsafeFeatures: append([]string{}, unsafe...)}
	for _, file := range files {
		items := native(file.objects)
		input.FileMemory.Proxies = append(input.FileMemory.Proxies, items.Proxies...)
		input.FileMemory.Visitors = append(input.FileMemory.Visitors, items.Visitors...)
	}
	input.StoreFile = common.Store.Path
	if input.StoreFile == "" {
		input.StoreFile = emptyStorePath
	}
	if input.StoreFile == "" {
		return Input{}, failure("store_unavailable")
	}
	if !filepath.IsAbs(input.StoreFile) {
		// Native runClientWithAggregator resolves Store relative to the main
		// configuration directory; include/credential paths use process CWD.
		input.StoreFile = filepath.Join(filepath.Dir(input.ConfigFile), input.StoreFile)
	}
	data, _, err := regularNoLinks(input.StoreFile)
	if err != nil && !os.IsNotExist(err) {
		return Input{}, failure("migration_source_unavailable")
	}
	if err == nil {
		if common.Store.Path == "" {
			return Input{}, failure("migration_target_exists")
		}
		input.StoreMemory, err = DecodeStore(data)
		if err != nil {
			return Input{}, err
		}
	}
	return input, nil
}

func migrationObjectHash(o object) string {
	data, _ := json.Marshal(o.raw)
	return hash(data)
}
