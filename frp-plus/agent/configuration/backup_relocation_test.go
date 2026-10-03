//go:build linux || darwin

package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
)

type relocationFixture struct {
	source, target BackupGraphPolicy
	captured       managed.ContextSnapshotV3
	manifest       managed.CheckpointManifestV2
	variants       []managed.StoreVariant
}

func newRelocationFixture(t *testing.T) relocationFixture {
	t.Helper()
	path, options, variants := checkpointFixtureV2(t)
	parent := filepath.Dir(path)
	targetParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := filepath.Join(targetParent, "relocated")
	external, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	externalFile := filepath.Join(external, "private-ca.pem")
	backupWrite(t, externalFile, []byte("external-file-private-material"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append([]byte("user = '{{.Envs.USER}}'\n"), raw...)
	raw = bytes.ReplaceAll(raw, []byte("trustedCaFile = 'tls/ca.crt'"), []byte("trustedCaFile = '"+externalFile+"'"))
	raw = append(raw, []byte("\n[auth]\nmethod='token'\ntoken='{{ index .Envs \"TOKEN\" }}'\n")...)
	// The env-derived path is intentionally not serialized as an env value.
	raw = bytes.ReplaceAll(raw, []byte(fmt.Sprintf("tokenFile = %q", filepath.Join(parent, "agent.token"))), []byte("tokenFile = '{{ .Envs.ROOT }}/agent.token'"))
	backupWrite(t, path, raw)
	includePath := filepath.Join(parent, "includes/site.toml")
	includeRaw, err := os.ReadFile(includePath)
	if err != nil {
		t.Fatal(err)
	}
	includeRaw = append(includeRaw, []byte("\n[[proxies]]\nname='web-domain'\ntype='http'\ncustomDomains=['{{.Envs.DOMAIN}}']\nlocalPort=18080\n")...)
	backupWrite(t, includePath, includeRaw)
	store := []byte(fmt.Sprintf(`{"proxies":[{"name":"stored","type":"stcp","secretKey":"literal-secret-preserved","enabled":false,"localPort":9090,"annotations":{"owner":"example"}},{"name":"stored-cert","type":"tcp","plugin":{"type":"https2http","localAddr":"127.0.0.1:8080","crtPath":%q,"keyPath":%q}}],"visitors":[]}`, filepath.Join(parent, "plugin/site.crt"), filepath.Join(parent, "plugin/site.key")))
	backupWrite(t, options.StorePath, store)
	variants[0].Snapshot = managed.StoreSnapshot{Exists: true, Bytes: store}
	historical := []byte(`{"proxies":[{"name":"history","type":"tcp","plugin":{"type":"https2http","crtPath":"plugin/historical.crt","keyPath":"plugin/historical.key"}}],"visitors":[]}`)
	variants = append(variants, managed.StoreVariant{Path: "managed/operations/test.old", Snapshot: managed.StoreSnapshot{}}, managed.StoreVariant{Path: "managed/operations/test.new", Snapshot: managed.StoreSnapshot{Exists: true, Bytes: historical}})
	source := BackupGraphPolicy{ConfigFile: path, WorkingDir: parent, StoreFile: options.StorePath, Roots: []BackupRoot{{ID: "installation", Path: parent}}, Mappings: []BackupMapping{{ID: "external-ca", Path: externalFile}}, TemplateEnv: map[string]string{"ROOT": parent, "USER": "stable-namespace", "TOKEN": "environment-secret-never-saved", "DOMAIN": "unchanged.example.invalid"}}
	target := BackupGraphPolicy{ConfigFile: filepath.Join(targetRoot, "agent.toml"), WorkingDir: targetRoot, StoreFile: filepath.Join(targetRoot, "managed/store.json"), Roots: []BackupRoot{{ID: "installation", Path: targetRoot}}, Mappings: []BackupMapping{{ID: "external-ca", Path: filepath.Join(targetParent, "relocated-ca.pem")}}, TemplateEnv: map[string]string{"ROOT": targetRoot, "USER": "stable-namespace", "TOKEN": "environment-secret-never-saved", "DOMAIN": "unchanged.example.invalid"}}
	captured, err := CheckpointCaptureV3(source)(context.Background(), variants)
	if err != nil {
		t.Fatal("capture", err)
	}
	manifest := managed.CheckpointManifestV2{CheckpointManifest: managed.CheckpointManifest{Version: 3, Root: options.Root, ConfigFile: path, WorkingDir: parent, ContextRevision: captured.Revision}, GraphSHA256: backupmanifest.Digest(captured.GraphBytes)}
	return relocationFixture{source, target, captured, manifest, variants}
}
func (f relocationFixture) run(ctx context.Context) (managed.RelocationContextV3, error) {
	return PrepareRelocationV3(f.source, f.target)(ctx, f.manifest, f.captured.Files, f.variants, f.captured.GraphBytes, f.captured.PolicyBytes)
}

func TestRelocationV3SealedClosurePreservesBusinessAndTemplates(t *testing.T) {
	f := newRelocationFixture(t)
	before, _ := json.Marshal(f.variants)
	if err := os.RemoveAll(f.source.WorkingDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.source.Mappings[0].Path); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(context.Background())
	if err != nil {
		t.Fatal("pure relocate", err)
	}
	if out.Context.Revision == f.captured.Revision {
		t.Fatal("old context reused")
	}
	if _, err := os.Stat(f.target.WorkingDir); !os.IsNotExist(err) {
		t.Fatal("pure planner wrote target")
	}
	if len(out.TargetRoots) != 1 || out.TargetRoots[0] != f.target.WorkingDir || len(out.TargetFiles) != 1 || out.TargetFiles[0] != f.target.Mappings[0].Path {
		t.Fatal("destination authorization broadened")
	}
	after, _ := json.Marshal(f.variants)
	if !bytes.Equal(before, after) {
		t.Fatal("historical variants changed")
	}
	for _, file := range out.Context.Files {
		if bytes.Contains(file.Bytes, []byte(f.target.TemplateEnv["TOKEN"])) {
			t.Fatal("environment secret materialized")
		}
		if strings.Contains(file.Path, "historical.") {
			t.Fatal("old-only executable dependency relocated")
		}
		if file.Path == f.target.ConfigFile {
			if !bytes.Contains(file.Bytes, []byte(".Envs")) || bytes.Contains(file.Bytes, []byte(f.source.WorkingDir)) {
				t.Fatal("template lost or literal path unchanged")
			}
		}
	}
	if !bytes.Contains(out.Store.Bytes, []byte("literal-secret-preserved")) || !bytes.Contains(out.Store.Bytes, []byte(`"enabled": false`)) || !bytes.Contains(out.Store.Bytes, []byte("annotations")) {
		t.Fatal("Store secret/disabled/advanced fields dropped")
	}
	targetVariants := []managed.StoreVariant{{Path: "managed/store.json", Snapshot: out.Store}}
	for _, v := range f.variants[1:] {
		v.Historical = true
		targetVariants = append(targetVariants, v)
	}
	targetManifest := managed.CheckpointManifestV2{CheckpointManifest: managed.CheckpointManifest{Version: 3, Root: filepath.Dir(f.target.StoreFile), ConfigFile: f.target.ConfigFile, WorkingDir: f.target.WorkingDir, ContextRevision: out.Context.Revision}, GraphSHA256: backupmanifest.Digest(out.Context.GraphBytes)}
	if err := ValidateCheckpointContextV3(f.target)(context.Background(), targetManifest, out.Context.Files, targetVariants, out.Context.GraphBytes, out.Context.PolicyBytes); err != nil {
		t.Fatal("target sealed validation", err)
	}
}

func TestRelocationV3RejectsImplicitBusinessAndPolicyChanges(t *testing.T) {
	for _, name := range []string{"namespace", "domain", "secret", "mixed-path-business", "missing-mapping", "different-id", "source-overlap", "source-ancestor", "tampered-graph", "missing-source-file"} {
		t.Run(name, func(t *testing.T) {
			f := newRelocationFixture(t)
			switch name {
			case "namespace":
				f.target.TemplateEnv["USER"] = "changed-namespace"
			case "domain":
				f.target.TemplateEnv["DOMAIN"] = "changed.example.invalid"
			case "secret":
				f.target.TemplateEnv["TOKEN"] = "rotated-secret-without-consent"
			case "mixed-path-business":
				// A path string can also be valid proxy metadata. Changing ROOT must
				// never silently change that independent business value.
				for i, file := range f.captured.Files {
					if file.Path == f.source.ConfigFile {
						file.Bytes = append([]byte("metadatas = { original = '{{.Envs.ROOT}}' }\n"), file.Bytes...)
						backupWrite(t, file.Path, file.Bytes)
						f.captured.Files[i] = file
					}
				}
				captured, err := CheckpointCaptureV3(f.source)(context.Background(), f.variants)
				if err != nil {
					t.Fatal(err)
				}
				f.captured = captured
				f.manifest.ContextRevision = captured.Revision
				f.manifest.GraphSHA256 = backupmanifest.Digest(captured.GraphBytes)
			case "missing-mapping":
				f.target.Mappings = nil
			case "different-id":
				f.target.Mappings[0].ID = "new-unrelated-authority"
			case "source-overlap":
				f.target.Roots[0].Path = filepath.Join(f.source.WorkingDir, "nested")
				f.target.WorkingDir = f.target.Roots[0].Path
				f.target.ConfigFile = filepath.Join(f.target.WorkingDir, "agent.toml")
				f.target.StoreFile = filepath.Join(f.target.WorkingDir, "managed/store.json")
			case "source-ancestor":
				f.target.Roots[0].Path = filepath.Dir(f.source.WorkingDir)
				f.target.WorkingDir = f.target.Roots[0].Path
				f.target.ConfigFile = filepath.Join(f.target.WorkingDir, "agent.toml")
				f.target.StoreFile = filepath.Join(f.target.WorkingDir, "managed/store.json")
			case "tampered-graph":
				f.captured.GraphBytes = append(f.captured.GraphBytes, ' ')
			case "missing-source-file":
				f.captured.Files = f.captured.Files[1:]
			}
			_, err := f.run(context.Background())
			if err == nil {
				t.Fatal("unauthorized relocation accepted")
			}
			if strings.Contains(err.Error(), f.source.TemplateEnv["TOKEN"]) || strings.Contains(err.Error(), "rotated-secret") {
				t.Fatal("secret in error")
			}
		})
	}
}

func TestRelocationNativeFormatsPreserveScalarTemplates(t *testing.T) {
	for _, format := range []string{"json", "toml", "yaml"} {
		t.Run(format, func(t *testing.T) {
			oldRoot := "/private/old-install"
			newRoot := "/private/new-install"
			source := BackupGraphPolicy{ConfigFile: oldRoot + "/agent.toml", WorkingDir: oldRoot, StoreFile: oldRoot + "/managed/store.json", Roots: []BackupRoot{{ID: "installation", Path: oldRoot}}, TemplateEnv: map[string]string{"KEY": oldRoot + "/site.key", "PORT": "18000", "TOKEN": "static-environment-secret"}}
			target := BackupGraphPolicy{ConfigFile: newRoot + "/agent.toml", WorkingDir: newRoot, StoreFile: newRoot + "/managed/store.json", Roots: []BackupRoot{{ID: "installation", Path: newRoot}}, TemplateEnv: map[string]string{"KEY": newRoot + "/site.key", "PORT": "18000", "TOKEN": "static-environment-secret"}}
			mapping, err := newBackupRelocationMap(source, target)
			if err != nil {
				t.Fatal(err)
			}
			data := `{"proxies":[{"name":"plugin","type":"tcp","enabled":false,"remotePort":{{ .Envs.PORT }},"plugin":{"type":"https2http","crtPath":"/private/old-install/site.crt","keyPath":"{{ index .Envs "KEY" }}"}},{"name":"secret","type":"stcp","secretKey":"{{ .Envs.TOKEN }}"}]}`
			if format == "toml" {
				data = "[[proxies]]\nname='plugin'\ntype='tcp'\nenabled=false\nremotePort={{ .Envs.PORT }}\n[proxies.plugin]\ntype='https2http'\ncrtPath='/private/old-install/site.crt'\nkeyPath='{{ index .Envs \"KEY\" }}'\n[[proxies]]\nname='secret'\ntype='stcp'\nsecretKey='{{ .Envs.TOKEN }}'\n"
			}
			if format == "yaml" {
				data = "proxies:\n  - name: plugin\n    type: tcp\n    enabled: false\n    remotePort: {{ .Envs.PORT }}\n    plugin:\n      type: https2http\n      crtPath: /private/old-install/site.crt\n      keyPath: '{{ index .Envs \"KEY\" }}'\n  - name: secret\n    type: stcp\n    secretKey: '{{ .Envs.TOKEN }}'\n"
			}
			out, err := relocateBackupConfig(context.Background(), source, target, mapping, oldRoot+"/include."+format, newRoot+"/include."+format, []byte(data), false)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(out, []byte(source.TemplateEnv["TOKEN"])) || !bytes.Contains(out, []byte(".Envs.TOKEN")) || bytes.Contains(out, []byte(oldRoot)) {
				t.Fatal("template secret expanded or literal path unmapped")
			}
		})
	}
}

func TestRelocationSamePathPreservesPendingRecoveryMaterials(t *testing.T) {
	f := newRelocationFixture(t)
	f.target = f.source
	out, err := f.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Context.Revision != f.manifest.ContextRevision || !bytes.Equal(out.Store.Bytes, f.variants[0].Snapshot.Bytes) {
		t.Fatal("same-path restore changed current CAS material")
	}
	if len(out.Context.Files) != len(f.captured.Files) {
		t.Fatal("same-path restore omitted old/new dependencies")
	}
	byPath := map[string]managed.ContextFile{}
	for _, file := range out.Context.Files {
		byPath[file.Path] = file
	}
	for _, file := range f.captured.Files {
		got, ok := byPath[file.Path]
		if !ok || got.ModifiedNS != file.ModifiedNS || !bytes.Equal(got.Bytes, file.Bytes) {
			t.Fatal("same-path material changed")
		}
	}
}
