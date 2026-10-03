package control

import (
	"context"
	"database/sql"
	"errors"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// ObserveAuditContext compares only the non-Store context. Managed Store writes
// legitimately change the full revision and cannot be called external drift.
// A new service identity starts a new baseline, including explicit restores.
func (s *Store) ObserveAuditContext(ctx context.Context, node, service, revision, observer string) error {
	if !validID(node) || !shared.ValidConfigOperationID(service) || !shared.ValidConfigDigest(revision) || !operationText(observer, 128) {
		return ErrInvalid
	}
	return s.call(ctx, func(tx *sql.Tx) error {
		var before string
		err := tx.QueryRow("SELECT context_revision FROM config_audit_observations WHERE node_id=? AND service_id=?", node, service).Scan(&before)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if before == revision {
			return nil
		}
		now := s.cfg.Now().UnixMilli()
		if before != "" {
			id, err := shared.NewConfigOperationID()
			if err != nil {
				return err
			}
			_, err = appendAuditTx(tx, now, AuditInput{Kind: "external_drift", ActorKind: "unknown", Observer: observer, NodeID: node, ServiceID: service, Code: "external_change", Summary: &AuditSummary{ContextRevision: revision, Warnings: []string{}}}, "audit", id)
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec("INSERT INTO config_audit_observations(node_id,service_id,context_revision,updated_at_ms) VALUES(?,?,?,?) ON CONFLICT(node_id,service_id) DO UPDATE SET context_revision=excluded.context_revision,updated_at_ms=excluded.updated_at_ms", node, service, revision, now)
		return err
	})
}
