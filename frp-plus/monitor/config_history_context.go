package monitor

import (
	"context"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"reflect"
)

// A final historical operation retains its original service identity. A fully
// confirmed restore ancestry permits only reading its immutable journal and
// updating material availability; it never authorizes dispatch to a new owner.
func (s *Service) queryRestoredHistory(ctx context.Context, o *control.ConfigOperation, current, actor string) (*control.ConfigOperation, string) {
	if shared.ConfigOperationActive(o.State) || o.Agent == nil || !shared.ValidConfigOperationID(current) {
		return o, "service_mismatch"
	}
	identity, err := s.control.Get(ctx, o.NodeID)
	if err != nil {
		return o, "service_mismatch"
	}
	allowed, err := s.control.ConfirmedRestoreLineage(ctx, o.NodeID, o.ServiceID, current, identity.TokenSHA256)
	if err != nil || !allowed {
		return o, "service_mismatch"
	}
	command := shared.ConfigCommand{Action: "query", ServiceID: current, OperationID: o.OperationID, BaseRevision: o.BaseRevision, ContextRevision: o.Agent.ContextRevision, CandidateDigest: o.CandidateDigest}
	r, err := s.configCoordinator.invoke(ctx, o.NodeID, command)
	if err != nil {
		return o, configFailureCode(err)
	}
	observed := *o
	observed.ServiceID = current
	if !configResultMatches(&observed, r) || r.Operation.State != o.State {
		return o, "conflict"
	}
	a, b := *o.Agent, *r.Operation
	// The journal remains byte-for-byte business history, including old runtime
	// facts; only the independent material projection can change after relocation.
	a.MaterialsState, b.MaterialsState = "", ""
	a.MaterialsExpiredAtMS, b.MaterialsExpiredAtMS = nil, nil
	a.MaterialsExpiryReason, b.MaterialsExpiryReason = "", ""
	if a != b {
		return o, "conflict"
	}
	allowed, err = s.control.ConfirmedRestoreLineage(ctx, o.NodeID, o.ServiceID, current, identity.TokenSHA256)
	if err != nil || !allowed {
		return o, "service_mismatch"
	}
	code := r.Code
	if r.Operation.MaterialsState != "context_changed" && r.Operation.MaterialsState != "expired" {
		code = "service_mismatch"
	}
	if reflect.DeepEqual(o.Agent, r.Operation) {
		return o, code
	}
	updated, err := s.configTransition(ctx, o, o.State, "recovered", actor, r.Operation)
	if err != nil {
		return o, "conflict"
	}
	return updated, code
}
