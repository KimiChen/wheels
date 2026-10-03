package control

import (
	"context"
	"database/sql"
	"errors"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// ObserveConfigRestoreConfirmed records an authenticated, request-bound Agent
// observation. A receipt alone, including an acknowledged receipt, cannot prove
// offline confirmation and subsequent runtime verification. The caller obtains
// token before dispatch; the transaction checks it against the live node again.
// Repeated observations preserve the original retention clock.
func (s *Store) ObserveConfigRestoreConfirmed(ctx context.Context, node, service, token string, info shared.RestoreInfo) error {
	if !validID(node) || !shared.ValidConfigOperationID(service) || !shared.ValidConfigDigest(token) || info.Validate() != nil || info.State != "confirmed" {
		return ErrInvalid
	}
	return s.call(ctx, func(tx *sql.Tx) error {
		if err := restoreNodeToken(tx, node, token); err != nil {
			return err
		}
		r, err := scanConfigRestore(tx.QueryRow("SELECT "+restoreColumns+" FROM config_restores WHERE node_id=? AND epoch=?", node, info.Epoch))
		if err != nil {
			return err
		}
		if r.ID != info.AcknowledgementID || r.ServiceID != service || r.TokenSHA256 != token || r.BackupServiceID != info.BackupServiceID || r.ReplacedServiceID != info.ReplacedServiceID || r.ManifestDigest != info.ManifestDigest || r.ContextRevision != info.ContextRevision || r.StoreDigest != info.StoreDigest {
			return ErrConflict
		}
		var at int64
		err = tx.QueryRow("SELECT confirmed_at_ms FROM config_restore_completions WHERE receipt_id=?", r.ID).Scan(&at)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		at = max(s.cfg.Now().UnixMilli(), r.UpdatedAtMS)
		if _, err = tx.Exec("INSERT INTO config_restore_completions(receipt_id,confirmed_at_ms) VALUES(?,?)", r.ID, at); err != nil {
			return err
		}
		_, err = appendAuditTx(tx, at, AuditInput{Kind: "restore", ActorKind: "system", Actor: "system:agent-observer", Observer: "agent", NodeID: node, ServiceID: service, State: "confirmed", Code: "restore_confirmed", Summary: &AuditSummary{ContextRevision: r.ContextRevision, CandidateDigest: r.StoreDigest, Warnings: []string{}}}, "restore_completion", r.ID)
		return err
	})
}
