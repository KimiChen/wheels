package configuration

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupPolicyCanonicalKeysAndExplicitEnvironment(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(root, 0700)
	policy := BackupGraphPolicy{ConfigFile: filepath.Join(root, "agent.toml"), WorkingDir: root, StoreFile: filepath.Join(root, "managed", "store.json"), Roots: []BackupRoot{{ID: "installation", Path: root}}, TemplateEnv: map[string]string{"TOKEN": "synthetic-env-secret"}}
	raw, err := EncodeBackupPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("synthetic-env-secret")) {
		t.Fatal("environment serialized")
	}
	for _, text := range []string{strings.Replace(string(raw), `"roots":`, `"Roots":`, 1), strings.Replace(string(raw), `"path":`, `"Path":`, 1), strings.Replace(string(raw), `"roots":`, `"roots":[],"roots":`, 1), strings.Replace(string(raw), `"version":1`, `"version":1,"unknown":true`, 1)} {
		if _, err = DecodeBackupPolicy([]byte(text), nil); err == nil {
			t.Fatal("ambiguous policy accepted")
		}
	}
	first, err := DecodeBackupPolicy(raw, policy.TemplateEnv)
	if err != nil {
		t.Fatal(err)
	}
	if first.TemplateEnv["TOKEN"] != "synthetic-env-secret" {
		t.Fatal("explicit env missing")
	}
	path := filepath.Join(root, "env.json")
	os.WriteFile(path, []byte(`{"TOKEN":"synthetic-env-secret"}`), 0600)
	if values, err := LoadBackupEnvironment(path); err != nil || values["TOKEN"] != "synthetic-env-secret" {
		t.Fatal("explicit env", err)
	}
	for _, text := range []string{`{"TOKEN":"a","TOKEN":"b"}`, `{"TOKEN":123}`, `null`, `{"BAD.NAME":"a"}`} {
		os.WriteFile(path, []byte(text), 0600)
		if _, err = LoadBackupEnvironment(path); err == nil {
			t.Fatal("invalid env accepted")
		}
	}
	os.Chmod(path, 0644)
	if _, err = LoadBackupEnvironment(path); err == nil {
		t.Fatal("public env file accepted")
	}
}
func TestBackupPolicyCanonicalizesRootOrder(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := BackupGraphPolicy{ConfigFile: filepath.Join(root, "one", "agent.toml"), WorkingDir: filepath.Join(root, "one"), StoreFile: filepath.Join(root, "one", "managed", "store.json"), Roots: []BackupRoot{{ID: "z-root", Path: filepath.Join(root, "two")}, {ID: "a-root", Path: filepath.Join(root, "one")}}}
	a, err := EncodeBackupPolicy(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Roots[0], base.Roots[1] = base.Roots[1], base.Roots[0]
	b, err := EncodeBackupPolicy(base)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatal("policy order changes identity", err)
	}
}
