package configuration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type migrationFixture struct {
	dir, file, include, root, store string
	main                            map[string]any
}

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f := &migrationFixture{dir: dir, file: filepath.Join(dir, "frpc.json"), include: filepath.Join(dir, "include.json"), root: filepath.Join(dir, "managed")}
	f.store = filepath.Join(f.root, "store.json")
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("private-telemetry-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	f.main = map[string]any{"serverAddr": "127.0.0.1", "serverPort": 7000, "includes": []string{f.include},
		"telemetry": map[string]any{"enabled": true, "endpoint": "wss://monitor.invalid/agent/v1/ws", "tokenFile": filepath.Join(dir, "token")},
		"proxies": []any{
			map[string]any{"name": "web", "type": "http", "localPort": 8080, "customDomains": []string{"example.invalid"}, "httpPassword": "private-http-secret", "requestHeaders": map[string]any{"set": map[string]string{"Authorization": "private-header-secret"}}, "metadatas": map[string]string{"private": "retained"}, "loadBalancer": map[string]string{"group": "group", "groupKey": "private-group-secret"}},
			map[string]any{"name": "disabled", "type": "tcp", "localPort": 8081, "remotePort": 0, "enabled": false},
			map[string]any{"name": "peer", "type": "stcp", "localPort": 8082, "secretKey": "private-stcp-secret"},
		}}
	f.write(t)
	writeMigrationJSON(t, f.include, map[string]any{"visitors": []any{map[string]any{"name": "visit", "type": "stcp", "serverName": "peer", "bindPort": -1, "secretKey": "private-stcp-secret"}}})
	return f
}
func writeMigrationJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *migrationFixture) write(t *testing.T) { writeMigrationJSON(t, f.file, f.main) }
func (f *migrationFixture) input(t *testing.T) Input {
	t.Helper()
	in, err := LoadMigrationInput(f.file, f.dir, f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	return in
}
func (f *migrationFixture) plan(t *testing.T, selections ...MigrationSelection) *MigrationPlan {
	t.Helper()
	plan, err := PlanMigration(f.input(t), MigrationRequest{TargetRoot: f.root, TargetStore: f.store, Select: selections})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
func (f *migrationFixture) install(t *testing.T, plan *MigrationPlan) {
	t.Helper()
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.store, plan.Candidate, 0600); err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{}
	for _, object := range plan.Remove {
		selected[object.Kind+"/"+object.Name] = true
	}
	for _, path := range []string{f.file, f.include} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if json.Unmarshal(data, &value) != nil {
			t.Fatal("fixture")
		}
		for _, pair := range [][2]string{{"proxies", "proxy"}, {"visitors", "visitor"}} {
			if list, ok := value[pair[0]].([]any); ok {
				kept := []any{}
				for _, raw := range list {
					o := raw.(map[string]any)
					if !selected[pair[1]+"/"+o["name"].(string)] {
						kept = append(kept, o)
					}
				}
				value[pair[0]] = kept
			}
		}
		if path == f.file {
			value["store"] = map[string]string{"path": f.store}
			value["telemetry"].(map[string]any)["configManagement"] = map[string]any{"enabled": true, "root": f.root}
			f.main = value
		}
		writeMigrationJSON(t, path, value)
	}
}

func TestMigrationSubsetDisabledIncludeAndPrivateNativeFields(t *testing.T) {
	f := newMigrationFixture(t)
	before, _ := os.ReadFile(f.file)
	plan := f.plan(t, MigrationSelection{"proxy", "web"}, MigrationSelection{"proxy", "disabled"}, MigrationSelection{"visitor", "visit"})
	if len(plan.Remove) != 3 || len(plan.Files) != 2 || ValidateMigrationPlan(plan) != nil {
		t.Fatal("incomplete migration material")
	}
	current, _ := os.ReadFile(f.file)
	if string(current) != string(before) {
		t.Fatal("plan wrote source")
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Fatal("plan created target")
	}
	for _, secret := range []string{"private-http-secret", "private-header-secret", "private-group-secret", "private-stcp-secret"} {
		if !strings.Contains(string(plan.Candidate), secret) {
			t.Fatal("private native field was lost")
		}
	}
	objects, err := parseStore(plan.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range objects {
		if o.name == "disabled" {
			found = true
			if o.proxy.GetBaseConfig().Enabled == nil || *o.proxy.GetBaseConfig().Enabled {
				t.Fatal("disabled state lost")
			}
		}
	}
	if !found {
		t.Fatal("disabled object lost")
	}
	for _, source := range plan.Sources {
		if source.Exists && hash(source.Data) != source.SHA256 {
			t.Fatal("recovery bytes mismatch")
		}
	}
	f.install(t, plan)
	if err := CheckMigration(plan, f.input(t), false); code(err) != "migration_offline_required" {
		t.Fatal("offline acknowledgement not required")
	}
	if err := CheckMigration(plan, f.input(t), true); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationKeepsExistingStoreAndDoesNotMoveIt(t *testing.T) {
	f := newMigrationFixture(t)
	old := filepath.Join(f.dir, "old-store.json")
	data := []byte(`{"proxies":[{"name":"existing","type":"tcp","localPort":9090,"remotePort":0}],"visitors":[]}`)
	if err := os.WriteFile(old, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.main["store"] = map[string]string{"path": old}
	f.write(t)
	plan := f.plan(t, MigrationSelection{"proxy", "disabled"})
	items, _ := parseStore(plan.Candidate)
	if len(items) != 2 {
		t.Fatal("old Store object lost")
	}
	f.install(t, plan)
	if err := CheckMigration(plan, f.input(t), true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(old)
	if string(after) != string(data) {
		t.Fatal("migration moved or rewrote old Store")
	}
}

func TestMigrationRejectsUnknownTemplatePluginDuplicateAndManagedInputs(t *testing.T) {
	cases := map[string]func(*testing.T, *migrationFixture){
		"unknown": func(t *testing.T, f *migrationFixture) { f.main["unknownPrivateOption"] = "private-value"; f.write(t) },
		"template": func(t *testing.T, f *migrationFixture) {
			data, _ := os.ReadFile(f.file)
			data = append(data, []byte("\n{{ .Envs.PRIVATE }}")...)
			os.WriteFile(f.file, data, 0600)
		},
		"plugin": func(t *testing.T, f *migrationFixture) {
			p := f.main["proxies"].([]any)[1].(map[string]any)
			p["plugin"] = map[string]any{"type": "http_proxy"}
			f.write(t)
		},
		"duplicate": func(t *testing.T, f *migrationFixture) {
			p := f.main["proxies"].([]any)
			f.main["proxies"] = append(p, p[1])
			f.write(t)
		},
		"managed": func(t *testing.T, f *migrationFixture) {
			f.main["telemetry"].(map[string]any)["configManagement"] = map[string]any{"enabled": true, "root": f.root}
			f.write(t)
		},
		"yaml_ignored": func(t *testing.T, f *migrationFixture) {
			f.file = filepath.Join(f.dir, "bad.yaml")
			os.WriteFile(f.file, []byte(".ignored: private-value\nserverPort: 7000\n"), 0600)
		},
		"yaml_disguised_as_json": func(t *testing.T, f *migrationFixture) {
			os.WriteFile(f.file, []byte(".ignored: private-value\nserverPort: 7000\n"), 0600)
		},
		"unknown_nested": func(t *testing.T, f *migrationFixture) {
			f.main["proxies"].([]any)[1].(map[string]any)["unknownPrivateOption"] = "private-value"
			f.write(t)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMigrationFixture(t)
			mutate(t, f)
			in, err := LoadMigrationInput(f.file, f.dir, f.store, nil)
			if err == nil {
				_, err = PlanMigration(in, MigrationRequest{TargetRoot: f.root, TargetStore: f.store, Select: []MigrationSelection{{"proxy", "disabled"}}})
			}
			if err == nil {
				t.Fatal("unsupported source accepted")
			}
			if strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), f.dir) {
				t.Fatal("private diagnostics escaped")
			}
		})
	}
}

func TestMigrationCheckRejectsIncompleteRemovalDriftAndOtherCommonChanges(t *testing.T) {
	for _, mode := range []string{"not_removed", "store_drift", "disabled_change", "common_change", "dependency_change", "include_common_change", "permissions"} {
		t.Run(mode, func(t *testing.T) {
			f := newMigrationFixture(t)
			plan := f.plan(t, MigrationSelection{"proxy", "disabled"})
			f.install(t, plan)
			switch mode {
			case "not_removed":
				f.main["proxies"] = append(f.main["proxies"].([]any), map[string]any{"name": "disabled", "type": "tcp", "localPort": 8081, "remotePort": 0, "enabled": false})
				f.write(t)
			case "store_drift":
				os.WriteFile(f.store, append(plan.Candidate, ' '), 0600)
			case "disabled_change":
				data := strings.Replace(string(plan.Candidate), `"enabled": false`, `"enabled": true`, 1)
				os.WriteFile(f.store, []byte(data), 0600)
			case "common_change":
				f.main["serverPort"] = 7001
				f.write(t)
			case "dependency_change":
				os.WriteFile(filepath.Join(f.dir, "token"), []byte("changed"), 0600)
			case "include_common_change":
				data, _ := os.ReadFile(f.include)
				var value map[string]any
				json.Unmarshal(data, &value)
				value["serverPort"] = 7001
				writeMigrationJSON(t, f.include, value)
			case "permissions":
				os.Chmod(f.store, 0644)
			}
			in, err := LoadMigrationInput(f.file, f.dir, f.store, nil)
			if err == nil {
				err = CheckMigration(plan, in, true)
			}
			if err == nil {
				t.Fatal("invalid cutover verified")
			}
		})
	}
}

func TestMigrationRejectsStaleInputUnsafeTargetAndMixedRecoveryMaterials(t *testing.T) {
	for _, mode := range []string{"stale", "wrong_revision", "unsafe_target", "recovery_material", "selection_duplicate", "store_owner"} {
		t.Run(mode, func(t *testing.T) {
			f := newMigrationFixture(t)
			in := f.input(t)
			request := MigrationRequest{TargetRoot: f.root, TargetStore: f.store, Select: []MigrationSelection{{"proxy", "disabled"}}}
			switch mode {
			case "stale":
				f.main["serverPort"] = 7001
				f.write(t)
			case "wrong_revision":
				request.ExpectedRevision = strings.Repeat("a", 64)
			case "unsafe_target":
				request.TargetStore = filepath.Join(f.dir, "escape.json")
			case "recovery_material":
				os.Mkdir(f.root, 0700)
				os.WriteFile(filepath.Join(f.root, "identity.json"), []byte("private-old-identity"), 0600)
			case "selection_duplicate":
				request.Select = append(request.Select, request.Select[0])
			case "store_owner":
				request.Select = []MigrationSelection{{"proxy", "absent"}}
			}
			if _, err := PlanMigration(in, request); err == nil {
				t.Fatal("unsafe migration accepted")
			}
		})
	}
}

func TestMigrationRelativeStoreUsesNativeConfigDirectory(t *testing.T) {
	f := newMigrationFixture(t)
	configDir := filepath.Join(f.dir, "config")
	if os.Mkdir(configDir, 0700) != nil {
		t.Fatal("directory")
	}
	f.file = filepath.Join(configDir, "frpc.json")
	f.main["store"] = map[string]string{"path": "old-store.json"}
	f.write(t)
	realStore := filepath.Join(configDir, "old-store.json")
	os.WriteFile(realStore, []byte(`{"proxies":[{"name":"native-store","type":"tcp","localPort":9000}],"visitors":[]}`), 0600)
	os.WriteFile(filepath.Join(f.dir, "old-store.json"), []byte(`{"proxies":[{"name":"wrong-cwd-store","type":"tcp","localPort":9001}],"visitors":[]}`), 0600)
	in := f.input(t)
	if in.StoreFile != realStore {
		t.Fatal("relative Store did not match native resolution")
	}
	plan := f.plan(t, MigrationSelection{"proxy", "disabled"})
	if !strings.Contains(string(plan.Candidate), "native-store") || strings.Contains(string(plan.Candidate), "wrong-cwd-store") {
		t.Fatal("migration read a different Store from native FRP")
	}
	for _, source := range plan.Sources {
		if source.Kind == "store" && source.Path != realStore {
			t.Fatal("recovery Store path differs")
		}
	}
}

func TestMigrationRecoveryRejectsMissingDependencyAndWrongStoreBinding(t *testing.T) {
	for _, mode := range []string{"missing_dependency", "wrong_store", "missing_include", "extra_include"} {
		t.Run(mode, func(t *testing.T) {
			f := newMigrationFixture(t)
			plan := f.plan(t, MigrationSelection{"proxy", "disabled"})
			switch mode {
			case "missing_dependency":
				for i, s := range plan.Sources {
					if s.Kind == "local_file" {
						plan.Sources = append(plan.Sources[:i], plan.Sources[i+1:]...)
						for j := range plan.Files {
							if plan.Files[j].Source > i {
								plan.Files[j].Source--
							}
						}
						for j := range plan.Remove {
							if plan.Remove[j].Source > i {
								plan.Remove[j].Source--
							}
						}
						break
					}
				}
			case "wrong_store":
				for i, s := range plan.Sources {
					if s.Kind == "store" {
						plan.Sources[i].Path = filepath.Join(f.dir, "wrong-original-store.json")
					}
				}
			case "missing_include":
				plan.Includes = nil
			case "extra_include":
				plan.Includes[0].Files = append(plan.Includes[0].Files, filepath.Join(f.dir, "unknown.json"))
			}
			if ValidateMigrationPlan(plan) == nil {
				t.Fatal("incomplete recovery material accepted")
			}
		})
	}
}

func TestMigrationTargetMatchesManagedFilenameBoundary(t *testing.T) {
	f := newMigrationFixture(t)
	for _, name := range []string{"store.config.json", "space name.json", "Operations", "SECRETS", "Identity.json", "restore.json", "Restore.json", ".lock", strings.Repeat("x", 129) + ".json"} {
		if migrationTarget(f.root, filepath.Join(f.root, name)) == nil {
			t.Fatalf("invalid managed Store name accepted: %q", name)
		}
	}
	for _, name := range []string{"store.json", "native-store_2.json", "store"} {
		if migrationTarget(f.root, filepath.Join(f.root, name)) != nil {
			t.Fatal("valid managed Store name rejected")
		}
	}
}
