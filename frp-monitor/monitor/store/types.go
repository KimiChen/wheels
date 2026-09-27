// Package store keeps bounded, asynchronous minute aggregates and traffic counters.
// It never persists connection or online state.
package store

import "time"

type Config struct {
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
type TrafficResult struct {
	Day             string  `json:"day"`
	RXBytes         *string `json:"rx_bytes"`
	TXBytes         *string `json:"tx_bytes"`
	CoverageSeconds float64 `json:"coverage_seconds"`
	Resets          int     `json:"resets"`
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
