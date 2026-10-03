package configuration

import (
	"context"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func restoreView(value managed.RestoreStatus) *shared.RestoreInfo {
	return &shared.RestoreInfo{State: value.State, Epoch: value.Epoch, BackupServiceID: value.BackupServiceID, ReplacedServiceID: value.ReplacedServiceID, ManifestDigest: value.ManifestDigest, ContextRevision: value.ContextRevision, StoreDigest: value.StoreDigest, AcknowledgementID: value.AcknowledgementID, RuntimeLoaded: value.RuntimeLoaded, ResourcesReady: value.ResourcesReady, OperationsCount: value.OperationsCount}
}
func (p *Provider) HandleRestore(ctx context.Context, command shared.RestoreCommand) shared.RestoreResult {
	result := shared.RestoreResult{Meta: command.Meta, RequestID: command.RequestID, ServiceID: p.engine.ServiceID(), Action: command.Action, Code: "ok"}
	result.Sequence = 2
	result.CollectedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if command.ValidateAt(time.Now()) != nil {
		result.Code = "invalid_request"
		return result
	}
	if command.ServiceID != "" && command.ServiceID != result.ServiceID {
		result.Code = "service_mismatch"
		return result
	}
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		result.Code = "timeout"
		return result
	default:
		result.Code = "busy"
		return result
	}
	if ctx.Err() != nil {
		result.Code = "timeout"
		return result
	}
	status := p.engine.RestoreStatus()
	if command.Action == "acknowledge" {
		var err error
		status, err = p.engine.AcknowledgeRestore(ctx, command.Epoch, command.ManifestDigest, command.ContextRevision, command.StoreDigest, command.AcknowledgementID)
		if err != nil {
			result.Code = resultCode(err)
			status = p.engine.RestoreStatus()
		}
	}
	result.Restore = restoreView(status)
	if result.Validate() != nil {
		result.Code = "unavailable"
		result.Restore = nil
	}
	return result
}
