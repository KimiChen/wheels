package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// Provider is the bounded process-local bridge between native inspection and
// the durable Store engine. input must return a complete cloned native view.
// It must release native locks before returning: Engine callbacks independently
// acquire those locks while HandleConfig waits for application/verification.
type Provider struct {
	engine *managed.Engine
	input  func() (Input, error)
	gate   chan struct{}
}

func NewProvider(engine *managed.Engine, input func() (Input, error)) (*Provider, error) {
	if engine == nil || input == nil {
		return nil, errors.New("configuration provider requires local engine and native input")
	}
	return &Provider{engine: engine, input: input, gate: make(chan struct{}, 1)}, nil
}

func (p *Provider) HandleConfig(ctx context.Context, command shared.ConfigCommand) shared.ConfigResult {
	result := shared.ConfigResult{Meta: command.Meta, RequestID: command.RequestID, ServiceID: p.engine.ServiceID(), Action: command.Action, OperationID: command.OperationID, Code: "ok"}
	// Transport assigns the next uplink sequence; it is independent of this
	// command's downlink sequence. Use a valid placeholder for projection checks.
	result.Sequence = 2
	result.CollectedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if command.ValidateAt(time.Now()) != nil {
		result.Code = "invalid_request"
		return result
	}
	if command.ServiceID != p.engine.ServiceID() && !(command.Action == "inspect" && command.ServiceID == "") {
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
	switch command.Action {
	case "inspect":
		input, err := p.input()
		if err != nil {
			result.Code = "unavailable"
			break
		}
		inspection, err := Inspect(input)
		if err != nil {
			result.Code = resultCode(err)
			break
		}
		result.Inventory = inventoryView(inspection)
	case "secret":
		if err := p.engine.PutSecret(ctx, command.Secret.Reference, command.Secret.Value); err != nil {
			result.Code = resultCode(err)
		} else {
			result.SecretReference = command.Secret.Reference
		}
	case "prepare":
		p.prepare(ctx, command, &result)
	default:
		p.execute(ctx, command, &result)
	}
	// This projection is checked before transport serialization. A future
	// native field or malformed snapshot cannot silently enlarge the wire API.
	if result.Validate() != nil {
		result.Code = "too_large"
		result.Inventory, result.Preview, result.Operation, result.SecretReference = nil, nil, nil, ""
	}
	return result
}

// Hash the exact edit intent, including the immutable local references. This
// lets an already-completed prepare retry return its journal without rebuilding
// the old candidate from the now-different Store or executing it again.
func commandIntentDigest(command shared.ConfigCommand) string {
	value := struct {
		ServiceID, OperationID, BaseRevision, IdempotencyKey string
		Deadline                                             int64
		Changes                                              []shared.ConfigChange
	}{command.ServiceID, command.OperationID, command.BaseRevision, command.IdempotencyKey, command.OperationDeadlineAtMS, command.Changes}
	data, _ := json.Marshal(value)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (p *Provider) prepare(ctx context.Context, command shared.ConfigCommand, result *shared.ConfigResult) {
	digest := commandIntentDigest(command)
	if previous, err := p.engine.Query(ctx, command.OperationID); err == nil {
		if previous.RequestDigest != digest || previous.BaseRevision != command.BaseRevision || previous.Deadline.UnixMilli() != command.OperationDeadlineAtMS {
			result.Code = "conflict"
			return
		}
		if previous.State != managed.Prepared {
			result.Operation = operationView(previous)
			return
		}
	} else if !errors.Is(err, managed.ErrNotFound) {
		result.Code = resultCode(err)
		return
	}
	input, err := p.input()
	if err != nil {
		result.Code = "unavailable"
		return
	}
	references := []string{}
	changes := make([]Change, 0, len(command.Changes))
	for _, change := range command.Changes {
		item := Change{Operation: change.Operation, Kind: change.Kind, Name: change.Name, Type: change.Type, CloneFrom: change.CloneFrom, Fields: map[string]json.RawMessage{}, Secrets: map[string]SecretAction{}}
		for _, field := range change.Fields {
			item.Fields[field.Path] = append(json.RawMessage(nil), field.Value...)
		}
		for _, secret := range change.Secrets {
			item.Secrets[secret.Path] = SecretAction{Mode: secret.Mode, Reference: secret.Reference}
			if secret.Mode == "reference" {
				references = append(references, secret.Reference)
			}
		}
		changes = append(changes, item)
	}
	input.LocalSecrets, err = p.engine.ResolveSecrets(ctx, references)
	if err != nil {
		result.Code = "secret_reference_unavailable"
		return
	}
	prepared, err := Prepare(input, Request{ExpectedRevision: command.BaseRevision, Changes: changes})
	if err != nil {
		result.Code = resultCode(err)
		return
	}
	if ctx.Err() != nil {
		result.Code = "timeout"
		return
	}
	op, err := p.engine.Prepare(ctx, managed.Request{
		ID: command.OperationID, IdempotencyKey: command.IdempotencyKey, RequestDigest: digest,
		BaseRevision: prepared.BaseRevision, ContextRevision: prepared.ContextRevision,
		ExpectedDigest: managed.Digest(managed.StoreSnapshot{Exists: prepared.OriginalStoreExists, Bytes: prepared.OriginalStoreBytes}),
		Candidate:      prepared.StoreBytes, Deadline: time.UnixMilli(command.OperationDeadlineAtMS),
	})
	if err != nil {
		result.Code = resultCode(err)
		if op.ID != "" {
			result.Operation = operationView(op)
		}
		return
	}
	result.Operation = operationView(op)
	if op.State == managed.Prepared {
		result.Preview = previewView(prepared, op.NewDigest)
	}
}

func (p *Provider) execute(ctx context.Context, command shared.ConfigCommand, result *shared.ConfigResult) {
	op, err := p.engine.Query(ctx, command.OperationID)
	if err != nil {
		result.Code = resultCode(err)
		return
	}
	if op.BaseRevision != command.BaseRevision || (command.ContextRevision != "" && op.ContextRevision != command.ContextRevision) || (command.CandidateDigest != "" && op.NewDigest != command.CandidateDigest) {
		result.Code = "conflict"
		return
	}
	switch command.Action {
	case "apply":
		op, err = p.engine.Apply(ctx, command.OperationID)
	case "rollback":
		op, err = p.engine.Rollback(ctx, command.OperationID)
	case "cancel":
		if op.State == managed.Prepared {
			op, err = p.engine.Rollback(ctx, command.OperationID)
		} else if op.State != managed.Cancelled {
			result.Code = "conflict"
			return
		}
	case "query":
	default:
		result.Code = "invalid_request"
		return
	}
	// Keep known durable facts alongside a refused or interrupted action. A
	// confirmed history record does not make an obsolete rollback successful.
	if op.ID != "" {
		result.Operation = operationView(op)
	}
	if err != nil {
		result.Code = resultCode(err)
	}
}

func resultCode(err error) string {
	if e, ok := err.(*Error); ok && shared.ValidConfigResultCode(e.Code) {
		return e.Code
	}
	switch {
	case errors.Is(err, managed.ErrConflict):
		return "conflict"
	case errors.Is(err, managed.ErrBusy):
		return "busy"
	case errors.Is(err, managed.ErrNotFound):
		return "operation_not_found"
	case errors.Is(err, managed.ErrExpired):
		return "expired"
	case errors.Is(err, managed.ErrInvalid):
		return "invalid_request"
	case errors.Is(err, managed.ErrRuntime):
		return "runtime_failed"
	case errors.Is(err, managed.ErrStorage), errors.Is(err, managed.ErrUnsafePath), errors.Is(err, managed.ErrRecovery):
		return "write_failed"
	case errors.Is(err, managed.ErrOutcomeUnknown):
		return "outcome_unknown"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "unavailable"
	}
}

func operationCode(code string) string {
	switch code {
	case "":
		return ""
	case "source_drift", "store_drift":
		return "source_drift"
	case "deadline_expired", "rollback_timeout":
		return "timeout"
	case "journal_failed", "snapshot_invalid", "store_write_failed", "rollback_store_failed":
		return "write_failed"
	case "runtime_apply_failed", "rollback_apply_failed":
		return "runtime_failed"
	case "verification_failed", "rollback_verification_failed":
		return "verify_failed"
	case "startup_recovery":
		return "recovery_started"
	case "rollback_requested":
		return "rolling_back"
	case "cancelled_before_apply":
		return "cancelled"
	case "engine_closing":
		return "connection_lost"
	default:
		return "outcome_unknown"
	}
}

func operationView(op managed.Operation) *shared.ConfigOperationView {
	return &shared.ConfigOperationView{OperationID: op.ID, BaseRevision: op.BaseRevision, ContextRevision: op.ContextRevision,
		OldDigest: op.OldDigest, CandidateDigest: op.NewDigest, State: op.State, ErrorCode: operationCode(op.ErrorCode),
		CreatedAtMS: op.CreatedAt.UnixMilli(), UpdatedAtMS: op.UpdatedAt.UnixMilli(), DeadlineAtMS: op.Deadline.UnixMilli(),
		StorePersisted: op.StorePersisted, RuntimeApplied: op.RuntimeApplied, RuntimeLoaded: op.Verification.RuntimeLoaded,
		ResourcesReady: op.Verification.ResourcesReady, BusinessChecked: op.Verification.BusinessChecked}
}

func objectView(object ObjectView) shared.ConfigObjectView {
	view := shared.ConfigObjectView{Kind: object.Kind, Name: object.Name, Type: object.Type, Source: object.Source,
		Writable: object.Writable, Active: object.Active, Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPresence{},
		ReadOnlyFields: append([]string{}, object.ReadOnlyFields...), Issues: issueView(object.Issues)}
	paths := make([]string, 0, len(object.Fields))
	for path := range object.Fields {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		view.Fields = append(view.Fields, shared.ConfigFieldPatch{Path: path, Value: append(json.RawMessage(nil), object.Fields[path]...)})
	}
	paths = paths[:0]
	for path := range object.Secrets {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		view.Secrets = append(view.Secrets, shared.ConfigSecretPresence{Path: path, Present: object.Secrets[path].Present})
	}
	return view
}

func issueView(issues []Issue) []shared.ConfigIssue {
	result := make([]shared.ConfigIssue, 0, len(issues))
	for _, issue := range issues {
		result = append(result, shared.ConfigIssue{Code: issue.Code})
	}
	return result
}

func inventoryView(in *Inspection) *shared.ConfigInventory {
	out := &shared.ConfigInventory{Revision: in.Revision, ContextRevision: in.ContextRevision, State: in.State,
		Objects: []shared.ConfigObjectView{}, Dependencies: []shared.ConfigDependency{}, Issues: issueView(in.Issues)}
	for _, object := range in.Objects {
		out.Objects = append(out.Objects, objectView(object))
	}
	for _, dep := range in.Dependencies {
		out.Dependencies = append(out.Dependencies, shared.ConfigDependency{Kind: dep.Kind, State: dep.State})
	}
	return out
}

func previewView(in *Prepared, digest string) *shared.ConfigPreview {
	out := &shared.ConfigPreview{BaseRevision: in.BaseRevision, ContextRevision: in.ContextRevision, CandidateDigest: digest,
		Changes: []shared.ConfigChangePreview{}, Warnings: issueView(in.Warnings)}
	for _, change := range in.Changes {
		view := shared.ConfigChangePreview{Operation: change.Operation}
		if change.Before != nil {
			value := objectView(*change.Before)
			view.Before = &value
		}
		if change.After != nil {
			value := objectView(*change.After)
			view.After = &value
		}
		out.Changes = append(out.Changes, view)
	}
	return out
}
