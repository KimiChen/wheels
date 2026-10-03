package monitor

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type configRestoreRequest struct {
	ServiceID                 string `json:"service_id"`
	Epoch                     string `json:"epoch"`
	ManifestDigest            string `json:"manifest_digest"`
	ContextRevision           string `json:"context_revision"`
	StoreDigest               string `json:"store_digest"`
	ExpectedActiveOperationID string `json:"expected_active_operation_id"`
	ExpectedActiveVersion     int64  `json:"expected_active_version"`
}
type configRestoreResponse struct {
	Code            string                   `json:"code"`
	NodeID          string                   `json:"node_id"`
	ServiceID       string                   `json:"service_id"`
	ReceivedAtMS    int64                    `json:"received_at_ms"`
	Restore         *shared.RestoreInfo      `json:"restore"`
	Receipt         *control.ConfigRestore   `json:"receipt"`
	ActiveOperation *control.ConfigOperation `json:"active_operation"`
}

func restoreRequestValid(input configRestoreRequest) bool {
	return shared.ValidConfigOperationID(input.ServiceID) && shared.ValidConfigOperationID(input.Epoch) && shared.ValidConfigDigest(input.ManifestDigest) && shared.ValidConfigDigest(input.ContextRevision) && shared.ValidConfigDigest(input.StoreDigest) && ((input.ExpectedActiveOperationID == "" && input.ExpectedActiveVersion == 0) || (shared.ValidConfigOperationID(input.ExpectedActiveOperationID) && input.ExpectedActiveVersion > 0))
}
func restoreRequestMatches(input configRestoreRequest, r shared.RestoreResult) bool {
	value := r.Restore
	return r.Code == "ok" && r.ServiceID == input.ServiceID && value != nil && value.Validate() == nil && value.Epoch == input.Epoch && value.ManifestDigest == input.ManifestDigest && value.ContextRevision == input.ContextRevision && value.StoreDigest == input.StoreDigest && (value.State == "verified" || value.State == "acknowledged" || value.State == "confirmed")
}
func (s *Service) restoreResponse(ctx context.Context, node string, r shared.RestoreResult, code string) (configRestoreResponse, error) {
	out := configRestoreResponse{Code: code, NodeID: node, ServiceID: r.ServiceID, ReceivedAtMS: time.Now().UnixMilli(), Restore: r.Restore}
	if r.Restore != nil && r.Restore.Epoch != "" {
		receipt, err := s.control.GetConfigRestore(ctx, node, r.Restore.Epoch)
		if err != nil && !errors.Is(err, control.ErrNotFound) {
			return out, err
		}
		out.Receipt = receipt
	}
	active, err := s.control.GetActiveConfigOperation(ctx, node)
	if err != nil && !errors.Is(err, control.ErrNotFound) {
		return out, err
	}
	out.ActiveOperation = active
	return out, nil
}
func (s *Service) handleConfigRestore(w http.ResponseWriter, r *http.Request, ctx context.Context, node string, parts []string, actor string) {
	inspect := len(parts) == 4 && r.Method == http.MethodGet
	acknowledge := len(parts) == 5 && parts[4] == "acknowledge" && r.Method == http.MethodPost
	if !inspect && !acknowledge {
		http.NotFound(w, r)
		return
	}
	var input configRestoreRequest
	if acknowledge && (!decodeAdmin(w, r, &input)) {
		return
	}
	if acknowledge && !restoreRequestValid(input) {
		configHTTPError(w, ErrConfigInvalid)
		return
	}
	release, ok := s.configCoordinator.acquire(node)
	if !ok {
		configHTTPError(w, ErrConfigBusy)
		return
	}
	defer release()
	identity, err := s.control.Get(ctx, node)
	if err != nil {
		configHTTPError(w, err)
		return
	}
	observed, err := s.configCoordinator.invokeRestore(ctx, node, shared.RestoreCommand{Action: "inspect"})
	if err != nil {
		configHTTPError(w, err)
		return
	}
	if observed.Code != "ok" || observed.Restore == nil || observed.Restore.Validate() != nil {
		adminJSON(w, 503, map[string]string{"code": observed.Code})
		return
	}
	if inspect {
		out, err := s.restoreResponse(ctx, node, observed, "ok")
		if err != nil {
			configHTTPError(w, err)
		} else {
			adminJSON(w, 200, out)
		}
		return
	}
	if !restoreRequestMatches(input, observed) {
		configHTTPError(w, control.ErrConflict)
		return
	}
	prior, err := s.control.GetConfigRestore(ctx, node, input.Epoch)
	if err != nil && !errors.Is(err, control.ErrNotFound) {
		configHTTPError(w, err)
		return
	}
	active, err := s.control.GetActiveConfigOperation(ctx, node)
	if err != nil && !errors.Is(err, control.ErrNotFound) {
		configHTTPError(w, err)
		return
	}
	if prior == nil && ((active == nil && (input.ExpectedActiveOperationID != "" || input.ExpectedActiveVersion != 0)) || (active != nil && (active.OperationID != input.ExpectedActiveOperationID || active.Version != input.ExpectedActiveVersion))) {
		configHTTPError(w, control.ErrConflict)
		return
	}
	info := observed.Restore
	if active != nil {
		if active.ServiceID != info.BackupServiceID && active.ServiceID != info.ReplacedServiceID {
			configHTTPError(w, control.ErrConflict)
			return
		}
		// Rotation makes stale commands unusable. Explicit restore lineage permits
		// this one read-only query of the old journal under the new local identity.
		command := shared.ConfigCommand{Action: "query", ServiceID: observed.ServiceID, OperationID: active.OperationID, BaseRevision: active.BaseRevision, CandidateDigest: active.CandidateDigest}
		if active.Agent != nil {
			command.ContextRevision = active.Agent.ContextRevision
		}
		result, e := s.configCoordinator.invoke(ctx, node, command)
		if e != nil {
			configHTTPError(w, e)
			return
		}
		if result.ServiceID != observed.ServiceID {
			configHTTPError(w, control.ErrConflict)
			return
		}
		if result.Operation != nil {
			// The immutable node/service lineage was checked above. Reuse the existing
			// journal matching rules while retaining the old audit operation owner.
			result.ServiceID = active.ServiceID
			if !configResultMatches(active, result) || shared.ConfigOperationActive(result.Operation.State) {
				configHTTPError(w, control.ErrConflict)
				return
			}
			next, e := s.recordConfigResult(ctx, active, result, actor)
			if e != nil {
				configHTTPError(w, e)
				return
			}
			active = next
			if !shared.ConfigOperationActive(active.State) {
				active = nil
			}
		} else if result.Code != "operation_not_found" {
			adminJSON(w, 409, map[string]string{"code": result.Code})
			return
		}
	}
	id, err := shared.NewConfigOperationID()
	if err != nil {
		configHTTPError(w, ErrConfigUnavailable)
		return
	}
	claim := control.ClaimConfigRestoreRequest{ID: id, NodeID: node, ServiceID: observed.ServiceID, Epoch: info.Epoch, BackupServiceID: info.BackupServiceID, ReplacedServiceID: info.ReplacedServiceID, ManifestDigest: info.ManifestDigest, ContextRevision: info.ContextRevision, StoreDigest: info.StoreDigest, Creator: actor, ExpectedTokenSHA256: identity.TokenSHA256}
	if active != nil {
		claim.ExpectedActiveOperationID = active.OperationID
		claim.ExpectedVersion = active.Version
	}
	receipt, _, err := s.control.ClaimConfigRestore(ctx, claim)
	if err != nil {
		configHTTPError(w, err)
		return
	}
	current, err := s.control.Get(ctx, node)
	if err != nil || current.TokenSHA256 != identity.TokenSHA256 {
		configHTTPError(w, control.ErrConflict)
		return
	}
	ack := shared.RestoreCommand{Action: "acknowledge", ServiceID: observed.ServiceID, Epoch: info.Epoch, ManifestDigest: info.ManifestDigest, ContextRevision: info.ContextRevision, StoreDigest: info.StoreDigest, AcknowledgementID: receipt.ID}
	result, err := s.configCoordinator.invokeRestore(ctx, node, ack)
	code := "ok"
	if err != nil {
		code = configFailureCode(err)
	} else {
		code = result.Code
		if code == "ok" && result.ServiceID == observed.ServiceID && result.Restore != nil && result.Restore.Validate() == nil && result.Restore.Epoch == info.Epoch && result.Restore.ManifestDigest == info.ManifestDigest && result.Restore.ContextRevision == info.ContextRevision && result.Restore.StoreDigest == info.StoreDigest && result.Restore.AcknowledgementID == receipt.ID && (result.Restore.State == "acknowledged" || result.Restore.State == "confirmed") {
			if _, err = s.control.ConfirmConfigRestore(ctx, receipt.ID, receipt.Version, actor); err != nil {
				configHTTPError(w, err)
				return
			}
			observed = result
		} else if code == "ok" {
			code = "conflict"
		}
	}
	// A lost acknowledgement reply leaves the durable claim pending; retrying
	// this explicit request reuses its ID. It never sends apply or rollback.
	responseCtx, done := context.WithTimeout(s.ctx, time.Second)
	defer done()
	out, err := s.restoreResponse(responseCtx, node, observed, code)
	if err != nil {
		configHTTPError(w, err)
		return
	}
	adminJSON(w, 200, out)
}
