package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const maxConfigSecretUploads = 2

type configCreateRequest struct {
	ServiceID      string                     `json:"service_id"`
	BaseRevision   string                     `json:"base_revision"`
	IdempotencyKey string                     `json:"idempotency_key"`
	DeadlineAtMS   int64                      `json:"deadline_at_ms"`
	Changes        []shared.ConfigChange      `json:"changes"`
	SecretValues   []shared.ConfigSecretInput `json:"secret_values"`
}
type configActionRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	ContextRevision string `json:"context_revision"`
	CandidateDigest string `json:"candidate_digest"`
}
type configOperationResponse struct {
	Code                 string                         `json:"code"`
	Operation            *control.ConfigOperation       `json:"operation"`
	Events               []control.ConfigOperationEvent `json:"events"`
	Agent                *shared.ConfigOperationView    `json:"agent"`
	AgentReceivedAtMS    int64                          `json:"agent_received_at_ms"`
	Preview              *shared.ConfigPreview          `json:"preview,omitempty"`
	EventsTruncated      bool                           `json:"events_truncated"`
	Replayed             bool                           `json:"replayed"`
	ServerRestorePending bool                           `json:"server_restore_pending"`
}

func (s *Service) configResponse(ctx context.Context, o *control.ConfigOperation, code string, preview *shared.ConfigPreview, replayed bool) (configOperationResponse, error) {
	events, err := s.control.ListRecentConfigOperationEvents(ctx, o.OperationID, 256)
	return configOperationResponse{Code: code, Operation: o, Events: events, Agent: o.Agent, AgentReceivedAtMS: o.AgentReceivedAtMS, Preview: preview, Replayed: replayed, EventsTruncated: o.Version > int64(len(events)), ServerRestorePending: s.serverRestorePending}, err
}
func configHTTPError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "unavailable"
	switch {
	case errors.Is(err, ErrServerRestorePending):
		status, code = 409, "server_restore_pending"
	case errors.Is(err, control.ErrAuditCapacity):
		status, code = 503, "audit_capacity"
	case errors.Is(err, control.ErrInvalid), errors.Is(err, ErrConfigInvalid):
		status, code = 400, "invalid_request"
	case errors.Is(err, control.ErrNotFound):
		status, code = 404, "operation_not_found"
	case errors.Is(err, control.ErrConflict), errors.Is(err, ErrConfigServiceMismatch):
		status, code = 409, "conflict"
	case errors.Is(err, ErrConfigBusy):
		status, code = 409, "busy"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, code = 504, "timeout"
	}
	adminJSON(w, status, map[string]string{"code": code})
}
func validateConfigCreate(input configCreateRequest) error {
	if !shared.ValidConfigOperationID(input.ServiceID) || !shared.ValidConfigOperationID(input.IdempotencyKey) || !shared.ValidConfigDigest(input.BaseRevision) || len(input.SecretValues) > maxConfigSecretUploads {
		return ErrConfigInvalid
	}
	if shared.ValidateConfigChanges(input.Changes) != nil || len(input.Changes) == 0 {
		return ErrConfigInvalid
	}
	refs := map[string]bool{}
	for _, c := range input.Changes {
		for _, v := range c.Secrets {
			if v.Mode == "reference" {
				refs[v.Reference] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, v := range input.SecretValues {
		if !shared.ValidConfigOperationID(v.Reference) || !refs[v.Reference] || seen[v.Reference] || len(v.Value) < 1 || len(v.Value) > 1024 || !utf8.ValidString(v.Value) || strings.ContainsAny(v.Value, "\x00\r\n") {
			return ErrConfigInvalid
		}
		seen[v.Reference] = true
	}
	return nil
}
func configChangeSummary(changes []shared.ConfigChange) []control.ConfigOperationChange {
	result := make([]control.ConfigOperationChange, 0, len(changes))
	for _, c := range changes {
		fields := []string{}
		for _, f := range c.Fields {
			fields = append(fields, f.Path)
		}
		for _, f := range c.Secrets {
			fields = append(fields, f.Path)
		}
		result = append(result, control.ConfigOperationChange{Kind: c.Kind, Name: c.Name, Action: c.Operation, Fields: fields, CloneFrom: c.CloneFrom})
	}
	return result
}
func (s *Service) handleConfigAdmin(w http.ResponseWriter, r *http.Request, path, actor string) bool {
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[0] != "nodes" || parts[2] != "configuration" {
		return false
	}
	if len(parts) > 6 || !validNodeID(parts[1]) {
		http.NotFound(w, r)
		return true
	}
	nodeID := parts[1]
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if len(parts) == 3 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return true
		}
		release, ok := s.configCoordinator.acquire(nodeID)
		if !ok {
			configHTTPError(w, ErrConfigBusy)
			return true
		}
		defer release()
		result, err := s.configCoordinator.invoke(ctx, nodeID, shared.ConfigCommand{Action: "inspect"})
		if err != nil {
			configHTTPError(w, err)
			return true
		}
		if result.Code != "ok" || result.Inventory == nil {
			adminJSON(w, 503, map[string]string{"code": result.Code})
			return true
		}
		if err := s.control.ObserveAuditContext(ctx, nodeID, result.ServiceID, result.Inventory.ContextRevision, actor); err != nil {
			configHTTPError(w, err)
			return true
		}
		adminJSON(w, 200, struct {
			Code                 string                  `json:"code"`
			NodeID               string                  `json:"node_id"`
			ServiceID            string                  `json:"service_id"`
			ReceivedAtMS         int64                   `json:"received_at_ms"`
			Inventory            *shared.ConfigInventory `json:"inventory"`
			ServerRestorePending bool                    `json:"server_restore_pending"`
		}{"ok", nodeID, result.ServiceID, time.Now().UnixMilli(), result.Inventory, s.serverRestorePending})
		return true
	}
	if parts[3] == "restore" {
		s.handleConfigRestore(w, r, ctx, nodeID, parts, actor)
		return true
	}
	if parts[3] != "operations" {
		http.NotFound(w, r)
		return true
	}
	if len(parts) == 4 {
		switch r.Method {
		case http.MethodGet:
			operations, err := s.control.ListConfigOperations(ctx, nodeID, 100)
			if err != nil {
				configHTTPError(w, err)
			} else {
				adminJSON(w, 200, map[string]any{"operations": operations})
			}
		case http.MethodPost:
			s.createConfigOperation(w, r, ctx, nodeID, actor)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return true
	}
	if !shared.ValidConfigOperationID(parts[4]) {
		configHTTPError(w, control.ErrNotFound)
		return true
	}
	o, err := s.control.GetConfigOperation(ctx, parts[4])
	if err != nil {
		configHTTPError(w, err)
		return true
	}
	if o.NodeID != nodeID {
		configHTTPError(w, control.ErrNotFound)
		return true
	}
	if len(parts) == 5 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return true
		}
		code := "ok"
		if shared.ConfigOperationActive(o.State) || o.Agent != nil {
			if release, ok := s.configCoordinator.acquire(nodeID); ok {
				queryCtx, done := context.WithTimeout(ctx, 2*time.Second)
				o, code = s.queryConfigOperation(queryCtx, o, configReconcileActor)
				done()
				release()
			} else {
				code = "busy"
			}
		}
		response, e := s.configResponse(ctx, o, code, nil, false)
		if e != nil {
			configHTTPError(w, e)
		} else {
			adminJSON(w, 200, response)
		}
		return true
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return true
	}
	if parts[5] != "apply" && parts[5] != "rollback" && parts[5] != "cancel" {
		http.NotFound(w, r)
		return true
	}
	s.actionConfigOperation(w, r, ctx, o, parts[5], actor)
	return true
}

// Retry only an explicit local busy rejection, which guarantees the command
// was not queued. Never retry an operation after a timeout or lost response.
func (s *Service) pacedConfigCommand(ctx context.Context, node string, c shared.ConfigCommand, operation *control.ConfigOperation, actor string) (shared.ConfigResult, error) {
	if serverRestoreActionBlocked(s.serverRestorePending, c.Action) {
		return shared.ConfigResult{}, errors.Join(ErrConfigNotSent, ErrServerRestorePending)
	}
	for attempts := 0; attempts < 3; attempts++ {
		kind, code := "dispatch", "dispatch_"+c.Action
		if attempts > 0 {
			kind, code = "retry", "busy"
		}
		if err := s.recordConfigurationAudit(ctx, operation, kind, code, actor, nil, nil); err != nil {
			return shared.ConfigResult{}, errors.Join(ErrConfigNotSent, err)
		}
		r, err := s.configCoordinator.invoke(ctx, node, c)
		if !errors.Is(err, ErrConfigBusy) || attempts == 2 {
			return r, err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return shared.ConfigResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	return shared.ConfigResult{}, ErrConfigBusy
}
func (s *Service) createConfigOperation(w http.ResponseWriter, r *http.Request, ctx context.Context, node, actor string) {
	if s.serverRestorePending {
		configHTTPError(w, ErrServerRestorePending)
		return
	}
	var input configCreateRequest
	if !decodeAdmin(w, r, &input) {
		return
	}
	if err := validateConfigCreate(input); err != nil {
		configHTTPError(w, err)
		return
	}
	release, ok := s.configCoordinator.acquire(node)
	if !ok {
		configHTTPError(w, ErrConfigBusy)
		return
	}
	defer release()
	id, err := shared.NewConfigOperationID()
	if err != nil {
		configHTTPError(w, err)
		return
	}
	data, _ := json.Marshal(input)
	digest := sha256.Sum256(data)
	clear(data)
	o, replayed, err := s.control.CreateConfigOperation(ctx, control.CreateConfigOperationRequest{OperationID: id, NodeID: node, ServiceID: input.ServiceID, BaseRevision: input.BaseRevision, Creator: actor, DeadlineAtMS: input.DeadlineAtMS, IdempotencyKey: input.IdempotencyKey, RequestDigest: hex.EncodeToString(digest[:]), Changes: configChangeSummary(input.Changes)})
	if err != nil {
		configHTTPError(w, err)
		return
	}
	if replayed {
		response, e := s.configResponse(ctx, o, "ok", nil, true)
		if e != nil {
			configHTTPError(w, e)
		} else {
			adminJSON(w, 200, response)
		}
		return
	}
	code := "ok"
	var preview *shared.ConfigPreview
	for i := range input.SecretValues {
		secret := input.SecretValues[i]
		result, e := s.pacedConfigCommand(ctx, node, shared.ConfigCommand{Action: "secret", ServiceID: input.ServiceID, Secret: &secret}, o, actor)
		input.SecretValues[i].Value = ""
		secret.Value = ""
		if e != nil {
			code = configFailureCode(e)
			break
		}
		if result.Code != "ok" {
			code = result.Code
			break
		}
		o, e = s.configTransition(ctx, o, "draft", "secret_stored", actor, nil)
		if e != nil {
			configHTTPError(w, e)
			return
		}
	}
	if code == "ok" {
		// A deadline shorter than the command must be clipped before transport
		// validation; persistent operation deadlines are never silently extended.
		prepareCtx, done := context.WithDeadline(ctx, time.UnixMilli(input.DeadlineAtMS))
		result, e := s.pacedConfigCommand(prepareCtx, node, shared.ConfigCommand{Action: "prepare", ServiceID: input.ServiceID, OperationID: o.OperationID, BaseRevision: input.BaseRevision, IdempotencyKey: input.IdempotencyKey, OperationDeadlineAtMS: input.DeadlineAtMS, Changes: input.Changes}, o, actor)
		done()
		if e != nil {
			code = configFailureCode(e)
			if errors.Is(e, ErrConfigNotSent) {
				// A fresh prepare that never entered the queue cannot have created
				// an Agent transaction. Do not strand an offline/old node's lease.
				auditCtx, cancelAudit := context.WithTimeout(s.ctx, time.Second)
				updated, auditErr := s.configTransition(auditCtx, o, "rejected", code, actor, nil)
				cancelAudit()
				if auditErr == nil {
					o = updated
				} else {
					o = s.configUnknown(o, code, actor)
				}
			} else {
				o = s.configUnknown(o, code, actor)
			}
		} else {
			code = result.Code
			if result.Operation != nil {
				updated, e := s.recordConfigResult(ctx, o, result, actor)
				o = updated
				if e == nil {
					preview = result.Preview
					if err := s.recordConfigurationPreview(ctx, o, preview, actor, configChangeSummary(input.Changes)); err != nil {
						configHTTPError(w, err)
						return
					}
				} else {
					code = "conflict"
					o = s.configUnknown(o, code, actor)
				}
			} else {
				terminal := configPrepareFailureState(code)
				updated, e := s.configTransition(ctx, o, terminal, code, actor, nil)
				if e == nil {
					o = updated
				} else {
					o = s.configUnknown(o, code, actor)
				}
			}
		}
	} else { // No prepare was sent; uploaded local references are not configuration.
		updated, e := s.configTransition(ctx, o, "rejected", code, actor, nil)
		if e == nil {
			o = updated
		}
	}
	// The request deadline may already have elapsed; return durable metadata using
	// a short service-owned read without ever retrying a write command.
	replyCtx, done := context.WithTimeout(s.ctx, time.Second)
	defer done()
	response, e := s.configResponse(replyCtx, o, code, preview, false)
	if e != nil {
		configHTTPError(w, e)
	} else {
		adminJSON(w, http.StatusCreated, response)
	}
}
func (s *Service) actionConfigOperation(w http.ResponseWriter, r *http.Request, ctx context.Context, o *control.ConfigOperation, action, actor string) {
	if serverRestoreActionBlocked(s.serverRestorePending, action) {
		configHTTPError(w, ErrServerRestorePending)
		return
	}
	var input configActionRequest
	if !decodeAdmin(w, r, &input) {
		return
	}
	if input.ExpectedVersion < 1 || (!shared.ValidConfigDigest(input.ContextRevision) && input.ContextRevision != "") || (!shared.ValidConfigDigest(input.CandidateDigest) && input.CandidateDigest != "") {
		configHTTPError(w, ErrConfigInvalid)
		return
	}
	release, ok := s.configCoordinator.acquire(o.NodeID)
	if !ok {
		configHTTPError(w, ErrConfigBusy)
		return
	}
	defer release()
	// Re-read after the gate: another request may have advanced the operation
	// between routing and acquiring its node reservation.
	current, err := s.control.GetConfigOperation(ctx, o.OperationID)
	if err != nil {
		configHTTPError(w, err)
		return
	}
	o = current
	if o.Version != input.ExpectedVersion || input.CandidateDigest != o.CandidateDigest || (o.Agent != nil && input.ContextRevision != o.Agent.ContextRevision) {
		configHTTPError(w, control.ErrConflict)
		return
	}
	if action == "rollback" && o.State == "confirmed" && o.Agent != nil && o.Agent.MaterialsState != "expired" && o.Agent.MaterialsState != "context_changed" {
		var code string
		o, code = s.queryConfigOperation(ctx, o, configReconcileActor)
		if code != "ok" || o.State != "confirmed" {
			response, e := s.configResponse(ctx, o, code, nil, false)
			if e != nil {
				configHTTPError(w, e)
			} else {
				adminJSON(w, 200, response)
			}
			return
		}
	}
	if action == "rollback" && o.Agent != nil && (o.Agent.MaterialsState == "expired" || o.Agent.MaterialsState == "context_changed") {
		code := "operation_not_found"
		if o.Agent.MaterialsState == "context_changed" {
			code = "context_changed"
		}
		response, err := s.configResponse(ctx, o, code, nil, false)
		if err != nil {
			configHTTPError(w, err)
		} else {
			adminJSON(w, 200, response)
		}
		return
	}
	state := o.State
	switch action {
	case "apply":
		if o.State != "prepared" || o.Agent == nil {
			configHTTPError(w, control.ErrConflict)
			return
		}
		state = "applying"
	case "rollback":
		if (o.State != "confirmed" && o.State != "rollback_failed" && o.State != "outcome_unknown") || o.Agent == nil {
			configHTTPError(w, control.ErrConflict)
			return
		}
		state = "rolling_back"
	case "cancel":
		if o.State != "prepared" && o.State != "draft" && o.State != "outcome_unknown" {
			configHTTPError(w, control.ErrConflict)
			return
		}
		state = "outcome_unknown"
	}
	if state != o.State || action == "cancel" {
		o, err = s.configTransition(ctx, o, state, state, actor, nil)
		if err != nil {
			configHTTPError(w, err)
			return
		}
	}
	result, err := s.pacedConfigCommand(ctx, o.NodeID, shared.ConfigCommand{Action: action, ServiceID: o.ServiceID, OperationID: o.OperationID, BaseRevision: o.BaseRevision, ContextRevision: input.ContextRevision, CandidateDigest: input.CandidateDigest}, o, actor)
	code := "ok"
	if err != nil {
		code = configFailureCode(err)
		o = s.configUnknown(o, code, actor)
	} else if result.Operation != nil {
		code = result.Code
		updated, e := s.recordConfigResult(ctx, o, result, actor)
		o = updated
		if e != nil {
			code = "conflict"
			o = s.configUnknown(o, code, actor)
		}
	} else {
		code = result.Code
		o = s.configUnknown(o, code, actor)
	}
	replyCtx, done := context.WithTimeout(s.ctx, time.Second)
	defer done()
	response, e := s.configResponse(replyCtx, o, code, nil, false)
	if e != nil {
		configHTTPError(w, e)
	} else {
		adminJSON(w, 200, response)
	}
}

// These are emitted by the pure adapter before Engine.Prepare can persist a
// journal. Storage/transport/runtime errors remain uncertain without facts.
func configPrepareFailureState(code string) string {
	switch code {
	case "conflict", "source_conflict", "source_drift", "revision_conflict", "ownership_conflict", "dependency_conflict", "source_changed", "already_exists":
		return "conflict"
	case "invalid_request", "unauthorized", "unsupported", "expired", "invalid_config", "validation_failed", "managed_capacity":
		return "rejected"
	}
	if shared.ValidConfigIssueCode(code) && code != "native_warning" && code != "start_filtered" {
		return "rejected"
	}
	return "outcome_unknown"
}
