package monitor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fatedier/frp/pkg/config"
	v1 "github.com/fatedier/frp/pkg/config/v1"
)

func TestNativeConfigurationFormats(t *testing.T) {
	fixtures := map[string]string{
		"toml": `bindPort = 7000
[monitor]
enabled = true
databaseFile = "control.sqlite"
historyDataPath = "history"
githubClientID = "fixture-client"
githubClientSecretFile = "github.secret"
githubCallbackURL = "https://monitor.example.invalid/api/admin/v1/auth/github/callback"
githubAdminUsers = ["example-admin"]
`,
		"yaml": `bindPort: 7000
monitor:
  enabled: true
  databaseFile: control.sqlite
  historyDataPath: history
  githubClientID: fixture-client
  githubClientSecretFile: github.secret
  githubCallbackURL: https://monitor.example.invalid/api/admin/v1/auth/github/callback
  githubAdminUsers: [example-admin]
`,
		"json": `{"bindPort":7000,"monitor":{"enabled":true,"databaseFile":"control.sqlite","historyDataPath":"history","githubClientID":"fixture-client","githubClientSecretFile":"github.secret","githubCallbackURL":"https://monitor.example.invalid/api/admin/v1/auth/github/callback","githubAdminUsers":["example-admin"]}}`,
	}
	for format, content := range fixtures {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server."+format)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, legacy, err := config.LoadServerConfig(path, true)
			if err != nil {
				t.Fatal(err)
			}
			m := cfg.Monitor
			if legacy || cfg.BindPort != 7000 || !m.Enabled || m.DatabaseFile != "control.sqlite" || m.HistoryDataPath != "history" || m.RetentionDays != 7 || len(m.GitHubAdminUsers) != 1 || m.GitHubAdminUsers[0] != "example-admin" || m.BindAddr != "127.0.0.1" {
				t.Fatal("native loader lost monitoring fields or defaults")
			}
		})
	}
}

func TestNativeEnvironmentTemplateAndStrictMonitorConfig(t *testing.T) {
	content := []byte(`monitor:
  enabled: true
  databaseFile: control.sqlite
  historyDataPath: {{ printf "%q" (or .Envs.FRP_MONITOR_HISTORY_DATA_PATH "") }}
`)
	for _, history := range []string{"", "history"} {
		rendered, err := config.RenderWithTemplate(content, &config.Values{Envs: map[string]string{"FRP_MONITOR_HISTORY_DATA_PATH": history}})
		if err != nil {
			t.Fatal(err)
		}
		var cfg v1.ServerConfig
		if err := config.LoadConfigure(rendered, &cfg, true, "yaml"); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Complete(); err != nil || cfg.Monitor.HistoryDataPath != history {
			t.Fatal("environment template must explicitly select the history directory")
		}
	}
	var cfg v1.ServerConfig
	if err := config.LoadConfigure([]byte(`{"monitor":{"historyEnabled":true}}`), &cfg, true, "json"); err == nil {
		t.Fatal("obsolete history switch was silently accepted")
	}
}
