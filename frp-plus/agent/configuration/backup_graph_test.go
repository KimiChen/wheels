//go:build linux || darwin

package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	nativeconfig "github.com/fatedier/frp/pkg/config"
)

type backupFixture struct {
	policy                            BackupGraphPolicy
	root, main, include, token, store string
	config                            map[string]any
}

func backupWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f backupFixture) writeMain(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	backupWrite(t, f.main, data)
}
func newBackupFixture(t *testing.T) backupFixture {
	t.Helper()
	t.Setenv("http_proxy", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	f := backupFixture{root: root, main: filepath.Join(root, "config", "agent.json"), include: filepath.Join(root, "runtime", "includes", "a.json"), token: filepath.Join(root, "runtime", "token"), store: filepath.Join(root, "config", "store.json")}
	f.config = map[string]any{"serverAddr": "127.0.0.1", "serverPort": 7000, "includes": []string{"includes/*.json"}, "store": map[string]any{"path": "store.json"}, "auth": map[string]any{"method": "token", "tokenSource": map[string]any{"type": "file", "file": map[string]any{"path": "token"}}}, "transport": map[string]any{"tls": map[string]any{"certFile": "cert.pem", "keyFile": "key.pem", "trustedCaFile": "ca.pem"}}, "proxies": []any{map[string]any{"name": "from-file", "type": "tcp", "localIP": "127.0.0.1", "localPort": 8080, "remotePort": 0}}}
	f.writeMain(t)
	backupWrite(t, f.include, []byte(`{"includes":["/must-not-be-read/*.json"],"proxies":[{"name":"from-include","type":"tcp","localPort":8081,"remotePort":0,"enabled":false}]}`))
	backupWrite(t, f.store, []byte(`{"proxies":[{"name":"from-store","type":"udp","localPort":8082,"remotePort":0}],"visitors":[]}`))
	backupWrite(t, filepath.Join(root, "runtime", "store.json"), []byte("wrong-cwd-store-decoy"))
	backupWrite(t, f.token, []byte("private-token-canary"))
	for _, name := range []string{"cert.pem", "key.pem", "ca.pem"} {
		backupWrite(t, filepath.Join(root, "runtime", name), []byte("private-material-"+name))
	}
	f.policy = BackupGraphPolicy{ConfigFile: f.main, WorkingDir: filepath.Join(root, "runtime"), StoreFile: f.store, Roots: []BackupRoot{{ID: "installation", Path: root}}}
	return f
}
func mustBackupGraph(t *testing.T, f backupFixture) *BackupGraph {
	t.Helper()
	graph, err := CaptureBackupGraph(context.Background(), f.policy)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}
func backupClone(t *testing.T, graph *BackupGraph) *BackupGraph {
	t.Helper()
	copy := *graph
	data, err := json.Marshal(graph.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(data, &copy.Manifest) != nil {
		t.Fatal("clone")
	}
	copy.Files = nil
	for _, file := range graph.Files {
		copy.Files = append(copy.Files, BackupGraphFile{ID: file.ID, Bytes: append([]byte(nil), file.Bytes...)})
	}
	return &copy
}
func backupReseal(t *testing.T, graph *BackupGraph) {
	t.Helper()
	data, err := backupmanifest.Encode(graph.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	graph.ManifestBytes, graph.ManifestDigest = data, backupmanifest.Digest(data)
}
func backupAssertRejected(t *testing.T, f backupFixture, graph *BackupGraph) {
	t.Helper()
	if _, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err == nil {
		t.Fatal("accepted invalid graph")
	}
}

func TestBackupGraphNativePathsAndSealedValidation(t *testing.T) {
	f := newBackupFixture(t)
	t.Chdir(f.policy.WorkingDir)
	native, err := nativeconfig.LoadClientConfigResult(f.main, true)
	if err != nil || len(native.Proxies) != 2 || native.ProxyOrigins["from-include"] != "include" {
		t.Fatalf("native fixture load: %v", err)
	}
	before, err := os.Stat(f.store)
	if err != nil {
		t.Fatal(err)
	}
	graph := mustBackupGraph(t, f)
	if graph.Manifest.Version != 2 || graph.Manifest.Kind != backupmanifest.Kind || len(graph.Manifest.Files) != 7 || len(graph.Manifest.Includes) != 1 || len(graph.Manifest.Includes[0].Files) != 1 || graph.Manifest.Includes[0].Files[0] != f.include {
		t.Fatalf("unexpected graph: counts %d/%d", len(graph.Manifest.Files), len(graph.Manifest.Includes))
	}
	for _, file := range graph.Manifest.Files {
		if file.Path == filepath.Join(f.policy.WorkingDir, "store.json") || strings.Contains(file.Path, "must-not-be-read") {
			t.Fatal("wrong native path semantics")
		}
	}
	if bytes.Contains(graph.ManifestBytes, []byte("private-token-canary")) {
		t.Fatal("manifest leaked bytes")
	}
	after, _ := os.Stat(f.store)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("capture mutated Store")
	}
	if err := CheckBackupGraph(context.Background(), f.policy, graph); err != nil {
		t.Fatal(err)
	}
	// Whitespace is not a source change: the caller's raw manifest seal can
	// differ from canonical encoding while the underlying inventory is equal.
	pretty := backupClone(t, graph)
	pretty.ManifestBytes, err = json.MarshalIndent(pretty.Manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	pretty.ManifestDigest = backupmanifest.Digest(pretty.ManifestBytes)
	if err := CheckBackupGraph(context.Background(), f.policy, pretty); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("http_proxy", "http://environment-must-not-be-read.invalid")
	validated, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	if validated.ManifestDigest != graph.ManifestDigest {
		t.Fatal("digest changed")
	}
	graph.Files[0].Bytes[0] ^= 1
	if bytes.Equal(graph.Files[0].Bytes, validated.Files[0].Bytes) {
		t.Fatal("sealed payload aliases caller")
	}
}

func TestBackupGraphMissingStoreAndFormats(t *testing.T) {
	for _, format := range []string{"json", "toml", "yaml"} {
		t.Run(format, func(t *testing.T) {
			f := newBackupFixture(t)
			if format != "json" {
				f.main = filepath.Join(filepath.Dir(f.main), "agent."+format)
				f.policy.ConfigFile = f.main
				data := "store.path = 'store.json'\nincludes = ['../runtime/includes/*.json']\n"
				if format == "yaml" {
					data = "store:\n  path: store.json\nincludes:\n  - includes/*.json\n"
				}
				backupWrite(t, f.main, []byte(data))
			}
			if err := os.Remove(f.store); err != nil {
				t.Fatal(err)
			}
			graph := mustBackupGraph(t, f)
			found := false
			for _, entry := range graph.Manifest.Files {
				if entry.Kind == "store" {
					found = true
					if entry.Exists {
						t.Fatal("missing Store became present")
					}
				}
			}
			if !found {
				t.Fatal("missing Store omission")
			}
			if _, err := os.Stat(f.store); !os.IsNotExist(err) {
				t.Fatal("capture created Store")
			}
			if _, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupGraphTrustedPolicyAndExternalMapping(t *testing.T) {
	f := newBackupFixture(t)
	external, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(external, "auth.token")
	backupWrite(t, path, []byte("external-private-canary"))
	f.config["auth"] = map[string]any{"tokenSource": map[string]any{"type": "file", "file": map[string]any{"path": path}}}
	f.writeMain(t)
	if _, err := CaptureBackupGraph(context.Background(), f.policy); err == nil {
		t.Fatal("unmapped external read")
	}
	f.policy.Mappings = []BackupMapping{{ID: "external-token", Path: path}}
	graph := mustBackupGraph(t, f)
	if _, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	narrow := f.policy
	narrow.Mappings = nil
	if _, err := ValidateBackupGraph(context.Background(), narrow, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err == nil {
		t.Fatal("manifest broadened caller policy")
	}
	for _, mutate := range []func(*BackupGraphPolicy){
		func(p *BackupGraphPolicy) {
			p.Roots = append(p.Roots, BackupRoot{ID: "nested", Path: filepath.Join(f.root, "runtime")})
		},
		func(p *BackupGraphPolicy) { p.Roots[0].Path = "/" },
		func(p *BackupGraphPolicy) { p.Mappings = append(p.Mappings, BackupMapping{ID: "wrong", Path: f.token}) },
		func(p *BackupGraphPolicy) { p.StoreFile = filepath.Join(f.root, "wrong.json") },
		func(p *BackupGraphPolicy) { p.WorkingDir = filepath.Join(external, "not-authorized") },
	} {
		p := f.policy
		p.Roots = append([]BackupRoot(nil), f.policy.Roots...)
		mutate(&p)
		if _, err := CaptureBackupGraph(context.Background(), p); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}

func TestBackupGraphClosureTamperingAndNoDiskFallback(t *testing.T) {
	f := newBackupFixture(t)
	original := mustBackupGraph(t, f)
	tests := map[string]func(*BackupGraph){
		"required-file-and-edge-omission": func(g *BackupGraph) {
			id := ""
			for i, file := range g.Manifest.Files {
				if file.Path == f.token {
					id = file.ID
					g.Manifest.Files = append(g.Manifest.Files[:i], g.Manifest.Files[i+1:]...)
					break
				}
			}
			for i, file := range g.Files {
				if file.ID == id {
					g.Files = append(g.Files[:i], g.Files[i+1:]...)
					break
				}
			}
			refs := g.Manifest.References[:0]
			for _, ref := range g.Manifest.References {
				if ref.To != f.token {
					refs = append(refs, ref)
				}
			}
			g.Manifest.References = refs
		},
		"payload-missing":                 func(g *BackupGraph) { g.Files = g.Files[1:] },
		"payload-corrupt":                 func(g *BackupGraph) { g.Files[0].Bytes[0] ^= 1 },
		"include-list-truncated":          func(g *BackupGraph) { g.Manifest.Includes[0].Files = []string{} },
		"directory-omitted":               func(g *BackupGraph) { g.Manifest.Directories = []backupmanifest.Directory{} },
		"matching-directory-file-omitted": func(g *BackupGraph) { g.Manifest.Directories[0].Entries = []backupmanifest.Entry{} },
		"invented-edge": func(g *BackupGraph) {
			g.Manifest.References = append(g.Manifest.References, backupmanifest.Reference{From: f.main, Field: "invented", To: f.token})
		},
		"duplicate-payload":   func(g *BackupGraph) { g.Files = append(g.Files, g.Files[0]) },
		"store-path-forged":   func(g *BackupGraph) { g.Manifest.StoreFile = filepath.Join(f.root, "another-store.json") },
		"mode-forged":         func(g *BackupGraph) { g.Manifest.Files[0].Mode = 0666 },
		"logical-path-forged": func(g *BackupGraph) { g.Manifest.Files[0].LogicalPath = "roots/installation/../escape" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			g := backupClone(t, original)
			mutate(g)
			backupReseal(t, g)
			backupAssertRejected(t, f, g)
		})
	}
	// Removing both inventory and include material yields a different possible
	// original source tree. Only a trusted externally held digest distinguishes
	// that replacement from the originally captured tree.
	g := backupClone(t, original)
	g.Manifest.Directories[0].Entries = []backupmanifest.Entry{}
	backupReseal(t, g)
	if _, err := ValidateBackupGraph(context.Background(), f.policy, g.ManifestBytes, g.Files, original.ManifestDigest); err == nil {
		t.Fatal("trusted seal ignored")
	}
}

func TestBackupGraphDriftAndPinnedRoots(t *testing.T) {
	for _, which := range []string{"bytes", "mtime", "include-added", "unmatched-added", "replacement-inode", "root-replaced"} {
		t.Run(which, func(t *testing.T) {
			f := newBackupFixture(t)
			_, err := captureBackupGraph(context.Background(), f.policy, func() {
				switch which {
				case "bytes":
					backupWrite(t, f.token, []byte("changed-token"))
				case "mtime":
					stamp := time.Now().Add(-time.Hour)
					if err := os.Chtimes(f.token, stamp, stamp); err != nil {
						t.Fatal(err)
					}
				case "include-added":
					backupWrite(t, filepath.Join(filepath.Dir(f.include), "b.json"), []byte(`{"proxies":[]}`))
				case "unmatched-added":
					backupWrite(t, filepath.Join(filepath.Dir(f.include), "note.txt"), []byte("unmatched"))
				case "replacement-inode":
					info, _ := os.Stat(f.token)
					data, _ := os.ReadFile(f.token)
					if err := os.Rename(f.token, f.token+".old"); err != nil {
						t.Fatal(err)
					}
					backupWrite(t, f.token, data)
					if err := os.Chtimes(f.token, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				case "root-replaced":
					if err := os.Rename(f.root, f.root+".old"); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.RemoveAll(f.root + ".old") })
					if err := os.Mkdir(f.root, 0700); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err == nil {
				t.Fatal("source drift accepted")
			}
		})
	}
}

func TestBackupGraphUnsupportedInputs(t *testing.T) {
	cases := map[string]func(*backupFixture){
		"template": func(f *backupFixture) { backupWrite(t, f.main, []byte(`{"store":{"path":"{{ .Envs.STORE }}"}}`)) },
		"yaml-disguised-json": func(f *backupFixture) {
			backupWrite(t, f.main, []byte("store:\n  path: store.json\n.ignored: secret\n"))
		},
		"yaml-disguised-toml": func(f *backupFixture) {
			f.main = strings.TrimSuffix(f.main, "json") + "toml"
			f.policy.ConfigFile = f.main
			backupWrite(t, f.main, []byte("store:\n  path: store.json\n"))
		},
		"unknown-field": func(f *backupFixture) { f.config["unrecognized"] = "value"; f.writeMain(t) },
		"exec": func(f *backupFixture) {
			f.config["auth"] = map[string]any{"tokenSource": map[string]any{"type": "exec", "exec": map[string]any{"command": "/must-not-execute"}}}
			f.writeMain(t)
		},
		"assets": func(f *backupFixture) { f.config["webServer"] = map[string]any{"assetsDir": "."}; f.writeMain(t) },
		"static-empty-path": func(f *backupFixture) {
			f.config["proxies"] = []any{map[string]any{"name": "static", "type": "tcp", "plugin": map[string]any{"type": "static_file"}}}
			f.writeMain(t)
		},
		"socket": func(f *backupFixture) {
			f.config["proxies"] = []any{map[string]any{"name": "socket", "type": "tcp", "plugin": map[string]any{"type": "unix_domain_socket", "unixPath": "/tmp/unbacked.sock"}}}
			f.writeMain(t)
		},
		"duplicate-object": func(f *backupFixture) {
			backupWrite(t, f.include, []byte(`{"proxies":[{"name":"from-file","type":"tcp"}]}`))
		},
		"duplicate-pattern": func(f *backupFixture) {
			f.config["includes"] = []string{"includes/*.json", "includes/a.json"}
			f.writeMain(t)
		},
		"unmapped-include-directory": func(f *backupFixture) { f.config["includes"] = []string{"/unmapped/path/*.json"}; f.writeMain(t) },
		"missing-token": func(f *backupFixture) {
			if err := os.Remove(f.token); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(f *backupFixture) {
			if err := os.Rename(f.token, f.token+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.token+".real", f.token); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(f *backupFixture) {
			if err := os.Link(f.token, f.token+".link"); err != nil {
				t.Fatal(err)
			}
		},
		"writable": func(f *backupFixture) {
			if err := os.Chmod(f.token, 0666); err != nil {
				t.Fatal(err)
			}
		},
		"ambient-http-proxy": func(f *backupFixture) { t.Setenv("http_proxy", "http://unrecorded.invalid") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newBackupFixture(t)
			mutate(&f)
			_, err := CaptureBackupGraph(context.Background(), f.policy)
			if err == nil {
				t.Fatal("unsupported source accepted")
			}
			if strings.Contains(err.Error(), f.root) || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "must-not-execute") {
				t.Fatal("private error leak")
			}
		})
	}
}

func TestBackupGraphBudgetsCancellationAndConcurrency(t *testing.T) {
	f := newBackupFixture(t)
	for _, limits := range []BackupGraphLimits{{Files: 2}, {FileBytes: 8}, {TotalBytes: 64}, {Entries: 1}, {Edges: 1}, {Files: backupmanifest.MaxFiles + 1}} {
		p := f.policy
		p.Limits = limits
		if limits.Entries == 1 {
			backupWrite(t, filepath.Join(filepath.Dir(f.include), "extra.txt"), []byte("x"))
		}
		if _, err := CaptureBackupGraph(context.Background(), p); err == nil {
			t.Fatal("budget ignored")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CaptureBackupGraph(ctx, f.policy); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	graph := mustBackupGraph(t, f)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestBackupGraphStorePluginFiles(t *testing.T) {
	f := newBackupFixture(t)
	backupWrite(t, f.store, []byte(`{"proxies":[{"name":"stored-tls","type":"tcp","plugin":{"type":"https2http","localAddr":"127.0.0.1:8080","crtPath":"cert.pem","keyPath":"key.pem"}}],"visitors":[]}`))
	graph := mustBackupGraph(t, f)
	found := 0
	for _, ref := range graph.Manifest.References {
		if ref.From == f.store && strings.Contains(ref.Field, "plugin.") {
			found++
		}
	}
	if found != 2 {
		t.Fatal("Store plugin dependencies omitted")
	}
	if _, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err != nil {
		t.Fatal(err)
	}
}

func TestBackupGraphStrictSourceAndDirectoryBoundaries(t *testing.T) {
	for _, name := range []string{"yaml-dot-key", "yaml-alias", "json-duplicate", "unknown-store-field", "missing-cwd", "ancestor-symlink", "matched-directory", "unmatched-link", "bad-glob", "zero-mtime"} {
		t.Run(name, func(t *testing.T) {
			f := newBackupFixture(t)
			accept := false
			switch name {
			case "yaml-dot-key", "yaml-alias":
				f.main = filepath.Join(filepath.Dir(f.main), "agent.yaml")
				f.policy.ConfigFile = f.main
				data := "store:\n  path: store.json\n.ignored: private\n"
				if name == "yaml-alias" {
					data = "store: &store\n  path: store.json\n"
				}
				backupWrite(t, f.main, []byte(data))
			case "json-duplicate":
				backupWrite(t, f.main, []byte(`{"store":{"path":"store.json","path":"other.json"}}`))
			case "unknown-store-field":
				backupWrite(t, f.store, []byte(`{"proxies":[],"visitors":[],"unknown":"value"}`))
			case "missing-cwd":
				f.policy.WorkingDir = filepath.Join(f.root, "missing-cwd")
			case "ancestor-symlink":
				original := filepath.Dir(f.include)
				moved := original + "-real"
				if err := os.Rename(original, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, original); err != nil {
					t.Fatal(err)
				}
			case "matched-directory":
				if err := os.Mkdir(filepath.Join(filepath.Dir(f.include), "ignored.json"), 0700); err != nil {
					t.Fatal(err)
				}
				accept = true
			case "unmatched-link":
				if err := os.Symlink("/must-not-read", filepath.Join(filepath.Dir(f.include), "unmatched.txt")); err != nil {
					t.Fatal(err)
				}
				accept = true
			case "bad-glob":
				f.config["includes"] = []string{"includes/["}
				f.writeMain(t)
			case "zero-mtime":
				stamp := time.Unix(0, 0)
				if err := os.Chtimes(f.token, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			graph, err := CaptureBackupGraph(context.Background(), f.policy)
			if (err == nil) != accept {
				t.Fatalf("accept=%v: %v", accept, err)
			}
			if accept {
				if _, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
