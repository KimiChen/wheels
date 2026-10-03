package control

import (
	"context"
	"database/sql"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// ConfirmedRestoreLineage authorizes a read of immutable historical journal
// facts, never a command or rebinding. Only backup ancestry proves the material
// was carried forward; the replaced target identity is not an ancestor.
func (s *Store) ConfirmedRestoreLineage(ctx context.Context, node, ancestor, current, token string) (bool, error) {
	if !validID(node) || !shared.ValidConfigOperationID(ancestor) || !shared.ValidConfigOperationID(current) || !shared.ValidConfigDigest(token) {
		return false, ErrInvalid
	}
	allowed := false
	err := s.call(ctx, func(tx *sql.Tx) error {
		if err := restoreNodeToken(tx, node, token); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT r.service_id,r.backup_service_id FROM config_restores r JOIN config_restore_completions c ON c.receipt_id=r.id WHERE r.node_id=? AND r.token_sha256=? AND r.state='acknowledged' ORDER BY c.confirmed_at_ms DESC,r.id DESC LIMIT 129`, node, token)
		if err != nil {
			return err
		}
		defer rows.Close()
		parents := map[string]string{}
		count := 0
		for rows.Next() {
			var child, parent string
			if err = rows.Scan(&child, &parent); err != nil {
				return err
			}
			count++
			if count > 128 {
				return ErrConflict
			}
			if _, exists := parents[child]; exists {
				return ErrConflict
			}
			parents[child] = parent
		}
		if err = rows.Err(); err != nil {
			return err
		}
		seen := map[string]bool{}
		for steps := 0; steps < 128; steps++ {
			if current == ancestor {
				allowed = len(seen) > 0
				return nil
			}
			if seen[current] {
				return ErrConflict
			}
			seen[current] = true
			parent, ok := parents[current]
			if !ok {
				return nil
			}
			current = parent
		}
		return ErrConflict
	})
	return allowed, err
}
