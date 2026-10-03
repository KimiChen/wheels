package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const configReconcileActor = "system:configuration-reconcile"

// Locks cover only configuration work. At most four entries exist and every
// release deletes its entry; offline nodes cannot grow a permanent lock map.
type configCoordinator struct {
	mu             sync.Mutex
	nodes          map[string]bool
	slots          chan struct{}
	command        func(context.Context, string, shared.ConfigCommand) (shared.ConfigResult, error)
	restoreCommand func(context.Context, string, shared.RestoreCommand) (shared.RestoreResult, error)
}

func newConfigCoordinator(s *Service) *configCoordinator {
	return &configCoordinator{nodes: map[string]bool{}, slots: make(chan struct{}, 4), command: s.ConfigCommand, restoreCommand: s.RestoreCommand}
}
func (c *configCoordinator) acquire(node string) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nodes[node] {
		return nil, false
	}
	select {
	case c.slots <- struct{}{}:
	default:
		return nil, false
	}
	c.nodes[node] = true
	return func() { c.mu.Lock(); delete(c.nodes, node); <-c.slots; c.mu.Unlock() }, true
}
func configFailureCode(err error) string {
	switch {
	case errors.Is(err, ErrServerRestorePending):
		return "server_restore_pending"
	case errors.Is(err, ErrConfigBusy):
		return "busy"
	case errors.Is(err, ErrConfigServiceMismatch):
		return "service_mismatch"
	case errors.Is(err, ErrConfigInvalid):
		return "invalid_request"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "unavailable"
	}
}
func configAuditCode(code string) string {
	if shared.ValidConfigEventCode(code) {
		return code
	}
	if shared.ValidConfigIssueCode(code) {
		return "validation_failed"
	}
	return "failed"
}
func (s *Service) configTransition(ctx context.Context, o *control.ConfigOperation, state, code, actor string, agent *shared.ConfigOperationView) (*control.ConfigOperation, error) {
	input := control.ConfigOperationTransition{ExpectedVersion: o.Version, NextState: state, Code: configAuditCode(code), Actor: actor, Agent: agent}
	if agent != nil {
		input.CandidateDigest = agent.CandidateDigest
		input.AgentReceivedAtMS = time.Now().UnixMilli()
	}
	return s.control.TransitionConfigOperation(ctx, o.OperationID, input)
}
func (s *Service) configUnknown(o *control.ConfigOperation, code, actor string) *control.ConfigOperation {
	if !shared.ConfigOperationActive(o.State) || o.State == "outcome_unknown" {
		return o
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()
	next, err := s.configTransition(ctx, o, "outcome_unknown", code, actor, nil)
	if err == nil {
		return next
	}
	return o
}
func configResultMatches(o *control.ConfigOperation, r shared.ConfigResult) bool {
	if r.ServiceID != o.ServiceID || r.OperationID != o.OperationID || r.Operation == nil || r.Operation.Validate() != nil {
		return false
	}
	v := r.Operation
	if o.Agent != nil && o.Agent.MaterialsState == "context_changed" && v.MaterialsState != "context_changed" && v.MaterialsState != "expired" {
		return false
	}
	if o.Agent != nil && o.Agent.MaterialsState == "expired" && (v.MaterialsState != "expired" || v.MaterialsExpiredAtMS == nil || o.Agent.MaterialsExpiredAtMS == nil || *v.MaterialsExpiredAtMS != *o.Agent.MaterialsExpiredAtMS || v.MaterialsExpiryReason != o.Agent.MaterialsExpiryReason) {
		return false
	}
	return v.OperationID == o.OperationID && v.BaseRevision == o.BaseRevision && (o.CandidateDigest == "" || o.CandidateDigest == v.CandidateDigest) && (o.Agent == nil || (o.Agent.ContextRevision == v.ContextRevision && o.Agent.OldDigest == v.OldDigest && o.Agent.CreatedAtMS == v.CreatedAtMS && o.Agent.UpdatedAtMS <= v.UpdatedAtMS)) && v.DeadlineAtMS == o.DeadlineAtMS
}

// Journal observations may skip transient states. Record the necessary local
// CAS steps; never infer success from a command acknowledgement or send apply.
func (s *Service) recordConfigResult(ctx context.Context, o *control.ConfigOperation, r shared.ConfigResult, actor string) (*control.ConfigOperation, error) {
	if !configResultMatches(o, r) {
		return o, ErrConfigServiceMismatch
	}
	v := r.Operation
	if o.Agent != nil && (o.State == v.State || (o.State == "outcome_unknown" && time.Now().UnixMilli() >= o.DeadlineAtMS && (v.State == "prepared" || v.State == "applying" || v.State == "validated"))) {
		a, _ := json.Marshal(o.Agent)
		b, _ := json.Marshal(v)
		if string(a) == string(b) {
			return o, nil
		}
	}
	target := v.State
	for steps := 0; steps < 5; steps++ {
		next := target
		if o.State != target && !shared.ConfigOperationTransitionAllowed(o.State, target) {
			switch {
			case o.State == "draft" && target == "prepared":
				next = "validated"
			case o.State == "applying" && target == "confirmed":
				next = "verifying"
			case (o.State == "rollback_failed" || o.State == "confirmed") && target == "rolled_back":
				// Native startup may finish recovery while the controller is
				// offline. Retain the legal recovery transition when its query
				// observes only the final journal; do not send another rollback.
				next = "rolling_back"
			default:
				next = "outcome_unknown"
			}
		}
		if o.State == "draft" && target == "prepared" {
			next = "validated"
		}
		// A journal recovered after its deadline is still evidence. Retain the
		// observation under unknown until the Agent cancels/reverts expired prepare.
		if (next == "validated" || next == "prepared" || next == "applying") && o.State != next && time.Now().UnixMilli() >= o.DeadlineAtMS {
			next = "outcome_unknown"
		}
		agent := v
		if next == "outcome_unknown" && o.CandidateDigest == "" && o.State != "outcome_unknown" {
			agent = nil
		}
		updated, err := s.configTransition(ctx, o, next, next, actor, agent)
		if err != nil {
			return o, err
		}
		o = updated
		if next == target {
			return o, nil
		}
		if next == "outcome_unknown" && (target == "validated" || target == "prepared" || target == "applying") && time.Now().UnixMilli() >= o.DeadlineAtMS {
			updated, err := s.configTransition(ctx, o, o.State, "recovered", actor, v)
			if err != nil {
				return o, err
			}
			return updated, nil
		}
	}
	return o, ErrConfigInvalid
}
func (s *Service) queryConfigOperation(ctx context.Context, o *control.ConfigOperation, actor string) (*control.ConfigOperation, string) {
	command := shared.ConfigCommand{Action: "query", ServiceID: o.ServiceID, OperationID: o.OperationID, BaseRevision: o.BaseRevision, CandidateDigest: o.CandidateDigest}
	if o.Agent != nil {
		command.ContextRevision = o.Agent.ContextRevision
	}
	r, err := s.configCoordinator.invoke(ctx, o.NodeID, command)
	if err != nil {
		if errors.Is(err, ErrConfigServiceMismatch) {
			s.mu.Lock()
			current := ""
			if node := s.nodes[o.NodeID]; node != nil && node.conn != nil && node.configLink != nil && node.configLink.configEnabled {
				current = node.configLink.serviceID
			}
			s.mu.Unlock()
			if current != "" {
				return s.queryRestoredHistory(ctx, o, current, actor)
			}
		}
		return o, configFailureCode(err)
	}
	if r.ServiceID != o.ServiceID && r.Code == "service_mismatch" {
		return s.queryRestoredHistory(ctx, o, r.ServiceID, actor)
	}
	if r.ServiceID != o.ServiceID || r.OperationID != o.OperationID {
		return o, "service_mismatch"
	}
	if r.Operation != nil {
		updated, e := s.recordConfigResult(ctx, o, r, actor)
		if e != nil {
			return updated, "conflict"
		}
		return updated, r.Code
	}
	// A serialized authenticated journal lookup proves the prepare never left
	// a recoverable operation. Offline/timeout responses never release the lease.
	if r.Code == "operation_not_found" && (o.State == "draft" || o.State == "outcome_unknown") && o.CandidateDigest == "" {
		updated, e := s.configTransition(ctx, o, "cancelled", "operation_not_found", actor, nil)
		if e == nil {
			return updated, r.Code
		}
	}
	return o, r.Code
}
func (s *Service) configReconcileLoop() {
	defer s.wg.Done()
	cursor := ""
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
		operations, err := s.control.ListActiveConfigOperations(ctx, cursor, 16)
		cancel()
		if err == nil {
			for _, o := range operations {
				cursor = o.OperationID
				release, ok := s.configCoordinator.acquire(o.NodeID)
				if !ok {
					continue
				}
				ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
				_, _ = s.queryConfigOperation(ctx, &o, configReconcileActor)
				cancel()
				release()
				if s.ctx.Err() != nil {
					return
				}
			}
			if len(operations) < 16 {
				cursor = ""
			}
		}
		timer.Reset(5 * time.Second)
	}
}

func (c *configCoordinator) invoke(ctx context.Context, node string, command shared.ConfigCommand) (shared.ConfigResult, error) {
	c.mu.Lock()
	fn := c.command
	c.mu.Unlock()
	return fn(ctx, node, command)
}

func (c *configCoordinator) invokeRestore(ctx context.Context, node string, command shared.RestoreCommand) (shared.RestoreResult, error) {
	c.mu.Lock()
	fn := c.restoreCommand
	c.mu.Unlock()
	return fn(ctx, node, command)
}
