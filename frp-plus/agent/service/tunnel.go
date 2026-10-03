package service

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// One outstanding call across all sessions bounds an uncooperative provider.
// Sampling and host reports keep running; a late sample is never made fresh.
func (s *Service) tunnelSnapshot() *shared.TunnelSnapshot {
	if s.tunnelProvider == nil {
		return nil
	}
	unavailable := shared.EmptyTunnelSnapshot("client", "unavailable")
	if s.tunnelResults == nil {
		s.tunnelResults = make(chan shared.TunnelSnapshot, 1)
	}
	if s.tunnelPending {
		select {
		case <-s.tunnelResults:
			s.tunnelPending = false
		default:
			return &unavailable
		}
	}
	s.tunnelPending = true
	results, provider := s.tunnelResults, s.tunnelProvider
	go func() { results <- collectTunnel(provider) }()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case value := <-results:
		s.tunnelPending = false
		return &value
	case <-timer.C:
		return &unavailable
	case <-s.ctx.Done():
		return &unavailable
	}
}

func collectTunnel(provider shared.TunnelProvider) (out shared.TunnelSnapshot) {
	out = shared.EmptyTunnelSnapshot("client", "unavailable")
	defer func() {
		if recover() != nil {
			out = shared.EmptyTunnelSnapshot("client", "unavailable")
		}
	}()
	got := provider()
	if got.Source != "client" {
		return out
	}
	if err := got.Validate(); err != nil {
		if !errors.Is(err, shared.ErrTunnelTooLarge) {
			return out
		}
		got.State = "truncated"
		got.Objects = []shared.TunnelObject{}
		if len(got.Events) > shared.MaxTunnelEvents {
			return out
		}
		if got.Validate() != nil {
			return out
		}
	}
	data, err := json.Marshal(got)
	if err != nil || json.Unmarshal(data, &out) != nil {
		return shared.EmptyTunnelSnapshot("client", "unavailable")
	}
	return out
}
