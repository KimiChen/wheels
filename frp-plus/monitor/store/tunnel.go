package store

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompb"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type tunnelSample struct {
	series   string
	at       time.Time
	counters shared.TunnelCounters
}
type tunnelBaseline struct {
	at           time.Time
	epoch        string
	rx, tx       uint64
	good         bool
	unknownSince time.Time
}
type TunnelPoint struct {
	AtMS                      int64    `json:"at_ms"`
	Quality                   string   `json:"quality"`
	ConnectionsQuality        string   `json:"connections_quality"`
	Samples                   int      `json:"samples"`
	CoverageSeconds           float64  `json:"coverage_seconds"`
	Connections               *float64 `json:"connections"`
	RXRecordedBytes           *string  `json:"rx_recorded_bytes"`
	TXRecordedBytes           *string  `json:"tx_recorded_bytes"`
	RXEstimatedBytesPerSecond *float64 `json:"rx_estimated_bytes_per_second"`
	TXEstimatedBytesPerSecond *float64 `json:"tx_estimated_bytes_per_second"`
}
type TunnelGap struct {
	FromMS          int64   `json:"from_ms"`
	ToMS            int64   `json:"to_ms"`
	Reason          string  `json:"reason"`
	RXRecordedBytes *string `json:"rx_recorded_bytes"`
	TXRecordedBytes *string `json:"tx_recorded_bytes"`
	Quality         string  `json:"quality"`
}
type TunnelHistoryResult struct {
	StepSeconds  int64         `json:"step_seconds"`
	Precision    string        `json:"precision"`
	Points       []TunnelPoint `json:"points"`
	GapIntervals []TunnelGap   `json:"gap_intervals"`
}

func tunnelNumber(p *string) (uint64, bool) {
	if p == nil {
		return 0, false
	}
	v, e := strconv.ParseUint(*p, 10, 64)
	return v, e == nil && strconv.FormatUint(v, 10) == *p
}
func (s *Store) AcceptTunnel(series string, at time.Time, c shared.TunnelCounters) bool {
	if !validNode(series) || !validAt(at) {
		return false
	}
	good := func(q string, p *string) bool {
		switch q {
		case "ok":
			_, ok := tunnelNumber(p)
			return ok
		case "unknown", "unsupported":
			return p == nil
		default:
			return false
		}
	}
	if !good(c.Quality, c.RXBytes) || !good(c.Quality, c.TXBytes) || !good(c.ConnectionsQuality, c.Connections) {
		return false
	}
	// Copy scalars: no caller-owned pointer is retained by the async queue.
	copyValue := func(p *string) *string {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	c.RXBytes = copyValue(c.RXBytes)
	c.TXBytes = copyValue(c.TXBytes)
	c.Connections = copyValue(c.Connections)
	return s.offer(event{tunnel: &tunnelSample{series: series, at: at, counters: c}})
}
func tunnelRow(field, series string, at int64, value float64) storage.MetricRow {
	return storage.MetricRow{MetricNameRaw: storage.MarshalMetricNameRaw(nil, []prompb.Label{{Name: "__name__", Value: "frpmonitor_tunnel_" + field}, {Name: "tunnel_series_id", Value: series}}), Timestamp: at, Value: value}
}
func (s *Store) consumeTunnel(v tunnelSample) {
	if s.tunnelBaselines == nil {
		s.tunnelBaselines = map[string]tunnelBaseline{}
	}
	if len(s.tunnelBaselines) >= 1024 {
		if _, ok := s.tunnelBaselines[v.series]; !ok {
			var oldest string
			var at time.Time
			for id, b := range s.tunnelBaselines {
				if oldest == "" || b.at.Before(at) {
					oldest, at = id, b.at
				}
			}
			delete(s.tunnelBaselines, oldest)
		}
	}
	old, exists := s.tunnelBaselines[v.series]
	if exists && v.at.Equal(old.at) {
		return
	}
	if exists && v.at.Before(old.at) {
		// Never move the reference clock backwards. The next forward sample
		// establishes a fresh baseline instead of counting an overlap twice.
		old.good = false
		old.unknownSince = old.at
		s.tunnelBaselines[v.series] = old
		return
	}
	c := v.counters
	rx, rok := tunnelNumber(c.RXBytes)
	tx, tok := tunnelNumber(c.TXBytes)
	good := c.Quality == "ok" && rok && tok
	add := func(field string, value float64) {
		s.rows = append(s.rows, tunnelRow(field, v.series, v.at.UnixMilli(), value))
	}
	if n, ok := tunnelNumber(c.Connections); c.ConnectionsQuality == "ok" && ok {
		add("connections", float64(n))
	}
	unknownSince := time.Time{}
	if c.Quality != "unsupported" && c.Accounting != "not_observed" {
		if !exists {
			add("marker", 1)
			if !good {
				unknownSince = v.at
			}
		} else if !good {
			unknownSince = old.unknownSince
			if unknownSince.IsZero() {
				unknownSince = old.at
			}
		} else {
			reason := float64(0)
			start := old.at
			if !v.at.After(old.at) || c.Epoch != old.epoch || old.good && (rx < old.rx || tx < old.tx) {
				reason = 2
			} else if !old.good {
				reason = 3
				if !old.unknownSince.IsZero() {
					start = old.unknownSince
				}
			} else if v.at.Sub(old.at) > max(10*time.Second, 3*s.cfg.ReportInterval) {
				reason = 1
			}
			if reason != 0 {
				add("gap_start", float64(start.UnixMilli()))
				add("gap_reason", reason)
				if reason == 1 && old.good {
					add("gap_rx", float64(rx-old.rx))
					add("gap_tx", float64(tx-old.tx))
				}
			}
			if reason == 0 && old.good {
				add("rx", float64(rx-old.rx))
				add("tx", float64(tx-old.tx))
				add("coverage", v.at.Sub(old.at).Seconds())
				add("samples", 1)
			}
		}
	}
	s.tunnelBaselines[v.series] = tunnelBaseline{at: v.at, epoch: c.Epoch, rx: rx, tx: tx, good: good, unknownSince: unknownSince}
	if len(s.rows) >= 512 {
		_ = s.persist(false)
	}
}

func (s *Store) TunnelHistory(ctx context.Context, series string, from, to time.Time, step time.Duration) (TunnelHistoryResult, error) {
	from, to, step, err := queryRange(series, from, to, step)
	if err != nil {
		return TunnelHistoryResult{}, err
	}
	out := TunnelHistoryResult{StepSeconds: int64(step / time.Second), Precision: "approximate", Points: []TunnelPoint{}, GapIntervals: []TunnelGap{}}
	count := int((to.Sub(from) + step - 1) / step)
	rx := make([]float64, count)
	tx := make([]float64, count)
	means := make([]average, count)
	gapRows := map[int64]map[string]float64{}
	for i := 0; i < count; i++ {
		out.Points = append(out.Points, TunnelPoint{AtMS: from.Add(time.Duration(i) * step).UnixMilli(), Quality: "unknown", ConnectionsQuality: "unknown"})
	}
	err = s.scanTunnel(ctx, series, from, to, func(field string, at int64, value float64) error {
		idx := int((at - from.UnixMilli()) / step.Milliseconds())
		if idx < 0 || idx >= count {
			return errors.New("invalid tunnel history bucket")
		}
		p := &out.Points[idx]
		switch field {
		case "rx":
			rx[idx] += value
		case "tx":
			tx[idx] += value
		case "samples":
			p.Samples++
		case "coverage":
			p.CoverageSeconds = math.Min(step.Seconds(), p.CoverageSeconds+math.Max(0, value))
		case "connections":
			means[idx].add(value)
		case "gap_start", "gap_reason", "gap_rx", "gap_tx":
			// Coalesce gaps in each result bucket, keeping memory and response
			// bounded even during prolonged intermittent collection failures.
			key := p.AtMS
			if gapRows[key] == nil {
				gapRows[key] = map[string]float64{}
			}
			g := gapRows[key]
			g["gap_end"] = math.Max(g["gap_end"], float64(at))
			switch field {
			case "gap_start":
				if g[field] == 0 || value < g[field] {
					g[field] = value
				}
			case "gap_reason":
				g[field] = math.Max(g[field], value)
			default:
				g[field] += value
			}
		case "marker":
		default:
			return errors.New("invalid tunnel history field")
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	number := func(v float64) *string { x := strconv.FormatFloat(v, 'f', 0, 64); return &x }
	for i := range out.Points {
		p := &out.Points[i]
		if means[i].n > 0 {
			v := means[i].value
			p.Connections = &v
			p.ConnectionsQuality = "ok"
		}
		if p.Samples > 0 && p.CoverageSeconds > 0 {
			p.Quality = "partial"
			if p.CoverageSeconds >= step.Seconds() {
				p.Quality = "ok"
			}
			p.RXRecordedBytes = number(rx[i])
			p.TXRecordedBytes = number(tx[i])
			r, t := rx[i]/p.CoverageSeconds, tx[i]/p.CoverageSeconds
			p.RXEstimatedBytesPerSecond = &r
			p.TXEstimatedBytesPerSecond = &t
		}
	}
	for at, v := range gapRows {
		start, ok := v["gap_start"]
		if !ok {
			continue
		}
		reason := "invalid_sample"
		switch v["gap_reason"] {
		case 1:
			reason = "collection_gap"
		case 2:
			reason = "counter_reset"
		}
		g := TunnelGap{FromMS: max(from.UnixMilli(), int64(start)), ToMS: int64(v["gap_end"]), Reason: reason, Quality: "partial"}
		if r, ok := v["gap_rx"]; ok && reason == "collection_gap" {
			g.RXRecordedBytes = number(r)
		}
		if t, ok := v["gap_tx"]; ok && reason == "collection_gap" {
			g.TXRecordedBytes = number(t)
		}
		if g.FromMS < g.ToMS {
			out.GapIntervals = append(out.GapIntervals, g)
			idx := int((at - from.UnixMilli()) / step.Milliseconds())
			out.Points[idx].Quality = "partial"
			out.Points[idx].RXEstimatedBytesPerSecond = nil
			out.Points[idx].TXEstimatedBytesPerSecond = nil
		}
	}
	sort.Slice(out.GapIntervals, func(i, j int) bool { return out.GapIntervals[i].FromMS < out.GapIntervals[j].FromMS })
	return out, nil
}

func (s *Store) scanTunnel(ctx context.Context, series string, from, to time.Time, visit func(string, int64, float64) error) (err error) {
	parent := ctx
	defer func() {
		if recover() != nil {
			err = errors.New("history query failed")
		}
		err = s.queryError(parent, err)
	}()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return err
	}
	s.queryMu.RLock()
	defer s.queryMu.RUnlock()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	tfs := storage.NewTagFilters()
	if err = tfs.Add([]byte("tunnel_series_id"), []byte(series), false, false); err != nil {
		return err
	}
	tr := storage.TimeRange{MinTimestamp: from.UnixMilli(), MaxTimestamp: to.UnixMilli() - 1}
	deadline, _ := ctx.Deadline()
	var search storage.Search
	search.Init(nil, s.db, []*storage.TagFilters{tfs}, tr, 11, uint64(deadline.Unix()+1))
	defer search.MustClose()
	var timestamps []int64
	var values []float64
	seen := 0
	for search.NextMetricBlock() {
		if err = ctx.Err(); err != nil {
			return err
		}
		mbr := search.MetricBlockRef
		var mn storage.MetricName
		if err = mn.Unmarshal(mbr.MetricName); err != nil {
			return errors.New("invalid history series")
		}
		field := strings.TrimPrefix(string(mn.MetricGroup), "frpmonitor_tunnel_")
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)
		if err = block.UnmarshalData(); err != nil {
			return errors.New("invalid history block")
		}
		timestamps, values = block.AppendRowsWithTimeRangeFilter(timestamps[:0], values[:0], tr)
		seen += len(timestamps)
		if seen > 4000000 {
			return errors.New("history sample limit exceeded")
		}
		for i, at := range timestamps {
			if math.IsNaN(values[i]) || math.IsInf(values[i], 0) {
				return errors.New("invalid history value")
			}
			if err = visit(field, at, values[i]); err != nil {
				return err
			}
		}
	}
	return search.Error()
}
