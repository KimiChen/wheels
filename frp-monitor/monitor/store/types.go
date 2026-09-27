// Package store provides optional embedded VictoriaMetrics history.
// It never owns business traffic counters, connection state or online state.
package store

import "time"

type Config struct {
	// Path is a dedicated private directory, not a SQLite database file.
	Path           string
	RetentionDays  int
	ReportInterval time.Duration
	// QueueCapacity defaults to 4096 and is bounded at 65536.
	QueueCapacity int
}

type Field struct {
	Value           *string `json:"value"`
	Samples         int     `json:"samples"`
	CoverageSeconds float64 `json:"coverage_seconds"`
}
type Point struct {
	At              time.Time        `json:"at"`
	Samples         int              `json:"samples"`
	CoverageSeconds float64          `json:"coverage_seconds"`
	Fields          map[string]Field `json:"fields"`
}
type HistoryResult struct {
	StepSeconds int64   `json:"step_seconds"`
	Points      []Point `json:"points"`
}
type ProbePoint struct {
	At        time.Time `json:"at"`
	LatencyMS *float64  `json:"latency_ms"`
	Samples   int       `json:"samples"`
	Failures  int       `json:"failures"`
}
type ProbeHistoryResult struct {
	StepSeconds int64        `json:"step_seconds"`
	Points      []ProbePoint `json:"points"`
	LatestAt    *time.Time   `json:"latest_at"`
	LatencyMS   *float64     `json:"latency_ms"`
	Samples     int          `json:"samples"`
	Failures    int          `json:"failures"`
}
type Status struct {
	Degraded    bool   `json:"degraded"`
	Dropped     uint64 `json:"dropped"`
	WriteErrors uint64 `json:"write_errors"`
	QueryErrors uint64 `json:"query_errors"`
	QueueDepth  int    `json:"queue_depth"`
	LastError   string `json:"last_error,omitempty"`
}
