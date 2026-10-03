package shared

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	ConfigRestoreCapability   = "config.restore.v1"
	MaxRestoreControlBytes    = 16 * 1024
	MaxRestoreOperations      = 1024
	MaxRestoreCommandDuration = time.Minute
)

// RestoreProvider exposes only restore identity and verification facts. It is
// independently negotiated, explicitly enabled and never a public observation.
// Authorization, durable idempotency and current restoration identity are local
// provider responsibilities; transport negotiation alone does not authorize it.
type RestoreProvider interface {
	HandleRestore(context.Context, RestoreCommand) RestoreResult
}

type RestoreCommand struct {
	Meta
	RequestID         string `json:"request_id"`
	ServiceID         string `json:"service_id"`
	Action            string `json:"action"`
	Epoch             string `json:"epoch"`
	ManifestDigest    string `json:"manifest_digest"`
	ContextRevision   string `json:"context_revision"`
	StoreDigest       string `json:"store_digest"`
	AcknowledgementID string `json:"acknowledgement_id"`
	DeadlineAtMS      int64  `json:"deadline_at_ms"`
}

type RestoreResult struct {
	Meta
	RequestID string       `json:"request_id"`
	ServiceID string       `json:"service_id"`
	Action    string       `json:"action"`
	Code      string       `json:"code"`
	Restore   *RestoreInfo `json:"restore,omitempty"`
}

// RestoreInfo deliberately contains no paths, object values or credentials.
// Runtime facts represent local resource verification, not business traffic.
type RestoreInfo struct {
	State             string `json:"state"`
	Epoch             string `json:"epoch"`
	BackupServiceID   string `json:"backup_service_id"`
	ReplacedServiceID string `json:"replaced_service_id"`
	ManifestDigest    string `json:"manifest_digest"`
	ContextRevision   string `json:"context_revision"`
	StoreDigest       string `json:"store_digest"`
	AcknowledgementID string `json:"acknowledgement_id"`
	RuntimeLoaded     bool   `json:"runtime_loaded"`
	ResourcesReady    bool   `json:"resources_ready"`
	OperationsCount   int    `json:"operations_count"`
}

func restoreSize(value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > MaxRestoreControlBytes || checkJSON(data) != nil {
		return errors.New("invalid restore control size or JSON")
	}
	return nil
}
func (c RestoreCommand) Validate() error {
	invalid := errors.New("invalid restore command")
	if err := c.Meta.Validate(); err != nil {
		return err
	}
	if c.Sequence < 2 || !ValidConfigOperationID(c.RequestID) || !detailEnum(c.Action, "inspect", "acknowledge") || (c.ServiceID == "" && c.Action != "inspect") || (c.ServiceID != "" && !ValidConfigOperationID(c.ServiceID)) || !configMillis(c.DeadlineAtMS) {
		return invalid
	}
	collected, _ := time.Parse(time.RFC3339Nano, c.CollectedAt)
	if c.DeadlineAtMS <= collected.UnixMilli() || c.DeadlineAtMS-collected.UnixMilli() > MaxRestoreCommandDuration.Milliseconds() {
		return invalid
	}
	if c.Action == "inspect" {
		if c.Epoch != "" || c.ManifestDigest != "" || c.ContextRevision != "" || c.StoreDigest != "" || c.AcknowledgementID != "" {
			return invalid
		}
	} else if !ValidConfigOperationID(c.Epoch) || !ValidConfigOperationID(c.AcknowledgementID) || !ValidConfigDigest(c.ManifestDigest) || !ValidConfigDigest(c.ContextRevision) || !ValidConfigDigest(c.StoreDigest) {
		return invalid
	}
	return restoreSize(c)
}
func (c RestoreCommand) ValidateAt(now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.DeadlineAtMS <= now.UnixMilli() || c.DeadlineAtMS-now.UnixMilli() > MaxRestoreCommandDuration.Milliseconds() {
		return errors.New("restore command expired or too far in future")
	}
	return nil
}
func (i RestoreInfo) Validate() error {
	invalid := errors.New("invalid restore information")
	if i.State == "none" {
		if i != (RestoreInfo{State: "none"}) {
			return invalid
		}
		return nil
	}
	if !detailEnum(i.State, "pending", "verified", "acknowledged", "confirmed") || !ValidConfigOperationID(i.Epoch) || !ValidConfigOperationID(i.BackupServiceID) || (i.ReplacedServiceID != "" && !ValidConfigOperationID(i.ReplacedServiceID)) || !ValidConfigDigest(i.ManifestDigest) || !ValidConfigDigest(i.ContextRevision) || i.OperationsCount < 0 || i.OperationsCount > MaxRestoreOperations {
		return invalid
	}
	if i.State == "pending" {
		if i.StoreDigest != "" || i.AcknowledgementID != "" || i.RuntimeLoaded || i.ResourcesReady {
			return invalid
		}
	} else {
		if !ValidConfigDigest(i.StoreDigest) || !i.RuntimeLoaded || !i.ResourcesReady {
			return invalid
		}
		if i.State == "verified" {
			if i.AcknowledgementID != "" {
				return invalid
			}
		} else if !ValidConfigOperationID(i.AcknowledgementID) {
			return invalid
		}
	}
	return nil
}
func (r RestoreResult) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return err
	}
	if r.Sequence < 2 || !ValidConfigOperationID(r.RequestID) || !ValidConfigOperationID(r.ServiceID) || !detailEnum(r.Action, "inspect", "acknowledge") || !ValidConfigResultCode(r.Code) {
		return errors.New("invalid restore result")
	}
	if r.Restore != nil {
		if err := r.Restore.Validate(); err != nil {
			return err
		}
	}
	if r.Code == "ok" && (r.Restore == nil || (r.Action == "acknowledge" && !detailEnum(r.Restore.State, "acknowledged", "confirmed"))) {
		return errors.New("missing restore result facts")
	}
	return restoreSize(r)
}

// MatchesRestoreResult binds a provider or transport reply to its request.
// Error replies may carry the current (changed) restore identity for diagnosis.
func MatchesRestoreResult(c RestoreCommand, r RestoreResult) bool {
	if r.Validate() != nil || r.RequestID != c.RequestID || r.Action != c.Action || (c.ServiceID != "" && r.ServiceID != c.ServiceID && r.Code != "service_mismatch") {
		return false
	}
	if c.Action == "acknowledge" && r.Code == "ok" {
		i := r.Restore
		return i.Epoch == c.Epoch && i.ManifestDigest == c.ManifestDigest && i.ContextRevision == c.ContextRevision && i.StoreDigest == c.StoreDigest && i.AcknowledgementID == c.AcknowledgementID
	}
	return true
}
