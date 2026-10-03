package control

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// AuditRetentionMigration follows the history migration in the same startup
// transaction. The caller owns the database version and pre-migration backup.
//
//go:embed audit_retention_schema.sql
var AuditRetentionMigration string

var ErrAuditCapacity = fmt.Errorf("%w: audit capacity reached", ErrConflict)
var ErrAuditCursorExpired = fmt.Errorf("%w: audit history was pruned", ErrConflict)

const auditGCBatch = 32
const auditProtectedStates = "'draft','validated','prepared','applying','verifying','outcome_unknown','rolling_back','rollback_failed'"

type AuditGCResult struct {
	Rows       int64
	Events     int64
	Bytes      int64
	Generation string
}

func (s *Store) auditPolicy() shared.AuditConfig {
	p := s.cfg.Audit
	p.Complete()
	return p
}

func auditUsageTx(tx *sql.Tx) (rows, bytes, generation int64, err error) {
	err = tx.QueryRow("SELECT audit_rows,audit_bytes+event_bytes,generation FROM config_audit_retention WHERE singleton=1").Scan(&rows, &bytes, &generation)
	return
}

func (s *Store) auditRetentionTx(tx *sql.Tx) (AuditRetention, error) {
	p := s.auditPolicy()
	r := AuditRetention{AuditDays: p.RetentionDays, SnapshotDays: 30, CleanupEnabled: true, MaxRows: p.MaxRows, MaxBytes: p.MaxBytes}
	var generation int64
	err := tx.QueryRow("SELECT audit_rows,audit_bytes+event_bytes,generation,last_gc_at_ms FROM config_audit_retention WHERE singleton=1").Scan(&r.Rows, &r.Bytes, &generation, &r.LastGCAtMS)
	r.Generation = strconv.FormatInt(generation, 10)
	r.CapacityBlocked = r.Rows >= r.MaxRows || r.Bytes >= r.MaxBytes
	return r, err
}

func (s *Store) auditAdmissionTx(tx *sql.Tx) error {
	rows, bytes, _, err := auditUsageTx(tx)
	if err != nil {
		return err
	}
	p := s.auditPolicy()
	if rows >= p.MaxRows || bytes >= p.MaxBytes {
		return ErrAuditCapacity
	}
	return nil
}

// maintenanceCall yields to already queued work and checks again on the
// worker. At most one bounded maintenance transaction can precede new work.
func (s *Store) maintenanceCall(ctx context.Context, fn func(*sql.Tx) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(s.queue) != 0 {
		return ErrAuditBusy
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	r := request{ctx: ctx, done: make(chan error, 1), call: func(tx *sql.Tx) error {
		if len(s.queue) != 0 {
			return ErrAuditBusy
		}
		return fn(tx)
	}}
	select {
	case <-s.stop:
		return ErrClosed
	default:
	}
	select {
	case s.queue <- r:
	default:
		return ErrAuditBusy
	}
	select {
	case err := <-r.done:
		if err == ErrAuditBusy {
			return err
		}
		return s.classify(err)
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	}
}

// PruneAudit removes only expired administrative history. Current operation
// metadata and idempotency keys remain. Capacity never shortens the TTL.
func (s *Store) PruneAudit(ctx context.Context) (*AuditGCResult, error) {
	result := new(AuditGCResult)
	err := s.maintenanceCall(ctx, func(tx *sql.Tx) error {
		now := s.cfg.Now().UnixMilli()
		cutoff := now - int64(s.auditPolicy().RetentionDays)*auditDayMS
		_, before, generation, err := auditUsageTx(tx)
		if err != nil {
			return err
		}
		// Whole unfinished operation timelines are protected, including events
		// predating the active phase. Terminal history ages from completion.
		// Restore lineage ages only after matching authenticated confirmation;
		// acknowledged receipts alone never release their history.
		q := `SELECT a.audit_id FROM config_audit_entries a WHERE a.recorded_at_ms<?
 AND (a.operation_id='' OR EXISTS(SELECT 1 FROM config_operations o WHERE o.operation_id=a.operation_id AND o.state NOT IN (` + auditProtectedStates + `) AND o.updated_at_ms<?))
 AND NOT EXISTS(SELECT 1 FROM config_restores r WHERE r.node_id=a.node_id AND a.service_id IN (r.service_id,r.backup_service_id,r.replaced_service_id) AND NOT EXISTS(SELECT 1 FROM config_restore_completions c WHERE c.receipt_id=r.id AND c.confirmed_at_ms<?))
 ORDER BY a.recorded_at_ms,a.audit_id LIMIT ?`
		rows, err := tx.Query(q, cutoff, cutoff, cutoff, auditGCBatch)
		if err != nil {
			return err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			removed, err := auditDeleteWithinBudget(tx, "DELETE FROM config_audit_entries WHERE audit_id=?", id, before)
			if err != nil {
				return err
			}
			if !removed {
				break
			}
			result.Rows++
		}
		eventRows, err := tx.Query(`SELECT e.event_id FROM config_operation_events e JOIN config_operations o ON o.operation_id=e.operation_id WHERE e.created_at_ms<? AND o.updated_at_ms<? AND o.state NOT IN (`+auditProtectedStates+`) AND NOT EXISTS(SELECT 1 FROM config_restores r WHERE r.node_id=o.node_id AND o.service_id IN (r.service_id,r.backup_service_id,r.replaced_service_id) AND NOT EXISTS(SELECT 1 FROM config_restore_completions c WHERE c.receipt_id=r.id AND c.confirmed_at_ms<?)) ORDER BY e.event_id LIMIT ?`, cutoff, cutoff, cutoff, auditGCBatch)
		if err != nil {
			return err
		}
		eventIDs := []int64{}
		for eventRows.Next() {
			var id int64
			if err = eventRows.Scan(&id); err != nil {
				eventRows.Close()
				return err
			}
			eventIDs = append(eventIDs, id)
		}
		err = eventRows.Err()
		eventRows.Close()
		if err != nil {
			return err
		}
		for _, id := range eventIDs {
			removed, err := auditDeleteWithinBudget(tx, "DELETE FROM config_operation_events WHERE event_id=?", id, before)
			if err != nil {
				return err
			}
			if !removed {
				break
			}
			result.Events++
		}
		_, after, _, err := auditUsageTx(tx)
		if err != nil {
			return err
		}
		result.Bytes = before - after
		if result.Rows+result.Events > 0 {
			generation++
			if _, err = tx.Exec("UPDATE config_audit_retention SET generation=?,pruned_rows=pruned_rows+?,pruned_events=pruned_events+?,last_gc_at_ms=? WHERE singleton=1", generation, result.Rows, result.Events, now); err != nil {
				return err
			}
			id, err := shared.NewConfigOperationID()
			if err != nil {
				return err
			}
			_, err = appendAuditTx(tx, now, AuditInput{Kind: "state", ActorKind: "system", Actor: "system:audit-retention", Code: "audit_gc", Summary: &AuditSummary{Rows: int(result.Rows + result.Events), Bytes: int(result.Bytes), Warnings: []string{}}}, "audit", id)
			if err != nil {
				return err
			}
		}
		result.Generation = strconv.FormatInt(generation, 10)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RunAuditMaintenance exits with its service context. It neither checkpoints
// WAL nor vacuums SQLite, so an export reader never makes GC a long writer.
func (s *Store) RunAuditMaintenance(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		work, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		result, err := s.PruneAudit(work)
		cancel()
		delay := time.Minute
		if err == nil && result.Rows+result.Events >= auditGCBatch {
			delay = time.Second
		}
		timer.Reset(delay)
	}
}

// A savepoint includes cascaded object rows and counter triggers. An individual
// deletion exceeding the remaining budget is undone without losing earlier
// eligible work; the enclosing transaction still commits GC and its audit atomically.
func auditDeleteWithinBudget(tx *sql.Tx, statement string, id, before int64) (bool, error) {
	if _, err := tx.Exec("SAVEPOINT audit_gc_item"); err != nil {
		return false, err
	}
	if _, err := tx.Exec(statement, id); err != nil {
		return false, err
	}
	_, after, _, err := auditUsageTx(tx)
	if err != nil {
		return false, err
	}
	allowed := before-after <= MaxAuditExportBytes
	if !allowed {
		if _, err = tx.Exec("ROLLBACK TO audit_gc_item"); err != nil {
			return false, err
		}
	}
	_, err = tx.Exec("RELEASE audit_gc_item")
	return allowed, err
}
