package configuration

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
)

// Policy is supplied locally by the administrator; an archive cannot grant
// itself permission to read or replace a file. Values are never environment data.
type BackupPolicyPath struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}
type BackupPolicy struct {
	Version    int                `json:"version"`
	ConfigFile string             `json:"config_file"`
	WorkingDir string             `json:"working_dir"`
	StoreFile  string             `json:"store_file"`
	Roots      []BackupPolicyPath `json:"roots"`
	Files      []BackupPolicyPath `json:"files"`
}

func DecodeBackupPolicy(data []byte, env map[string]string) (BackupGraphPolicy, error) {
	var v BackupPolicy
	if len(data) > 1<<20 || strictJSON(data) != nil {
		return BackupGraphPolicy{}, backupErr("policy_invalid")
	}
	if err := canonicalBackupPolicy(data); err != nil {
		return BackupGraphPolicy{}, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || v.Version != 1 || v.Roots == nil || v.Files == nil {
		return BackupGraphPolicy{}, backupErr("policy_invalid")
	}
	p := BackupGraphPolicy{ConfigFile: v.ConfigFile, WorkingDir: v.WorkingDir, StoreFile: v.StoreFile, TemplateEnv: env}
	for _, r := range v.Roots {
		p.Roots = append(p.Roots, BackupRoot{ID: r.ID, Path: r.Path})
	}
	for _, f := range v.Files {
		p.Mappings = append(p.Mappings, BackupMapping{ID: f.ID, Path: f.Path})
	}
	p, _, err := normalizeBackupPolicy(p)
	if err != nil || p.ConfigFile != filepath.Join(p.WorkingDir, "agent.toml") || p.StoreFile != filepath.Join(p.WorkingDir, "managed", "store.json") {
		return p, backupErr("policy_invalid")
	}
	return p, nil
}
func EncodeBackupPolicy(p BackupGraphPolicy) ([]byte, error) {
	var err error
	p, _, err = normalizeBackupPolicy(p)
	if err != nil {
		return nil, err
	}
	v := BackupPolicy{Version: 1, ConfigFile: p.ConfigFile, WorkingDir: p.WorkingDir, StoreFile: p.StoreFile, Roots: []BackupPolicyPath{}, Files: []BackupPolicyPath{}}
	for _, r := range p.Roots {
		v.Roots = append(v.Roots, BackupPolicyPath{r.ID, r.Path})
	}
	for _, f := range p.Mappings {
		v.Files = append(v.Files, BackupPolicyPath{f.ID, f.Path})
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, backupErr("policy_invalid")
	}
	_, err = DecodeBackupPolicy(data, p.TemplateEnv)
	return data, err
}
func LoadBackupPolicy(path string, env map[string]string) (BackupGraphPolicy, error) {
	f, err := maintenancePrivate(path)
	if err != nil {
		return BackupGraphPolicy{}, backupErr("policy_invalid")
	}
	return DecodeBackupPolicy(f.Bytes, env)
}

func canonicalBackupPolicy(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := d.Token()
		if err != nil {
			return backupErr("policy_invalid")
		}
		switch token {
		case json.Delim('{'):
			for d.More() {
				token, err := d.Token()
				key, ok := token.(string)
				if err != nil || !ok {
					return backupErr("policy_invalid")
				}
				switch key {
				case "version", "config_file", "working_dir", "store_file", "roots", "files", "id", "path":
				default:
					return backupErr("policy_invalid")
				}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case json.Delim('['):
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return backupErr("policy_invalid")
	}
	return nil
}

// LoadBackupEnvironment reads only an explicit local private map. Neither the
// map nor its filename is copied to a checkpoint or public summary.
func LoadBackupEnvironment(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	f, err := maintenancePrivate(path)
	if err != nil {
		return nil, backupErr("environment_invalid")
	}
	if len(f.Bytes) > 1<<20 || strictJSON(f.Bytes) != nil {
		return nil, backupErr("environment_invalid")
	}
	var env map[string]string
	if json.Unmarshal(f.Bytes, &env) != nil || env == nil || len(env) > 256 {
		return nil, backupErr("environment_invalid")
	}
	name := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	for k, v := range env {
		if !name.MatchString(k) || len(v) > 65536 {
			return nil, backupErr("environment_invalid")
		}
	}
	return env, nil
}
