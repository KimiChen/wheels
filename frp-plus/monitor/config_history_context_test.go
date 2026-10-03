package monitor

import (
	"context"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"testing"
)

func TestConfigAdminHistoricalMaterialsRequireCompletedLineage(t *testing.T) {
	for _, mode := range []string{"missing", "acknowledged", "confirmed", "retained", "changed_business"} {
		t.Run(mode, func(t *testing.T) {
			s, _, admin := testAdmin(t)
			agent := installConfigAgent(s)
			cookie, session := login(t, s, admin)
			ctx := context.Background()
			response := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
			expectStatus(t, response, 201)
			created := decodeConfigResponse(t, response)
			path := configAdminPath + "/operations/" + created.Operation.OperationID
			action := configActionRequest{ExpectedVersion: created.Operation.Version, ContextRevision: created.Agent.ContextRevision, CandidateDigest: created.Operation.CandidateDigest}
			response = adminRequest(t, s, "POST", path+"/apply", configBody(t, action), cookie, session.CSRF, nil)
			expectStatus(t, response, 200)
			original := decodeConfigResponse(t, response)
			node, err := s.control.Get(ctx, original.Operation.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			current := configAdminID(40)
			if mode != "missing" {
				parent := original.Operation.ServiceID
				for n := 0; n < 2; n++ {
					if n == 0 {
						current = configAdminID(41)
					} else {
						current = configAdminID(42)
					}
					req := control.ClaimConfigRestoreRequest{ID: configAdminID(43), NodeID: node.ID, ServiceID: current, Epoch: configAdminID(44), BackupServiceID: parent, ManifestDigest: original.Agent.CandidateDigest, ContextRevision: original.Agent.ContextRevision, StoreDigest: original.Agent.CandidateDigest, Creator: "operator", ExpectedTokenSHA256: node.TokenSHA256}
					receipt, _, err := s.control.ClaimConfigRestore(ctx, req)
					if err != nil {
						t.Fatal(err)
					}
					receipt, err = s.control.ConfirmConfigRestore(ctx, receipt.ID, receipt.Version)
					if err != nil {
						t.Fatal(err)
					}
					if mode != "acknowledged" {
						info := shared.RestoreInfo{State: "confirmed", Epoch: receipt.Epoch, BackupServiceID: receipt.BackupServiceID, ManifestDigest: receipt.ManifestDigest, ContextRevision: receipt.ContextRevision, StoreDigest: receipt.StoreDigest, AcknowledgementID: receipt.ID, RuntimeLoaded: true, ResourcesReady: true}
						if err = s.control.ObserveConfigRestoreConfirmed(ctx, node.ID, current, node.TokenSHA256, info); err != nil {
							t.Fatal(err)
						}
					}
					parent = current
				}
			}
			queryCurrent, mutations := 0, 0
			s.configCoordinator.mu.Lock()
			s.configCoordinator.command = func(_ context.Context, _ string, c shared.ConfigCommand) (shared.ConfigResult, error) {
				r := shared.ConfigResult{ServiceID: current, Action: c.Action, OperationID: c.OperationID, Code: "service_mismatch"}
				if c.Action != "query" {
					mutations++
					return r, nil
				}
				if c.ServiceID == current {
					queryCurrent++
					r.Code = "ok"
					v := *original.Agent
					v.MaterialsState = "context_changed"
					if mode == "retained" {
						v.MaterialsState = original.Agent.MaterialsState
					}
					if mode == "changed_business" {
						v.BusinessChecked = true
					}
					r.Operation = &v
				}
				return r, nil
			}
			s.configCoordinator.mu.Unlock()
			response = adminRequest(t, s, "GET", path, "", cookie, "", nil)
			expectStatus(t, response, 200)
			observed := decodeConfigResponse(t, response)
			if mode == "confirmed" {
				if observed.Agent.MaterialsState != "context_changed" || observed.Operation.ServiceID != original.Operation.ServiceID || observed.Operation.State != "confirmed" {
					t.Fatal("history rebound or missing projection")
				}
			} else if observed.Agent.MaterialsState == "context_changed" {
				t.Fatal("unproven history promoted")
			}
			if (mode == "missing" || mode == "acknowledged") && queryCurrent != 0 {
				t.Fatal("new identity queried without completed chain")
			}
			action.ExpectedVersion = observed.Operation.Version
			response = adminRequest(t, s, "POST", path+"/rollback", configBody(t, action), cookie, session.CSRF, nil)
			expectStatus(t, response, 200)
			refused := decodeConfigResponse(t, response)
			if refused.Operation.State != "confirmed" || mutations != 0 {
				t.Fatal("historical command dispatched or terminal changed")
			}
			if mode == "confirmed" && refused.Code != "context_changed" {
				t.Fatal("context refusal", refused.Code)
			}
			_ = agent
		})
	}
}
