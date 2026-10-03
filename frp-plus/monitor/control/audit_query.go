package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const MaxAuditCursor = 1024
const auditDayMS = int64(24 * time.Hour / time.Millisecond)

type AuditFilter struct {
	FromMS      int64  `json:"from_ms"`
	ToMS        int64  `json:"to_ms"`
	ActorKind   string `json:"actor_kind"`
	Actor       string `json:"actor"`
	NodeID      string `json:"node_id"`
	ServiceID   string `json:"service_id"`
	ObjectKind  string `json:"object_kind"`
	ObjectName  string `json:"object_name"`
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
	Kind        string `json:"kind"`
}

// AuditRetention reports local controller policy and observed logical usage.
// SnapshotDays is the Agent default, not a claim about a remote Agent override.
type AuditRetention struct {
	AuditDays       int    `json:"audit_days"`
	SnapshotDays    int    `json:"snapshot_days"`
	CleanupEnabled  bool   `json:"cleanup_enabled"`
	MaxRows         int64  `json:"max_rows"`
	MaxBytes        int64  `json:"max_bytes"`
	Rows            int64  `json:"rows"`
	Bytes           int64  `json:"bytes"`
	CapacityBlocked bool   `json:"capacity_blocked"`
	Generation      string `json:"generation"`
	LastGCAtMS      int64  `json:"last_gc_at_ms"`
}

type AuditPage struct {
	Items       []AuditItem    `json:"items"`
	NextCursor  string         `json:"next_cursor"`
	HasMore     bool           `json:"has_more"`
	WatermarkID string         `json:"watermark_id"`
	Filters     AuditFilter    `json:"filters"`
	Retention   AuditRetention `json:"retention"`
}

type auditCursor struct {
	Version     int    `json:"v"`
	WatermarkID string `json:"w"`
	LastAtMS    int64  `json:"t"`
	LastID      string `json:"i"`
	FromMS      int64  `json:"f"`
	ToMS        int64  `json:"u"`
	FilterHash  string `json:"h"`
	Generation  string `json:"g"`
}

func NormalizeAuditFilter(input AuditFilter, now int64) (AuditFilter, error) {
	if input.ToMS == 0 {
		input.ToMS = now + 1
	}
	if input.FromMS == 0 {
		input.FromMS = max(1, input.ToMS-90*auditDayMS)
	}
	if input.FromMS < 1 || input.ToMS <= input.FromMS || input.ToMS > 253402300799999 || input.ToMS-input.FromMS > 366*auditDayMS || (input.ActorKind != "" && input.ActorKind != "github" && input.ActorKind != "system" && input.ActorKind != "unknown") || (input.Actor != "" && !operationText(input.Actor, 128)) || (input.ActorKind == "unknown" && input.Actor != "") || (input.NodeID != "" && !validID(input.NodeID)) || (input.ServiceID != "" && !operationText(input.ServiceID, 128)) || (input.ObjectKind != "" && input.ObjectKind != "proxy" && input.ObjectKind != "visitor") || (input.ObjectName != "" && !operationText(input.ObjectName, 256)) || (input.OperationID != "" && !shared.ValidConfigOperationID(input.OperationID)) || (input.State != "" && !shared.ValidConfigOperationState(input.State) && input.State != "pending" && input.State != "acknowledged") || (input.Kind != "" && !ValidAuditKind(input.Kind)) {
		return AuditFilter{}, ErrInvalid
	}
	return input, nil
}

func auditFilterHash(filter AuditFilter) string {
	data, _ := json.Marshal(filter)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func parseAuditCursor(raw string) (*auditCursor, error) {
	if len(raw) > MaxAuditCursor {
		return nil, ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	cursor := new(auditCursor)
	if decoder.Decode(cursor) != nil || decoder.Decode(new(any)) != io.EOF || cursor.Version != 2 || !auditDecimal(cursor.Generation, true) || !auditDecimal(cursor.WatermarkID, false) || !auditDecimal(cursor.LastID, false) || !shared.ValidConfigDigest(cursor.FilterHash) || cursor.LastAtMS < cursor.FromMS || cursor.LastAtMS >= cursor.ToMS {
		return nil, ErrInvalid
	}
	w, _ := strconv.ParseInt(cursor.WatermarkID, 10, 64)
	i, _ := strconv.ParseInt(cursor.LastID, 10, 64)
	if i > w {
		return nil, ErrInvalid
	}
	return cursor, nil
}

func auditWhere(filter AuditFilter, watermark string, cursor *auditCursor) (string, []any) {
	where := "recorded_at_ms>=? AND recorded_at_ms<? AND audit_id<=?"
	args := []any{filter.FromMS, filter.ToMS, watermark}
	for _, pair := range [][2]string{{"actor_kind", filter.ActorKind}, {"actor", filter.Actor}, {"node_id", filter.NodeID}, {"service_id", filter.ServiceID}, {"operation_id", filter.OperationID}, {"state", filter.State}, {"kind", filter.Kind}} {
		if pair[1] != "" {
			where += " AND " + pair[0] + "=?"
			args = append(args, pair[1])
		}
	}
	if filter.ObjectKind != "" || filter.ObjectName != "" {
		where += " AND EXISTS(SELECT 1 FROM config_audit_objects obj WHERE obj.audit_id=config_audit_entries.audit_id"
		if filter.ObjectKind != "" {
			where += " AND obj.kind=?"
			args = append(args, filter.ObjectKind)
		}
		if filter.ObjectName != "" {
			where += " AND obj.name=?"
			args = append(args, filter.ObjectName)
		}
		where += ")"
	}
	if cursor != nil {
		where += " AND (recorded_at_ms<? OR (recorded_at_ms=? AND audit_id<?))"
		args = append(args, cursor.LastAtMS, cursor.LastAtMS, cursor.LastID)
	}
	return where, args
}

func auditWatermark(tx *sql.Tx) (string, error) {
	var watermark int64
	err := tx.QueryRow("SELECT COALESCE(max(audit_id),0) FROM config_audit_entries").Scan(&watermark)
	return strconv.FormatInt(watermark, 10), err
}

func (s *Store) ListAudit(ctx context.Context, filter AuditFilter, rawCursor string, limit int) (*AuditPage, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	var cursor *auditCursor
	var err error
	if rawCursor != "" {
		cursor, err = parseAuditCursor(rawCursor)
		if err != nil {
			return nil, err
		}
		if filter.FromMS == 0 {
			filter.FromMS = cursor.FromMS
		}
		if filter.ToMS == 0 {
			filter.ToMS = cursor.ToMS
		}
	}
	filter, err = NormalizeAuditFilter(filter, s.cfg.Now().UnixMilli())
	if err != nil || (cursor != nil && cursor.FilterHash != auditFilterHash(filter)) {
		return nil, ErrInvalid
	}
	page := &AuditPage{Items: []AuditItem{}, Filters: filter}
	err = s.call(ctx, func(tx *sql.Tx) error {
		retention, err := s.auditRetentionTx(tx)
		if err != nil {
			return err
		}
		page.Retention = retention
		if cursor != nil && cursor.Generation != retention.Generation {
			return ErrAuditCursorExpired
		}
		watermark, err := auditWatermark(tx)
		if err != nil {
			return err
		}
		if cursor != nil {
			available, _ := strconv.ParseInt(watermark, 10, 64)
			requested, _ := strconv.ParseInt(cursor.WatermarkID, 10, 64)
			if requested > available {
				return ErrInvalid
			}
			watermark = cursor.WatermarkID
		}
		page.WatermarkID = watermark
		where, args := auditWhere(filter, watermark, cursor)
		args = append(args, limit+1)
		rows, err := tx.Query("SELECT "+auditColumns+" FROM config_audit_entries WHERE "+where+" ORDER BY recorded_at_ms DESC,audit_id DESC LIMIT ?", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanAuditItem(rows)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, *item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		page.HasMore, page.Items = true, page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		data, _ := json.Marshal(auditCursor{Version: 2, Generation: page.Retention.Generation, WatermarkID: page.WatermarkID, LastAtMS: last.RecordedAtMS, LastID: last.AuditID, FromMS: filter.FromMS, ToMS: filter.ToMS, FilterHash: auditFilterHash(filter)})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}
