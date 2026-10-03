package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
)

type tunnelStorage struct {
	State         string `json:"state"`
	RetentionDays int    `json:"retention_days"`
	MaxEvents     int    `json:"max_events,omitempty"`
}

func (s *Service) tunnelResponse(value any) map[string]any {
	data, _ := json.Marshal(value)
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)
	// Typed IDs/counters remain strings. JSON conversion never receives a native
	// configuration, credential, endpoint, or error object.
	event := tunnelStorage{State: "ready", RetentionDays: 30, MaxEvents: control.MaxTunnelEvents}
	if s.tunnelHistory != nil {
		s.tunnelHistory.mu.RLock()
		event.State = s.tunnelHistory.status
		s.tunnelHistory.mu.RUnlock()
	}
	metric := tunnelStorage{State: "disabled", RetentionDays: s.cfg.RetentionDays}
	if metric.RetentionDays == 0 {
		metric.RetentionDays = 7
	}
	if s.storeFailed {
		metric.State = "degraded"
	}
	if s.store != nil {
		metric.State = "ready"
		if s.store.Status().Degraded {
			metric.State = "degraded"
		}
	}
	out["code"] = "ok"
	out["generated_at_ms"] = time.Now().UnixMilli()
	out["event_storage"] = event
	out["metric_storage"] = metric
	return out
}
func tunnelHTTPError(w http.ResponseWriter, err error) {
	status, code := 503, "unavailable"
	if errors.Is(err, control.ErrInvalid) {
		status, code = 400, "invalid_request"
	}
	if errors.Is(err, control.ErrNotFound) {
		status, code = 404, "tunnel_not_found"
	}
	adminJSON(w, status, map[string]string{"code": code})
}
func tunnelQuery(r *http.Request, allowed string) (control.TunnelFilter, string, int, string, error) {
	f := control.TunnelFilter{}
	cursor, window := "", "1h"
	limit := 50
	if len(r.URL.RawQuery) > 4096 {
		return f, "", 0, "", control.ErrInvalid
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return f, "", 0, "", control.ErrInvalid
	}
	for key, entries := range q {
		if len(entries) != 1 || !strings.Contains(","+allowed+",", ","+key+",") {
			return f, "", 0, "", control.ErrInvalid
		}
		v := entries[0]
		switch key {
		case "node_id":
			f.NodeID = v
		case "kind":
			f.Kind = v
		case "source":
			f.Source = v
		case "instance_id":
			f.InstanceID = v
		case "cursor":
			cursor = v
		case "window":
			window = v
		case "limit":
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 200 || strconv.Itoa(n) != v {
				return f, "", 0, "", control.ErrInvalid
			}
			limit = n
		case "from_ms", "to_ms":
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 1 || strconv.FormatInt(n, 10) != v {
				return f, "", 0, "", control.ErrInvalid
			}
			if key == "from_ms" {
				f.FromMS = n
			} else {
				f.ToMS = n
			}
		}
	}
	return f, cursor, limit, window, nil
}
func (s *Service) handleTunnelAdmin(w http.ResponseWriter, r *http.Request, path string) bool {
	if path != "tunnels" && !strings.HasPrefix(path, "tunnels/") {
		return false
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return true
	}
	if s.control == nil {
		tunnelHTTPError(w, control.ErrClosed)
		return true
	}
	select {
	case s.queries <- struct{}{}:
		defer func() { <-s.queries }()
	default:
		tunnelHTTPError(w, control.ErrTunnelBusy)
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	now := time.Now()
	if path == "tunnels" {
		f, c, limit, _, err := tunnelQuery(r, "node_id,kind,limit,cursor")
		if err != nil {
			tunnelHTTPError(w, err)
			return true
		}
		page, err := s.control.ListTunnels(ctx, f, c, limit)
		if err != nil {
			tunnelHTTPError(w, err)
			return true
		}
		for i := range page.Items {
			for j := range page.Items[i].CurrentInstances {
				s.overlayTunnelInstance(&page.Items[i].CurrentInstances[j], now)
			}
		}
		adminJSON(w, 200, s.tunnelResponse(page))
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) != 3 || !validNodeID(parts[1]) {
		tunnelHTTPError(w, control.ErrInvalid)
		return true
	}
	id := parts[1]
	switch parts[2] {
	case "instances":
		f, c, limit, _, err := tunnelQuery(r, "source,limit,cursor")
		if err != nil {
			tunnelHTTPError(w, err)
			break
		}
		page, err := s.control.ListTunnelInstances(ctx, id, f, c, limit)
		if err != nil {
			tunnelHTTPError(w, err)
			break
		}
		for i := range page.Items {
			s.overlayTunnelInstance(&page.Items[i], now)
		}
		adminJSON(w, 200, s.tunnelResponse(page))
	case "events":
		f, c, limit, _, err := tunnelQuery(r, "instance_id,from_ms,to_ms,limit,cursor")
		if err != nil {
			tunnelHTTPError(w, err)
			break
		}
		// Explicit paired bounds make every cursor page use the same range.
		if (f.FromMS == 0) != (f.ToMS == 0) {
			tunnelHTTPError(w, control.ErrInvalid)
			break
		}
		page, err := s.control.ListTunnelEvents(ctx, id, f, c, limit)
		if err != nil {
			tunnelHTTPError(w, err)
			break
		}
		adminJSON(w, 200, s.tunnelResponse(page))
	case "history":
		f, _, _, window, err := tunnelQuery(r, "instance_id,window")
		if err != nil || !validNodeID(f.InstanceID) {
			tunnelHTTPError(w, control.ErrInvalid)
			break
		}
		duration, ok := map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}[window]
		if !ok {
			tunnelHTTPError(w, control.ErrInvalid)
			break
		}
		instance, err := s.control.GetTunnelInstance(ctx, id, f.InstanceID)
		if err != nil {
			tunnelHTTPError(w, err)
			break
		}
		history := store.TunnelHistoryResult{StepSeconds: 60, Precision: "approximate", Points: []store.TunnelPoint{}, GapIntervals: []store.TunnelGap{}}
		if s.store != nil {
			history, err = s.store.TunnelHistory(ctx, instance.InstanceID, now.Add(-duration), now, 0)
			if err != nil {
				tunnelHTTPError(w, err)
				break
			}
		}
		for i := range history.Points {
			if instance.Counters.Quality == "unsupported" {
				history.Points[i].Quality = "unsupported"
			}
			if instance.Counters.ConnectionsQuality == "unsupported" {
				history.Points[i].ConnectionsQuality = "unsupported"
			}
		}
		out := s.tunnelResponse(history)
		out["instance_id"] = instance.InstanceID
		out["generation"] = instance.Generation
		out["accounting"] = instance.Accounting
		out["byte_scope"] = instance.ByteScope
		adminJSON(w, 200, out)
	default:
		tunnelHTTPError(w, control.ErrInvalid)
	}
	return true
}
