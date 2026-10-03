package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func TestConfigQueryRecordsCompletedRecoveryAfterRollbackFailure(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	prepared := decodeConfigResponse(t, adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil))
	path := configAdminPath + "/operations/" + prepared.Operation.OperationID
	action := configActionRequest{ExpectedVersion: prepared.Operation.Version, ContextRevision: prepared.Agent.ContextRevision, CandidateDigest: prepared.Operation.CandidateDigest}
	confirmed := decodeConfigResponse(t, adminRequest(t, s, "POST", path+"/apply", configBody(t, action), cookie, session.CSRF, nil))
	if confirmed.Operation.State != "confirmed" {
		t.Fatal("fixture did not confirm its initial configuration")
	}
	ctx := context.Background()
	rolling, err := s.configTransition(ctx, confirmed.Operation, "rolling_back", "rolling_back", "test-admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	facts := *confirmed.Agent
	facts.State, facts.ErrorCode = "rollback_failed", "verify_failed"
	facts.StorePersisted, facts.RuntimeLoaded, facts.ResourcesReady = false, false, false
	facts.UpdatedAtMS = time.Now().UnixMilli()
	result := shared.ConfigResult{ServiceID: rolling.ServiceID, OperationID: rolling.OperationID, Code: "ok", Operation: &facts}
	failed, err := s.recordConfigResult(ctx, rolling, result, "test-admin")
	if err != nil || failed.State != "rollback_failed" {
		t.Fatal("fixture could not retain failed recovery")
	}
	// The native startup, not a controller command, has since recovered.
	facts.State, facts.ErrorCode = "rolled_back", "recovered"
	facts.RuntimeApplied, facts.RuntimeLoaded, facts.ResourcesReady = false, true, true
	facts.UpdatedAtMS = time.Now().UnixMilli()
	agent.mu.Lock()
	agent.operations[failed.OperationID] = facts
	before := len(agent.calls)
	agent.mu.Unlock()
	recovered, code := s.queryConfigOperation(ctx, failed, configReconcileActor)
	if code != "ok" || recovered.State != "rolled_back" || recovered.Agent == nil || !recovered.Agent.ResourcesReady {
		t.Fatal("completed native recovery remained stranded behind failed history")
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.calls) != before+1 || agent.calls[before] != "query" {
		t.Fatal("recovery reconciliation executed a mutation")
	}
	active, err := s.control.ListActiveConfigOperations(ctx, "", 100)
	if err != nil || len(active) != 0 {
		t.Fatal("completed recovery retained its active reservation")
	}
}
