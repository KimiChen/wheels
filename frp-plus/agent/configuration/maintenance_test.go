//go:build linux || darwin

package configuration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	nativeconfig "github.com/fatedier/frp/pkg/config"
)

func maintenanceFixture(t *testing.T) (string, managed.Options) {
	t.Helper()
	t.Setenv("http_proxy", "")
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "managed")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "agent.toml")
	config := fmt.Sprintf(`serverAddr = "127.0.0.1"
serverPort = 17000
[store]
path = %q
[telemetry]
enabled = true
endpoint = "ws://127.0.0.1:17401/agent/v1/ws"
tokenFile = %q
allowInsecureLoopback = true
[telemetry.configManagement]
enabled = true
root = %q
`, filepath.Join(root, "store.json"), filepath.Join(parent, "agent.token"), root)
	files := map[string]string{"agent.toml": config, "agent.token": "synthetic-token", "installation.json": `{"format":2,"roles":["agent"],"managed":{"version":1,"root":"managed","store":"store.json"}}`}
	for name, data := range files {
		if err = os.WriteFile(filepath.Join(parent, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	opts := managed.Options{Root: root, StorePath: filepath.Join(root, "store.json")}
	engine, err := managed.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Close(); err != nil {
		t.Fatal(err)
	}
	return path, opts
}
func TestMaintenanceCaptureMatchesNativeRecoveryContext(t *testing.T) {
	path, opts := maintenanceFixture(t)
	ctx := context.Background()
	capture := MaintenanceCapture(path)
	view, err := capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := nativeconfig.LoadClientConfigResult(path, true)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := InspectContext(Input{ConfigFile: path, StoreFile: opts.StorePath, WorkingDir: filepath.Dir(path), StartupCommon: loaded.Common, ReloadCommon: loaded.Common, FileMemory: Objects{Proxies: loaded.Proxies, Visitors: loaded.Visitors}, TemplateEnv: map[string]string{}})
	if err != nil || revision != view.Revision {
		t.Fatal("checkpoint changed native context")
	}
	output := filepath.Join(filepath.Dir(path), "checkpoint")
	exported, err := managed.ExportCheckpoint(ctx, opts, capture, output)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := managed.InstallCheckpoint(ctx, output, opts.Root, exported.ManifestDigest, capture, ValidateMaintenanceCheckpoint)
	if err != nil || installed.State != "pending" {
		t.Fatalf("restore failed: %v", err)
	}
	again, err := capture(ctx)
	if err != nil || again.Revision != revision {
		t.Fatal("restore failed to preserve path/mtime/common revision")
	}
}
func TestMaintenanceRejectsExternalDependenciesAndTemplates(t *testing.T) {
	for _, test := range []struct{ name, extra string }{
		{"include", `includes = ["*.toml"]`},
		{"template", `user = "{{ .Envs.USER }}"`},
		{"dependency", `[transport.tls]
trustedCaFile = "/private/not-in-checkpoint.pem"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, _ := maintenanceFixture(t)
			data, _ := os.ReadFile(path)
			if err := os.WriteFile(path, append([]byte(test.extra+"\n"), data...), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := MaintenanceCapture(path)(context.Background()); err == nil {
				t.Fatal("unsupported closure was accepted")
			}
		})
	}
}
func TestMaintenanceRejectsArchivedStorePluginPaths(t *testing.T) {
	path, opts := maintenanceFixture(t)
	if err := os.WriteFile(opts.StorePath, []byte(`{"proxies":[{"name":"private","type":"tcp","plugin":{"type":"static_file","localPath":"/private/data"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaintenanceCapture(path)(context.Background()); err == nil {
		t.Fatal("external archived plugin data was omitted")
	}
}

func TestMaintenanceRejectsImplicitPluginWorkingDirectory(t *testing.T) {
	path, opts := maintenanceFixture(t)
	if err := os.WriteFile(opts.StorePath, []byte(`{"proxies":[{"name":"private","type":"tcp","plugin":{"type":"static_file"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaintenanceCapture(path)(context.Background()); err == nil {
		t.Fatal("implicit working-directory plugin dependency was omitted")
	}
}
