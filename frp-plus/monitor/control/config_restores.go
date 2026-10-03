package control

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// ConfigRestore is a durable administrator takeover receipt. Pending means the
// claim exists, not that the Agent accepted it. Acknowledged still requires the
// Agent's separate local offline confirmation and next-start verification.
type ConfigRestore struct {
	ID                string `json:"id"`
	NodeID            string `json:"node_id"`
	ServiceID         string `json:"service_id"`
	Epoch             string `json:"epoch"`
	BackupServiceID   string `json:"backup_service_id"`
	ReplacedServiceID string `json:"replaced_service_id"`
	ManifestDigest    string `json:"manifest_digest"`
	ContextRevision   string `json:"context_revision"`
	StoreDigest       string `json:"store_digest"`
	State             string `json:"state"`
	Creator           string `json:"creator"`
	CreatedAtMS       int64  `json:"created_at_ms"`
	UpdatedAtMS       int64  `json:"updated_at_ms"`
	Version           int64  `json:"version"`
	TokenSHA256       string `json:"-"`
}

type ClaimConfigRestoreRequest struct {
	ID                        string
	NodeID                    string
	ServiceID                 string
	Epoch                     string
	BackupServiceID           string
	ReplacedServiceID         string
	ManifestDigest            string
	ContextRevision           string
	StoreDigest               string
	Creator                   string
	ExpectedTokenSHA256       string
	ExpectedActiveOperationID string
	ExpectedVersion           int64
}

const restoreColumns = "id,node_id,service_id,epoch,backup_service_id,replaced_service_id,manifest_digest,context_revision,store_digest,state,creator,created_at_ms,updated_at_ms,version,token_sha256"
const restoreActiveStates = "('draft','validated','prepared','applying','verifying','outcome_unknown','rolling_back','rollback_failed')"

// GetActiveConfigOperation uses the same predicate as the unique node lease
// index, without depending on the position of an operation in audit pages.
func (s *Store) GetActiveConfigOperation(ctx context.Context, nodeID string) (*ConfigOperation, error) {
	if !validID(nodeID) {
		return nil, ErrInvalid
	}
	var result *ConfigOperation
	err := s.call(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = scanConfigOperation(tx.QueryRow("SELECT "+operationColumns+" FROM config_operations WHERE node_id=? AND state IN "+restoreActiveStates, nodeID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func validRestoreIdentity(service, epoch, backup, replaced, manifest, revision, store string) bool {
	return shared.ValidConfigOperationID(service) && shared.ValidConfigOperationID(epoch) && shared.ValidConfigOperationID(backup) && (replaced == "" || shared.ValidConfigOperationID(replaced)) && service != backup && service != replaced && shared.ValidConfigDigest(manifest) && shared.ValidConfigDigest(revision) && shared.ValidConfigDigest(store)
}

func scanConfigRestore(row interface{ Scan(...any) error }) (*ConfigRestore, error) {
	r := new(ConfigRestore)
	err := row.Scan(&r.ID, &r.NodeID, &r.ServiceID, &r.Epoch, &r.BackupServiceID, &r.ReplacedServiceID, &r.ManifestDigest, &r.ContextRevision, &r.StoreDigest, &r.State, &r.Creator, &r.CreatedAtMS, &r.UpdatedAtMS, &r.Version, &r.TokenSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !shared.ValidConfigOperationID(r.ID) || !validID(r.NodeID) || !validRestoreIdentity(r.ServiceID, r.Epoch, r.BackupServiceID, r.ReplacedServiceID, r.ManifestDigest, r.ContextRevision, r.StoreDigest) || (r.State != "pending" && r.State != "acknowledged") || !operationText(r.Creator, 128) || !shared.ValidConfigDigest(r.TokenSHA256) || r.Version < 1 || r.CreatedAtMS <= 0 || r.UpdatedAtMS < r.CreatedAtMS {
		return nil, errors.New("invalid persisted restoration receipt")
	}
	return r, nil
}

func restoreNodeToken(tx *sql.Tx, nodeID, expected string) error {
	var token string
	if err := tx.QueryRow("SELECT token_sha256 FROM nodes WHERE id=?", nodeID).Scan(&token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if token != expected {
		return ErrConflict
	}
	return nil
}

// ClaimConfigRestore records consent before sending an acknowledgement. Only
// the exact expected prior active operation from the restored identity lineage
// can be superseded. Its Agent observation is retained unchanged for audit.
func (s *Store) ClaimConfigRestore(ctx context.Context, input ClaimConfigRestoreRequest) (*ConfigRestore, bool, error) {
	if !shared.ValidConfigOperationID(input.ID) || !validID(input.NodeID) || !validRestoreIdentity(input.ServiceID, input.Epoch, input.BackupServiceID, input.ReplacedServiceID, input.ManifestDigest, input.ContextRevision, input.StoreDigest) || !operationText(input.Creator, 128) || !shared.ValidConfigDigest(input.ExpectedTokenSHA256) || (input.ExpectedActiveOperationID == "" && input.ExpectedVersion != 0) || (input.ExpectedActiveOperationID != "" && (!shared.ValidConfigOperationID(input.ExpectedActiveOperationID) || input.ExpectedVersion < 1)) {
		return nil, false, ErrInvalid
	}
	var result *ConfigRestore
	replayed := false
	err := s.call(ctx, func(tx *sql.Tx) error {
		if err := restoreNodeToken(tx, input.NodeID, input.ExpectedTokenSHA256); err != nil {
			return err
		}
		old, err := scanConfigRestore(tx.QueryRow("SELECT "+restoreColumns+" FROM config_restores WHERE node_id=? AND epoch=?", input.NodeID, input.Epoch))
		if err == nil {
			if old.ServiceID != input.ServiceID || old.BackupServiceID != input.BackupServiceID || old.ReplacedServiceID != input.ReplacedServiceID || old.ManifestDigest != input.ManifestDigest || old.ContextRevision != input.ContextRevision || old.StoreDigest != input.StoreDigest || old.TokenSHA256 != input.ExpectedTokenSHA256 {
				return ErrConflict
			}
			// Another administrator may resume the same receipt. The original
			// creator, acknowledgement ID and prior supersession event remain.
			result, replayed = old, true
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		active, err := scanConfigOperation(tx.QueryRow("SELECT "+operationColumns+" FROM config_operations WHERE node_id=? AND state IN "+restoreActiveStates, input.NodeID))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if (active == nil && input.ExpectedActiveOperationID != "") || (active != nil && (active.OperationID != input.ExpectedActiveOperationID || active.Version != input.ExpectedVersion || active.Version == math.MaxInt64 || (active.ServiceID != input.BackupServiceID && active.ServiceID != input.ReplacedServiceID))) {
			return ErrConflict
		}
		now := s.cfg.Now().UnixMilli()
		if active != nil {
			active.State, active.Version, active.UpdatedAtMS = "cancelled", active.Version+1, max(now, active.UpdatedAtMS)
			updated, err := tx.Exec("UPDATE config_operations SET state=?,version=?,updated_at_ms=? WHERE operation_id=? AND version=?", active.State, active.Version, active.UpdatedAtMS, active.OperationID, input.ExpectedVersion)
			if err != nil {
				return err
			}
			count, err := updated.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return ErrConflict
			}
			if err = appendConfigOperationEvent(tx, active, "restore_superseded", []byte("[]"), now, input.Creator); err != nil {
				return err
			}
		}
		result = &ConfigRestore{ID: input.ID, NodeID: input.NodeID, ServiceID: input.ServiceID, Epoch: input.Epoch, BackupServiceID: input.BackupServiceID, ReplacedServiceID: input.ReplacedServiceID, ManifestDigest: input.ManifestDigest, ContextRevision: input.ContextRevision, StoreDigest: input.StoreDigest, State: "pending", Creator: input.Creator, CreatedAtMS: now, UpdatedAtMS: now, Version: 1, TokenSHA256: input.ExpectedTokenSHA256}
		_, err = tx.Exec("INSERT INTO config_restores("+restoreColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", result.ID, result.NodeID, result.ServiceID, result.Epoch, result.BackupServiceID, result.ReplacedServiceID, result.ManifestDigest, result.ContextRevision, result.StoreDigest, result.State, result.Creator, result.CreatedAtMS, result.UpdatedAtMS, result.Version, result.TokenSHA256)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}

// GetConfigRestore also reads receipts for deleted nodes; audit is never
// cascaded away. Claim and confirmation still require the live credential.
func (s *Store) GetConfigRestore(ctx context.Context, nodeID, epoch string) (*ConfigRestore, error) {
	if !validID(nodeID) || !shared.ValidConfigOperationID(epoch) {
		return nil, ErrInvalid
	}
	var result *ConfigRestore
	err := s.call(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = scanConfigRestore(tx.QueryRow("SELECT "+restoreColumns+" FROM config_restores WHERE node_id=? AND epoch=?", nodeID, epoch))
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ConfirmConfigRestore(ctx context.Context, id string, expectedVersion int64) (*ConfigRestore, error) {
	if !shared.ValidConfigOperationID(id) || expectedVersion < 1 {
		return nil, ErrInvalid
	}
	var result *ConfigRestore
	err := s.call(ctx, func(tx *sql.Tx) error {
		r, err := scanConfigRestore(tx.QueryRow("SELECT "+restoreColumns+" FROM config_restores WHERE id=?", id))
		if err != nil {
			return err
		}
		if err = restoreNodeToken(tx, r.NodeID, r.TokenSHA256); err != nil {
			return err
		}
		if r.State == "acknowledged" {
			if expectedVersion != r.Version && expectedVersion != r.Version-1 {
				return ErrConflict
			}
			result = r
			return nil
		}
		if r.Version != expectedVersion || r.Version == math.MaxInt64 {
			return ErrConflict
		}
		r.State, r.Version, r.UpdatedAtMS = "acknowledged", r.Version+1, max(s.cfg.Now().UnixMilli(), r.UpdatedAtMS)
		updated, err := tx.Exec("UPDATE config_restores SET state=?,version=?,updated_at_ms=? WHERE id=? AND version=?", r.State, r.Version, r.UpdatedAtMS, r.ID, expectedVersion)
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrConflict
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
