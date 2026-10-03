package control

import (
	"context"
	"database/sql"
	"math"
	"strings"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const (
	MaxServerRestoreReceipts         = 10000
	MaxServerRestoreReplacementDepth = 128
)

// ServerRestoreReceipt is local database evidence, never a remote DTO. A nil
// completion means no observation; a present zero timestamp is corrupt evidence.
type ServerRestoreReceipt struct {
	Receipt            ConfigRestore `json:"-"`
	CompletedAtMS      *int64        `json:"-"`
	CurrentTokenSHA256 string        `json:"-"`
}

// ServerRestoreConfirmation reports only the local ledger's recovery condition.
// Ready is not a statement about current Agent reachability or business health.
// The offline caller must separately verify its context, service stoppage and
// explicit operator acknowledgement before removing the startup write gate.
type ServerRestoreConfirmation struct {
	Ready            bool   `json:"ready"`
	Code             string `json:"code"`
	ActiveOperations int64  `json:"active_operations"`
	Receipts         int    `json:"receipts"`
	Completed        int    `json:"completed"`
	Superseded       int    `json:"superseded"`
}

func validServerRestoreReceipt(r ConfigRestore) bool {
	return shared.ValidConfigOperationID(r.ID) && validID(r.NodeID) &&
		validRestoreIdentity(r.ServiceID, r.Epoch, r.BackupServiceID, r.ReplacedServiceID, r.ManifestDigest, r.ContextRevision, r.StoreDigest) &&
		(r.State == "pending" || r.State == "acknowledged") && operationText(r.Creator, 128) &&
		shared.ValidConfigDigest(r.TokenSHA256) && r.Version >= 1 && r.CreatedAtMS > 0 && r.UpdatedAtMS >= r.CreatedAtMS
}

// EvaluateServerRestoreConfirmation never alters receipt or operation state.
// Historical completed receipts remain valid after node deletion/token rotation.
// Incomplete receipts need a unique replacement chain under the current binding
// ending in a genuine completed receipt. Backup ancestry and audit event text do
// not prove that a replaced installation ceased to be an unfinished recovery.
func EvaluateServerRestoreConfirmation(states map[string]int64, receipts []ServerRestoreReceipt) ServerRestoreConfirmation {
	out := ServerRestoreConfirmation{Code: "invalid_operation_state", Receipts: len(receipts)}
	for state, count := range states {
		if !shared.ValidConfigOperationState(state) || count < 0 {
			return out
		}
		if shared.ConfigOperationActive(state) {
			if count > math.MaxInt64-out.ActiveOperations {
				return out
			}
			out.ActiveOperations += count
		}
	}
	if out.ActiveOperations != 0 {
		out.Code = "active_operations"
		return out
	}
	if len(receipts) > MaxServerRestoreReceipts {
		out.Code = "receipt_limit"
		return out
	}
	type identity struct{ node, service string }
	children := make(map[identity][]int, len(receipts))
	ids := make(map[string]bool, len(receipts))
	epochs := make(map[identity]bool, len(receipts))
	services := make(map[identity]bool, len(receipts))
	complete := make([]bool, len(receipts))
	for i, evidence := range receipts {
		r := evidence.Receipt
		if !validServerRestoreReceipt(r) || ids[r.ID] || epochs[identity{r.NodeID, r.Epoch}] || services[identity{r.NodeID, r.ServiceID}] {
			out.Code = "invalid_receipt"
			return out
		}
		ids[r.ID], epochs[identity{r.NodeID, r.Epoch}], services[identity{r.NodeID, r.ServiceID}] = true, true, true
		// A real Agent observation may precede a delayed Admin ack. Completion
		// is an independent fact. Without acknowledgement the receipt must
		// still be proved superseded, or require an explicit acknowledgement.
		if evidence.CompletedAtMS != nil {
			if *evidence.CompletedAtMS <= 0 || *evidence.CompletedAtMS < r.CreatedAtMS {
				out.Code = "invalid_completion"
				return out
			}
			if r.State == "acknowledged" {
				complete[i] = true
				out.Completed++
			}
		}
		if r.ReplacedServiceID != "" {
			key := identity{r.NodeID, r.ReplacedServiceID}
			children[key] = append(children[key], i)
		}
	}
	for start, evidence := range receipts {
		if complete[start] {
			continue
		}
		token := evidence.Receipt.TokenSHA256
		seen := make(map[int]bool)
		at := start
		for depth := 0; ; depth++ {
			if seen[at] {
				out.Code = "receipt_chain_cycle"
				return out
			}
			seen[at] = true
			current := receipts[at]
			if current.Receipt.TokenSHA256 != token || current.CurrentTokenSHA256 != token {
				out.Code = "receipt_binding"
				return out
			}
			if complete[at] {
				out.Superseded++
				break
			}
			if depth >= MaxServerRestoreReplacementDepth {
				out.Code = "receipt_chain_limit"
				return out
			}
			next := children[identity{current.Receipt.NodeID, current.Receipt.ServiceID}]
			if len(next) == 0 {
				out.Code = "receipt_unresolved"
				if current.CompletedAtMS != nil {
					out.Code = "receipt_acknowledgement_required"
				}
				return out
			}
			if len(next) != 1 {
				out.Code = "receipt_chain_ambiguous"
				return out
			}
			if receipts[next[0]].Receipt.CreatedAtMS < current.Receipt.CreatedAtMS {
				out.Code = "receipt_chain_order"
				return out
			}
			at = next[0]
		}
	}
	out.Ready, out.Code = true, "ok"
	return out
}

// InspectServerRestoreConfirmation reads one caller-owned SQLite snapshot. It
// issues SELECT statements only, does not migrate an old schema, and refuses
// missing tables/errors rather than interpreting them as an empty safe ledger.
// Offline tools must open mode=ro/query_only and retain their service-stop lease.
func InspectServerRestoreConfirmation(ctx context.Context, tx *sql.Tx) (ServerRestoreConfirmation, error) {
	out := ServerRestoreConfirmation{Code: "unavailable"}
	rows, err := tx.QueryContext(ctx, "SELECT state,COUNT(*) FROM config_operations GROUP BY state LIMIT 16")
	if err != nil {
		return out, err
	}
	states := make(map[string]int64)
	for rows.Next() {
		var state string
		var count int64
		if err = rows.Scan(&state, &count); err != nil {
			rows.Close()
			return out, err
		}
		states[state] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	// Foreign keys may have been disabled by an external writer. An orphaned
	// completion cannot silently disappear from the left-joined evidence set.
	var orphan bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM config_restore_completions c LEFT JOIN config_restores r ON r.id=c.receipt_id WHERE r.id IS NULL)").Scan(&orphan); err != nil {
		return out, err
	}
	if orphan {
		out.Code = "invalid_completion"
		return out, nil
	}
	columns := "r." + strings.ReplaceAll(restoreColumns, ",", ",r.")
	rows, err = tx.QueryContext(ctx, "SELECT "+columns+",c.confirmed_at_ms,n.token_sha256 FROM config_restores r LEFT JOIN config_restore_completions c ON c.receipt_id=r.id LEFT JOIN nodes n ON n.id=r.node_id ORDER BY r.id LIMIT ?", MaxServerRestoreReceipts+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	receipts := make([]ServerRestoreReceipt, 0)
	for rows.Next() {
		var evidence ServerRestoreReceipt
		r := &evidence.Receipt
		var at sql.NullInt64
		var token sql.NullString
		if err = rows.Scan(&r.ID, &r.NodeID, &r.ServiceID, &r.Epoch, &r.BackupServiceID, &r.ReplacedServiceID, &r.ManifestDigest, &r.ContextRevision, &r.StoreDigest, &r.State, &r.Creator, &r.CreatedAtMS, &r.UpdatedAtMS, &r.Version, &r.TokenSHA256, &at, &token); err != nil {
			return out, err
		}
		if at.Valid {
			value := at.Int64
			evidence.CompletedAtMS = &value
		}
		evidence.CurrentTokenSHA256 = token.String
		receipts = append(receipts, evidence)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	return EvaluateServerRestoreConfirmation(states, receipts), nil
}

// CheckServerRestoreConfirmation uses an independent read-only WAL snapshot;
// it cannot occupy the serialized control write queue or mutate recovery facts.
func (s *Store) CheckServerRestoreConfirmation(ctx context.Context) (ServerRestoreConfirmation, error) {
	out := ServerRestoreConfirmation{Code: "unavailable"}
	err := s.readAuditSnapshot(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = InspectServerRestoreConfirmation(ctx, tx)
		return err
	})
	return out, err
}
