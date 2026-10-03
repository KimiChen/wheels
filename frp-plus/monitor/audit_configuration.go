package monitor

import (
	"context"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func (s *Service) recordConfigurationAudit(ctx context.Context, o *control.ConfigOperation, kind, code, actor string, summary *control.AuditSummary, changes []control.ConfigOperationChange) error {
	actorKind := "github"
	if strings.HasPrefix(actor, "system:") {
		actorKind = "system"
	}
	if actor == "" {
		actorKind = "unknown"
	}
	input := control.AuditInput{Kind: kind, ActorKind: actorKind, Actor: actor, Code: code, Summary: summary, Changes: changes}
	if o != nil {
		input.NodeID, input.ServiceID, input.OperationID, input.State = o.NodeID, o.ServiceID, o.OperationID, o.State
		input.OperationVersion = &o.Version
	}
	_, err := s.control.RecordAudit(ctx, input)
	return err
}

func (s *Service) recordConfigurationPreview(ctx context.Context, o *control.ConfigOperation, preview *shared.ConfigPreview, actor string, changes []control.ConfigOperationChange) error {
	if preview == nil {
		return nil
	}
	summary := &control.AuditSummary{BaseRevision: preview.BaseRevision, ContextRevision: preview.ContextRevision, CandidateDigest: preview.CandidateDigest, Warnings: []string{}, ReloadRequired: len(preview.Changes) > 0}
	for _, warning := range preview.Warnings {
		summary.Warnings = append(summary.Warnings, warning.Code)
	}
	return s.recordConfigurationAudit(ctx, o, "preview", "preview_available", actor, summary, changes)
}
