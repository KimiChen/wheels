package shared

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationManagementRequiresLocalExplicitRoot(t *testing.T) {
	if err := (&AgentConfig{}).Complete(); err != nil {
		t.Fatal("legacy disabled telemetry must remain valid", err)
	}
	root := filepath.Join(t.TempDir(), "managed")
	a := AgentConfig{Enabled: true, Endpoint: "wss://monitor.example.invalid/agent/v1/ws", TokenFile: "token", ConfigManagement: &ConfigManagementConfig{Enabled: true, Root: root}}
	if err := a.Complete(); err != nil {
		t.Fatal(err)
	}
	a.Enabled = false
	if a.Complete() == nil || a.Validate() == nil {
		t.Fatal("write management cannot silently start without telemetry")
	}
	a.Enabled = true
	for _, root := range []string{"", "relative", string(filepath.Separator), root + string(filepath.Separator) + "..", root + "\n"} {
		a.ConfigManagement.Root = root
		if a.Validate() == nil {
			t.Fatalf("accepted invalid managed root %q", root)
		}
	}
	a.ConfigManagement = &ConfigManagementConfig{Enabled: false}
	if err := a.Validate(); err != nil {
		t.Fatal("management remains opt-in", err)
	}
}

func TestMonitorConfigurationBoundary(t *testing.T) {
	a := AgentConfig{Enabled: true, Endpoint: "wss://monitor.example.invalid/agent/v1/ws", TokenFile: "token"}
	if err := a.Complete(); err != nil || a.IntervalSeconds != 1 {
		t.Fatalf("defaults: %v", err)
	}
	for _, endpoint := range []string{"ws://monitor.example.invalid/agent/v1/ws", "ws://localhost/agent/v1/ws", "wss://user:secret@monitor.example.invalid/agent/v1/ws", "wss://monitor.example.invalid/agent/v1/ws?token=secret", "wss://monitor.example.invalid/other", "wss://monitor.example.invalid:0/agent/v1/ws"} {
		c := a
		c.Endpoint = endpoint
		c.AllowInsecureLoopback = true
		if c.Validate() == nil {
			t.Errorf("accepted forbidden endpoint %s", endpoint)
		}
	}
	a.Endpoint = "ws://127.0.0.1:7401/agent/v1/ws"
	if a.Validate() == nil {
		t.Fatal("plain loopback needs opt-in")
	}
	a.AllowInsecureLoopback = true
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	m := MonitorConfig{Enabled: true, DatabaseFile: "control.sqlite"}
	if err := m.Complete(); err != nil || m.BindAddr != "127.0.0.1" || m.BindPort != 7401 {
		t.Fatalf("defaults: %v", err)
	}
	for _, port := range []int{0, 65536} {
		c := m
		c.BindPort = port
		if c.Validate() == nil {
			t.Errorf("accepted bind port %d", port)
		}
	}
	m.BindAddr = "0.0.0.0"
	if m.Validate() == nil {
		t.Fatal("public plaintext listener accepted")
	}
	m.CertFile = "cert.pem"
	m.KeyFile = "key.pem"
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.KeyFile = ""
	if m.Validate() == nil {
		t.Fatal("partial TLS config accepted")
	}
}

func TestGitHubCallbackValidation(t *testing.T) {
	m := MonitorConfig{
		Enabled: true, BindAddr: "127.0.0.1", BindPort: 7401, ServerID: "default",
		DatabaseFile: "control.sqlite", ReportIntervalSeconds: 1,
		GitHubClientID: "client", GitHubClientSecretFile: "secret",
		GitHubCallbackURL: "https://monitor.example.invalid/api/admin/v1/auth/github/callback",
		GitHubAdminUsers:  []string{"admin"},
	}
	for _, callback := range []string{
		m.GitHubCallbackURL,
		"https://monitor.example.invalid:8443/api/admin/v1/auth/github/callback",
		"http://127.0.0.1:8080/api/admin/v1/auth/github/callback",
	} {
		m.GitHubCallbackURL = callback
		if err := m.Validate(); err != nil {
			t.Errorf("rejected valid callback %s: %v", callback, err)
		}
	}
	for _, callback := range []string{
		"https://monitor.example.invalid:0/api/admin/v1/auth/github/callback",
		"https://monitor.example.invalid:65536/api/admin/v1/auth/github/callback",
		"https://" + strings.Repeat("a", 4096) + ".invalid/api/admin/v1/auth/github/callback",
	} {
		m.GitHubCallbackURL = callback
		if m.Validate() == nil {
			t.Errorf("accepted forbidden callback %s", callback)
		}
	}
}

func TestHistoryConfiguration(t *testing.T) {
	c := MonitorConfig{Enabled: true, DatabaseFile: "control.sqlite", HistoryDataPath: "history"}
	if err := c.Complete(); err != nil || c.RetentionDays != 7 {
		t.Fatalf("history defaults: %v", err)
	}
	for _, days := range []int{-1, 0, 366} {
		c.RetentionDays = days
		if c.Validate() == nil {
			t.Fatalf("accepted retention %d", days)
		}
	}
	c.RetentionDays = 7
	c.HistoryDataPath = ""
	if err := c.Validate(); err != nil {
		t.Fatal("empty history path must disable history", err)
	}
	c.HistoryDataPath = "invalid\x00path"
	if c.Validate() == nil {
		t.Fatal("accepted invalid history path")
	}
}
