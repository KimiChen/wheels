package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func TestServerRestoreGateStartupFailsClosed(t *testing.T) {
	database := filepath.Join(t.TempDir(), "control.sqlite")
	marker := database + serverRestoreGateSuffix
	if serverRestoreGateAtStart(database) {
		t.Fatal("ordinary installation was blocked")
	}
	for _, content := range []string{"", "not JSON", `{"version":999}`} {
		if err := os.WriteFile(marker, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if !serverRestoreGateAtStart(database) {
			t.Fatal("an invalid recovery marker opened writes")
		}
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), marker); err != nil {
		t.Fatal(err)
	}
	if !serverRestoreGateAtStart(database) {
		t.Fatal("dangling recovery marker opened writes")
	}
}

func restartRestoreAdmin(t *testing.T, cfg shared.MonitorConfig) *Service {
	t.Helper()
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.cfg.GitHubCallbackURL = "http://" + s.Address() + "/api/admin/v1/auth/github/callback"
	s.admin.github.callback = s.cfg.GitHubCallbackURL
	s.admin.github.client = githubFixtureClient("operator")
	return s
}

func TestServerRestoreGateBlocksBeforeIntentAndKeepsCancel(t *testing.T) {
	s, _, admin := testAdmin(t)
	a := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	preparedResponse := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
	expectStatus(t, preparedResponse, 201)
	prepared := decodeConfigResponse(t, preparedResponse)
	cfg := s.cfg
	s.Close()
	if err := os.WriteFile(cfg.DatabaseFile+serverRestoreGateSuffix, []byte("private recovery marker"), 0600); err != nil {
		t.Fatal(err)
	}
	s = restartRestoreAdmin(t, cfg)
	restoredAgent := installConfigAgent(s)
	restoredAgent.mu.Lock()
	restoredAgent.operations = a.operations
	restoredAgent.mu.Unlock()
	cookie, session = login(t, s, admin)
	inv := adminRequest(t, s, "GET", configAdminPath, "", cookie, "", nil)
	expectStatus(t, inv, 200)
	var inventory struct {
		ServerRestorePending bool `json:"server_restore_pending"`
	}
	if err := json.NewDecoder(inv.Body).Decode(&inventory); err != nil || !inventory.ServerRestorePending {
		t.Fatal("missing restore gate", err)
	}
	inv.Body.Close()
	actionBody := configBody(t, configActionRequest{ExpectedVersion: prepared.Operation.Version, ContextRevision: prepared.Agent.ContextRevision, CandidateDigest: prepared.Operation.CandidateDigest})
	for _, suffix := range []string{"/operations", "/operations/" + prepared.Operation.OperationID + "/apply", "/operations/" + prepared.Operation.OperationID + "/rollback"} {
		body := actionBody
		if suffix == "/operations" {
			body = configBody(t, configAdminInput())
		}
		response := adminRequest(t, s, "POST", configAdminPath+suffix, body, cookie, session.CSRF, nil)
		expectStatus(t, response, http.StatusConflict)
		var value map[string]string
		if err := json.NewDecoder(response.Body).Decode(&value); err != nil || value["code"] != "server_restore_pending" {
			t.Fatalf("gate response: %v %v", value, err)
		}
		response.Body.Close()
	}
	ops, err := s.control.ListConfigOperations(context.Background(), "1", 100)
	if err != nil || len(ops) != 1 || ops[0].Version != prepared.Operation.Version {
		t.Fatalf("blocked request changed durable intent: %v %v", ops, err)
	}
	restoredAgent.mu.Lock()
	for _, call := range restoredAgent.calls {
		if call != "inspect" && call != "query" {
			t.Errorf("blocked request dispatched %s", call)
		}
	}
	restoredAgent.mu.Unlock()
	query := adminRequest(t, s, "GET", configAdminPath+"/operations/"+prepared.Operation.OperationID, "", cookie, "", nil)
	expectStatus(t, query, 200)
	queried := decodeConfigResponse(t, query)
	if !queried.ServerRestorePending {
		t.Fatal("operation omitted gate")
	}
	actionBody = configBody(t, configActionRequest{ExpectedVersion: queried.Operation.Version, ContextRevision: queried.Agent.ContextRevision, CandidateDigest: queried.Operation.CandidateDigest})
	cancelledResponse := adminRequest(t, s, "POST", configAdminPath+"/operations/"+prepared.Operation.OperationID+"/cancel", actionBody, cookie, session.CSRF, nil)
	expectStatus(t, cancelledResponse, 200)
	if got := decodeConfigResponse(t, cancelledResponse); got.Operation.State != "cancelled" || !got.ServerRestorePending {
		t.Fatal("recovery cancel blocked", got)
	}
	// Offline marker removal does not reopen a running controller.
	if err := os.Remove(cfg.DatabaseFile + serverRestoreGateSuffix); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil), 409)
	s.Close()
	s = restartRestoreAdmin(t, cfg)
	installConfigAgent(s)
	cookie, session = login(t, s, admin)
	response := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
	expectStatus(t, response, 201)
	if got := decodeConfigResponse(t, response); got.ServerRestorePending || got.Operation.State != "prepared" {
		t.Fatal("confirmed restart did not reopen", got)
	}
}

func TestServerRestoreGateBlocksTransportAndAuditDispatch(t *testing.T) {
	s := &Service{serverRestorePending: true}
	for _, action := range []string{"prepare", "apply", "rollback"} {
		c := shared.ConfigCommand{Action: action}
		_, err := s.ConfigCommand(context.Background(), "1", c)
		if !errors.Is(err, ErrServerRestorePending) || !errors.Is(err, ErrConfigNotSent) {
			t.Fatalf("transport %s: %v", action, err)
		}
		_, err = s.pacedConfigCommand(context.Background(), "1", c, nil, "operator")
		if !errors.Is(err, ErrServerRestorePending) || !errors.Is(err, ErrConfigNotSent) || configFailureCode(err) != "server_restore_pending" {
			t.Fatalf("dispatch %s: %v", action, err)
		}
	}
	for _, action := range []string{"inspect", "query", "cancel", "secret"} {
		_, err := s.ConfigCommand(context.Background(), "1", shared.ConfigCommand{Action: action})
		if !errors.Is(err, ErrConfigUnavailable) || errors.Is(err, ErrServerRestorePending) {
			t.Fatalf("recovery %s was blocked: %v", action, err)
		}
	}
}

func TestServerRestoreGateKeepsObservationAndRecovery(t *testing.T) {
	for _, action := range []string{"prepare", "apply", "rollback"} {
		if !serverRestoreActionBlocked(true, action) || serverRestoreActionBlocked(false, action) {
			t.Fatalf("invalid write gate for %s", action)
		}
	}
	for _, action := range []string{"inspect", "query", "cancel", "secret"} {
		if serverRestoreActionBlocked(true, action) {
			t.Fatalf("recovery operation %s was blocked", action)
		}
	}
}
