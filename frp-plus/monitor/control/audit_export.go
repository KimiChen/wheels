package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const MaxAuditExportRows = 10000
const MaxAuditExportBytes = 8 * 1024 * 1024

var ErrAuditExportLimit = errors.New("audit export exceeds bounded output")
var ErrAuditBusy = errors.New("audit export reader is already active")

type AuditExport struct {
	Data    []byte       `json:"-"`
	Summary AuditSummary `json:"-"`
}

type auditExportHeader struct {
	Schema      int            `json:"schema"`
	Kind        string         `json:"kind"`
	ExportID    string         `json:"export_id"`
	CreatedAtMS int64          `json:"created_at_ms"`
	WatermarkID string         `json:"watermark_id"`
	Filters     AuditFilter    `json:"filters"`
	Retention   AuditRetention `json:"retention"`
}

// ExportAudit generates all bytes before returning. Its requested/prepared
// records are durable before HTTP may send anything; prepared does not claim
// that a browser received or saved the file. Its own events are above watermark.
func (s *Store) ExportAudit(ctx context.Context, filter AuditFilter, actor string) (*AuditExport, error) {
	filter, err := NormalizeAuditFilter(filter, s.cfg.Now().UnixMilli())
	if err != nil || !operationText(actor, 128) {
		return nil, ErrInvalid
	}
	id, err := shared.NewConfigOperationID()
	if err != nil {
		return nil, err
	}
	kind, actor := auditActor(actor)
	summary := &AuditSummary{ExportID: id, FilterDigest: auditFilterHash(filter), Warnings: []string{}}
	input := AuditInput{Kind: "export", ActorKind: kind, Actor: actor, Code: "export_requested", Summary: summary}
	header := auditExportHeader{Schema: 1, Kind: "audit_export", ExportID: id, CreatedAtMS: s.cfg.Now().UnixMilli(), Filters: filter}
	err = s.call(ctx, func(tx *sql.Tx) error {
		watermark, err := auditWatermark(tx)
		if err != nil {
			return err
		}
		header.Retention, err = s.auditRetentionTx(tx)
		if err != nil {
			return err
		}
		header.WatermarkID, summary.WatermarkID = watermark, watermark
		_, err = appendAuditTx(tx, header.CreatedAtMS, input, "export_requested", id)
		return err
	})
	if err != nil {
		return nil, err
	}
	failed := func(original error) (*AuditExport, error) {
		failure := input
		failure.Code = "export_failed"
		// The client's cancellation must not cancel the failure audit attempt.
		auditCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = s.RecordAudit(auditCtx, failure)
		return nil, original
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(true)
	_ = encoder.Encode(header)
	count := 0
	err = s.readAuditSnapshot(ctx, func(tx *sql.Tx) error {
		retention, err := s.auditRetentionTx(tx)
		if err != nil {
			return err
		}
		if retention.Generation != header.Retention.Generation {
			return ErrAuditCursorExpired
		}
		where, args := auditWhere(filter, header.WatermarkID, nil)
		args = append(args, MaxAuditExportRows+1)
		rows, err := tx.Query("SELECT audit_id FROM config_audit_entries WHERE "+where+" ORDER BY recorded_at_ms DESC,audit_id DESC LIMIT ?", args...)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
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
		if len(ids) > MaxAuditExportRows {
			return ErrAuditExportLimit
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			detail, err := auditDetailTx(tx, id)
			if err != nil {
				return err
			}
			line, err := json.Marshal(struct {
				Schema int    `json:"schema"`
				Kind   string `json:"kind"`
				*AuditDetail
			}{1, "audit_entry", detail})
			if err != nil {
				return err
			}
			if output.Len()+len(line)+1 > MaxAuditExportBytes {
				return ErrAuditExportLimit
			}
			output.Write(line)
			output.WriteByte('\n')
			count++
		}
		return nil
	})
	if err != nil {
		return failed(err)
	}
	input.Code, summary.Rows, summary.Bytes = "export_prepared", count, output.Len()
	if _, err = s.RecordAudit(ctx, input); err != nil {
		return failed(err)
	}
	return &AuditExport{Data: output.Bytes(), Summary: *summary}, nil
}

// Export scanning uses an independent read-only WAL snapshot. It must not
// monopolize the serialized control write/ingestion queue while rendering up
// to 8 MiB. Only requested/prepared/failure audit writes use that queue.
func (s *Store) readAuditSnapshot(ctx context.Context, read func(*sql.Tx) error) error {
	if !s.auditReadMu.TryLock() {
		return ErrAuditBusy
	}
	defer s.auditReadMu.Unlock()
	u := url.URL{Scheme: "file", Path: s.cfg.Path}
	q := url.Values{"mode": []string{"ro"}, "_pragma": []string{"query_only(ON)", "busy_timeout(5000)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = read(tx); err != nil {
		return err
	}
	return tx.Commit()
}
