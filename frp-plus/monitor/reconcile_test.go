package monitor

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func frpPtr[T any](v T) *T { return &v }

func reconcileFixture() (shared.ServerSnapshot, []ReconcileNode) {
	b := &shared.FRPBinding{ServerID: "server", User: "team", RawClientID: "stable"}
	report := &shared.FRP{Association: shared.FRPAssociation{ServerID: "server", User: "team", RawClientID: frpPtr("stable")}, ControlState: "connected", Proxies: []shared.Proxy{{Name: "echo", Type: "tcp", Enabled: true, Status: "running", LocalTarget: frpPtr("127.0.0.1:8000")}}}
	s := shared.ServerSnapshot{State: "ready", GeneratedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), ServerID: "server", TrafficScope: "server_local_day", Clients: []shared.ServerClient{{User: "team", RawClientID: frpPtr("stable"), ClientID: "stable", Online: true}}, Proxies: []shared.ServerProxy{{Name: "team.echo", Type: "tcp", User: "team", ClientID: "stable", Online: true, Connections: frpPtr("1"), TodayRXBytes: frpPtr("9007199254740993"), TodayTXBytes: frpPtr("42")}}}
	return s, []ReconcileNode{{ID: "node-a", Binding: b, Report: report, Fresh: true}}
}

func TestReconcileRequiresTrustedUniqueExactBinding(t *testing.T) {
	tests := []struct {
		name string
		edit func(*shared.ServerSnapshot, []ReconcileNode) []ReconcileNode
		want string
	}{
		{"matched", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode { return n }, "matched"},
		{"unbound claim never binds", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode { n[0].Binding = nil; return n }, "unbound"},
		{"raw client absent", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode {
			n[0].Binding = nil
			n[0].Report.Association.RawClientID = nil
			return n
		}, "transient"},
		{"stale report", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode { n[0].Fresh = false; return n }, "stale"},
		{"missing report", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode { n[0].Report = nil; return n }, "stale"},
		{"wrong user", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode {
			n[0].Report.Association.User = "other"
			return n
		}, "mismatch"},
		{"wrong server", func(s *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode { s.ServerID = "other"; return n }, "mismatch"},
		{"claim switched client", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode {
			n[0].Report.Association.RawClientID = frpPtr("other")
			return n
		}, "mismatch"},
		{"server absent", func(s *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode { s.Clients = nil; return n }, "missing"},
		{"snapshot busy", func(s *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode { s.State = "busy"; return n }, "unavailable"},
		{"server duplicates", func(s *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode {
			s.Clients = append(s.Clients, s.Clients[0])
			return n
		}, "conflict"},
		{"binding duplicates", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode {
			n = append(n, ReconcileNode{ID: "node-b", Binding: n[0].Binding})
			return n
		}, "conflict"},
		{"untrusted live duplicate does not deny owner", func(_ *shared.ServerSnapshot, n []ReconcileNode) []ReconcileNode {
			n = append(n, ReconcileNode{ID: "node-b", Report: n[0].Report, Fresh: true})
			return n
		}, "matched"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, n := reconcileFixture()
			n = tc.edit(&s, n)
			r := Reconcile(s, n)
			if r.Nodes[0].State != tc.want {
				t.Fatalf("state=%s want=%s", r.Nodes[0].State, tc.want)
			}
			if tc.want != "matched" {
				for _, c := range r.Clients {
					if c.AgentID != nil {
						t.Fatalf("unsafe binding in %s", tc.name)
					}
				}
				for _, p := range r.Proxies {
					if p.AgentID != nil {
						t.Fatalf("unsafe proxy binding in %s", tc.name)
					}
				}
			}
		})
	}
}

func TestReconcileProxyOwnershipAndPrecision(t *testing.T) {
	s, n := reconcileFixture()
	r := Reconcile(s, n)
	p := r.Nodes[0].Proxies[0]
	if p.ServerState != "registered" || p.Server == nil || *p.Server.TodayRXBytes != "9007199254740993" || p.Server.AgentID == nil || *p.Server.AgentID != "node-a" {
		t.Fatalf("proxy=%+v", p)
	}
	encoded, _ := json.Marshal(r)
	if !strings.Contains(string(encoded), `"today_rx_bytes":"9007199254740993"`) {
		t.Fatal("counter lost JSON string precision")
	}
	for _, field := range []string{"user", "client", "type"} {
		t.Run(field, func(t *testing.T) {
			s, n := reconcileFixture()
			switch field {
			case "user":
				s.Proxies[0].User = "other"
			case "client":
				s.Proxies[0].ClientID = "other"
			case "type":
				s.Proxies[0].Type = "udp"
			}
			p := Reconcile(s, n).Nodes[0].Proxies[0]
			if p.ServerState != "conflict" || p.Server != nil {
				t.Fatalf("ownership violation: %+v", p)
			}
		})
	}
	// A raw name already beginning with user still receives one additional
	// native prefix; stripping/guessing prefixes could associate another proxy.
	n[0].Report.Proxies[0].Name = "team.echo"
	if p := Reconcile(s, n).Nodes[0].Proxies[0]; p.ServerState != "missing" {
		t.Fatalf("guessed wire name: %+v", p)
	}
}

func TestReconcileUninstrumentedNoProxyAndImmutableInputs(t *testing.T) {
	s, n := reconcileFixture()
	s.Clients = append(s.Clients, shared.ServerClient{User: "team", ClientID: "temporary-run", Online: true})
	s.Proxies = append(s.Proxies, shared.ServerProxy{Name: "team.legacy", Type: "udp", User: "team", ClientID: "temporary-run", Online: true})
	n[0].Report.Proxies = []shared.Proxy{}
	before, _ := json.Marshal(s)
	r := Reconcile(s, n)
	after, _ := json.Marshal(s)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("input mutated")
	}
	if r.Nodes[0].State != "matched" || len(r.Nodes[0].Proxies) != 0 || r.Nodes[0].ServerOnline == nil || !*r.Nodes[0].ServerOnline {
		t.Fatal("no-proxy client did not match")
	}
	if len(r.Clients) != 2 || len(r.Proxies) != 2 {
		t.Fatal("native clients/proxies hidden")
	}
	for _, p := range r.Proxies {
		if p.ClientID == "temporary-run" && p.AgentID != nil {
			t.Fatal("legacy run ID was treated as stable identity")
		}
	}
	// Offline registry entries keep exact identity but never claim online.
	s.Clients[0].Online = false
	if r := Reconcile(s, n); r.Nodes[0].ServerOnline == nil || *r.Nodes[0].ServerOnline {
		t.Fatal("offline registry presented online")
	}
}

func TestReconcileDistinctTupleAndProxyStates(t *testing.T) {
	s, n := reconcileFixture()
	s.Clients = append(s.Clients, shared.ServerClient{User: "other", RawClientID: frpPtr("stable"), ClientID: "stable", Online: true})
	if r := Reconcile(s, n); r.Nodes[0].State != "matched" {
		t.Fatal("same stable ID in another user caused conflict")
	}
	s.Proxies[0].Online = false
	if p := Reconcile(s, n).Nodes[0].Proxies[0]; p.ServerState != "offline" {
		t.Fatalf("state=%s", p.ServerState)
	}
	s.Proxies = nil
	n[0].Report.Proxies[0].Enabled = false
	if p := Reconcile(s, n).Nodes[0].Proxies[0]; p.ServerState != "disabled" {
		t.Fatalf("state=%s", p.ServerState)
	}
	// A stale competing report is not a current conflict, while duplicate trusted
	// bindings always remain a configuration conflict until explicitly edited.
	n = append(n, ReconcileNode{ID: "node-b", Report: n[0].Report, Fresh: false})
	if r := Reconcile(s, n); r.Nodes[0].State != "matched" {
		t.Fatal("stale claim held binding hostage")
	}
}

func TestReconcileNodesSortNumerically(t *testing.T) {
	s, n := reconcileFixture()
	n[0].ID = "10"
	n = append(n, ReconcileNode{ID: "2"}, ReconcileNode{ID: "1"})
	r := Reconcile(s, n)
	if len(r.Nodes) != 3 || r.Nodes[0].ID != "1" || r.Nodes[1].ID != "2" || r.Nodes[2].ID != "10" {
		t.Fatalf("lexical node order: %+v", r.Nodes)
	}
}

func TestUntrustedClaimsCannotDenyOrStealTrustedOwner(t *testing.T) {
	for _, boundAttacker := range []bool{false, true} {
		s, n := reconcileFixture()
		attacker := ReconcileNode{ID: "node-b", Report: n[0].Report, Fresh: true}
		want := "conflict"
		if boundAttacker {
			attacker.Binding = &shared.FRPBinding{ServerID: "server", User: "attacker", RawClientID: "own-client"}
			want = "mismatch"
		}
		n = append(n, attacker)
		r := Reconcile(s, n)
		if r.Nodes[0].State != "matched" || r.Nodes[1].State != want {
			t.Fatalf("owner/attacker states %+v", r.Nodes)
		}
		for _, c := range r.Clients {
			if c.AgentID == nil || *c.AgentID != "node-a" {
				t.Fatal("untrusted claim displaced client owner")
			}
		}
		for _, p := range r.Proxies {
			if p.AgentID == nil || *p.AgentID != "node-a" {
				t.Fatal("untrusted claim displaced proxy owner")
			}
		}
		if r.Nodes[1].Client != nil || r.Nodes[1].ServerOnline != nil || r.Nodes[1].Proxies[0].Server != nil {
			t.Fatal("attacker acquired private attribution")
		}
	}
}
