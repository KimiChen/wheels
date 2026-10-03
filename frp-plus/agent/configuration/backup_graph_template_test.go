//go:build linux || darwin

package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	nativeconfig "github.com/fatedier/frp/pkg/config"
)

func TestBackupTemplatesReproduceNativeWithoutArchivingValues(t *testing.T) {
	f := newBackupFixture(t)
	f.config["serverAddr"] = "BACKUP_HOST"
	f.config["auth"] = map[string]any{"token": "BACKUP_SECRET"}
	f.config["includes"] = []string{"BACKUP_INCLUDE"}
	f.writeMain(t)
	data, err := os.ReadFile(f.main)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte("BACKUP_HOST"), []byte("{{ .Envs.HOST }}"))
	data = bytes.ReplaceAll(data, []byte("BACKUP_SECRET"), []byte(`{{ index .Envs "TOKEN" }}`))
	data = bytes.ReplaceAll(data, []byte("BACKUP_INCLUDE"), []byte("{{ $.Envs.INCLUDE }}"))
	backupWrite(t, f.main, data)
	backupWrite(t, f.include, []byte(`{"proxies":[{"name":"from-include","type":"tcp","localIP":"{{ index $.Envs "HOST" }}","localPort":80}]}`))
	secret := "environment-secret-must-never-be-archived"
	f.policy.TemplateEnv = map[string]string{"HOST": "127.0.0.1", "TOKEN": secret, "INCLUDE": "includes/*.json", "UNUSED": "unrelated-value-never-captured"}
	graph := mustBackupGraph(t, f)
	if graph.Manifest.Version != backupmanifest.TemplateVersion || len(graph.Manifest.TemplateRequirements) != 2 {
		t.Fatal("missing template restore requirements")
	}
	if !reflect.DeepEqual(graph.Manifest.TemplateRequirements[0].Names, []string{"HOST", "INCLUDE", "TOKEN"}) || !reflect.DeepEqual(graph.Manifest.TemplateRequirements[1].Names, []string{"HOST"}) {
		t.Fatal("incomplete or unordered names")
	}
	for _, requirement := range graph.Manifest.TemplateRequirements {
		raw, err := os.ReadFile(requirement.From)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := nativeconfig.RenderWithTemplate(raw, &nativeconfig.Values{Envs: f.policy.TemplateEnv})
		if err != nil || backupmanifest.Digest(rendered) != requirement.RenderedSHA256 {
			t.Fatal("render differs from native")
		}
	}
	if bytes.Contains(graph.ManifestBytes, []byte(secret)) || bytes.Contains(graph.ManifestBytes, []byte("unrelated-value")) {
		t.Fatal("manifest contains environment value")
	}
	for _, file := range graph.Files {
		if bytes.Contains(file.Bytes, []byte(secret)) || bytes.Contains(file.Bytes, []byte("unrelated-value")) {
			t.Fatal("rendered secret escaped into payload")
		}
	}
	policyJSON, err := json.Marshal(f.policy)
	if err != nil || bytes.Contains(policyJSON, []byte(secret)) || bytes.Contains(policyJSON, []byte("TemplateEnv")) {
		t.Fatal("policy serialized environment")
	}
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN", "ambient-value-must-not-be-used")
	if _, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{
		"missing":         {"HOST": "127.0.0.1", "INCLUDE": "includes/*.json"},
		"changed-secret":  {"HOST": "127.0.0.1", "INCLUDE": "includes/*.json", "TOKEN": "another-secret"},
		"changed-closure": {"HOST": "127.0.0.1", "INCLUDE": "includes/other.json", "TOKEN": secret},
	} {
		t.Run(name, func(t *testing.T) {
			policy := f.policy
			policy.TemplateEnv = env
			_, err := ValidateBackupGraph(context.Background(), policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest)
			if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "another-secret") {
				t.Fatal("changed restore input accepted or leaked")
			}
		})
	}
	f.policy.TemplateEnv["UNUSED"] = "changed-unrelated-session"
	if _, err := ValidateBackupGraph(context.Background(), f.policy, graph.ManifestBytes, graph.Files, graph.ManifestDigest); err != nil {
		t.Fatal("unreferenced environment affected seal")
	}
}

func TestBackupTemplateRequirementsCannotBeOmittedOrForged(t *testing.T) {
	f := newBackupFixture(t)
	f.config["serverAddr"] = "{{ .Envs.HOST }}"
	f.writeMain(t)
	f.policy.TemplateEnv = map[string]string{"HOST": "127.0.0.1", "OTHER": "127.0.0.1"}
	original := mustBackupGraph(t, f)
	for name, alter := range map[string]func(*BackupGraph){
		"omit":   func(g *BackupGraph) { g.Manifest.Version = 2; g.Manifest.TemplateRequirements = nil },
		"name":   func(g *BackupGraph) { g.Manifest.TemplateRequirements[0].Names = []string{"OTHER"} },
		"digest": func(g *BackupGraph) { g.Manifest.TemplateRequirements[0].RenderedSHA256 = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			g := backupClone(t, original)
			alter(g)
			backupReseal(t, g)
			backupAssertRejected(t, f, g)
		})
	}
	for _, entry := range original.Manifest.Files {
		if entry.Path == f.main {
			for _, file := range original.Files {
				if file.ID == entry.ID && !bytes.Contains(file.Bytes, []byte(".Envs.HOST")) {
					t.Fatal("template source was replaced")
				}
			}
		}
	}
}

func TestBackupTemplateBoundedNativeRendering(t *testing.T) {
	env := map[string]string{"ADDR": "127.0.0.1", "TOKEN": "private-template-error-canary", "RANGE": "1-3"}
	for _, input := range []string{
		`{{ .Envs.ADDR }}:{{index .Envs "TOKEN"}}`,
		`{{$.Envs.ADDR}}:{{index $.Envs "TOKEN"}}`,
		`{{$x := .Envs.ADDR}}{{if eq $x "127.0.0.1"}}{{printf "%q" $x}}{{end}}`,
		`{{range $i, $n := parseNumberRange .Envs.RANGE}}{{$i}}={{$n}};{{end}}`,
		`{{define "constant"}}constant{{end}}{{template "constant"}}`,
		`{{parseNumberRangePair "1-3" "4-6"}}`,
		`{{html .Envs.TOKEN}} {{js .Envs.TOKEN}} {{urlquery .Envs.TOKEN}}`,
	} {
		got, _, err := RenderBackupTemplate(context.Background(), []byte(input), env)
		native, nativeErr := nativeconfig.RenderWithTemplate([]byte(input), &nativeconfig.Values{Envs: env})
		if err != nil || nativeErr != nil || !bytes.Equal(got, native) {
			t.Fatalf("native output mismatch for supported template: %v", err)
		}
	}
	for name, input := range map[string]string{
		"dynamic-index":         `{{$key := "ADDR"}}{{index .Envs $key}}`,
		"whole-map":             `{{printf "%v" .Envs}}`,
		"whole-root":            `{{printf "%v" .}}`,
		"env-alias":             `{{$env := .Envs}}{{index $env "TOKEN"}}`,
		"unknown-function":      `{{readFile "/private"}}`,
		"unused-definition":     `{{define "hidden"}}{{range .Envs}}{{.}}{{end}}{{end}}constant`,
		"missing":               `{{ .Envs.MISSING }}`,
		"unsupported-name":      `{{index .Envs "bad\nname"}}`,
		"recursive":             `{{define "loop"}}{{template "loop"}}{{end}}{{template "loop"}}`,
		"huge-range":            `{{range $x := parseNumberRange "0-9223372036854775807"}}{{$x}}{{end}}`,
		"range-overflow":        `{{parseNumberRange "9223372036854775807-9223372036854775807,1-1024"}}`,
		"huge-width":            `{{printf "%1000000000s" .Envs.TOKEN}}`,
		"dynamic-width":         `{{printf "%*s" 1000000000 .Envs.TOKEN}}`,
		"no-output-nested-loop": `{{range $a := parseNumberRange "1-1024"}}{{range $b := parseNumberRange "1-1024"}}{{end}}{{end}}`,
		"rendered-oversize":     `{{range $a := parseNumberRange "1-1024"}}{{printf "%4096s" "x"}}{{end}}`,
		"native-error-secret":   `{{parseNumberRange .Envs.TOKEN}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := RenderBackupTemplate(context.Background(), []byte(input), env)
			if err == nil || strings.Contains(err.Error(), env["TOKEN"]) || strings.Contains(err.Error(), "/private") {
				t.Fatal("unsafe template accepted or native diagnostic leaked")
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := RenderBackupTemplate(cancelled, []byte("{{.Envs.ADDR}}"), env); err != context.Canceled {
		t.Fatal("cancelled render proceeded")
	}
}

func TestBackupConstantTemplatesAndEmptyEnvironmentValue(t *testing.T) {
	for _, input := range []string{`{{define "empty"}}{{end}}{{template "empty"}}`, `{{ .Envs.EMPTY }}`} {
		f := newBackupFixture(t)
		f.config["user"] = input
		f.writeMain(t)
		f.policy.TemplateEnv = map[string]string{"EMPTY": ""}
		raw, _ := os.ReadFile(f.main)
		raw = bytes.ReplaceAll(raw, []byte(`\"`), []byte(`"`))
		backupWrite(t, f.main, raw)
		g := mustBackupGraph(t, f)
		if g.Manifest.Version != 3 || len(g.Manifest.TemplateRequirements) != 1 || g.Manifest.TemplateRequirements[0].Names == nil {
			t.Fatal("constant template provenance omitted")
		}
		if _, err := ValidateBackupGraph(context.Background(), f.policy, g.ManifestBytes, g.Files, g.ManifestDigest); err != nil {
			t.Fatal(err)
		}
	}
}
