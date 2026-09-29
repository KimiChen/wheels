package monitor

import (
	"sort"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// ReconcileNode contains a trusted binding and an untrusted current claim.
// Fresh must include both a live monitoring session and fresh report data.
type ReconcileNode struct {
	ID      string
	Binding *shared.FRPBinding
	Report  *shared.FRP
	Fresh   bool
}

type Reconciliation struct {
	State        string                `json:"state"`
	GeneratedAt  time.Time             `json:"generated_at"`
	ServerID     string                `json:"server_id"`
	TrafficScope string                `json:"traffic_scope"`
	Nodes        []NodeReconciliation  `json:"nodes"`
	Clients      []shared.ServerClient `json:"clients"`
	Proxies      []shared.ServerProxy  `json:"proxies"`
}

type NodeReconciliation struct {
	ID           string               `json:"id"`
	State        string               `json:"state"`
	ServerOnline *bool                `json:"server_online"`
	Client       *shared.ServerClient `json:"client"`
	Proxies      []ReconciledProxy    `json:"proxies"`
}

type ReconciledProxy struct {
	Name         string              `json:"name"`
	Type         string              `json:"type"`
	LocalTarget  *string             `json:"local_target"`
	Enabled      bool                `json:"enabled"`
	ClientStatus string              `json:"client_status"`
	ServerState  string              `json:"server_state"`
	Server       *shared.ServerProxy `json:"server"`
}

type bindingKey struct{ server, user, client string }

func trustedKey(b *shared.FRPBinding) (bindingKey, bool) {
	if b == nil || b.Validate() != nil {
		return bindingKey{}, false
	}
	return bindingKey{b.ServerID, b.User, b.RawClientID}, true
}

func claimedKey(f *shared.FRP) (bindingKey, bool) {
	if f == nil || f.Association.RawClientID == nil {
		return bindingKey{}, false
	}
	a := f.Association
	return trustedKey(&shared.FRPBinding{ServerID: a.ServerID, User: a.User, RawClientID: *a.RawClientID})
}

// Reconcile never persists a binding or guesses identities from proxy names,
// addresses, hostnames or run IDs. Arrays and returned objects are fresh copies;
// inputs are immutable snapshots and may be read by other API requests.
func Reconcile(server shared.ServerSnapshot, nodes []ReconcileNode) Reconciliation {
	out := Reconciliation{State: server.State, GeneratedAt: server.GeneratedAt, ServerID: server.ServerID, TrafficScope: server.TrafficScope, Nodes: make([]NodeReconciliation, 0, len(nodes)), Clients: append([]shared.ServerClient{}, server.Clients...), Proxies: append([]shared.ServerProxy{}, server.Proxies...)}
	if out.State == "" {
		out.State = "unavailable"
	}
	bindings, claims := map[bindingKey]int{}, map[bindingKey]int{}
	clients := map[bindingKey][]int{}
	proxies := map[string][]int{}
	for _, n := range nodes {
		if key, ok := trustedKey(n.Binding); ok {
			bindings[key]++
		}
		if key, ok := claimedKey(n.Report); ok && n.Fresh {
			claims[key]++
		}
	}
	for i, c := range out.Clients {
		out.Clients[i].AgentID = nil
		if c.RawClientID != nil && *c.RawClientID != "" {
			key := bindingKey{server.ServerID, c.User, *c.RawClientID}
			clients[key] = append(clients[key], i)
		}
	}
	for i, p := range out.Proxies {
		out.Proxies[i].AgentID = nil
		proxies[p.Name] = append(proxies[p.Name], i)
	}
	for _, n := range nodes {
		r := NodeReconciliation{ID: n.ID, State: "unbound", Proxies: []ReconciledProxy{}}
		key, bound := trustedKey(n.Binding)
		claim, stable := claimedKey(n.Report)
		switch {
		// Untrusted claims may mark their own unbound rows ambiguous, but
		// cannot deny an independently authorized owner's binding.
		case bound && bindings[key] > 1 || !bound && stable && n.Fresh && claims[claim] > 1:
			r.State = "conflict"
		case !bound:
			if n.Report != nil && !stable {
				r.State = "transient"
			}
		case n.Report == nil || !n.Fresh:
			r.State = "stale"
		case !stable || claim != key:
			r.State = "mismatch"
		case server.State != "ready":
			r.State = "unavailable"
		case key.server != server.ServerID:
			r.State = "mismatch"
		case len(clients[key]) > 1:
			r.State = "conflict"
		case len(clients[key]) == 0:
			r.State = "missing"
		default:
			r.State = "matched"
			i := clients[key][0]
			id := n.ID
			out.Clients[i].AgentID = &id
			c := out.Clients[i]
			r.Client = &c
			online := c.Online
			r.ServerOnline = &online
		}
		if n.Report != nil {
			for _, p := range n.Report.Proxies {
				rp := ReconciledProxy{Name: p.Name, Type: p.Type, LocalTarget: p.LocalTarget, Enabled: p.Enabled, ClientStatus: p.Status, ServerState: "unavailable"}
				if r.State == "matched" {
					rp.ServerState = "missing"
					// Agent configurations use raw names; the native FRP wire name
					// always adds exactly one user prefix (pkg/naming.AddUserPrefix).
					name := p.Name
					if key.user != "" {
						name = key.user + "." + name
					}
					indices := proxies[name]
					if len(indices) > 1 {
						rp.ServerState = "conflict"
					} else if len(indices) == 1 {
						i := indices[0]
						sp := out.Proxies[i]
						if sp.User != key.user || sp.ClientID != key.client || sp.Type != p.Type {
							rp.ServerState = "conflict"
						} else {
							id := n.ID
							out.Proxies[i].AgentID = &id
							sp = out.Proxies[i]
							rp.Server = &sp
							rp.ServerState = "offline"
							if sp.Online {
								rp.ServerState = "registered"
							}
						}
					}
					if !p.Enabled && rp.ServerState == "missing" {
						rp.ServerState = "disabled"
					}
				}
				r.Proxies = append(r.Proxies, rp)
			}
		}
		out.Nodes = append(out.Nodes, r)
	}
	// Server-only proxies remain visible and may belong to a uniquely matched
	// client even when the agent omits them (configuration/report skew).
	for i, p := range out.Proxies {
		key := bindingKey{server.ServerID, p.User, p.ClientID}
		if indices := clients[key]; len(indices) == 1 && out.Clients[indices[0]].AgentID != nil {
			out.Proxies[i].AgentID = out.Clients[indices[0]].AgentID
		}
	}
	// Node IDs are numeric autoincrement values; order them like the public
	// snapshot does, not lexically.
	sort.Slice(out.Nodes, func(i, j int) bool { return nodeIDLess(out.Nodes[i].ID, out.Nodes[j].ID) })
	sort.Slice(out.Clients, func(i, j int) bool {
		a, b := out.Clients[i], out.Clients[j]
		if a.User != b.User {
			return a.User < b.User
		}
		return a.ClientID < b.ClientID
	})
	sort.Slice(out.Proxies, func(i, j int) bool { return out.Proxies[i].Name < out.Proxies[j].Name })
	return out
}
