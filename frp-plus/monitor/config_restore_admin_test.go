package monitor

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func TestRestoreAdminCASLostAckAndPrivacy(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	service := configAdminID(0)
	info := shared.RestoreInfo{State: "verified", Epoch: configAdminID(0), BackupServiceID: configTestService, ManifestDigest: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), StoreDigest: strings.Repeat("c", 64), RuntimeLoaded: true, ResourcesReady: true}
	calls := 0
	lose := true
	s.configCoordinator.restoreCommand = func(ctx context.Context, node string, c shared.RestoreCommand) (shared.RestoreResult, error) {
		calls++
		if c.Action == "acknowledge" {
			receipt, err := s.control.GetConfigRestore(ctx, node, info.Epoch)
			if err != nil || receipt.ID != c.AcknowledgementID {
				t.Error("ack preceded durable takeover")
			}
			info.State = "acknowledged"
			info.AcknowledgementID = c.AcknowledgementID
			if lose {
				lose = false
				return shared.RestoreResult{}, context.DeadlineExceeded
			}
		}
		copy := info
		return shared.RestoreResult{ServiceID: service, Code: "ok", Restore: &copy}, nil
	}
	path := configAdminPath + "/restore"
	res := adminRequest(t, s, "GET", path, "", cookie, "", nil)
	expectStatus(t, res, 200)
	var out configRestoreResponse
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		t.Fatal("invalid restore response")
	}
	res.Body.Close()
	input := configRestoreRequest{ServiceID: service, Epoch: info.Epoch, ManifestDigest: info.ManifestDigest, ContextRevision: info.ContextRevision, StoreDigest: info.StoreDigest}
	before := calls
	expectStatus(t, adminRequest(t, s, "POST", path+"/acknowledge", configBody(t, input), cookie, "bad", nil), 403)
	if calls != before {
		t.Fatal("CSRF failure reached restore provider")
	}
	bad := input
	bad.StoreDigest = strings.Repeat("d", 64)
	expectStatus(t, adminRequest(t, s, "POST", path+"/acknowledge", configBody(t, bad), cookie, session.CSRF, nil), 409)
	res = adminRequest(t, s, "POST", path+"/acknowledge", configBody(t, input), cookie, session.CSRF, nil)
	expectStatus(t, res, 200)
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		t.Fatal("invalid lost ack response")
	}
	res.Body.Close()
	if out.Code != "timeout" || out.Receipt == nil || out.Receipt.State != "pending" {
		t.Fatal("lost acknowledgement was assumed successful")
	}
	receiptID := out.Receipt.ID
	res = adminRequest(t, s, "POST", path+"/acknowledge", configBody(t, input), cookie, session.CSRF, nil)
	expectStatus(t, res, 200)
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		t.Fatal("invalid repeated ack response")
	}
	res.Body.Close()
	if out.Code != "ok" || out.Receipt.State != "acknowledged" || out.Receipt.ID != receiptID || out.Restore.State != "acknowledged" {
		t.Fatal("ack retry changed restoration identity")
	}
	res = adminRequest(t, s, "GET", "/api/public/v1/nodes", "", nil, "", nil)
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, key := range []string{"manifest_digest", info.Epoch, "acknowledgement_id", service} {
		if strings.Contains(string(data), key) {
			t.Fatal("restore lineage leaked publicly")
		}
	}
}

func TestRestoreAdminExplicitlySupersedesOnlyMissingOldJournal(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	agent := installConfigAgent(s)
	input := configAdminInput()
	res := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, input), cookie, session.CSRF, nil)
	expectStatus(t, res, 201)
	created := decodeConfigResponse(t, res)
	service := configAdminID(0)
	info := shared.RestoreInfo{State: "verified", Epoch: configAdminID(0), BackupServiceID: configTestService, ManifestDigest: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), StoreDigest: strings.Repeat("c", 64), RuntimeLoaded: true, ResourcesReady: true}
	s.configCoordinator.restoreCommand = func(ctx context.Context, node string, c shared.RestoreCommand) (shared.RestoreResult, error) {
		if c.Action == "acknowledge" {
			info.State = "acknowledged"
			info.AcknowledgementID = c.AcknowledgementID
		}
		copy := info
		return shared.RestoreResult{ServiceID: service, Code: "ok", Restore: &copy}, nil
	}
	queries := 0
	s.configCoordinator.command = func(ctx context.Context, node string, c shared.ConfigCommand) (shared.ConfigResult, error) {
		if c.Action != "query" {
			t.Error("restore takeover sent mutation")
		}
		queries++
		return shared.ConfigResult{ServiceID: service, Code: "operation_not_found", OperationID: c.OperationID}, nil
	}
	_ = agent
	request := configRestoreRequest{ServiceID: service, Epoch: info.Epoch, ManifestDigest: info.ManifestDigest, ContextRevision: info.ContextRevision, StoreDigest: info.StoreDigest}
	path := configAdminPath + "/restore/acknowledge"
	expectStatus(t, adminRequest(t, s, "POST", path, configBody(t, request), cookie, session.CSRF, nil), 409)
	request.ExpectedActiveOperationID = created.Operation.OperationID
	request.ExpectedActiveVersion = created.Operation.Version
	res = adminRequest(t, s, "POST", path, configBody(t, request), cookie, session.CSRF, nil)
	expectStatus(t, res, 200)
	res.Body.Close()
	if queries != 1 {
		t.Fatal("takeover did not query old journal")
	}
	old, err := s.control.GetConfigOperation(context.Background(), created.Operation.OperationID)
	if err != nil || old.State != "cancelled" || old.Agent == nil || old.Agent.State != "prepared" {
		t.Fatal("takeover forged native rollback facts")
	}
	events, err := s.control.ListConfigOperationEvents(context.Background(), old.OperationID)
	if err != nil || events[len(events)-1].Code != "restore_superseded" {
		t.Fatal("takeover lacked explicit superseded audit")
	}
	if _, err = s.control.GetActiveConfigOperation(context.Background(), "1"); err != control.ErrNotFound {
		t.Fatal("takeover left active lease")
	}
}

func TestRestoreAdminPersistsOnlyMatchingConfirmedObservation(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, _ := login(t, s, admin)
	node, err := s.control.Get(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	info := shared.RestoreInfo{State: "acknowledged", Epoch: configAdminID(0), BackupServiceID: configTestService, ManifestDigest: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), StoreDigest: strings.Repeat("c", 64), RuntimeLoaded: true, ResourcesReady: true}
	service := configAdminID(0)
	receipt, _, err := s.control.ClaimConfigRestore(context.Background(), control.ClaimConfigRestoreRequest{ID: configAdminID(0), NodeID: node.ID, ServiceID: service, Epoch: info.Epoch, BackupServiceID: info.BackupServiceID, ManifestDigest: info.ManifestDigest, ContextRevision: info.ContextRevision, StoreDigest: info.StoreDigest, Creator: "system:test", ExpectedTokenSHA256: node.TokenSHA256})
	if err != nil {
		t.Fatal(err)
	}
	info.AcknowledgementID = receipt.ID
	s.configCoordinator.restoreCommand = func(ctx context.Context, node string, c shared.RestoreCommand) (shared.RestoreResult, error) {
		copy := info
		return shared.RestoreResult{ServiceID: service, Code: "ok", Restore: &copy}, nil
	}
	count := func() int {
		page, err := s.control.ListAudit(context.Background(), control.AuditFilter{Kind: "restore"}, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, item := range page.Items {
			if item.Code == "restore_confirmed" {
				n++
			}
		}
		return n
	}
	r := adminRequest(t, s, "GET", configAdminPath+"/restore", "", cookie, "", nil)
	expectStatus(t, r, 200)
	r.Body.Close()
	if count() != 0 {
		t.Fatal("acknowledgement claimed completed restore")
	}
	info.State = "confirmed"
	info.AcknowledgementID = configAdminID(0)
	r = adminRequest(t, s, "GET", configAdminPath+"/restore", "", cookie, "", nil)
	expectStatus(t, r, 409)
	r.Body.Close()
	if count() != 0 {
		t.Fatal("mismatched confirmation persisted")
	}
	info.AcknowledgementID = receipt.ID
	for i := 0; i < 2; i++ {
		r = adminRequest(t, s, "GET", configAdminPath+"/restore", "", cookie, "", nil)
		expectStatus(t, r, 200)
		r.Body.Close()
	}
	if count() != 1 {
		t.Fatal("confirmed proof missing or replayed")
	}
}
