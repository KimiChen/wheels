package monitor

import (
	"encoding/json"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

// No fields in this private report are projected into public or admin SSE.
// Persistence is a separate bounded consumer, never a socket-handler write.
func (s *Service) acceptTunnel(id string, conn *websocket.Conn, received time.Time, report shared.TunnelReport) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil || n.conn != conn || !n.tunnelEnabled || n.sessionID != report.SessionID || report.Sequence <= n.sequence {
		return false
	}
	n.sequence = report.Sequence
	n.lastSeen = received
	n.tunnel = &report.Snapshot
	n.tunnelAt = received
	return true
}

func (s *Service) sampleServerTunnels() {
	if s.tunnelProvider == nil {
		return
	}
	out := shared.EmptyTunnelSnapshot("server", "unavailable")
	defer func() { _ = recover(); s.serverTunnelSnapshot.Store(&out) }()
	v := s.tunnelProvider()
	if v.Source != "server" || v.Validate() != nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil || json.Unmarshal(data, &out) != nil {
		out = shared.EmptyTunnelSnapshot("server", "unavailable")
	}
}
