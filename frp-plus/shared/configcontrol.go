package shared

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxConfigControlBytes      = 240 * 1024
	MaxConfigObjects           = 1024
	MaxConfigDependencies      = 128
	MaxConfigIssues            = 64
	MaxConfigCommandDuration   = time.Minute
	MaxConfigOperationDuration = 24 * time.Hour
)

// ConfigProvider is an explicitly enabled local management boundary, separate
// from observation. Negotiating ConfigManageCapability does NOT authorize a
// command. The transport must authenticate the private management channel,
// check current session ownership/sequence and enforce local opt-in before
// invoking it. The provider must bind ServiceID and operation metadata to its
// local durable journal, and must never trust a node identity from the wire.
// Commands/results (especially Secret.Value) must never enter public JSON/SSE,
// telemetry, debug logs or audit payloads. Results contain only safe projections.
type ConfigProvider interface {
	HandleConfig(context.Context, ConfigCommand) ConfigResult
}

type ConfigCommand struct {
	Meta
	RequestID             string             `json:"request_id"`
	ServiceID             string             `json:"service_id"`
	Action                string             `json:"action"`
	OperationID           string             `json:"operation_id"`
	BaseRevision          string             `json:"base_revision"`
	ContextRevision       string             `json:"context_revision"`
	CandidateDigest       string             `json:"candidate_digest"`
	IdempotencyKey        string             `json:"idempotency_key"`
	DeadlineAtMS          int64              `json:"deadline_at_ms"`
	OperationDeadlineAtMS int64              `json:"operation_deadline_at_ms"`
	Changes               []ConfigChange     `json:"changes"`
	Secret                *ConfigSecretInput `json:"secret,omitempty"`
}

// Value is the sole credential-value field in the management contract. A
// reference is an opaque local vault key, never a file path or remote URL.
// Retrying the same reference must not overwrite a different stored value.
type ConfigSecretInput struct {
	Reference string `json:"reference"`
	Value     string `json:"value"`
}

type ConfigResult struct {
	Meta
	RequestID       string               `json:"request_id"`
	ServiceID       string               `json:"service_id"`
	Action          string               `json:"action"`
	OperationID     string               `json:"operation_id"`
	Code            string               `json:"code"`
	Inventory       *ConfigInventory     `json:"inventory,omitempty"`
	Preview         *ConfigPreview       `json:"preview,omitempty"`
	Operation       *ConfigOperationView `json:"operation,omitempty"`
	SecretReference string               `json:"secret_reference,omitempty"`
}

type ConfigIssue struct {
	Code string `json:"code"`
}
type ConfigDependency struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
}
type ConfigSecretPresence struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
}

// No native object maps are allowed. Native camelCase paths appear only as
// values in explicit arrays, retaining the protocol's recursive snake_case keys.
type ConfigObjectView struct {
	Kind           string                 `json:"kind"`
	Name           string                 `json:"name"`
	Type           string                 `json:"type"`
	Source         string                 `json:"source"`
	Writable       bool                   `json:"writable"`
	Active         bool                   `json:"active"`
	Fields         []ConfigFieldPatch     `json:"fields"`
	Secrets        []ConfigSecretPresence `json:"secrets"`
	ReadOnlyFields []string               `json:"read_only_fields"`
	Issues         []ConfigIssue          `json:"issues"`
}

type ConfigInventory struct {
	Revision        string             `json:"revision"`
	ContextRevision string             `json:"context_revision"`
	State           string             `json:"state"`
	Objects         []ConfigObjectView `json:"objects"`
	Dependencies    []ConfigDependency `json:"dependencies"`
	Issues          []ConfigIssue      `json:"issues"`
}

type ConfigChangePreview struct {
	Operation string            `json:"operation"`
	Before    *ConfigObjectView `json:"before"`
	After     *ConfigObjectView `json:"after"`
}
type ConfigPreview struct {
	BaseRevision    string                `json:"base_revision"`
	ContextRevision string                `json:"context_revision"`
	CandidateDigest string                `json:"candidate_digest"`
	Changes         []ConfigChangePreview `json:"changes"`
	Warnings        []ConfigIssue         `json:"warnings"`
}

// CandidateDigest uses the managed Store digest (presence byte plus bytes),
// not a hash of the display projection or the raw Store bytes alone.
// RuntimeLoaded/ResourcesReady are observations. BusinessChecked=false never
// implies business connectivity. OutcomeUnknown/RollbackFailed are results,
// not reasons to discard the operation or release its durable reservation.
type ConfigOperationView struct {
	MaterialsState        string `json:"materials_state,omitempty"`
	MaterialsExpiredAtMS  *int64 `json:"materials_expired_at_ms,omitempty"`
	MaterialsExpiryReason string `json:"materials_expiry_reason,omitempty"`
	OperationID           string `json:"operation_id"`
	BaseRevision          string `json:"base_revision"`
	ContextRevision       string `json:"context_revision"`
	OldDigest             string `json:"old_digest"`
	CandidateDigest       string `json:"candidate_digest"`
	State                 string `json:"state"`
	ErrorCode             string `json:"error_code"`
	CreatedAtMS           int64  `json:"created_at_ms"`
	UpdatedAtMS           int64  `json:"updated_at_ms"`
	DeadlineAtMS          int64  `json:"deadline_at_ms"`
	StorePersisted        bool   `json:"store_persisted"`
	RuntimeApplied        bool   `json:"runtime_applied"`
	RuntimeLoaded         bool   `json:"runtime_loaded"`
	ResourcesReady        bool   `json:"resources_ready"`
	BusinessChecked       bool   `json:"business_checked"`
}

func validConfigAction(action string) bool {
	return detailEnum(action, "inspect", "prepare", "apply", "query", "rollback", "cancel", "secret")
}
func configControlSize(value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > MaxConfigControlBytes {
		return errors.New("configuration control size limit")
	}
	// Keep direct provider validation as strict as received frame validation.
	if checkJSON(data) != nil {
		return errors.New("invalid configuration control JSON")
	}
	return nil
}
func configMillis(value int64) bool { return value > 0 && value <= 253402300799999 }

func (c ConfigCommand) Validate() error {
	invalid := errors.New("invalid configuration command")
	if err := c.Meta.Validate(); err != nil {
		return err
	}
	if c.Sequence < 2 || !ValidConfigOperationID(c.RequestID) || !validConfigAction(c.Action) ||
		(c.ServiceID == "" && c.Action != "inspect") || (c.ServiceID != "" && !ValidConfigOperationID(c.ServiceID)) ||
		!configMillis(c.DeadlineAtMS) || c.Changes == nil {
		return invalid
	}
	collected, _ := time.Parse(time.RFC3339Nano, c.CollectedAt)
	at := collected.UnixMilli()
	if c.DeadlineAtMS <= at || c.DeadlineAtMS-at > MaxConfigCommandDuration.Milliseconds() {
		return invalid
	}
	switch c.Action {
	case "inspect", "secret":
		if c.OperationID != "" || c.BaseRevision != "" || c.ContextRevision != "" || c.CandidateDigest != "" || c.IdempotencyKey != "" || c.OperationDeadlineAtMS != 0 || len(c.Changes) != 0 {
			return invalid
		}
		if c.Action == "inspect" && c.Secret != nil {
			return invalid
		}
		if c.Action == "secret" && (c.Secret == nil || !ValidConfigOperationID(c.Secret.Reference) || c.Secret.Value == "" || len(c.Secret.Value) > MaxStringBytes || !utf8.ValidString(c.Secret.Value) || strings.ContainsAny(c.Secret.Value, "\x00\r\n")) {
			return invalid
		}
	case "prepare":
		// Bound aggregate raw values before validators or JSON encoding allocate
		// another complete copy of a provider-supplied/direct command.
		valueBytes := 0
		for _, change := range c.Changes {
			for _, field := range change.Fields {
				valueBytes += len(field.Value)
				if valueBytes > MaxConfigChangeBytes {
					return invalid
				}
			}
		}
		if !ValidConfigOperationID(c.OperationID) || !ValidConfigOperationID(c.IdempotencyKey) || !ValidConfigDigest(c.BaseRevision) || c.ContextRevision != "" || c.CandidateDigest != "" || c.Secret != nil || ValidateConfigChanges(c.Changes) != nil || !configMillis(c.OperationDeadlineAtMS) || c.OperationDeadlineAtMS < c.DeadlineAtMS || c.OperationDeadlineAtMS-at > MaxConfigOperationDuration.Milliseconds() {
			return invalid
		}
	default:
		if !ValidConfigOperationID(c.OperationID) || !ValidConfigDigest(c.BaseRevision) || c.IdempotencyKey != "" || c.OperationDeadlineAtMS != 0 || len(c.Changes) != 0 || c.Secret != nil {
			return invalid
		}
		if c.Action == "apply" || c.Action == "rollback" {
			if !ValidConfigDigest(c.CandidateDigest) || !ValidConfigDigest(c.ContextRevision) {
				return invalid
			}
		} else if (c.CandidateDigest != "" && !ValidConfigDigest(c.CandidateDigest)) || (c.ContextRevision != "" && !ValidConfigDigest(c.ContextRevision)) {
			return invalid
		}
	}
	return configControlSize(c)
}

// ValidateAt is required immediately before execution. Structural decoding
// cannot establish freshness, session ownership, authorization or idempotency.
func (c ConfigCommand) ValidateAt(now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.DeadlineAtMS <= now.UnixMilli() || c.DeadlineAtMS-now.UnixMilli() > MaxConfigCommandDuration.Milliseconds() {
		return errors.New("configuration command expired or too far in future")
	}
	return nil
}

func ValidConfigIssueCode(code string) bool {
	return detailEnum(code, "limit_exceeded", "unsupported_dependency", "invalid_template", "invalid_source", "unsupported_source", "legacy_read_only", "source_drift", "store_unavailable", "duplicate_name", "ownership_conflict", "source_read_only", "advanced_read_only", "native_warning", "start_filtered", "invalid_memory", "invalid_change", "field_read_only", "invalid_secret", "invalid_secret_reference", "secret_reference_unavailable", "validation_failed", "runtime_capability_required", "dependency_conflict", "revision_conflict", "duplicate_change", "already_exists", "invalid_type", "not_found", "invalid_field", "duplicate_key", "invalid_kind", "invalid_name", "source_changed")
}
func ValidConfigResultCode(code string) bool {
	return code == "ok" || ValidConfigEventCode(code) || ValidConfigIssueCode(code) || detailEnum(code, "managed_capacity", "invalid_request", "unauthorized", "unavailable", "internal_error", "too_large")
}
func ValidConfigReadOnlyField(path string) bool {
	return detailEnum(path, "annotations", "metadatas", "loadBalancer", "healthCheck", "plugin", "natTraversal", "requestHeaders", "responseHeaders")
}
func validateConfigIssues(issues []ConfigIssue) error {
	if issues == nil || len(issues) > MaxConfigIssues {
		return errors.New("invalid configuration issues")
	}
	seen := map[string]bool{}
	for _, issue := range issues {
		if !ValidConfigIssueCode(issue.Code) || seen[issue.Code] {
			return errors.New("invalid configuration issue")
		}
		seen[issue.Code] = true
	}
	return nil
}
func configObjectType(kind, typ string) bool {
	return (kind == "proxy" && detailEnum(typ, "tcp", "udp", "http", "https", "tcpmux", "stcp", "sudp", "xtcp")) || (kind == "visitor" && detailEnum(typ, "stcp", "sudp", "xtcp"))
}
func (o ConfigObjectView) Validate() error {
	invalid := errors.New("invalid configuration object view")
	if !configObjectType(o.Kind, o.Type) || !detailText(o.Name, 256, false) || !detailEnum(o.Source, "file", "include", "store") || (o.Writable && o.Source != "store") || o.Fields == nil || len(o.Fields) > 64 || o.Secrets == nil || len(o.Secrets) > 8 || o.ReadOnlyFields == nil || len(o.ReadOnlyFields) > 16 || validateConfigIssues(o.Issues) != nil {
		return invalid
	}
	for _, issue := range o.Issues {
		if o.Writable && detailEnum(issue.Code, "ownership_conflict", "advanced_read_only", "source_read_only") {
			return invalid
		}
	}
	paths := map[string]bool{}
	for _, field := range o.Fields {
		if field.Validate() != nil || paths[field.Path] {
			return invalid
		}
		paths[field.Path] = true
	}
	for _, secret := range o.Secrets {
		if (!ValidConfigSecretPath(secret.Path) && !detailEnum(secret.Path, "plugin.password", "plugin.httpPassword")) || paths[secret.Path] {
			return invalid
		}
		paths[secret.Path] = true
	}
	for _, path := range o.ReadOnlyFields {
		if !ValidConfigReadOnlyField(path) || paths[path] {
			return invalid
		}
		paths[path] = true
	}
	return nil
}
func (i ConfigInventory) Validate() error {
	if !ValidConfigDigest(i.Revision) || !ValidConfigDigest(i.ContextRevision) || !detailEnum(i.State, "ready", "read_only") || i.Objects == nil || len(i.Objects) > MaxConfigObjects || i.Dependencies == nil || len(i.Dependencies) > MaxConfigDependencies || validateConfigIssues(i.Issues) != nil {
		return errors.New("invalid configuration inventory")
	}
	conflictVisible := false
	for _, issue := range i.Issues {
		conflictVisible = conflictVisible || issue.Code == "duplicate_name" || issue.Code == "ownership_conflict"
	}
	seen, valueBytes := map[[2]string]bool{}, 0
	for _, o := range i.Objects {
		key := [2]string{o.Kind, o.Name}
		if seen[key] && (i.State != "read_only" || !conflictVisible) {
			return errors.New("unexplained duplicate inventory object")
		}
		seen[key] = true
		for _, field := range o.Fields {
			valueBytes += len(field.Value)
			if valueBytes > MaxConfigControlBytes {
				return errors.New("configuration inventory size limit")
			}
		}
		if o.Validate() != nil || (i.State == "read_only" && o.Writable) {
			return errors.New("invalid inventory object")
		}
	}
	// Duplicate names are visible evidence of source conflicts, not overwritten.
	for _, d := range i.Dependencies {
		if !detailEnum(d.Kind, "file", "include", "store", "local_file") || !detailEnum(d.State, "ready", "missing", "unavailable") {
			return errors.New("invalid configuration dependency")
		}
	}
	return nil
}
func (p ConfigPreview) Validate() error {
	invalid := errors.New("invalid configuration preview")
	if !ValidConfigDigest(p.BaseRevision) || !ValidConfigDigest(p.ContextRevision) || !ValidConfigDigest(p.CandidateDigest) || len(p.Changes) == 0 || len(p.Changes) > MaxConfigChanges || validateConfigIssues(p.Warnings) != nil {
		return invalid
	}
	seen, valueBytes := map[[2]string]bool{}, 0
	for _, change := range p.Changes {
		if !detailEnum(change.Operation, "create", "update", "delete", "enable", "disable") {
			return invalid
		}
		if (change.Operation == "create" && (change.Before != nil || change.After == nil)) || (change.Operation == "delete" && (change.Before == nil || change.After != nil)) || (change.Operation != "create" && change.Operation != "delete" && (change.Before == nil || change.After == nil)) {
			return invalid
		}
		var identity *ConfigObjectView
		for _, o := range []*ConfigObjectView{change.Before, change.After} {
			if o != nil {
				for _, field := range o.Fields {
					valueBytes += len(field.Value)
					if valueBytes > MaxConfigControlBytes {
						return invalid
					}
				}
				if o.Validate() != nil || o.Source != "store" || !o.Writable {
					return invalid
				}
				if identity != nil && (o.Kind != identity.Kind || o.Name != identity.Name || o.Type != identity.Type) {
					return invalid
				}
				identity = o
			}
		}
		key := [2]string{identity.Kind, identity.Name}
		if seen[key] {
			return invalid
		}
		seen[key] = true
	}
	return nil
}
func (o ConfigOperationView) Validate() error {
	invalid := errors.New("invalid configuration operation view")
	if !ValidConfigOperationID(o.OperationID) || !ValidConfigDigest(o.BaseRevision) || !ValidConfigDigest(o.ContextRevision) || !ValidConfigDigest(o.OldDigest) || !ValidConfigDigest(o.CandidateDigest) || !ValidConfigOperationState(o.State) || (o.ErrorCode != "" && (o.ErrorCode == "ok" || !ValidConfigResultCode(o.ErrorCode))) || !configMillis(o.CreatedAtMS) || !configMillis(o.UpdatedAtMS) || !configMillis(o.DeadlineAtMS) || o.UpdatedAtMS < o.CreatedAtMS || o.DeadlineAtMS <= o.CreatedAtMS || o.DeadlineAtMS-o.CreatedAtMS > MaxConfigOperationDuration.Milliseconds() || (o.ResourcesReady && !o.RuntimeLoaded) {
		return invalid
	}
	switch o.MaterialsState {
	case "context_changed":
		if ConfigOperationActive(o.State) || o.MaterialsExpiredAtMS != nil || o.MaterialsExpiryReason != "" {
			return invalid
		}
	case "", "retained":
		if o.MaterialsExpiredAtMS != nil || o.MaterialsExpiryReason != "" {
			return invalid
		}
	case "expired":
		if ConfigOperationActive(o.State) || o.MaterialsExpiredAtMS == nil || !configMillis(*o.MaterialsExpiredAtMS) || *o.MaterialsExpiredAtMS < o.CreatedAtMS || o.MaterialsExpiryReason != "ttl" {
			return invalid
		}
	default:
		return invalid
	}
	if o.State == "confirmed" && (!o.StorePersisted || !o.RuntimeApplied || !o.RuntimeLoaded || !o.ResourcesReady) {
		return invalid
	}
	if o.State == "rolled_back" && (o.StorePersisted || o.RuntimeApplied || !o.RuntimeLoaded || !o.ResourcesReady) {
		return invalid
	}
	return nil
}
func (r ConfigResult) Validate() error {
	invalid := errors.New("invalid configuration result")
	if err := r.Meta.Validate(); err != nil {
		return err
	}
	if r.Sequence < 2 || !ValidConfigOperationID(r.RequestID) || !ValidConfigOperationID(r.ServiceID) || !validConfigAction(r.Action) || !ValidConfigResultCode(r.Code) {
		return invalid
	}
	if r.Action == "inspect" || r.Action == "secret" {
		if r.OperationID != "" {
			return invalid
		}
	} else if !ValidConfigOperationID(r.OperationID) {
		return invalid
	}
	if r.Operation != nil && (r.Operation.Validate() != nil || r.Operation.OperationID != r.OperationID) {
		return invalid
	}
	if r.Code != "ok" {
		if r.Inventory != nil || r.Preview != nil || r.SecretReference != "" || (r.Operation != nil && (r.Action == "inspect" || r.Action == "secret")) {
			return invalid
		}
		return configControlSize(r)
	}
	switch r.Action {
	case "inspect":
		if r.Inventory == nil || r.Inventory.Validate() != nil || r.Preview != nil || r.Operation != nil || r.SecretReference != "" {
			return invalid
		}
	case "secret":
		if !ValidConfigOperationID(r.SecretReference) || r.Inventory != nil || r.Preview != nil || r.Operation != nil {
			return invalid
		}
	case "prepare":
		if r.Inventory != nil || r.Operation == nil || r.SecretReference != "" || (r.Operation.State == "prepared" && r.Preview == nil) {
			return invalid
		}
		// A replay after the durable operation advanced must only query that
		// operation; it cannot recreate or reapply a historical candidate.
		if r.Preview != nil && (r.Preview.Validate() != nil || r.Preview.BaseRevision != r.Operation.BaseRevision || r.Preview.ContextRevision != r.Operation.ContextRevision || r.Preview.CandidateDigest != r.Operation.CandidateDigest) {
			return invalid
		}
	default:
		if r.Inventory != nil || r.Preview != nil || r.Operation == nil || r.SecretReference != "" {
			return invalid
		}
	}
	return configControlSize(r)
}
