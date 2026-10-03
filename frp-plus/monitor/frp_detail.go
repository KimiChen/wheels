package monitor

import (
	"net/http"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

type privateFRPDetail struct {
	NodeID     string            `json:"node_id"`
	State      string            `json:"state"`
	ReceivedAt *time.Time        `json:"received_at"`
	Detail     *shared.FRPDetail `json:"detail"`
}

func (s *Service) frpDetailSnapshot(id string, now time.Time) (privateFRPDetail, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil {
		return privateFRPDetail{}, false
	}
	detail, state, at := privateDetail(n, now, s.cfg.ReportIntervalSeconds)
	return privateFRPDetail{NodeID: id, State: state, ReceivedAt: at, Detail: detail}, true
}

// The list/SSE contains only status and timestamps. Large private details are
// requested for one node, so adding nodes does not multiply detail broadcasts.
func (s *Service) handleFRPDetail(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	value, ok := s.frpDetailSnapshot(id, time.Now())
	if !ok {
		http.NotFound(w, r)
		return
	}
	adminJSON(w, http.StatusOK, value)
}

// acceptFRPDetail uses the same connection ownership and sequence space as
// metrics and probe results, but never refreshes their sampling timestamps.
func (s *Service) acceptFRPDetail(id string, conn *websocket.Conn, received time.Time, report shared.FRPDetailReport) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil || n.conn != conn || !n.frpDetailEnabled || n.sessionID != report.SessionID || report.Sequence <= n.sequence {
		return false
	}
	n.sequence = report.Sequence
	n.lastSeen = received
	n.frpDetail = &report.Detail
	n.frpDetailAt = received
	return true
}

// privateDetail is called with s.mu held. Old/stale data cannot look current
// merely because another resource sample or heartbeat was received recently.
func privateDetail(n *node, now time.Time, interval int) (*shared.FRPDetail, string, *time.Time) {
	if !n.frpDetailEnabled {
		return nil, "unsupported", nil
	}
	if n.frpDetailAt.IsZero() {
		return nil, "waiting", nil
	}
	at := n.frpDetailAt.UTC()
	ttl := max(10*time.Second, time.Duration(interval)*3*time.Second)
	if n.conn == nil || now.Sub(at) > ttl || at.After(now.Add(5*time.Second)) {
		return nil, "stale", &at
	}
	if n.frpDetail == nil {
		return nil, "unavailable", &at
	}
	if n.frpDetail.State != "ready" {
		return nil, n.frpDetail.State, &at
	}
	return n.frpDetail, "ready", &at
}
