package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// ConfigOperation holds coordination metadata only. Neither candidate
// configuration nor native secret values belong in the control database.
type ConfigOperation struct {
	OperationID     string `json:"operation_id"`
	NodeID          string `json:"node_id"`
	ServiceID       string `json:"service_id"`
	BaseRevision    string `json:"base_revision"`
	CandidateDigest string `json:"candidate_digest"`
	Creator         string `json:"creator"`
	DeadlineAtMS    int64  `json:"deadline_at_ms"`
	IdempotencyKey  string `json:"idempotency_key"`
	RequestDigest   string `json:"request_digest"`
	State           string `json:"state"`
	Version         int64  `json:"version"`
	CreatedAtMS     int64  `json:"created_at_ms"`
	UpdatedAtMS     int64  `json:"updated_at_ms"`
}

type CreateConfigOperationRequest struct {
	OperationID     string
	NodeID          string
	ServiceID       string
	BaseRevision    string
	CandidateDigest string
	Creator         string
	DeadlineAtMS    int64
	IdempotencyKey  string
	RequestDigest   string
	Changes         []ConfigOperationChange
}

// ConfigOperationChange describes object/field names, never before/after values.
type ConfigOperationChange struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Action string   `json:"action"`
	Fields []string `json:"fields"`
}

type ConfigOperationTransition struct {
	ExpectedVersion int64
	NextState       string
	Code            string
	CandidateDigest string
	Changes         []ConfigOperationChange
}

type ConfigOperationEvent struct {
	EventID     int64                   `json:"event_id"`
	OperationID string                  `json:"operation_id"`
	Version     int64                   `json:"version"`
	State       string                  `json:"state"`
	Code        string                  `json:"code"`
	Changes     []ConfigOperationChange `json:"changes"`
	CreatedAtMS int64                   `json:"created_at_ms"`
}

const operationColumns = "operation_id,node_id,service_id,base_revision,candidate_digest,creator,deadline_at_ms,idempotency_key,request_digest,state,version,created_at_ms,updated_at_ms"

func scanConfigOperation(row interface{ Scan(...any) error }) (*ConfigOperation, error) {
	o := new(ConfigOperation)
	err := row.Scan(&o.OperationID, &o.NodeID, &o.ServiceID, &o.BaseRevision, &o.CandidateDigest, &o.Creator, &o.DeadlineAtMS, &o.IdempotencyKey, &o.RequestDigest, &o.State, &o.Version, &o.CreatedAtMS, &o.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}

func operationText(value string, maximum int) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func operationChanges(input []ConfigOperationChange) ([]byte, error) {
	if len(input) > 1024 {
		return nil, fmt.Errorf("%w: configuration change count", ErrInvalid)
	}
	if input == nil {
		input = []ConfigOperationChange{}
	}
	seen := make(map[string]bool, len(input))
	for _, c := range input {
		if (c.Kind != "proxy" && c.Kind != "visitor") || !operationText(c.Name, 256) || !operationAction(c.Action) || len(c.Fields) > 64 {
			return nil, fmt.Errorf("%w: configuration change summary", ErrInvalid)
		}
		key := c.Kind + "\x00" + c.Name
		if seen[key] {
			return nil, fmt.Errorf("%w: duplicate configuration change", ErrInvalid)
		}
		seen[key] = true
		fields := map[string]bool{}
		for _, field := range c.Fields {
			if !operationField(field) || fields[field] {
				return nil, fmt.Errorf("%w: configuration field summary", ErrInvalid)
			}
			fields[field] = true
		}
	}
	data, err := json.Marshal(input)
	if err != nil || len(data) > 64*1024 {
		return nil, fmt.Errorf("%w: configuration summary size", ErrInvalid)
	}
	return data, nil
}

func operationAction(value string) bool {
	switch value {
	case "create", "update", "delete", "enable", "disable", "rename":
		return true
	}
	return false
}

// Field paths are selected from the native managed-object vocabulary. Map
// keys, plugin parameters, URLs and submitted values cannot become audit text.
func operationField(value string) bool {
	switch value {
	case "name", "type", "enabled", "localIP", "localPort", "remotePort", "customDomains", "subdomain", "locations", "httpUser", "httpPassword", "hostHeaderRewrite", "routeByHTTPUser", "requestHeaders", "responseHeaders", "multiplexer", "secretKey", "allowUsers", "annotations", "metadatas", "transport", "transport.useEncryption", "transport.useCompression", "transport.bandwidthLimit", "transport.bandwidthLimitMode", "transport.proxyProtocolVersion", "healthCheck", "healthCheck.type", "healthCheck.timeoutSeconds", "healthCheck.maxFailed", "healthCheck.intervalSeconds", "healthCheck.path", "healthCheck.httpHeaders", "loadBalancer", "loadBalancer.group", "loadBalancer.groupKey", "plugin", "plugin.type", "serverUser", "serverName", "bindAddr", "bindPort", "protocol", "keepTunnelOpen", "maxRetriesAnHour", "minRetryInterval", "fallbackTo", "fallbackTimeoutMs", "natTraversal":
		return true
	}
	return false
}

func appendConfigOperationEvent(tx *sql.Tx, o *ConfigOperation, code string, changes []byte, now int64) error {
	_, err := tx.Exec("INSERT INTO config_operation_events(operation_id,version,state,code,changes_json,created_at_ms) VALUES(?,?,?,?,?,?)", o.OperationID, o.Version, o.State, code, string(changes), now)
	return err
}

func (s *Store) CreateConfigOperation(ctx context.Context, input CreateConfigOperationRequest) (*ConfigOperation, bool, error) {
	if !shared.ValidConfigOperationID(input.OperationID) || !shared.ValidConfigOperationID(input.IdempotencyKey) || !validID(input.NodeID) || !operationText(input.ServiceID, 128) || !operationText(input.Creator, 128) || !shared.ValidConfigDigest(input.BaseRevision) || !shared.ValidConfigDigest(input.RequestDigest) || (input.CandidateDigest != "" && !shared.ValidConfigDigest(input.CandidateDigest)) || input.DeadlineAtMS <= 0 {
		return nil, false, fmt.Errorf("%w: configuration operation", ErrInvalid)
	}
	changes, err := operationChanges(input.Changes)
	if err != nil {
		return nil, false, err
	}
	// Serialize caller-owned slices before enqueueing; cancellation can allow
	// the worker to finish after the caller has already returned.
	var result *ConfigOperation
	replayed := false
	err = s.call(ctx, func(tx *sql.Tx) error {
		old, err := scanConfigOperation(tx.QueryRow("SELECT "+operationColumns+" FROM config_operations WHERE idempotency_key=?", input.IdempotencyKey))
		if err == nil {
			// Match immutable request metadata as well as the supplied digest;
			// callers cannot replay a key against a different target/creator.
			if old.RequestDigest != input.RequestDigest || old.NodeID != input.NodeID || old.ServiceID != input.ServiceID || old.BaseRevision != input.BaseRevision || old.Creator != input.Creator || old.DeadlineAtMS != input.DeadlineAtMS || (input.CandidateDigest != "" && old.CandidateDigest != input.CandidateDigest) {
				return ErrConflict
			}
			result, replayed = old, true
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		now := s.cfg.Now().UnixMilli()
		if input.DeadlineAtMS <= now || input.DeadlineAtMS > now+int64((24*time.Hour)/time.Millisecond) {
			return fmt.Errorf("%w: configuration deadline", ErrInvalid)
		}
		var exists int
		if err := tx.QueryRow("SELECT 1 FROM nodes WHERE id=?", input.NodeID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		result = &ConfigOperation{OperationID: input.OperationID, NodeID: input.NodeID, ServiceID: input.ServiceID, BaseRevision: input.BaseRevision, CandidateDigest: input.CandidateDigest, Creator: input.Creator, DeadlineAtMS: input.DeadlineAtMS, IdempotencyKey: input.IdempotencyKey, RequestDigest: input.RequestDigest, State: "draft", Version: 1, CreatedAtMS: now, UpdatedAtMS: now}
		_, err = tx.Exec("INSERT INTO config_operations("+operationColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", result.OperationID, result.NodeID, result.ServiceID, result.BaseRevision, result.CandidateDigest, result.Creator, result.DeadlineAtMS, result.IdempotencyKey, result.RequestDigest, result.State, result.Version, result.CreatedAtMS, result.UpdatedAtMS)
		if err != nil {
			return err
		}
		return appendConfigOperationEvent(tx, result, "created", changes, now)
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}

func (s *Store) GetConfigOperation(ctx context.Context, id string) (*ConfigOperation, error) {
	if !shared.ValidConfigOperationID(id) {
		return nil, fmt.Errorf("%w: configuration operation ID", ErrInvalid)
	}
	var result *ConfigOperation
	err := s.call(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = scanConfigOperation(tx.QueryRow("SELECT "+operationColumns+" FROM config_operations WHERE operation_id=?", id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ListConfigOperations(ctx context.Context, nodeID string, limit int) ([]ConfigOperation, error) {
	if (nodeID != "" && !validID(nodeID)) || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("%w: configuration operation query", ErrInvalid)
	}
	result := []ConfigOperation{}
	err := s.call(ctx, func(tx *sql.Tx) error {
		query, args := "SELECT "+operationColumns+" FROM config_operations", []any{}
		if nodeID != "" {
			query += " WHERE node_id=?"
			args = append(args, nodeID)
		}
		query += " ORDER BY created_at_ms DESC,operation_id DESC LIMIT ?"
		args = append(args, limit)
		rows, err := tx.Query(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanConfigOperation(rows)
			if err != nil {
				return err
			}
			result = append(result, *o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) TransitionConfigOperation(ctx context.Context, id string, input ConfigOperationTransition) (*ConfigOperation, error) {
	if !shared.ValidConfigOperationID(id) || input.ExpectedVersion < 1 || !shared.ValidConfigOperationState(input.NextState) || !shared.ValidConfigEventCode(input.Code) || (input.CandidateDigest != "" && !shared.ValidConfigDigest(input.CandidateDigest)) {
		return nil, fmt.Errorf("%w: configuration transition", ErrInvalid)
	}
	changes, err := operationChanges(input.Changes)
	if err != nil {
		return nil, err
	}
	var result *ConfigOperation
	err = s.call(ctx, func(tx *sql.Tx) error {
		o, err := scanConfigOperation(tx.QueryRow("SELECT "+operationColumns+" FROM config_operations WHERE operation_id=?", id))
		if err != nil {
			return err
		}
		if o.Version != input.ExpectedVersion || o.Version == math.MaxInt64 || !shared.ConfigOperationTransitionAllowed(o.State, input.NextState) {
			return ErrConflict
		}
		now := s.cfg.Now().UnixMilli()
		if (input.NextState == "validated" || input.NextState == "prepared" || input.NextState == "applying") && now >= o.DeadlineAtMS {
			return ErrConflict
		}
		if input.CandidateDigest != "" {
			if o.CandidateDigest == "" && input.NextState != "validated" && input.NextState != "prepared" {
				return fmt.Errorf("%w: candidate digest assignment", ErrInvalid)
			}
			if o.CandidateDigest != "" && o.CandidateDigest != input.CandidateDigest {
				return ErrConflict
			}
			o.CandidateDigest = input.CandidateDigest
		}
		if o.CandidateDigest == "" && (input.NextState == "validated" || input.NextState == "prepared" || input.NextState == "applying" || input.NextState == "verifying" || input.NextState == "confirmed" || input.NextState == "rolling_back" || input.NextState == "rolled_back") {
			return fmt.Errorf("%w: validated candidate digest required", ErrInvalid)
		}
		o.State, o.Version, o.UpdatedAtMS = input.NextState, o.Version+1, now
		updated, err := tx.Exec("UPDATE config_operations SET state=?,version=?,candidate_digest=?,updated_at_ms=? WHERE operation_id=? AND version=?", o.State, o.Version, o.CandidateDigest, o.UpdatedAtMS, id, input.ExpectedVersion)
		if err != nil {
			return err
		}
		n, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrConflict
		}
		if err = appendConfigOperationEvent(tx, o, input.Code, changes, now); err != nil {
			return err
		}
		result = o
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ListConfigOperationEvents(ctx context.Context, id string) ([]ConfigOperationEvent, error) {
	if !shared.ValidConfigOperationID(id) {
		return nil, fmt.Errorf("%w: configuration operation ID", ErrInvalid)
	}
	result := []ConfigOperationEvent{}
	err := s.call(ctx, func(tx *sql.Tx) error {
		if _, err := scanConfigOperation(tx.QueryRow("SELECT "+operationColumns+" FROM config_operations WHERE operation_id=?", id)); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT event_id,operation_id,version,state,code,changes_json,created_at_ms FROM config_operation_events WHERE operation_id=? ORDER BY event_id", id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e ConfigOperationEvent
			var changes string
			if err := rows.Scan(&e.EventID, &e.OperationID, &e.Version, &e.State, &e.Code, &changes, &e.CreatedAtMS); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(changes), &e.Changes); err != nil {
				return errors.New("invalid persisted configuration event")
			}
			result = append(result, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
