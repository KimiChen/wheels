package monitor

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
)

type historyPoint struct {
	At              time.Time              `json:"at"`
	Samples         int                    `json:"samples"`
	CoverageSeconds float64                `json:"coverage_seconds"`
	CPU             *float64               `json:"cpu"`
	Load            []float64              `json:"load"`
	MemUsed         *string                `json:"mem_used"`
	MemTotal        *string                `json:"mem_total"`
	NetRX           *string                `json:"net_rx"`
	NetTX           *string                `json:"net_tx"`
	Fields          map[string]store.Field `json:"fields"`
}
type storageState struct {
	State   string `json:"state"`
	Dropped uint64 `json:"dropped"`
}
type publicProbe struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	IntervalSeconds uint32             `json:"interval_seconds"`
	LatestAt        *time.Time         `json:"latest_at"`
	LatencyMS       *float64           `json:"latency_ms"`
	Samples         int                `json:"samples"`
	Failures        int                `json:"failures"`
	FailureRate     *float64           `json:"failure_rate"`
	Points          []store.ProbePoint `json:"points"`
}
type publicHistory struct {
	NodeID      string         `json:"node_id"`
	GeneratedAt time.Time      `json:"generated_at"`
	Window      string         `json:"window"`
	StepSeconds int64          `json:"step_seconds"`
	Storage     storageState   `json:"storage"`
	ProbesState string         `json:"probes_state"`
	Points      []historyPoint `json:"points"`
	Probes      []publicProbe  `json:"probes"`
}

func (s *Service) storageStatus() storageState {
	out := storageState{State: "disabled"}
	if s.storeFailed {
		out.State = "degraded"
	}
	if s.store != nil {
		status := s.store.Status()
		out.State = "ready"
		out.Dropped = status.Dropped
		if status.Degraded {
			out.State = "degraded"
		}
	}
	return out
}
func number(value *string) *float64 {
	if value == nil {
		return nil
	}
	n, err := strconv.ParseFloat(*value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func projectPoint(p store.Point) historyPoint {
	fields := make(map[string]store.Field)
	for _, key := range []string{"cpu", "load1", "load2", "load3", "mem_used", "mem_total", "swap_used", "swap_total", "disk_used", "disk_total", "net_rx", "net_tx", "uptime", "tcp", "udp", "procs"} {
		if value, exists := p.Fields[key]; exists {
			fields[key] = value
		}
	}
	out := historyPoint{At: p.At, Samples: p.Samples, CoverageSeconds: p.CoverageSeconds, CPU: number(p.Fields["cpu"].Value), MemUsed: p.Fields["mem_used"].Value, MemTotal: p.Fields["mem_total"].Value, NetRX: p.Fields["net_rx"].Value, NetTX: p.Fields["net_tx"].Value, Fields: fields}
	l1, l2, l3 := number(p.Fields["load1"].Value), number(p.Fields["load2"].Value), number(p.Fields["load3"].Value)
	if l1 != nil && l2 != nil && l3 != nil {
		out.Load = []float64{*l1, *l2, *l3}
	}
	return out
}
func (s *Service) handleHistory(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/public/v1/nodes/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "history" || !idPattern.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	s.mu.Lock()
	n, exists := s.nodes[id]
	probeEnabled := exists && n.probeEnabled && n.conn != nil
	s.mu.Unlock()
	configs := s.configs.Load()
	if !exists || configs == nil || (*configs)[id] == nil || !(*configs)[id].IsPublic {
		http.NotFound(w, r)
		return
	}
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "1h"
	}
	type bounds struct{ span, step time.Duration }
	b, valid := map[string]bounds{"1h": {time.Hour, time.Minute}, "6h": {6 * time.Hour, time.Minute}, "24h": {24 * time.Hour, 5 * time.Minute}, "7d": {7 * 24 * time.Hour, 30 * time.Minute}}[window]
	if !valid || len(r.URL.Query()) > 1 || len(r.URL.Query()["window"]) > 1 {
		http.Error(w, "invalid history window", http.StatusBadRequest)
		return
	}
	select {
	case s.queries <- struct{}{}:
		defer func() { <-s.queries }()
	default:
		http.Error(w, "try later", http.StatusServiceUnavailable)
		return
	}
	now := time.Now().UTC()
	out := publicHistory{NodeID: id, GeneratedAt: now, Window: window, StepSeconds: int64(b.step / time.Second), Storage: s.storageStatus(), Points: []historyPoint{}, Probes: []publicProbe{}, ProbesState: "disabled"}
	book := s.tasks.Load()
	if len(book.Nodes[id]) > 0 {
		out.ProbesState = "waiting"
		if probeEnabled {
			out.ProbesState = "ready"
		}
	}
	if s.taskError.Load() {
		out.ProbesState = "degraded"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	queryFailed := false
	if s.store != nil {
		h, err := s.store.History(ctx, id, now.Add(-b.span), now, b.step)
		if err != nil {
			queryFailed = true
		} else {
			out.StepSeconds = h.StepSeconds
			for _, p := range h.Points {
				out.Points = append(out.Points, projectPoint(p))
			}
		}

	}
	for _, task := range book.Nodes[id] {
		p := publicProbe{ID: task.ID, Name: task.Name, IntervalSeconds: task.Interval, Points: []store.ProbePoint{}}
		if s.store != nil {
			h, err := s.store.ProbeHistory(ctx, id, probeKey(task), now.Add(-b.span), now, b.step)
			if err != nil {
				queryFailed = true
			} else {
				p.Points = h.Points
				p.LatestAt = h.LatestAt
				p.LatencyMS = h.LatencyMS
				p.Samples = h.Samples
				p.Failures = h.Failures
				if h.Samples > 0 {
					rate := 100 * float64(h.Failures) / float64(h.Samples)
					p.FailureRate = &rate
				}
			}
		}
		out.Probes = append(out.Probes, p)
	}
	if queryFailed {
		out.Storage.State = "degraded"
	}
	// A credential may be revoked while the bounded database queries run.
	s.mu.Lock()
	_, stillAuthorized := s.nodes[id]
	s.mu.Unlock()
	configs = s.configs.Load()
	if !stillAuthorized || configs == nil || (*configs)[id] == nil || !(*configs)[id].IsPublic {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = json.NewEncoder(w).Encode(out)
}
