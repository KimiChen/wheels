package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// Actual server rejection and local Visitor bind failure must roll the new
// candidate back before its deadline. Waiting/control/P2P distinctions also
// have native manager tests in patch 9; no business probe is fabricated here.
func TestConfigEarlyFailureNativeEndToEnd(t *testing.T) {
	agent, server := os.Getenv("FRP_CONFIG_EARLY_FAILURE_E2E_AGENT"), os.Getenv("FRP_CONFIG_E2E_SERVER")
	if agent == "" || server == "" {
		t.Skip("early-failure native E2E requires patch 9 Agent and FRP_CONFIG_E2E_SERVER")
	}
	for _, scenario := range []string{"server_rejection", "visitor_bind_failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil || os.Chmod(root, 0700) != nil {
				t.Fatal("cannot create private early-failure fixture")
			}
			write := func(name, value string) string {
				path := filepath.Join(root, name)
				if os.WriteFile(path, []byte(value), 0600) != nil {
					t.Fatal("cannot write early-failure fixture")
				}
				return path
			}
			managedRoot := filepath.Join(root, "managed")
			if os.Mkdir(managedRoot, 0700) != nil {
				t.Fatal("cannot create managed fixture")
			}
			store := filepath.Join(managedRoot, "store.json")
			original := []byte(`{"proxies":[],"visitors":[]}`)
			if os.WriteFile(store, original, 0600) != nil {
				t.Fatal("cannot initialize Store")
			}
			s, token, admin := testAdmin(t)
			cookie, session := login(t, s, admin)
			tokenFile := write("agent.token", token)
			controlHold, controlPort := configNativePort(t)
			defer controlHold.Close()
			blocked, blockedPort := configNativePort(t)
			defer blocked.Close()
			allowed, allowedPort := configNativePort(t)
			defer allowed.Close()
			authToken := "synthetic-early-failure-auth"
			serverConfig := write("server.toml", fmt.Sprintf("bindAddr = \"127.0.0.1\"\nbindPort = %d\nallowPorts = [{single = %d}]\n[auth]\nmethod = \"token\"\ntoken = %q\n", controlPort, allowedPort, authToken))
			agentConfig := write("agent.toml", fmt.Sprintf("serverAddr = \"127.0.0.1\"\nserverPort = %d\nclientID = \"1\"\nloginFailExit = false\ntransport.protocol = \"tcp\"\ntransport.wireProtocol = \"v2\"\nstore.path = %q\n[auth]\nmethod = \"token\"\ntoken = %q\n[telemetry]\nenabled = true\nendpoint = %q\ntokenFile = %q\nintervalSeconds = 1\nallowInsecureLoopback = true\n[telemetry.configManagement]\nenabled = true\nroot = %q\n", controlPort, store, authToken, "ws://"+s.Address()+"/agent/v1/ws", tokenFile, managedRoot))
			controlHold.Close()
			sp := startConfigNative(t, ctx, server, serverConfig, root, "server")
			configNativeWait(t, ctx, "control listener", []*configNativeProcess{sp}, func() bool { return configNativeOpen(controlPort) })
			ap := startConfigNative(t, ctx, agent, agentConfig, root, "agent")
			processes := []*configNativeProcess{sp, ap}
			var inventory struct {
				ServiceID string                  `json:"service_id"`
				Inventory *shared.ConfigInventory `json:"inventory"`
			}
			configNativeWait(t, ctx, "managed inventory", processes, func() bool {
				status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath, nil)
				return status == 200 && json.Unmarshal(data, &inventory) == nil && inventory.Inventory != nil && inventory.Inventory.State == "ready"
			})
			field := func(path string, value any) shared.ConfigFieldPatch {
				raw, _ := json.Marshal(value)
				return shared.ConfigFieldPatch{Path: path, Value: raw}
			}
			key, _ := shared.NewConfigOperationID()
			request := configCreateRequest{ServiceID: inventory.ServiceID, BaseRevision: inventory.Inventory.Revision, IdempotencyKey: key, DeadlineAtMS: time.Now().Add(time.Minute).UnixMilli(), SecretValues: []shared.ConfigSecretInput{}}
			change := shared.ConfigChange{Operation: "create", Kind: "proxy", Name: "rejected", Type: "tcp", Fields: []shared.ConfigFieldPatch{field("localIP", "127.0.0.1"), field("localPort", blockedPort), field("remotePort", blockedPort)}, Secrets: []shared.ConfigSecretPatch{}}
			if scenario == "visitor_bind_failure" {
				reference, _ := shared.NewConfigOperationID()
				change = shared.ConfigChange{Operation: "create", Kind: "visitor", Name: "rejected", Type: "stcp", Fields: []shared.ConfigFieldPatch{field("serverName", "unused-peer"), field("bindAddr", "127.0.0.1"), field("bindPort", blockedPort)}, Secrets: []shared.ConfigSecretPatch{{Path: "secretKey", Mode: "reference", Reference: reference}}}
				request.SecretValues = []shared.ConfigSecretInput{{Reference: reference, Value: "synthetic-visitor-secret"}}
			}
			request.Changes = []shared.ConfigChange{change}
			status, data := configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", configAdminPath+"/operations", request)
			if status != 201 {
				t.Fatal("early-failure prepare HTTP rejected")
			}
			prepared := configNativeDecode(t, data)
			if prepared.Operation.State != "prepared" || prepared.Agent == nil {
				t.Fatal("preflight did not accept locally valid candidate")
			}
			started := time.Now()
			path := configAdminPath + "/operations/" + prepared.Operation.OperationID
			status, data = configNativeHTTP(t, ctx, s, cookie, session.CSRF, "POST", path+"/apply", configActionRequest{ExpectedVersion: prepared.Operation.Version, ContextRevision: prepared.Agent.ContextRevision, CandidateDigest: prepared.Operation.CandidateDigest})
			if status != 200 {
				t.Fatal("early-failure apply HTTP rejected")
			}
			result := configNativeDecode(t, data)
			configNativeWait(t, ctx, "pre-deadline rollback", processes, func() bool {
				if result.Operation.State == "rolled_back" {
					return true
				}
				status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", path, nil)
				if status == 200 {
					result = configNativeDecode(t, data)
				}
				return result.Operation.State == "rolled_back"
			})
			if time.Since(started) > 15*time.Second || time.Now().UnixMilli() >= request.DeadlineAtMS-30000 {
				t.Fatal("definite attempt failure waited for the operation deadline")
			}
			if result.Agent == nil || result.Agent.ErrorCode != "verify_failed" || result.Agent.StorePersisted || result.Agent.RuntimeApplied || !result.Agent.RuntimeLoaded || !result.Agent.ResourcesReady || result.Agent.BusinessChecked {
				t.Fatal("early rollback misrepresented runtime facts")
			}
			actual, err := os.ReadFile(store)
			if err != nil || !bytes.Equal(actual, original) {
				t.Fatal("early failure did not restore exact Store bytes")
			}
			t.Log("known runtime attempt failure rolled back before deadline without business claims")
		})
	}
}
