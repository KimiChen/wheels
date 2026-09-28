// Package control owns the SQLite configuration and current traffic counters.
// It deliberately has no live sessions, metrics cache, or historical ledgers.
package control

import (
	"errors"
	"math/big"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

var (
	ErrClosed   = errors.New("control store closed")
	ErrNotFound = errors.New("node not found")
	ErrConflict = errors.New("configuration conflict")
	ErrInvalid  = errors.New("invalid control input")
)

type Config struct {
	Path           string
	ReportInterval time.Duration
	Location       *time.Location
	QueueCapacity  int
	Now            func() time.Time
}

// NodeConfig contains only administrator-editable values. Call DefaultNodeConfig
// when creating a node; UpdateNode replaces this configuration atomically.
type NodeConfig struct {
	Name                 string  `json:"name"`
	PublicNote           string  `json:"public_note"`
	PrivateNote          string  `json:"private_note"`
	IsPublic             bool    `json:"is_public"`
	PublishBilling       bool    `json:"publish_billing"`
	PublishTrafficPlan   bool    `json:"publish_traffic_plan"`
	PriceMinor           *string `json:"price_minor"`
	Currency             *string `json:"currency"`
	BillingCycle         *string `json:"billing_cycle"`
	ExpiresAtMS          *int64  `json:"expires_at_ms"`
	RenewalNote          string  `json:"renewal_note"`
	TrafficQuotaBytes    *string `json:"traffic_quota_bytes"`
	TrafficMode          string  `json:"traffic_mode"`
	TrafficResetMode     string  `json:"traffic_reset_mode"`
	TrafficResetDay      int     `json:"traffic_reset_day"`
	TrafficResetTimezone string  `json:"traffic_reset_timezone"`
}

// NodeGroup is an administrator-managed set of node memberships. Nodes may
// appear in several groups; an empty group remains a valid saved group.
type NodeGroup struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	NodeIDs        []string `json:"node_ids"`
	ConfigRevision int64    `json:"config_revision"`
}

func DefaultNodeConfig(name string) NodeConfig {
	return NodeConfig{Name: name, IsPublic: true, PublishTrafficPlan: true, TrafficMode: "max", TrafficResetMode: "monthly", TrafficResetDay: 1, TrafficResetTimezone: "UTC"}
}

type Node struct {
	ID string `json:"id"`
	NodeConfig
	TokenSHA256            string             `json:"-"`
	Binding                *shared.FRPBinding `json:"frp_binding"`
	TrafficPeriodStartAtMS *int64             `json:"traffic_period_start_at_ms"`
	TrafficPeriodEndAtMS   *int64             `json:"traffic_period_end_at_ms"`
	TrafficPeriodRXBytes   string             `json:"traffic_period_rx_bytes"`
	TrafficPeriodTXBytes   string             `json:"traffic_period_tx_bytes"`
	TrafficAdjustmentBytes string             `json:"traffic_adjustment_bytes"`
	TrafficPeriodPartial   bool               `json:"traffic_period_partial"`
	TrafficDay             *string            `json:"traffic_day"`
	TrafficTodayRXBytes    string             `json:"traffic_today_rx_bytes"`
	TrafficTodayTXBytes    string             `json:"traffic_today_tx_bytes"`
	TrafficTodayPartial    bool               `json:"traffic_today_partial"`
	CounterBootID          *string            `json:"-"`
	CounterInterface       *string            `json:"-"`
	CounterScope           *string            `json:"-"`
	CounterRXBytes         *string            `json:"-"`
	CounterTXBytes         *string            `json:"-"`
	CounterReceivedAtMS    *int64             `json:"-"`
	ConfigRevision         int64              `json:"config_revision"`
	CreatedAtMS            int64              `json:"created_at_ms"`
	UpdatedAtMS            int64              `json:"updated_at_ms"`
}

func number(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return n
}

func (n *Node) rawUsed() *big.Int {
	rx, tx := number(n.TrafficPeriodRXBytes), number(n.TrafficPeriodTXBytes)
	switch n.TrafficMode {
	case "total":
		return rx.Add(rx, tx)
	case "rx":
		return rx
	case "tx":
		return tx
	default:
		if tx.Cmp(rx) > 0 {
			return tx
		}
		return rx
	}
}

func (n *Node) UsedBytes() string {
	return new(big.Int).Add(n.rawUsed(), number(n.TrafficAdjustmentBytes)).String()
}
