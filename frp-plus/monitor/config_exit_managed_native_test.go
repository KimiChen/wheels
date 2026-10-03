package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/configuration"
	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
)

// This is the documented local maintenance procedure, starting from a real
// confirmed managed transaction. Disabling management is not a recovery tool:
// this fixture first proves that no unfinished transaction exists, stops the
// process, and validates a complete private checkpoint before changing the flag.
func TestConfigNativeExitManagementEndToEnd(t *testing.T) {
	runConfigNativeAdmin(t, "exit_management")
}

func configNativeExitDashboard(t *testing.T, config string) (int, string) {
	t.Helper()
	reservation, port := configNativePort(t)
	defer reservation.Close()
	password, err := randomToken()
	if err != nil {
		t.Fatal("cannot generate private Dashboard credential")
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal("cannot read private native configuration")
	}
	prefix := fmt.Sprintf("webServer.addr = \"127.0.0.1\"\nwebServer.port = %d\nwebServer.user = \"exit-test\"\nwebServer.password = %q\n", port, password)
	if os.WriteFile(config, append([]byte(prefix), data...), 0600) != nil {
		t.Fatal("cannot configure private native Dashboard")
	}
	return port, password
}

func runConfigNativeExitManagement(t *testing.T, ctx context.Context, s *Service, root, binary, config string, server, client *configNativeProcess, confirmed configOperationResponse, localPort, remotePort, visitorPort, dashboardPort int, password string) {
	t.Helper()
	if confirmed.Operation.State != "confirmed" || confirmed.Agent == nil || !confirmed.Agent.RuntimeLoaded || !confirmed.Agent.ResourcesReady {
		t.Fatal("exit management requires a confirmed native transaction")
	}
	if active, err := s.control.ListActiveConfigOperations(ctx, "", 100); err != nil || len(active) != 0 {
		t.Fatal("exit management started with unfinished control operations")
	}
	managedRoot := filepath.Join(root, "managed")
	storePath := filepath.Join(managedRoot, "store.json")
	storeBefore, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal("cannot read confirmed private Store")
	}
	call := func(method, path string, body []byte, authenticated bool) (int, []byte) {
		t.Helper()
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(callCtx, method, fmt.Sprintf("http://127.0.0.1:%d%s", dashboardPort, path), bytes.NewReader(body))
		if err != nil {
			t.Fatal("cannot create native maintenance request")
		}
		if authenticated {
			req.SetBasicAuth("exit-test", password)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("native maintenance API request failed")
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			t.Fatal("native maintenance API response exceeded bound")
		}
		return response.StatusCode, data
	}
	newReservation, newPort := configNativePort(t)
	defer newReservation.Close()
	payload, _ := json.Marshal(map[string]any{"name": "native-after-exit", "type": "tcp", "tcp": map[string]any{"localIP": "127.0.0.1", "localPort": localPort, "remotePort": newPort}})
	configNativeWait(t, ctx, "managed Dashboard", []*configNativeProcess{server, client}, func() bool { return configNativeOpen(dashboardPort) })
	if status, _ := call("POST", "/api/store/proxies", payload, true); status != http.StatusConflict {
		t.Fatal("managed native Store mutation was not rejected")
	}
	if status, _ := call("GET", "/api/reload", nil, true); status != http.StatusConflict {
		t.Fatal("managed native reload was not rejected")
	}
	stopRestoreNative(t, client, false)
	if client.failed.Load() {
		// Native TCP mode deliberately does not install the KCP/QUIC graceful
		// signal handler. A signal exit is expected, but an unrelated error is
		// not acceptable evidence of the requested maintenance stop.
		status, ok := client.cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatal("managed Agent stopped with an unexpected process status")
		}
		t.Log("native TCP Agent stopped by requested SIGINT; journal and checkpoint checks determine maintenance integrity")
	}
	if configNativeOpen(dashboardPort) || configNativeOpen(visitorPort) {
		t.Fatal("managed Agent retained local resources after stop")
	}
	profile := []byte(`{"format":2,"roles":["agent"],"managed":{"version":1,"root":"managed","store":"store.json"}}`)
	if os.WriteFile(filepath.Join(root, "installation.json"), profile, 0600) != nil {
		t.Fatal("cannot write private installation profile")
	}
	materialsBefore := configNativeExitMaterials(t, managedRoot)
	confirmedJournal := false
	for name, data := range materialsBefore {
		if !strings.HasPrefix(name, "operations/") || !strings.HasSuffix(name, ".json") {
			continue
		}
		var op managed.Operation
		if json.Unmarshal(data, &op) != nil {
			t.Fatal("cannot inspect durable operation state before exit")
		}
		switch op.State {
		case managed.Confirmed, managed.RolledBack, managed.Conflict, managed.Cancelled:
		default:
			t.Fatal("exit management started with unfinished native journal")
		}
		if op.ID == confirmed.Operation.OperationID && op.State == managed.Confirmed {
			confirmedJournal = true
		}
	}
	if !confirmedJournal {
		t.Fatal("confirmed native journal was not retained")
	}
	backupRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(backupRoot, 0700) != nil {
		t.Fatal("cannot secure separate pre-exit backup directory")
	}
	checkpoint := filepath.Join(backupRoot, "checkpoint-before-exit")
	var backup managed.CheckpointSummary
	configNativeExitCheckpoint(t, ctx, binary, root, config, checkpoint, &backup)
	if backup.Code != "ok" || backup.FileCount < 9 {
		t.Fatal("pre-exit checkpoint omitted native materials")
	}
	manifest, err := managed.ValidateCheckpointV2(ctx, checkpoint, backup.ManifestDigest, configuration.ValidateCheckpointContextV2)
	if err != nil || manifest.StoreDigest != managed.Digest(managed.StoreSnapshot{Exists: true, Bytes: storeBefore}) || manifest.ServiceID != confirmed.Operation.ServiceID {
		t.Fatal("pre-exit checkpoint failed sealed context and Store verification")
	}
	if !reflect.DeepEqual(materialsBefore, configNativeExitMaterials(t, managedRoot)) {
		t.Fatal("checkpoint changed native identity or transaction materials")
	}
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal("cannot read verified native main configuration")
	}
	const enabled = "[telemetry.configManagement]\nenabled = true\n"
	if bytes.Count(before, []byte(enabled)) != 1 {
		t.Fatal("maintenance fixture has ambiguous management switch")
	}
	unmanaged := bytes.Replace(before, []byte(enabled), []byte("[telemetry.configManagement]\nenabled = false\n"), 1)
	if os.WriteFile(config, unmanaged, 0600) != nil {
		t.Fatal("cannot disable management during local maintenance")
	}
	resumed := startConfigNative(t, ctx, binary, config, root, "agent-after-exit")
	processes := []*configNativeProcess{server, resumed}
	configNativeWait(t, ctx, "unmanaged restart preserves TCP and STCP Visitor", processes, func() bool {
		return configNativeOpen(dashboardPort) && configNativeEcho(remotePort) && configNativeEcho(visitorPort)
	})
	if current, err := os.ReadFile(storePath); err != nil || !bytes.Equal(current, storeBefore) {
		t.Fatal("disabling management changed the confirmed Store")
	}
	if status, _ := call("GET", "/api/store/proxies", nil, false); status != http.StatusUnauthorized {
		t.Fatal("exit management disabled Dashboard authentication")
	}
	newReservation.Close()
	if status, _ := call("POST", "/api/store/proxies", payload, true); status != http.StatusOK {
		t.Fatal("native Store create did not recover after management exit")
	}
	configNativeWait(t, ctx, "native Store write after management exit", processes, func() bool {
		return configNativeEcho(newPort) && configNativeEcho(remotePort) && configNativeEcho(visitorPort)
	})
	if status, _ := call("GET", "/api/store/proxies/native-after-exit", nil, true); status != http.StatusOK {
		t.Fatal("native Store read failed after management exit")
	}
	// File PUT and reload are separate native operations. Add one harmless
	// comment, then verify persistence before requesting an explicit reload.
	unmanaged = append([]byte("# locally maintained after managed exit\n"), unmanaged...)
	if status, _ := call("PUT", "/api/config", unmanaged, true); status != http.StatusOK {
		t.Fatal("native file write did not recover after management exit")
	}
	if current, err := os.ReadFile(config); err != nil || !bytes.Equal(current, unmanaged) {
		t.Fatal("native file write did not persist exact maintenance bytes")
	}
	if status, data := call("GET", "/api/reload", nil, true); status != http.StatusOK {
		code := "unexpected_error"
		if bytes.Contains(data, []byte("telemetry configuration changes require a process restart")) {
			code = "unchanged_telemetry_rejected"
		}
		t.Fatalf("native reload did not recover after management exit: status=%d code=%s", status, code)
	}
	configNativeWait(t, ctx, "native reload preserves Store forwarding", processes, func() bool {
		return configNativeEcho(newPort) && configNativeEcho(remotePort) && configNativeEcho(visitorPort)
	})
	if status, _ := call("DELETE", "/api/store/proxies/native-after-exit", nil, true); status != http.StatusOK {
		t.Fatal("native Store delete did not recover after management exit")
	}
	configNativeWait(t, ctx, "native withdrawal preserves confirmed forwarding", processes, func() bool {
		return !configNativeOpen(newPort) && configNativeEcho(remotePort) && configNativeEcho(visitorPort)
	})
	if !reflect.DeepEqual(materialsBefore, configNativeExitMaterials(t, managedRoot)) {
		t.Fatal("unmanaged maintenance rewrote preserved identity or journal")
	}
	if _, err = managed.ValidateCheckpointV2(ctx, checkpoint, backup.ManifestDigest, configuration.ValidateCheckpointContextV2); err != nil {
		t.Fatal("local maintenance changed the sealed pre-exit backup")
	}
	t.Log("native managed exit passed: confirmed/no active operation, requested stop, sealed checkpoint verification, local flag change/restart, retained identity/journal, authenticated native Store and file/reload writes, TCP/STCP Visitor forwarding")
}

func configNativeExitMaterials(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "store.json" || rel == ".lock" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4<<20 {
			return fmt.Errorf("invalid private transaction material")
		}
		result[filepath.ToSlash(rel)], err = os.ReadFile(path)
		return err
	})
	if err != nil || len(result) < 4 {
		t.Fatal("cannot preserve private managed transaction materials")
	}
	return result
}

func configNativeExitCheckpoint(t *testing.T, ctx context.Context, binary, root, config, checkpoint string, result *managed.CheckpointSummary) {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, "managed-maintenance", "checkpoint", "--config", config, "--output", checkpoint, "--dependency-graph", "--offline")
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C"}
	var output, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostic
	if err := cmd.Run(); err != nil {
		code := regexp.MustCompile(`maintenance_[a-z_]+`).FindString(output.String() + diagnostic.String())
		if code == "" {
			code = "unavailable"
		}
		t.Fatalf("native pre-exit checkpoint rejected: %s", code)
	}
	if output.Len() > 1<<20 || json.Unmarshal(output.Bytes(), result) != nil {
		t.Fatal("native pre-exit checkpoint returned invalid safe output")
	}
}
