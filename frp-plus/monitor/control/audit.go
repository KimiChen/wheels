package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// AuditSummary deliberately has no arbitrary strings or native field values.
// In particular, a preview's before/after configuration is never persisted.
type AuditSummary struct {
	BaseRevision    string   `json:"base_revision"`
	ContextRevision string   `json:"context_revision"`
	CandidateDigest string   `json:"candidate_digest"`
	FilterDigest    string   `json:"filter_digest"`
	ExportID        string   `json:"export_id"`
	WatermarkID     string   `json:"watermark_id"`
	Warnings        []string `json:"warnings"`
	ReloadRequired  bool     `json:"reload_required"`
	Rows            int      `json:"rows"`
	Bytes           int      `json:"bytes"`
}

type AuditItem struct {
	AuditID          string        `json:"audit_id"`
	RecordedAtMS     int64         `json:"recorded_at_ms"`
	OccurredAtMS     *int64        `json:"occurred_at_ms"`
	Kind             string        `json:"kind"`
	ActorKind        string        `json:"actor_kind"`
	Actor            string        `json:"actor"`
	Observer         string        `json:"observer"`
	NodeID           string        `json:"node_id"`
	ServiceID        string        `json:"service_id"`
	OperationID      string        `json:"operation_id"`
	OperationVersion *int64        `json:"operation_version"`
	State            string        `json:"state"`
	Code             string        `json:"code"`
	ObjectCount      int           `json:"object_count"`
	FieldCount       int           `json:"field_count"`
	PreviewSummary   *AuditSummary `json:"preview_summary"`
}

type AuditDetail struct {
	Item    AuditItem               `json:"item"`
	Changes []ConfigOperationChange `json:"changes"`
}

type AuditInput struct {
	Kind, ActorKind, Actor, Observer, NodeID, ServiceID, OperationID, State, Code string
	OccurredAtMS, OperationVersion                                                *int64
	Summary                                                                       *AuditSummary
	Changes                                                                       []ConfigOperationChange
}

func ValidAuditKind(value string) bool {
	switch value {
	case "state", "preview", "dispatch", "retry", "restore", "external_drift", "export":
		return true
	}
	return false
}

func validAuditCode(value string) bool {
	if shared.ValidConfigEventCode(value) || shared.ValidConfigResultCode(value) {
		return true
	}
	switch value {
	case "preview_available", "command_dispatched", "dispatch_prepare", "dispatch_apply", "dispatch_rollback", "dispatch_cancel", "dispatch_secret", "dispatch_query", "request_replayed", "restore_pending", "restore_acknowledged", "export_requested", "export_prepared", "export_failed":
		return true
	}
	return false
}

func auditActor(actor string) (string, string) {
	if actor == "" {
		return "unknown", ""
	}
	if strings.HasPrefix(actor, "system:") {
		return "system", actor
	}
	return "github", actor
}

func validAuditSummary(s *AuditSummary) bool {
	if s == nil {
		return true
	}
	for _, digest := range []string{s.BaseRevision, s.ContextRevision, s.CandidateDigest, s.FilterDigest} {
		if digest != "" && !shared.ValidConfigDigest(digest) {
			return false
		}
	}
	if (s.ExportID != "" && !shared.ValidConfigOperationID(s.ExportID)) || (s.WatermarkID != "" && !auditDecimal(s.WatermarkID, true)) || len(s.Warnings) > 64 || s.Rows < 0 || s.Rows > MaxAuditExportRows || s.Bytes < 0 || s.Bytes > MaxAuditExportBytes {
		return false
	}
	seen := map[string]bool{}
	for _, code := range s.Warnings {
		if !shared.ValidConfigIssueCode(code) || seen[code] {
			return false
		}
		seen[code] = true
	}
	return true
}

func auditDecimal(value string, zero bool) bool {
	n, err := strconv.ParseInt(value, 10, 64)
	return err == nil && n >= 0 && (zero || n != 0) && strconv.FormatInt(n, 10) == value
}

func validateAudit(input AuditInput) ([]byte, []byte, error) {
	kind, actor := auditActor(input.Actor)
	if !ValidAuditKind(input.Kind) || input.ActorKind != kind || input.Actor != actor || !validAuditCode(input.Code) || (actor != "" && !operationText(actor, 128)) || (input.Observer != "" && !operationText(input.Observer, 128)) || (input.NodeID != "" && !validID(input.NodeID)) || (input.ServiceID != "" && !operationText(input.ServiceID, 128)) || (input.OperationID != "" && !shared.ValidConfigOperationID(input.OperationID)) || (input.State != "" && !shared.ValidConfigOperationState(input.State) && input.State != "pending" && input.State != "acknowledged") || (input.OperationVersion != nil && *input.OperationVersion < 1) || (input.OccurredAtMS != nil && (*input.OccurredAtMS <= 0 || *input.OccurredAtMS > 253402300799999)) || !validAuditSummary(input.Summary) {
		return nil, nil, fmt.Errorf("%w: audit metadata", ErrInvalid)
	}
	if input.Kind == "external_drift" && (input.ActorKind != "unknown" || input.Actor != "") {
		return nil, nil, fmt.Errorf("%w: external actor is unknown", ErrInvalid)
	}
	changes, err := operationChanges(input.Changes)
	if err != nil {
		return nil, nil, err
	}
	var summary []byte
	if input.Summary != nil {
		copy := *input.Summary
		if copy.Warnings == nil {
			copy.Warnings = []string{}
		}
		summary, err = json.Marshal(copy)
		if err != nil || len(summary) > 8192 {
			return nil, nil, ErrInvalid
		}
	}
	return changes, summary, nil
}

func appendAuditTx(tx *sql.Tx, now int64, input AuditInput, originKind, originID string) (*AuditItem, error) {
	// Reuse the operation's target/field-name summary on its later lifecycle
	// entries. These are affected-object names, never a claimed second mutation.
	if len(input.Changes) == 0 && input.OperationID != "" {
		var initial string
		err := tx.QueryRow("SELECT changes_json FROM config_operation_events WHERE operation_id=? AND changes_json<>'[]' ORDER BY event_id LIMIT 1", input.OperationID).Scan(&initial)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if initial != "" && json.Unmarshal([]byte(initial), &input.Changes) != nil {
			return nil, ErrInvalid
		}
	}
	changes, summary, err := validateAudit(input)
	if err != nil {
		return nil, err
	}
	var node, summaryValue any
	if input.NodeID != "" {
		node = input.NodeID
	}
	if summary != nil {
		summaryValue = string(summary)
	}
	fields := 0
	for _, c := range input.Changes {
		fields += len(c.Fields)
	}
	result, err := tx.Exec("INSERT INTO config_audit_entries(recorded_at_ms,occurred_at_ms,kind,actor_kind,actor,observer,node_id,service_id,operation_id,operation_version,state,code,object_count,field_count,summary_json,changes_json,origin_kind,origin_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", now, input.OccurredAtMS, input.Kind, input.ActorKind, input.Actor, input.Observer, node, input.ServiceID, input.OperationID, input.OperationVersion, input.State, input.Code, len(input.Changes), fields, summaryValue, string(changes), originKind, originID)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	for _, c := range input.Changes {
		if _, err = tx.Exec("INSERT INTO config_audit_objects(audit_id,kind,name,action) VALUES(?,?,?,?)", id, c.Kind, c.Name, c.Action); err != nil {
			return nil, err
		}
	}
	return scanAuditItem(tx.QueryRow("SELECT "+auditColumns+" FROM config_audit_entries WHERE audit_id=?", id))
}

// RecordAudit serializes input before queueing, so a cancelled caller cannot
// change metadata while the worker completes its durable append.
func (s *Store) RecordAudit(ctx context.Context, input AuditInput) (*AuditItem, error) {
	if _, _, err := validateAudit(input); err != nil {
		return nil, err
	}
	data, _ := json.Marshal(input)
	var copied AuditInput
	_ = json.Unmarshal(data, &copied)
	id, err := shared.NewConfigOperationID()
	if err != nil {
		return nil, err
	}
	var item *AuditItem
	err = s.call(ctx, func(tx *sql.Tx) error {
		var err error
		item, err = appendAuditTx(tx, s.cfg.Now().UnixMilli(), copied, "audit", id)
		return err
	})
	return item, err
}

const auditColumns = "audit_id,recorded_at_ms,occurred_at_ms,kind,actor_kind,actor,observer,node_id,service_id,operation_id,operation_version,state,code,object_count,field_count,summary_json"

func scanAuditItem(row interface{ Scan(...any) error }) (*AuditItem, error) {
	item := new(AuditItem)
	var id int64
	var occurred, version, node sql.NullInt64
	var summary sql.NullString
	if err := row.Scan(&id, &item.RecordedAtMS, &occurred, &item.Kind, &item.ActorKind, &item.Actor, &item.Observer, &node, &item.ServiceID, &item.OperationID, &version, &item.State, &item.Code, &item.ObjectCount, &item.FieldCount, &summary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	item.AuditID = strconv.FormatInt(id, 10)
	if node.Valid {
		item.NodeID = strconv.FormatInt(node.Int64, 10)
	}
	if occurred.Valid {
		item.OccurredAtMS = &occurred.Int64
	}
	if version.Valid {
		item.OperationVersion = &version.Int64
	}
	if summary.Valid {
		decoder := json.NewDecoder(strings.NewReader(summary.String))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&item.PreviewSummary) != nil || item.PreviewSummary == nil || !validAuditSummary(item.PreviewSummary) {
			return nil, errors.New("invalid persisted audit summary")
		}
	}
	if id < 1 || item.RecordedAtMS < 1 || item.RecordedAtMS > 253402300799999 || item.ObjectCount < 0 || item.ObjectCount > 1024 || item.FieldCount < 0 || item.FieldCount > 65536 {
		return nil, errors.New("invalid persisted audit bounds")
	}
	if _, _, err := validateAudit(AuditInput{Kind: item.Kind, ActorKind: item.ActorKind, Actor: item.Actor, Observer: item.Observer, NodeID: item.NodeID, ServiceID: item.ServiceID, OperationID: item.OperationID, State: item.State, Code: item.Code, OccurredAtMS: item.OccurredAtMS, OperationVersion: item.OperationVersion, Summary: item.PreviewSummary}); err != nil {
		return nil, errors.New("invalid persisted audit metadata")
	}
	return item, nil
}

func auditDetailTx(tx *sql.Tx, id string) (*AuditDetail, error) {
	item, err := scanAuditItem(tx.QueryRow("SELECT "+auditColumns+" FROM config_audit_entries WHERE audit_id=?", id))
	if err != nil {
		return nil, err
	}
	var raw string
	if err = tx.QueryRow("SELECT changes_json FROM config_audit_entries WHERE audit_id=?", id).Scan(&raw); err != nil {
		return nil, err
	}
	detail := &AuditDetail{Item: *item, Changes: []ConfigOperationChange{}}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&detail.Changes) != nil {
		return nil, errors.New("invalid persisted audit changes")
	}
	if _, err := operationChanges(detail.Changes); err != nil {
		return nil, errors.New("invalid persisted audit changes")
	}
	return detail, nil
}

func (s *Store) GetAudit(ctx context.Context, id string) (*AuditDetail, error) {
	if !auditDecimal(id, false) {
		return nil, ErrInvalid
	}
	var result *AuditDetail
	err := s.call(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = auditDetailTx(tx, id)
		return err
	})
	return result, err
}
