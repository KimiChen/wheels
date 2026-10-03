package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const configAdminPath = "/api/admin/v1/nodes/1/configuration"

func configAdminID(n byte) string { id, _ := shared.NewConfigOperationID(); _ = n; return id }
func configAdminInput() configCreateRequest {
	return configCreateRequest{ServiceID: configTestService, BaseRevision: strings.Repeat("a", 64), IdempotencyKey: configAdminID(1), DeadlineAtMS: time.Now().Add(time.Hour).UnixMilli(), Changes: []shared.ConfigChange{{Operation: "create", Kind: "proxy", Name: "test-tunnel", Type: "stcp", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{{Path: "secretKey", Mode: "reference", Reference: configTestService}}}}, SecretValues: []shared.ConfigSecretInput{{Reference: configTestService, Value: "API-plaintext-sensitive"}}}
}
func configBody(t *testing.T, v any) string {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func decodeConfigResponse(t *testing.T, r *http.Response) configOperationResponse {
	t.Helper()
	var response configOperationResponse
	if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return response
}

type configAdminAgent struct {
	mu             sync.Mutex
	service        *Service
	operations     map[string]shared.ConfigOperationView
	calls          []string
	loseApply      bool
	offline        bool
	prepareCode    string
	refuseRollback bool
}

func installConfigAgent(s *Service) *configAdminAgent {
	a := &configAdminAgent{service: s, operations: map[string]shared.ConfigOperationView{}}
	s.configCoordinator.mu.Lock()
	s.configCoordinator.command = a.handle
	s.configCoordinator.mu.Unlock()
	return a
}
func (a *configAdminAgent) handle(ctx context.Context, node string, c shared.ConfigCommand) (shared.ConfigResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, c.Action)
	if a.offline {
		return shared.ConfigResult{}, ErrConfigUnavailable
	}
	r := shared.ConfigResult{ServiceID: configTestService, Action: c.Action, OperationID: c.OperationID, Code: "ok"}
	switch c.Action {
	case "inspect":
		r.Inventory = &shared.ConfigInventory{Revision: strings.Repeat("a", 64), ContextRevision: strings.Repeat("c", 64), State: "ready", Objects: []shared.ConfigObjectView{}, Dependencies: []shared.ConfigDependency{}, Issues: []shared.ConfigIssue{}}
	case "secret":
		active, err := a.service.control.ListActiveConfigOperations(ctx, "", 100)
		if err != nil || len(active) == 0 {
			return r, errors.New("secret sent before durable draft")
		}
		r.SecretReference = c.Secret.Reference
	case "prepare":
		if a.prepareCode != "" {
			r.Code = a.prepareCode
			return r, nil
		}
		saved, err := a.service.control.GetConfigOperation(ctx, c.OperationID)
		if err != nil || saved.State != "draft" {
			return r, errors.New("prepare sent before durable draft")
		}
		v := shared.ConfigOperationView{OperationID: c.OperationID, BaseRevision: c.BaseRevision, ContextRevision: strings.Repeat("c", 64), OldDigest: strings.Repeat("d", 64), CandidateDigest: strings.Repeat("b", 64), State: "prepared", CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli(), DeadlineAtMS: c.OperationDeadlineAtMS}
		a.operations[c.OperationID] = v
		r.Operation = &v
		r.Preview = &shared.ConfigPreview{BaseRevision: v.BaseRevision, ContextRevision: v.ContextRevision, CandidateDigest: v.CandidateDigest, Changes: []shared.ConfigChangePreview{}, Warnings: []shared.ConfigIssue{}}
	case "apply", "query", "cancel", "rollback":
		v, ok := a.operations[c.OperationID]
		if !ok {
			r.Code = "operation_not_found"
			return r, nil
		}
		switch c.Action {
		case "apply":
			saved, err := a.service.control.GetConfigOperation(ctx, c.OperationID)
			if err != nil || saved.State != "applying" {
				return r, errors.New("apply sent before CAS")
			}
			v.State = "confirmed"
			v.StorePersisted = true
			v.RuntimeApplied = true
			v.RuntimeLoaded = true
			v.ResourcesReady = true
		case "cancel":
			v.State = "cancelled"
		case "rollback":
			if v.MaterialsState == "expired" {
				r.Code = "operation_not_found"
				r.Operation = &v
				return r, nil
			}
			if a.refuseRollback {
				r.Code = "conflict"
				r.Operation = &v
				return r, nil
			}
			v.State = "rolled_back"
			v.StorePersisted = false
			v.RuntimeApplied = false
			v.RuntimeLoaded = true
			v.ResourcesReady = true
		}
		v.UpdatedAtMS = time.Now().UnixMilli()
		a.operations[c.OperationID] = v
		r.Operation = &v
		if c.Action == "apply" && a.loseApply {
			return shared.ConfigResult{}, context.DeadlineExceeded
		}
	}
	return r, nil
}
func TestConfigAdminAuthorizationAndSecretLimits(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	expectStatus(t, adminRequest(t, s, "GET", configAdminPath, "", nil, "", nil), 401)
	body := configBody(t, configAdminInput())
	expectStatus(t, adminRequest(t, s, "POST", configAdminPath+"/operations", body, cookie, "", nil), 403)
	expectStatus(t, adminRequest(t, s, "POST", configAdminPath+"/operations", body, cookie, session.CSRF, http.Header{"Origin": []string{"https://other.invalid"}}), 403)
	input := configAdminInput()
	for n := 0; n < 2; n++ {
		ref := configAdminID(byte(n))
		input.SecretValues = append(input.SecretValues, shared.ConfigSecretInput{Reference: ref, Value: "bounded-secret"})
		input.Changes = append(input.Changes, shared.ConfigChange{Operation: "create", Kind: "proxy", Name: "extra" + string(rune('a'+n)), Type: "stcp", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{{Path: "secretKey", Mode: "reference", Reference: ref}}})
	}
	expectStatus(t, adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, input), cookie, session.CSRF, nil), 400)
	agent.mu.Lock()
	calls := len(agent.calls)
	agent.mu.Unlock()
	if calls != 0 {
		t.Fatal("unauthorized/overlimit request reached Agent")
	}
	ops, err := s.control.ListConfigOperations(context.Background(), "1", 100)
	if err != nil || len(ops) != 0 {
		t.Fatal("invalid request created audit", ops, err)
	}
}
func TestConfigAdminPrepareApplyUnknownQueryAndPrivacy(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	input := configAdminInput()
	body := configBody(t, input)
	res := adminRequest(t, s, "POST", configAdminPath+"/operations", body, cookie, session.CSRF, nil)
	expectStatus(t, res, 201)
	created := decodeConfigResponse(t, res)
	if created.Operation == nil || created.Operation.State != "prepared" || created.Preview == nil || created.Agent == nil || created.AgentReceivedAtMS == 0 {
		t.Fatal("missing prepared facts", created)
	}
	serialized := configBody(t, created)
	if strings.Contains(serialized, input.SecretValues[0].Value) {
		t.Fatal("secret leaked in preview or events")
	}
	for _, e := range created.Events {
		if e.Actor != "operator" {
			t.Fatal("missing creator actor", e)
		}
	}
	res = adminRequest(t, s, "POST", configAdminPath+"/operations", body, cookie, session.CSRF, nil)
	expectStatus(t, res, 200)
	replayed := decodeConfigResponse(t, res)
	if !replayed.Replayed || replayed.Operation.OperationID != created.Operation.OperationID {
		t.Fatal("not idempotent", replayed)
	}
	changed := input
	changed.SecretValues = append([]shared.ConfigSecretInput{}, input.SecretValues...)
	changed.SecretValues[0].Value = "different-secret"
	expectStatus(t, adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, changed), cookie, session.CSRF, nil), 409)
	agent.mu.Lock()
	before := len(agent.calls)
	agent.loseApply = true
	agent.mu.Unlock()
	if before != 2 {
		t.Fatal("replay contacted Agent", before)
	}
	// Actions use the currently authenticated actor, not the operation creator.
	s.admin.mu.Lock()
	sess := s.admin.sessions[tokenHash(cookie.Value)]
	sess.Login = "approver"
	s.admin.sessions[tokenHash(cookie.Value)] = sess
	s.admin.mu.Unlock()
	action := configActionRequest{ExpectedVersion: created.Operation.Version, ContextRevision: created.Agent.ContextRevision, CandidateDigest: created.Operation.CandidateDigest}
	path := configAdminPath + "/operations/" + created.Operation.OperationID
	res = adminRequest(t, s, "POST", path+"/apply", configBody(t, action), cookie, session.CSRF, nil)
	expectStatus(t, res, 200)
	uncertain := decodeConfigResponse(t, res)
	if uncertain.Operation.State != "outcome_unknown" || uncertain.Code != "timeout" {
		t.Fatal("uncertain application falsely terminal", uncertain)
	}
	if uncertain.Events[len(uncertain.Events)-1].Actor != "approver" {
		t.Fatal("action actor lost")
	}
	expectStatus(t, adminRequest(t, s, "POST", path+"/apply", configBody(t, action), cookie, session.CSRF, nil), 409)
	res = adminRequest(t, s, "GET", path, "", cookie, "", nil)
	expectStatus(t, res, 200)
	confirmed := decodeConfigResponse(t, res)
	if confirmed.Operation.State != "confirmed" || !confirmed.Agent.StorePersisted || !confirmed.Agent.RuntimeApplied || !confirmed.Agent.RuntimeLoaded || !confirmed.Agent.ResourcesReady || confirmed.Agent.BusinessChecked {
		t.Fatal("actual Agent facts not recovered", confirmed)
	}
	agent.mu.Lock()
	applies := 0
	for _, c := range agent.calls {
		if c == "apply" {
			applies++
		}
	}
	agent.mu.Unlock()
	if applies != 1 {
		t.Fatal("recovery resent apply")
	}
	res = adminRequest(t, s, "GET", "/api/public/v1/nodes", "", nil, "", nil)
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(data), "candidate_digest") || strings.Contains(string(data), "API-plaintext-sensitive") || strings.Contains(string(data), "configuration") {
		t.Fatal("private configuration projected publicly", string(data))
	}
}
func TestConfigAdminOfflineHistoryAndNodeBinding(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	res := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
	expectStatus(t, res, 201)
	prepared := decodeConfigResponse(t, res)
	agent.mu.Lock()
	agent.offline = true
	agent.mu.Unlock()
	path := configAdminPath + "/operations/" + prepared.Operation.OperationID
	res = adminRequest(t, s, "GET", path, "", cookie, "", nil)
	expectStatus(t, res, 200)
	history := decodeConfigResponse(t, res)
	if history.Operation.State != "prepared" || history.Code != "unavailable" || len(history.Events) == 0 {
		t.Fatal("offline history lost", history)
	}
	expectStatus(t, adminRequest(t, s, "GET", strings.Replace(path, "nodes/1/", "nodes/2/", 1), "", cookie, "", nil), 404)
}
func TestConfigCoordinatorBoundedGatesAndRestartQueryOnly(t *testing.T) {
	s, _ := testMonitor(t)
	agent := installConfigAgent(s)
	releases := []func(){}
	for _, n := range []string{"1", "2", "3", "4"} {
		release, ok := s.configCoordinator.acquire(n)
		if !ok {
			t.Fatal("free gate rejected")
		}
		releases = append(releases, release)
	}
	if _, ok := s.configCoordinator.acquire("5"); ok {
		t.Fatal("global bound exceeded")
	}
	if _, ok := s.configCoordinator.acquire("1"); ok {
		t.Fatal("node lock reentered")
	}
	for _, release := range releases {
		release()
	}
	if len(s.configCoordinator.nodes) != 0 {
		t.Fatal("gate map leaked")
	}
	input := configAdminInput()
	id := configAdminID(1)
	o, _, err := s.control.CreateConfigOperation(context.Background(), control.CreateConfigOperationRequest{OperationID: id, NodeID: "1", ServiceID: input.ServiceID, BaseRevision: input.BaseRevision, Creator: "operator", DeadlineAtMS: input.DeadlineAtMS, IdempotencyKey: input.IdempotencyKey, RequestDigest: strings.Repeat("e", 64)})
	if err != nil {
		t.Fatal(err)
	}
	o = s.configUnknown(o, "connection_lost", "operator")
	agent.mu.Lock()
	agent.operations[id] = shared.ConfigOperationView{OperationID: id, BaseRevision: o.BaseRevision, ContextRevision: strings.Repeat("c", 64), OldDigest: strings.Repeat("d", 64), CandidateDigest: strings.Repeat("b", 64), State: "rolled_back", RuntimeLoaded: true, ResourcesReady: true, CreatedAtMS: o.CreatedAtMS, UpdatedAtMS: o.CreatedAtMS, DeadlineAtMS: o.DeadlineAtMS}
	agent.mu.Unlock()
	savedPath := s.cfg.DatabaseFile
	cfg := s.cfg
	s.Close()
	if savedPath == "" {
		t.Fatal("missing DB")
	}
	restarted, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	next := installConfigAgent(restarted)
	next.mu.Lock()
	next.operations = agent.operations
	next.mu.Unlock()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		observed, e := restarted.control.GetConfigOperation(context.Background(), id)
		if e == nil && observed.State == "rolled_back" {
			events, e := restarted.control.ListConfigOperationEvents(context.Background(), id)
			if e != nil || events[len(events)-1].Actor != configReconcileActor {
				t.Fatal("recovery actor", events, e)
			}
			next.mu.Lock()
			defer next.mu.Unlock()
			for _, action := range next.calls {
				if action != "query" {
					t.Fatal("startup performed write", action)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("restart did not query durable active operation")
}

func TestConfigAdminPureValidationFailureReleasesReservation(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	agent.mu.Lock()
	agent.prepareCode = "invalid_field"
	agent.mu.Unlock()
	response := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
	expectStatus(t, response, 201)
	failed := decodeConfigResponse(t, response)
	if failed.Code != "invalid_field" || failed.Operation.State != "rejected" {
		t.Fatal("pure validation failure became uncertain", failed)
	}
	active, err := s.control.ListActiveConfigOperations(context.Background(), "", 16)
	if err != nil || len(active) != 0 {
		t.Fatal("validation retained reservation", active, err)
	}
	if failed.Events[len(failed.Events)-1].Code != "validation_failed" {
		t.Fatal("pure error audit did not retain fixed reason")
	}
	for _, code := range []string{"invalid_field", "field_read_only", "advanced_read_only", "invalid_secret", "invalid_type", "unsupported_dependency"} {
		if configPrepareFailureState(code) != "rejected" {
			t.Fatal("missing pure rejection", code)
		}
	}
	for _, code := range []string{"ownership_conflict", "revision_conflict", "source_drift"} {
		if configPrepareFailureState(code) != "conflict" {
			t.Fatal("missing pure conflict", code)
		}
	}
	for _, code := range []string{"timeout", "write_failed", "outcome_unknown", "unavailable", "too_large"} {
		if configPrepareFailureState(code) != "outcome_unknown" {
			t.Fatal("uncertain effect treated as rejection", code)
		}
	}
}
func TestConfigAdminStaleRollbackPreservesConfirmedAndReleasesLease(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	prepareApply := func() configOperationResponse {
		r := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
		expectStatus(t, r, 201)
		p := decodeConfigResponse(t, r)
		action := configActionRequest{ExpectedVersion: p.Operation.Version, ContextRevision: p.Agent.ContextRevision, CandidateDigest: p.Operation.CandidateDigest}
		r = adminRequest(t, s, "POST", configAdminPath+"/operations/"+p.Operation.OperationID+"/apply", configBody(t, action), cookie, session.CSRF, nil)
		expectStatus(t, r, 200)
		p = decodeConfigResponse(t, r)
		if p.Operation.State != "confirmed" {
			t.Fatal("apply failed", p)
		}
		return p
	}
	a := prepareApply()
	_ = prepareApply()
	agent.mu.Lock()
	agent.refuseRollback = true
	agent.mu.Unlock()
	action := configActionRequest{ExpectedVersion: a.Operation.Version, ContextRevision: a.Agent.ContextRevision, CandidateDigest: a.Operation.CandidateDigest}
	r := adminRequest(t, s, "POST", configAdminPath+"/operations/"+a.Operation.OperationID+"/rollback", configBody(t, action), cookie, session.CSRF, nil)
	expectStatus(t, r, 200)
	refused := decodeConfigResponse(t, r)
	if refused.Code != "conflict" || refused.Operation.State != "confirmed" || !refused.Agent.StorePersisted {
		t.Fatal("stale rollback stranded historical operation", refused)
	}
	active, err := s.control.ListActiveConfigOperations(context.Background(), "", 16)
	if err != nil || len(active) != 0 {
		t.Fatal("stale rollback retained lease", active, err)
	}
}
func TestConfigAdminTwoNewSecretsAndOpaqueReferenceReuse(t *testing.T) {
	input := configAdminInput()
	ref := configAdminID(1)
	input.SecretValues = append(input.SecretValues, shared.ConfigSecretInput{Reference: ref, Value: "second"})
	input.Changes = append(input.Changes, shared.ConfigChange{Operation: "create", Kind: "proxy", Name: "second", Type: "stcp", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{{Path: "secretKey", Mode: "reference", Reference: ref}}})
	for _, name := range []string{"reuse-one", "reuse-two", "reuse-three"} {
		input.Changes = append(input.Changes, shared.ConfigChange{Operation: "create", Kind: "proxy", Name: name, Type: "stcp", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{{Path: "secretKey", Mode: "reference", Reference: ref}}})
	}
	if err := validateConfigCreate(input); err != nil {
		t.Fatal("two uploads and opaque reuse rejected", err)
	}
}
func TestConfigAdminPublicSSEExcludesOperationAndSecrets(t *testing.T) {
	s, _, admin := testAdmin(t)
	installConfigAgent(s)
	cookie, session := login(t, s, admin)
	res := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
	expectStatus(t, res, 201)
	created := decodeConfigResponse(t, res)
	s.publishSnapshot(time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+"/events/public", nil)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	reader := bufio.NewReader(stream.Body)
	_, err = reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	data, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"candidate_digest", "operation_id", "agent_received_at_ms", "secret_values", "API-plaintext-sensitive", created.Operation.OperationID} {
		if strings.Contains(data, private) {
			t.Fatal("public SSE exposed management data", private)
		}
	}
}

func TestConfigAdminPatchPrimitivesNullAndStrictBoundaries(t *testing.T) {
	for _, tt := range []struct{ name, path, value string }{
		{"number", "remotePort", "9001"}, {"null", "remotePort", "null"}, {"boolean", "transport.useEncryption", "true"}, {"string", "localIP", `"127.0.0.1"`}, {"array", "customDomains", `["example.invalid"]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, admin := testAdmin(t)
			installConfigAgent(s)
			cookie, session := login(t, s, admin)
			input := configAdminInput()
			input.Changes[0].Fields = []shared.ConfigFieldPatch{{Path: tt.path, Value: json.RawMessage(tt.value)}}
			res := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, input), cookie, session.CSRF, nil)
			expectStatus(t, res, 201)
			response := decodeConfigResponse(t, res)
			if response.Operation.State != "prepared" {
				t.Fatal("valid primitive was not accepted", response)
			}
		})
	}
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	for _, value := range []string{`{"secretKey":"hidden"}`, `[{"secretKey":"hidden"}]`, `[["nested"]]`, `[null]`} {
		input := configAdminInput()
		input.Changes[0].Fields = []shared.ConfigFieldPatch{{Path: "remotePort", Value: json.RawMessage(value)}}
		expectStatus(t, adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, input), cookie, session.CSRF, nil), 400)
	}
	input := configAdminInput()
	base := configBody(t, input)
	for _, body := range []string{strings.Replace(base, `"service_id":`, `"hidden_secret":"secret","service_id":`, 1), strings.Replace(base, `"service_id":`, `"service_id":"`+configTestService+`","service_id":`, 1), strings.Replace(base, `"changes":[`, `"changes":null,"ignored":[`, 1)} {
		expectStatus(t, adminRequest(t, s, "POST", configAdminPath+"/operations", body, cookie, session.CSRF, nil), 400)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.calls) != 0 {
		t.Fatal("malformed payload reached Agent")
	}
}

func TestConfigAdminCloneAuditRetainsSource(t *testing.T) {
	summary := configChangeSummary([]shared.ConfigChange{{Operation: "create", Kind: "visitor", Name: "renamed", Type: "stcp", CloneFrom: "original", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{}}})
	if len(summary) != 1 || summary[0].CloneFrom != "original" {
		t.Fatal("HTTP clone source lost before audit")
	}
}

func TestConfigAdminUndispatchedPrepareDoesNotStrandOfflineNode(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	input := configAdminInput()
	input.SecretValues = []shared.ConfigSecretInput{}
	// Use the actual transport with no connected Agent, not a mock whose error
	// might mean a queued command lost its response.
	r := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, input), cookie, session.CSRF, nil)
	expectStatus(t, r, http.StatusCreated)
	result := decodeConfigResponse(t, r)
	if result.Code != "unavailable" || result.Operation.State != "rejected" || result.Agent != nil {
		t.Fatal("undispatched prepare retained an ambiguous operation")
	}
	active, err := s.control.ListActiveConfigOperations(context.Background(), "", 16)
	if err != nil || len(active) != 0 {
		t.Fatal("offline node lease was stranded", err)
	}
	if len(result.Events) < 2 || result.Events[0].State != "draft" {
		t.Fatal("rejected offline request omitted its audit")
	}
}

func TestConfigAdminCapacityRejectionReleasesLease(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	agent.mu.Lock()
	agent.prepareCode = "managed_capacity"
	agent.mu.Unlock()
	r := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
	expectStatus(t, r, 201)
	out := decodeConfigResponse(t, r)
	if out.Code != "managed_capacity" || out.Operation.State != "rejected" || out.Agent != nil {
		t.Fatal("capacity rejection is uncertain", out)
	}
	active, err := s.control.ListActiveConfigOperations(context.Background(), "", 16)
	if err != nil || len(active) != 0 {
		t.Fatal("capacity retained lease", err)
	}
}

func TestConfigAdminExpiredRollbackNeverStrandsConfirmedOperation(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "first_observation", true: "known_expired"}[known], func(t *testing.T) {
			s, _, admin := testAdmin(t)
			agent := installConfigAgent(s)
			cookie, session := login(t, s, admin)
			r := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
			expectStatus(t, r, 201)
			out := decodeConfigResponse(t, r)
			action := configActionRequest{ExpectedVersion: out.Operation.Version, ContextRevision: out.Agent.ContextRevision, CandidateDigest: out.Operation.CandidateDigest}
			path := configAdminPath + "/operations/" + out.Operation.OperationID
			r = adminRequest(t, s, "POST", path+"/apply", configBody(t, action), cookie, session.CSRF, nil)
			expectStatus(t, r, 200)
			out = decodeConfigResponse(t, r)
			agent.mu.Lock()
			v := agent.operations[out.Operation.OperationID]
			at := v.UpdatedAtMS + 1
			v.MaterialsState = "expired"
			v.MaterialsExpiredAtMS = &at
			v.MaterialsExpiryReason = "ttl"
			agent.operations[out.Operation.OperationID] = v
			agent.mu.Unlock()
			if known {
				updated, code := s.queryConfigOperation(context.Background(), out.Operation, "system:test")
				if code != "ok" {
					t.Fatal(code)
				}
				out.Operation = updated
				out.Agent = updated.Agent
			}
			agent.mu.Lock()
			before := len(agent.calls)
			agent.mu.Unlock()
			action.ExpectedVersion = out.Operation.Version
			r = adminRequest(t, s, "POST", path+"/rollback", configBody(t, action), cookie, session.CSRF, nil)
			expectStatus(t, r, 200)
			out = decodeConfigResponse(t, r)
			if out.Code != "operation_not_found" || out.Operation.State != "confirmed" || out.Agent.MaterialsState != "expired" || !out.Agent.StorePersisted {
				t.Fatal("expired rollback corrupted terminal facts", out)
			}
			active, err := s.control.ListActiveConfigOperations(context.Background(), "", 16)
			if err != nil || len(active) != 0 {
				t.Fatal("expired rollback retained lease", err)
			}
			agent.mu.Lock()
			after := len(agent.calls)
			agent.mu.Unlock()
			if known && before != after {
				t.Fatal("known expiry still dispatched rollback")
			}
			retained := *out.Agent
			retained.MaterialsState = "retained"
			retained.MaterialsExpiredAtMS = nil
			retained.MaterialsExpiryReason = ""
			out.Operation.Agent = out.Agent
			if configResultMatches(out.Operation, shared.ConfigResult{ServiceID: out.Operation.ServiceID, OperationID: out.Operation.OperationID, Operation: &retained}) {
				t.Fatal("expired materials resurrected")
			}
		})
	}
}

func TestConfigRollbackPreflightWaitsForReadCapacity(t *testing.T) {
	for _, alwaysBusy := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity_recovers", true: "capacity_stays_busy"}[alwaysBusy], func(t *testing.T) {
			s, _, admin := testAdmin(t)
			a := installConfigAgent(s)
			cookie, session := login(t, s, admin)
			response := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, configAdminInput()), cookie, session.CSRF, nil)
			expectStatus(t, response, 201)
			prepared := decodeConfigResponse(t, response)
			path := configAdminPath + "/operations/" + prepared.Operation.OperationID
			action := configActionRequest{ExpectedVersion: prepared.Operation.Version, ContextRevision: prepared.Agent.ContextRevision, CandidateDigest: prepared.Operation.CandidateDigest}
			response = adminRequest(t, s, "POST", path+"/apply", configBody(t, action), cookie, session.CSRF, nil)
			expectStatus(t, response, 200)
			confirmed := decodeConfigResponse(t, response)
			queries := 0
			s.configCoordinator.mu.Lock()
			s.configCoordinator.command = func(ctx context.Context, node string, c shared.ConfigCommand) (shared.ConfigResult, error) {
				if c.Action == "query" {
					queries++
					if queries == 1 || alwaysBusy {
						return shared.ConfigResult{}, errors.Join(ErrConfigBusy, ErrConfigNotSent)
					}
				}
				return a.handle(ctx, node, c)
			}
			s.configCoordinator.mu.Unlock()
			action = configActionRequest{ExpectedVersion: confirmed.Operation.Version, ContextRevision: confirmed.Agent.ContextRevision, CandidateDigest: confirmed.Operation.CandidateDigest}
			response = adminRequest(t, s, "POST", path+"/rollback", configBody(t, action), cookie, session.CSRF, nil)
			expectStatus(t, response, 200)
			result := decodeConfigResponse(t, response)
			a.mu.Lock()
			rollbacks := 0
			for _, call := range a.calls {
				if call == "rollback" {
					rollbacks++
				}
			}
			a.mu.Unlock()
			if alwaysBusy {
				if queries != 3 || rollbacks != 0 || result.Code != "busy" || result.Operation.State != "confirmed" || result.Operation.Version != confirmed.Operation.Version {
					t.Fatal("busy preflight changed intent or exceeded retry bound", result.Code, queries, rollbacks)
				}
			} else if queries != 2 || rollbacks != 1 || result.Operation.State != "rolled_back" {
				t.Fatal("read capacity recovery repeated or failed rollback", result.Code, queries, rollbacks)
			}
			active, err := s.control.ListActiveConfigOperations(context.Background(), "", 16)
			if err != nil || len(active) != 0 {
				t.Fatal("preflight stranded an operation lease", err)
			}
		})
	}
}

func TestConfigRollbackPreflightHonorsCancellation(t *testing.T) {
	s, _, _ := testAdmin(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	s.configCoordinator.command = func(context.Context, string, shared.ConfigCommand) (shared.ConfigResult, error) {
		calls++
		cancel()
		return shared.ConfigResult{}, ErrConfigBusy
	}
	op := &control.ConfigOperation{NodeID: "1"}
	got, code := s.queryConfigBeforeRollback(ctx, op)
	if got != op || code != "timeout" || calls != 1 {
		t.Fatal("cancelled preflight retried or changed state", code, calls)
	}
}
