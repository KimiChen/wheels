package monitor

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
)

type nodeConfigs map[string]*control.Node
type nodeBilling struct {
	PriceMinor   *string `json:"price_minor"`
	Currency     *string `json:"currency"`
	BillingCycle *string `json:"billing_cycle"`
	ExpiresAtMS  *int64  `json:"expires_at_ms"`
	RenewalNote  string  `json:"renewal_note"`
}
type nodeToday struct {
	Day      string `json:"day"`
	Timezone string `json:"timezone"`
	RXBytes  string `json:"rx_bytes"`
	TXBytes  string `json:"tx_bytes"`
	Partial  bool   `json:"partial"`
}
type nodePlan struct {
	QuotaBytes      *string `json:"quota_bytes"`
	Mode            string  `json:"mode"`
	ResetMode       string  `json:"reset_mode"`
	ResetDay        int     `json:"reset_day"`
	ResetTimezone   string  `json:"reset_timezone"`
	PeriodStartAtMS *int64  `json:"period_start_at_ms"`
	PeriodEndAtMS   *int64  `json:"period_end_at_ms"`
	RXBytes         string  `json:"rx_bytes"`
	TXBytes         string  `json:"tx_bytes"`
	UsedBytes       string  `json:"used_bytes"`
	Partial         bool    `json:"partial"`
}
type nodeSettings struct {
	control.NodeConfig
	ConfigRevision int64 `json:"config_revision"`
}

func billingDTO(n *control.Node) *nodeBilling {
	return &nodeBilling{n.PriceMinor, n.Currency, n.BillingCycle, n.ExpiresAtMS, n.RenewalNote}
}

// todayDTO formats the control store's daily counters; loc must be the same
// accounting location the control store uses, or the day boundary would drift.
func todayDTO(n *control.Node, now time.Time, loc *time.Location) *nodeToday {
	local := now.In(loc)
	day := local.Format("2006-01-02")
	v := &nodeToday{Day: day, Timezone: local.Format("MST"), RXBytes: "0", TXBytes: "0", Partial: true}
	if n.TrafficDay != nil && *n.TrafficDay == day {
		v.RXBytes = n.TrafficTodayRXBytes
		v.TXBytes = n.TrafficTodayTXBytes
		v.Partial = n.TrafficTodayPartial
	}
	return v
}
func planDTO(n *control.Node) *nodePlan {
	return &nodePlan{n.TrafficQuotaBytes, n.TrafficMode, n.TrafficResetMode, n.TrafficResetDay, n.TrafficResetTimezone, n.TrafficPeriodStartAtMS, n.TrafficPeriodEndAtMS, n.TrafficPeriodRXBytes, n.TrafficPeriodTXBytes, n.UsedBytes(), n.TrafficPeriodPartial}
}
func (s *Service) refreshNodes(ctx context.Context) error {
	nodes, err := s.control.Nodes(ctx)
	if err != nil {
		return err
	}
	s.applyNodes(nodes)
	return nil
}

// Caller holds configMu after validating that this database snapshot is current.
func (s *Service) applyNodes(nodes []*control.Node) {
	configs := make(nodeConfigs, len(nodes))
	creds := make([]credential, 0, len(nodes))
	for _, n := range nodes {
		configs[n.ID] = n
		creds = append(creds, credential{AgentID: n.ID, Name: n.Name, TokenSHA256: n.TokenSHA256, FRPBinding: n.Binding})
	}
	s.configs.Store(&configs)
	s.applyCredentials(creds)
}
func controlError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, control.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, control.ErrConflict):
		http.Error(w, "configuration changed", 409)
	case errors.Is(err, control.ErrClosed), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		http.Error(w, "control unavailable", 503)
	case errors.Is(err, control.ErrInvalid):
		http.Error(w, "invalid configuration", 400)
	default:
		http.Error(w, "control unavailable", 503)
	}
}

// An enqueued transaction may have committed immediately before cancellation.
// Do not leave the previous authorization snapshot active while its outcome is
// uncertain; the periodic database refresh restores the committed state.
// Caller holds configMu, just like the successful write/refresh path.
func (s *Service) controlWriteError(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.invalidateCredentials()
	}
}
func (s *Service) updateSettings(w http.ResponseWriter, r *http.Request, id string, reset bool) {
	var input struct {
		control.NodeConfig
		ConfigRevision   int64   `json:"config_revision"`
		TrafficUsedBytes *string `json:"traffic_used_bytes"`
	}
	if reset {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		var body struct {
			ConfigRevision int64 `json:"config_revision"`
		}
		if !decodeAdmin(w, r, &body) {
			return
		}
		input.ConfigRevision = body.ConfigRevision
	} else {
		if r.Method != http.MethodPatch {
			methodNotAllowed(w, http.MethodPatch)
			return
		}
		if !decodeAdmin(w, r, &input) {
			return
		}
	}
	if input.ConfigRevision <= 0 {
		http.Error(w, "config_revision required", 400)
		return
	}
	n, err := func() (*control.Node, error) {
		s.configMu.Lock()
		defer s.configMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if reset {
			n, err := s.control.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			input.NodeConfig = n.NodeConfig
		}
		n, err := s.control.UpdateNode(ctx, id, input.NodeConfig, input.TrafficUsedBytes, reset, input.ConfigRevision)
		if err != nil {
			s.controlWriteError(err)
			return nil, err
		}
		s.refreshCommitted(ctx)
		return n, nil
	}()
	if err != nil {
		controlError(w, r, err)
		return
	}
	adminJSON(w, 200, nodeSettings{n.NodeConfig, n.ConfigRevision})
}

// A committed revocation must not wait for a successful database reread.
func (s *Service) refreshCommitted(ctx context.Context) {
	if s.refreshNodes(ctx) == nil {
		return
	}
	s.invalidateCredentials()
}

func (s *Service) invalidateCredentials() {
	empty := nodeConfigs{}
	s.configs.Store(&empty)
	s.applyCredentials(nil)
	s.credentialError.Store(true)
	s.invalidateAdminSnapshot()
}
