package shared

import (
	"errors"
	"time"
)

// FRPBinding is trusted monitor configuration, never accepted from an agent
// report. The tuple only authorizes reconciliation; native FRP still performs
// its own authentication independently.
type FRPBinding struct {
	ServerID    string `json:"server_id"`
	User        string `json:"user"`
	RawClientID string `json:"raw_client_id"`
}

func (b FRPBinding) Validate() error {
	if !bounded(b.ServerID, 128, false) || !bounded(b.User, 128, true) || !bounded(b.RawClientID, 128, false) {
		return errors.New("invalid FRP binding")
	}
	return nil
}

// ServerProvider is called by one monitor background sampler, never by a
// browser request. Implementations must return bounded copies without waiting
// for busy FRP locks. A non-ready snapshot cannot establish a binding.
type ServerProvider func() ServerSnapshot

// ServerSnapshot is private administrative data. It deliberately excludes
// native login tokens, OIDC claims, TLS settings, metadata and plugin options.
type ServerSnapshot struct {
	State        string         `json:"state"`
	GeneratedAt  time.Time      `json:"generated_at"`
	ServerID     string         `json:"server_id"`
	TrafficScope string         `json:"traffic_scope"`
	Clients      []ServerClient `json:"clients"`
	Proxies      []ServerProxy  `json:"proxies"`
}

type ServerClient struct {
	User         string  `json:"user"`
	RawClientID  *string `json:"raw_client_id"`
	ClientID     string  `json:"client_id"`
	Hostname     string  `json:"hostname"`
	IP           string  `json:"ip"`
	Version      string  `json:"version"`
	WireProtocol string  `json:"wire_protocol"`
	Online       bool    `json:"online"`
	AgentID      *string `json:"agent_id"`
}

type ServerProxy struct {
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	User         string  `json:"user"`
	ClientID     string  `json:"client_id"`
	Online       bool    `json:"online"`
	Connections  *string `json:"connections"`
	TodayRXBytes *string `json:"today_rx_bytes"`
	TodayTXBytes *string `json:"today_tx_bytes"`
	AgentID      *string `json:"agent_id"`
}
