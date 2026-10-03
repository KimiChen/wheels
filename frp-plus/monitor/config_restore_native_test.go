package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func TestConfigRestoreNativeEndToEnd(t *testing.T) {
	binary := os.Getenv("FRP_CONFIG_RESTORE_E2E_AGENT")
	if binary == "" {
		t.Skip("restore E2E requires FRP_CONFIG_RESTORE_E2E_AGENT with native patch 8")
	}
	t.Setenv("FRP_CONFIG_E2E_AGENT", binary)
	runConfigNativeAdmin(t, "restore")
}
func stopRestoreNative(t *testing.T, p *configNativeProcess, crash bool) {
	t.Helper()
	if crash {
		_ = p.cmd.Process.Kill()
	} else {
		_ = p.cmd.Process.Signal(os.Interrupt)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("native restore fixture did not stop")
	}
}
func nativeMaintenance(t *testing.T, ctx context.Context, binary, root string, target any, args ...string) {
	t.Helper()
	argv := append([]string{"managed-maintenance"}, args...)
	argv = append(argv, "--offline")
	cmd := exec.CommandContext(ctx, binary, argv...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C"}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &bytes.Buffer{}
	if cmd.Run() != nil {
		t.Fatal("native maintenance helper rejected fixture")
	}
	if output.Len() > 1<<20 || json.Unmarshal(output.Bytes(), target) != nil {
		t.Fatal("native maintenance returned invalid safe output")
	}
}
func runConfigNativeRestore(t *testing.T, ctx context.Context, s *Service, cookie *http.Cookie, csrf, root, binary, config string, server, client *configNativeProcess, oldService string, remotePort, visitorPort int) {
	t.Helper()
	if os.WriteFile(filepath.Join(root, "installation.json"), []byte(`{"format":2,"roles":["agent"],"managed":{"version":1,"root":"managed","store":"store.json"}}`), 0600) != nil {
		t.Fatal("cannot write restore installation profile")
	}
	var inventory struct {
		ServiceID string                  `json:"service_id"`
		Inventory *shared.ConfigInventory `json:"inventory"`
	}
	configNativeWait(t, ctx, "pre-backup inventory", []*configNativeProcess{server, client}, func() bool {
		status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath, nil)
		return status == 200 && json.Unmarshal(data, &inventory) == nil && inventory.Inventory != nil
	})
	key, _ := shared.NewConfigOperationID()
	request := configCreateRequest{ServiceID: inventory.ServiceID, BaseRevision: inventory.Inventory.Revision, IdempotencyKey: key, DeadlineAtMS: time.Now().Add(time.Minute).UnixMilli(), Changes: []shared.ConfigChange{{Operation: "delete", Kind: "visitor", Name: "e2e-visitor", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{}}}, SecretValues: []shared.ConfigSecretInput{}}
	status, data := configNativeHTTP(t, ctx, s, cookie, csrf, "POST", configAdminPath+"/operations", request)
	if status != 201 {
		t.Fatal("cannot prepare crash recovery journal")
	}
	pending := configNativeDecode(t, data)
	if pending.Operation.State != "prepared" {
		t.Fatal("native recovery fixture was not prepared")
	}
	stopRestoreNative(t, client, true)
	checkpoint := filepath.Join(root, "checkpoint")
	var backup managed.CheckpointSummary
	nativeMaintenance(t, ctx, binary, root, &backup, "checkpoint", "--config", config, "--output", checkpoint)
	if backup.Code != "ok" || backup.FileCount < 11 {
		t.Fatal("checkpoint omitted transaction/secret materials")
	}
	var installed managed.RestoreSummary
	nativeMaintenance(t, ctx, binary, root, &installed, "restore-install", "--checkpoint", checkpoint, "--root", filepath.Join(root, "managed"), "--manifest-digest", backup.ManifestDigest)
	if installed.State != "pending" || installed.ServiceID == oldService {
		t.Fatal("restore did not rotate identity or close gate")
	}
	resumed := startConfigNative(t, ctx, binary, config, root, "agent-restored")
	var view configRestoreResponse
	configNativeWait(t, ctx, "restored runtime verification", []*configNativeProcess{server, resumed}, func() bool {
		status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath+"/restore", nil)
		return status == 200 && json.Unmarshal(data, &view) == nil && view.Restore != nil && view.Restore.State == "verified"
	})
	if view.ServiceID != installed.ServiceID || view.Restore.BackupServiceID != oldService || view.ActiveOperation == nil {
		t.Fatal("restored identity lineage or old active operation absent")
	}
	configNativeWait(t, ctx, "restored TCP and Visitor forwarding", []*configNativeProcess{server, resumed}, func() bool { return configNativeEcho(remotePort) && configNativeEcho(visitorPort) })
	ack := configRestoreRequest{ServiceID: view.ServiceID, Epoch: view.Restore.Epoch, ManifestDigest: view.Restore.ManifestDigest, ContextRevision: view.Restore.ContextRevision, StoreDigest: view.Restore.StoreDigest, ExpectedActiveOperationID: view.ActiveOperation.OperationID, ExpectedActiveVersion: view.ActiveOperation.Version}
	status, _ = configNativeHTTP(t, ctx, s, cookie, "invalid", "POST", configAdminPath+"/restore/acknowledge", ack)
	if status != 403 {
		t.Fatal("restore takeover bypassed CSRF")
	}
	// Explicit takeover only queries the interrupted operation under its new
	// identity; its recovered rolled_back facts must remain in the old audit.
	if os.Getenv("FRP_CONFIG_RESTORE_BROWSER_HELPER") != "" {
		runConfigRestoreBrowser(t, ctx, s, cookie, root)
		// The page's inspect/query/acknowledge share the production command
		// budget. Read-only confirmation may wait for that budget to refill.
		configNativeWait(t, ctx, "browser restore receipt", []*configNativeProcess{server, resumed}, func() bool {
			status, data = configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath+"/restore", nil)
			return status == 200 && json.Unmarshal(data, &view) == nil && view.Code == "ok" && view.Receipt != nil && view.Receipt.State == "acknowledged"
		})
	} else {
		status, data = configNativeHTTP(t, ctx, s, cookie, csrf, "POST", configAdminPath+"/restore/acknowledge", ack)
	}
	if status != 200 || json.Unmarshal(data, &view) != nil || view.Code != "ok" || view.Receipt == nil || view.Receipt.State != "acknowledged" {
		t.Fatal("native restoration takeover failed")
	}
	old, err := s.control.GetConfigOperation(ctx, pending.Operation.OperationID)
	if err != nil || old.State != "rolled_back" || old.Agent == nil || !old.Agent.RuntimeLoaded {
		t.Fatal("takeover did not retain actual startup recovery")
	}
	// Even acknowledged restoration cannot accept old-service commands or new
	// edits until the separate offline confirmation and restart verification.
	_, err = s.ConfigCommand(ctx, "1", shared.ConfigCommand{Action: "inspect", ServiceID: oldService})
	if err == nil {
		t.Fatal("stale service command was not rejected")
	}
	stopRestoreNative(t, resumed, false)
	var confirmed managed.RestoreSummary
	nativeMaintenance(t, ctx, binary, root, &confirmed, "restore-confirm", "--config", config, "--epoch", installed.Epoch, "--manifest-digest", backup.ManifestDigest)
	if confirmed.State != "confirmed" {
		t.Fatal("offline restore confirmation absent")
	}
	final := startConfigNative(t, ctx, binary, config, root, "agent-confirmed")
	configNativeWait(t, ctx, "restored management activation", []*configNativeProcess{server, final}, func() bool {
		status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath, nil)
		return status == 200 && json.Unmarshal(data, &inventory) == nil && inventory.Inventory != nil && inventory.Inventory.State == "ready" && inventory.ServiceID == installed.ServiceID
	})
	configNativeWait(t, ctx, "confirmed restored forwarding", []*configNativeProcess{server, final}, func() bool { return configNativeEcho(remotePort) && configNativeEcho(visitorPort) })
	t.Log("native restore E2E passed: private checkpoint, crash journal recovery, rotated identity, closed write gate, authenticated CAS takeover, two offline steps, TCP and STCP Visitor forwarding")
}
