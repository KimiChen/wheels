package monitor

// Optional joint browser test: only GitHub's external identity provider is a
// fixture. Static assets, Admin API, SQLite, WebSocket, native processes and
// forwarding all use their actual implementations. The browser helper receives
// disposable credentials on stdin, never in command arguments or test output.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

func TestConfigNativeBrowserEndToEnd(t *testing.T) {
	runConfigNativeBrowserEndToEnd(t, false)
}

func TestConfigNativeMigrationBrowserEndToEnd(t *testing.T) {
	runConfigNativeBrowserEndToEnd(t, true)
}

func runConfigNativeBrowserEndToEnd(t *testing.T, migrate bool) {
	agent, server := os.Getenv("FRP_CONFIG_E2E_AGENT"), os.Getenv("FRP_CONFIG_E2E_SERVER")
	helper, node := os.Getenv("FRP_CONFIG_E2E_BROWSER_HELPER"), os.Getenv("FRP_CONFIG_E2E_NODE")
	if agent == "" || server == "" || helper == "" || node == "" {
		t.Skip("native browser E2E requires FRP_CONFIG_E2E_AGENT, SERVER, BROWSER_HELPER and NODE")
	}
	for _, path := range []string{agent, server, helper, node} {
		info, err := os.Stat(path)
		if !filepath.IsAbs(path) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("browser E2E inputs must be absolute regular files")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(root, 0700) != nil {
		t.Fatal("cannot secure browser E2E root")
	}
	write := func(name, value string) string {
		path := filepath.Join(root, name)
		if os.WriteFile(path, []byte(value), 0600) != nil {
			t.Fatal("cannot write private browser E2E input")
		}
		return path
	}
	managedRoot := filepath.Join(root, "managed")
	if os.Mkdir(managedRoot, 0700) != nil {
		t.Fatal("cannot create browser E2E Store root")
	}
	original := "{\"proxies\":[],\"visitors\":[]}\n"
	storePath := write("managed/store.json", original)
	s, token, admin := testAdmin(t)
	cookie, _ := login(t, s, admin)
	tokenPath := write("agent.token", token+"\n")
	controlReservation, controlPort := configNativePort(t)
	defer controlReservation.Close()
	remoteReservation, remotePort := configNativePort(t)
	defer remoteReservation.Close()
	visitorReservation, visitorPort := configNativePort(t)
	defer visitorReservation.Close()
	migrationReservation, migrationPort := configNativePort(t)
	defer migrationReservation.Close()
	local, localPort := configNativePort(t)
	var echoes sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			connection, err := local.Accept()
			if err != nil {
				return
			}
			echoes.Add(1)
			go func() {
				defer echoes.Done()
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	t.Cleanup(func() { local.Close(); <-acceptDone; echoes.Wait() })
	frpToken, err := randomToken()
	if err != nil {
		t.Fatal("cannot create disposable FRP credential")
	}
	tunnelSecret, err := randomToken()
	if err != nil {
		t.Fatal("cannot create disposable tunnel credential")
	}
	common := fmt.Sprintf("auth.method = \"token\"\nauth.token = %q\nlog.level = \"warn\"\n", frpToken)
	serverConfig := write("server.toml", fmt.Sprintf("bindAddr = \"127.0.0.1\"\nproxyBindAddr = \"127.0.0.1\"\nbindPort = %d\n", controlPort)+common)
	agentBase := fmt.Sprintf("serverAddr = \"127.0.0.1\"\nserverPort = %d\nclientID = \"1\"\nloginFailExit = false\ntransport.wireProtocol = \"v2\"\nstore.path = %q\n", controlPort, storePath) + common + fmt.Sprintf("\n[telemetry]\nenabled = true\nendpoint = %q\ntokenFile = %q\nserverID = \"browser-e2e\"\nintervalSeconds = 1\nallowInsecureLoopback = true\n", "ws://"+s.Address()+"/agent/v1/ws", tokenPath)
	agentManaged := agentBase + fmt.Sprintf("[telemetry.configManagement]\nenabled = true\nroot = %q\n", managedRoot)
	agentConfig := write("agent.toml", agentManaged)
	controlReservation.Close()
	frps := startConfigNative(t, ctx, server, serverConfig, root, "server")
	configNativeWait(t, ctx, "browser FRP control", []*configNativeProcess{frps}, func() bool { return configNativeOpen(controlPort) })
	if migrate {
		write("agent.toml", agentBase+fmt.Sprintf("\n[[proxies]]\nname = \"migration-tcp\"\ntype = \"tcp\"\nlocalIP = \"127.0.0.1\"\nlocalPort = %d\nremotePort = %d\n", localPort, migrationPort))
		migrationReservation.Close()
		old := startConfigNative(t, ctx, agent, agentConfig, root, "agent-before-migration")
		configNativeWait(t, ctx, "original file forwarding", []*configNativeProcess{frps, old}, func() bool { return configNativeEcho(migrationPort) })
		_ = old.cmd.Process.Signal(os.Interrupt)
		select {
		case <-old.done:
		case <-time.After(5 * time.Second):
			t.Fatal("original file service did not stop for migration")
		}
		cli := func(want string, arguments ...string) {
			command := exec.CommandContext(ctx, agent, arguments...)
			command.Dir = root
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C"}
			output, err := command.CombinedOutput()
			var status struct {
				State          string `json:"state"`
				RuntimeChecked bool   `json:"runtime_checked"`
			}
			if err != nil || json.Unmarshal(output, &status) != nil || status.State != want || status.RuntimeChecked {
				t.Fatal("native migration CLI did not return expected safe disk-only result")
			}
		}
		bundle := filepath.Join(root, "migration-review")
		cli("planned", "config-migrate", "plan", "-c", agentConfig, "--root", managedRoot, "--output", bundle, "--proxy", "migration-tcp")
		candidate, err := os.ReadFile(filepath.Join(bundle, "store.candidate.json"))
		if err != nil {
			t.Fatal("migration candidate unavailable")
		}
		original = string(candidate)
		write("managed/store.json", original)
		// Explicit fixture maintenance removes the old source definition. The CLI
		// itself does not rewrite arbitrary source paths or start native processes.
		write("agent.toml", agentManaged)
		cli("disk_verified", "config-migrate", "check", "-c", agentConfig, "--bundle", bundle, "--offline")
	} else {
		migrationPort = 0
	}
	frpc := startConfigNative(t, ctx, agent, agentConfig, root, "agent")
	configNativeWait(t, ctx, "browser native inventory", []*configNativeProcess{frps, frpc}, func() bool {
		status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath, nil)
		var value struct {
			Inventory struct{ State string } `json:"inventory"`
		}
		return status == 200 && json.Unmarshal(data, &value) == nil && value.Inventory.State == "ready"
	})
	if migrate {
		configNativeWait(t, ctx, "migrated Store forwarding", []*configNativeProcess{frps, frpc}, func() bool { return configNativeEcho(migrationPort) })
	}
	configNativeWait(t, ctx, "browser node online snapshot", []*configNativeProcess{frps, frpc}, func() bool {
		status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", "/api/admin/v1/nodes", nil)
		var snapshot struct {
			Nodes []struct {
				ID      string `json:"id"`
				Session string `json:"session"`
			} `json:"nodes"`
		}
		if status != 200 || json.Unmarshal(data, &snapshot) != nil {
			return false
		}
		for _, node := range snapshot.Nodes {
			if node.ID == "1" && node.Session == "online" {
				return true
			}
		}
		return false
	})
	remoteReservation.Close()
	visitorReservation.Close()
	payload, err := json.Marshal(map[string]any{
		"url": "http://" + s.Address(), "cookie": map[string]any{"name": cookie.Name, "value": cookie.Value},
		"local_port": localPort, "remote_port": remotePort, "visitor_port": visitorPort,
		"secret": tunnelSecret, "store": storePath, "migration_port": migrationPort,
	})
	if err != nil {
		t.Fatal("cannot encode private browser fixture")
	}
	command := exec.CommandContext(ctx, node, helper)
	command.Dir = root
	command.Stdin = bytes.NewReader(payload)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C", "PLAYWRIGHT_MODULE=" + os.Getenv("PLAYWRIGHT_MODULE"), "BROWSER_EXECUTABLE=" + os.Getenv("BROWSER_EXECUTABLE")}
	output, runErr := command.CombinedOutput()
	// Never print arbitrary browser output: assertions may otherwise include
	// request bodies, cookies, private paths or the disposable secret.
	var report struct {
		OK           bool     `json:"ok"`
		Stage        string   `json:"stage"`
		Completed    []string `json:"completed"`
		Observations []string `json:"observations"`
	}
	if json.Unmarshal(output, &report) != nil {
		t.Fatal("browser helper did not return its bounded safe report")
	}
	if runErr != nil || !report.OK {
		safe := regexp.MustCompile(`^[a-z0-9_:]{1,128}$`)
		if !safe.MatchString(report.Stage) || len(report.Observations) > 16 {
			t.Fatal("invalid safe browser diagnostics")
		}
		for _, observation := range report.Observations {
			if !safe.MatchString(observation) {
				t.Fatal("invalid safe browser observation")
			}
		}
		t.Fatalf("native browser flow failed at safe stage %q; bounded observations %v", report.Stage, report.Observations)
	}
	data, err := os.ReadFile(storePath)
	if err != nil || string(data) != original || configNativeOpen(remotePort) || configNativeOpen(visitorPort) {
		t.Fatal("browser rollback did not restore Store and withdraw listeners")
	}
	for _, path := range []string{s.cfg.DatabaseFile, s.cfg.DatabaseFile + "-wal"} {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal("cannot inspect browser E2E audit privacy")
		}
		if bytes.Contains(data, []byte(tunnelSecret)) {
			t.Fatal("browser tunnel secret escaped into audit persistence")
		}
	}
	t.Log("native browser E2E passed: real UI/Admin/WS/native validation, preview, explicit apply, TCP/STCP Visitor, secret privacy and three exact rollbacks")
}
