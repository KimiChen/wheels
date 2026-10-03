//go:build linux || darwin

package migrationbundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/agent/configuration"
)

func bundleFixture(t *testing.T) (string, *configuration.MigrationPlan) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0700)
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("private-token-value"), 0600)
	config := filepath.Join(dir, "client.json")
	value := map[string]any{"serverAddr": "127.0.0.1", "serverPort": 7000, "telemetry": map[string]any{"enabled": true, "endpoint": "wss://example.invalid/agent/v1/ws", "tokenFile": token}, "proxies": []any{map[string]any{"name": "private", "type": "stcp", "localPort": 8080, "secretKey": "private-native-value"}}}
	data, _ := json.Marshal(value)
	os.WriteFile(config, data, 0600)
	root := filepath.Join(dir, "managed")
	store := filepath.Join(root, "store.json")
	in, err := configuration.LoadMigrationInput(config, dir, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := configuration.PlanMigration(in, configuration.MigrationRequest{TargetRoot: root, TargetStore: store, Select: []configuration.MigrationSelection{{Kind: "proxy", Name: "private"}}})
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "bundle"), plan
}

func TestBundleRoundTripExclusivePrivateMaterial(t *testing.T) {
	path, plan := bundleFixture(t)
	if err := Write(path, plan); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.Candidate) != string(plan.Candidate) || len(loaded.Sources) != len(plan.Sources) {
		t.Fatal("private material changed")
	}
	for i, source := range loaded.Sources {
		if string(source.Data) != string(plan.Sources[i].Data) {
			t.Fatal("recovery bytes changed")
		}
	}
	if err := Write(path, plan); err == nil {
		t.Fatal("existing bundle overwritten")
	}
	if err := filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, _ := entry.Info()
		want := os.FileMode(0600)
		if entry.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Fatal("private mode lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBundleRejectsTamperLinksModesAndUnexpectedEntries(t *testing.T) {
	for _, mode := range []string{"candidate", "backup", "manifest_unknown", "manifest_duplicate", "symlink", "hardlink", "permissions", "root_permissions", "extra", "missing"} {
		t.Run(mode, func(t *testing.T) {
			path, plan := bundleFixture(t)
			if Write(path, plan) != nil {
				t.Fatal("write")
			}
			candidate := filepath.Join(path, candidateName)
			switch mode {
			case "candidate":
				os.WriteFile(candidate, []byte(`{"proxies":[],"visitors":[]}`), 0600)
			case "backup":
				os.WriteFile(filepath.Join(path, sourceName(0)), []byte("changed"), 0600)
			case "manifest_unknown":
				data, _ := os.ReadFile(filepath.Join(path, manifestName))
				data = append([]byte(`{"unknown":"private-secret",`), data[1:]...)
				os.WriteFile(filepath.Join(path, manifestName), data, 0600)
			case "manifest_duplicate":
				data, _ := os.ReadFile(filepath.Join(path, manifestName))
				data = append([]byte(`{"version":1,`), data[1:]...)
				os.WriteFile(filepath.Join(path, manifestName), data, 0600)
			case "symlink":
				os.Remove(candidate)
				os.Symlink(plan.ConfigFile, candidate)
			case "hardlink":
				os.Link(candidate, filepath.Join(filepath.Dir(path), "linked"))
			case "permissions":
				os.Chmod(candidate, 0644)
			case "root_permissions":
				os.Chmod(path, 0755)
			case "extra":
				os.WriteFile(filepath.Join(path, "unexpected"), nil, 0600)
			case "missing":
				os.Remove(filepath.Join(path, sourceName(0)))
			}
			if _, err := Read(path); err == nil {
				t.Fatal("unsafe bundle accepted")
			} else if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "private-secret") {
				t.Fatal("private diagnostic escaped")
			}
		})
	}
}

func TestBundleRejectsSymlinkAncestorAndReplacedRoot(t *testing.T) {
	path, plan := bundleFixture(t)
	alias := filepath.Join(filepath.Dir(path), "alias")
	if os.Symlink(filepath.Dir(path), alias) != nil {
		t.Fatal("symlink")
	}
	if Write(filepath.Join(alias, "bundle"), plan) == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if Write(path, plan) != nil {
		t.Fatal("write")
	}
	dir, err := openDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.close()
	if os.Rename(path, path+"-moved") != nil || os.Mkdir(path, 0700) != nil {
		t.Fatal("replacement")
	}
	if dir.write("escape", []byte("private")) == nil || dir.sync() == nil {
		t.Fatal("detached root silently accepted")
	}
}

func TestBundleRejectsOversizeAndPartialMaterial(t *testing.T) {
	path, plan := bundleFixture(t)
	plan.Sources[0].Data = make([]byte, configuration.MaxInputBytes+1)
	if Write(path, plan) == nil {
		t.Fatal("oversized material accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid plan created output")
	}
	os.Mkdir(path, 0700)
	os.WriteFile(filepath.Join(path, candidateName), []byte("partial"), 0600)
	if _, err := Read(path); err == nil {
		t.Fatal("partial bundle accepted")
	}
}
