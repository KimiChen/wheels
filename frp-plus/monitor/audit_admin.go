package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
)

func auditHTTPError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "unavailable"
	switch {
	case errors.Is(err, control.ErrAuditCursorExpired):
		status, code = 409, "audit_cursor_expired"
	case errors.Is(err, control.ErrInvalid):
		status, code = 400, "invalid_request"
	case errors.Is(err, control.ErrNotFound):
		status, code = 404, "audit_not_found"
	case errors.Is(err, control.ErrAuditExportLimit):
		status, code = 413, "export_limit"
	case errors.Is(err, control.ErrAuditBusy):
		status, code = 503, "busy"
	}
	adminJSON(w, status, map[string]string{"code": code})
}

func auditQuery(values url.Values) (control.AuditFilter, string, int, error) {
	filter := control.AuditFilter{}
	limit, cursor := 50, ""
	for key, entries := range values {
		if len(entries) != 1 {
			return filter, "", 0, control.ErrInvalid
		}
		value := entries[0]
		switch key {
		case "from_ms", "to_ms":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 || strconv.FormatInt(n, 10) != value {
				return filter, "", 0, control.ErrInvalid
			}
			if key == "from_ms" {
				filter.FromMS = n
			} else {
				filter.ToMS = n
			}
		case "actor_kind":
			filter.ActorKind = value
		case "actor":
			filter.Actor = value
		case "node_id":
			filter.NodeID = value
		case "service_id":
			filter.ServiceID = value
		case "object_kind":
			filter.ObjectKind = value
		case "object_name":
			filter.ObjectName = value
		case "operation_id":
			filter.OperationID = value
		case "state":
			filter.State = value
		case "kind":
			filter.Kind = value
		case "cursor":
			if len(value) > control.MaxAuditCursor {
				return filter, "", 0, control.ErrInvalid
			}
			cursor = value
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 100 || strconv.Itoa(n) != value {
				return filter, "", 0, control.ErrInvalid
			}
			limit = n
		default:
			return filter, "", 0, control.ErrInvalid
		}
	}
	return filter, cursor, limit, nil
}

// This router is called only after the normal Admin session, same-origin and
// write-CSRF checks. It never contacts an Agent and remains useful offline.
func (s *Service) handleAuditAdmin(w http.ResponseWriter, r *http.Request, path, actor string) bool {
	const prefix = "configuration/audit"
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		return false
	}
	if s.control == nil {
		adminJSON(w, 503, map[string]string{"code": "unavailable"})
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if path == prefix {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return true
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(r.URL.RawQuery) > 4096 {
			auditHTTPError(w, control.ErrInvalid)
			return true
		}
		filter, cursor, limit, err := auditQuery(values)
		if err != nil {
			auditHTTPError(w, err)
			return true
		}
		page, err := s.control.ListAudit(ctx, filter, cursor, limit)
		if err != nil {
			auditHTTPError(w, err)
		} else {
			adminJSON(w, 200, page)
		}
		return true
	}
	suffix := strings.TrimPrefix(path, prefix+"/")
	if suffix == "export" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return true
		}
		if r.URL.RawQuery != "" {
			auditHTTPError(w, control.ErrInvalid)
			return true
		}
		var input struct {
			Filters *control.AuditFilter `json:"filters"`
		}
		if !decodeAdmin(w, r, &input) {
			return true
		}
		if input.Filters == nil {
			auditHTTPError(w, control.ErrInvalid)
			return true
		}
		output, err := s.control.ExportAudit(ctx, *input.Filters, actor)
		if err != nil {
			auditHTTPError(w, err)
			return true
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="frp-plus-audit.jsonl"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(output.Data)))
		w.WriteHeader(http.StatusOK)
		if n, err := w.Write(output.Data); err != nil || n != len(output.Data) {
			auditCtx, finish := context.WithTimeout(s.ctx, time.Second)
			defer finish()
			_, _ = s.control.RecordAudit(auditCtx, control.AuditInput{Kind: "export", ActorKind: "github", Actor: actor, Code: "export_failed", Summary: &output.Summary})
		}
		return true
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return true
	}
	if r.URL.RawQuery != "" || strings.Contains(suffix, "/") {
		auditHTTPError(w, control.ErrInvalid)
		return true
	}
	detail, err := s.control.GetAudit(ctx, suffix)
	if err != nil {
		auditHTTPError(w, err)
	} else {
		adminJSON(w, 200, detail)
	}
	return true
}
