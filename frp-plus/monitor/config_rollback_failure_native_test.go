package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func TestConfigNativeRollbackFailureEndToEnd(t *testing.T) {
	runConfigNativeAdmin(t, "rollback_failure")
}

// A real listener prevents restoration of a previously working Visitor. Repair
// removes the port conflict offline, then uses native startup recovery/query.
func runConfigNativeRollbackFailure(t *testing.T, ctx context.Context, s *Service, cookie *http.Cookie, csrf, root, binary, config string, server, client *configNativeProcess, storePath string, remotePort, visitorPort int) {
	t.Helper()
	processes := []*configNativeProcess{server, client}
	original, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal("cannot read native failure fixture baseline")
	}
	var inventory struct {
		ServiceID string                  `json:"service_id"`
		Inventory *shared.ConfigInventory `json:"inventory"`
	}
	readInventory := func() {
		configNativeWait(t, ctx, "failure fixture inventory", processes, func() bool {
			status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", configAdminPath, nil)
			return status == 200 && json.Unmarshal(data, &inventory) == nil && inventory.Inventory != nil && inventory.Inventory.State == "ready"
		})
	}
	readInventory()
	newRequest := func() configCreateRequest {
		key, _ := shared.NewConfigOperationID()
		return configCreateRequest{ServiceID: inventory.ServiceID, BaseRevision: inventory.Inventory.Revision, IdempotencyKey: key, DeadlineAtMS: time.Now().Add(time.Minute).UnixMilli(), Changes: []shared.ConfigChange{{Operation: "delete", Kind: "visitor", Name: "e2e-visitor", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{}}}, SecretValues: []shared.ConfigSecretInput{}}
	}
	time.Sleep(1200 * time.Millisecond)
	status, data := configNativeHTTP(t, ctx, s, cookie, csrf, "POST", configAdminPath+"/operations", newRequest())
	if status != 201 {
		t.Fatal("cannot prepare Visitor removal for rollback failure")
	}
	prepared := configNativeDecode(t, data)
	if prepared.Operation.State != "prepared" || prepared.Agent == nil {
		t.Fatal("Visitor removal lacks a native prepared result")
	}
	path := configAdminPath + "/operations/" + prepared.Operation.OperationID
	action := configActionRequest{ExpectedVersion: prepared.Operation.Version, ContextRevision: prepared.Agent.ContextRevision, CandidateDigest: prepared.Operation.CandidateDigest}
	status, _ = configNativeHTTP(t, ctx, s, cookie, csrf, "POST", path+"/apply", action)
	if status != 200 {
		t.Fatal("Visitor removal was not accepted")
	}
	var observed configOperationResponse
	waitState := func(state string) {
		configNativeWait(t, ctx, "native operation "+state, processes, func() bool {
			status, data := configNativeHTTP(t, ctx, s, cookie, "", "GET", path, nil)
			if status != 200 {
				return false
			}
			observed = configNativeDecode(t, data)
			if observed.Operation.State != state {
				time.Sleep(900 * time.Millisecond)
				return false
			}
			return true
		})
	}
	waitState("confirmed")
	configNativeWait(t, ctx, "Visitor withdrawal", processes, func() bool { return !configNativeOpen(visitorPort) && configNativeEcho(remotePort) })
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", visitorPort))
	if err != nil {
		t.Fatal("cannot occupy old Visitor listener for failure fixture")
	}
	defer blocker.Close()
	action = configActionRequest{ExpectedVersion: observed.Operation.Version, ContextRevision: observed.Agent.ContextRevision, CandidateDigest: observed.Operation.CandidateDigest}
	time.Sleep(1200 * time.Millisecond)
	status, _ = configNativeHTTP(t, ctx, s, cookie, csrf, "POST", path+"/rollback", action)
	if status != 200 {
		t.Fatal("native rollback request was not accepted")
	}
	waitState("rollback_failed")
	if observed.Agent == nil || observed.Agent.RuntimeLoaded || observed.Agent.ResourcesReady || observed.Agent.BusinessChecked || observed.Agent.StorePersisted {
		t.Fatal("failed native rollback retained stale successful facts")
	}
	current, err := os.ReadFile(storePath)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("native failed rollback did not retain known old Store material")
	}
	status, _ = configNativeHTTP(t, ctx, s, cookie, csrf, "POST", configAdminPath+"/operations", newRequest())
	if status != 409 {
		t.Fatal("failed rollback allowed another active configuration operation")
	}
	stopRestoreNative(t, client, false)
	blocker.Close()
	resumed := startConfigNative(t, ctx, binary, config, root, "agent-after-rollback-repair")
	processes = []*configNativeProcess{server, resumed}
	waitState("rolled_back")
	if observed.Agent == nil || !observed.Agent.RuntimeLoaded || !observed.Agent.ResourcesReady {
		t.Fatal("native repair did not publish freshly verified rollback")
	}
	configNativeWait(t, ctx, "repaired TCP and Visitor business", processes, func() bool { return configNativeEcho(remotePort) && configNativeEcho(visitorPort) })
	readInventory()
	time.Sleep(1200 * time.Millisecond)
	status, data = configNativeHTTP(t, ctx, s, cookie, csrf, "POST", configAdminPath+"/operations", newRequest())
	if status != 201 {
		t.Fatal("native repaired runtime did not release management reservation")
	}
	check := configNativeDecode(t, data)
	if check.Operation.State != "prepared" || check.Agent == nil {
		t.Fatal("native repaired runtime cannot prepare a new candidate")
	}
	status, data = configNativeHTTP(t, ctx, s, cookie, csrf, "POST", configAdminPath+"/operations/"+check.Operation.OperationID+"/cancel", configActionRequest{ExpectedVersion: check.Operation.Version, ContextRevision: check.Agent.ContextRevision, CandidateDigest: check.Operation.CandidateDigest})
	if status != 200 || configNativeDecode(t, data).Operation.State != "cancelled" {
		t.Fatal("repair fixture could not cancel its verification-only candidate")
	}
	current, err = os.ReadFile(storePath)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("repair changed the original business configuration")
	}
	t.Log("native rollback failure and repair passed: actual listener conflict, no stale success facts, retained reservation, offline repair, startup recovery and TCP/STCP Visitor")
}
