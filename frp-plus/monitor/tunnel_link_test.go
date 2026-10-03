package monitor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

func dialTunnel(t *testing.T, s *Service, token, session string) *websocket.Conn {
	t.Helper()
	c, response, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if !hasCapability(strings.Split(response.Header.Get(shared.CapabilitiesHeader), ", "), shared.TunnelCapability) {
		t.Fatal("private capability not advertised")
	}
	h := fixture(t, "hello").Hello
	h.SessionID = session
	h.Capabilities = []string{"metrics.v1", "frp.v1", shared.TunnelCapability}
	send(t, c, "hello", "tunnel-hello", h)
	var answer struct {
		Result shared.HelloResult `json:"result"`
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	if c.ReadJSON(&answer) != nil || !hasCapability(answer.Result.Capabilities, shared.TunnelCapability) {
		t.Fatal("private capability not negotiated")
	}
	return c
}

func tunnelReport(session string, sequence uint64) shared.TunnelReport {
	return shared.TunnelReport{Meta: shared.Meta{Schema: 1, SessionID: session, Sequence: sequence, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Snapshot: shared.EmptyTunnelSnapshot("client", "unavailable")}
}

func TestTunnelSessionSequenceFreshnessAndPublicIsolation(t *testing.T) {
	s, token := testMonitor(t)
	old := dialTunnel(t, s, token, "tunnel-old")
	r := tunnelReport("tunnel-old", 2)
	r.Snapshot.State = "ready"
	r.Snapshot.ProcessEpoch = "11111111-1111-4111-8111-111111111111"
	r.Snapshot.CollectedAtMS = time.Now().UnixMilli()
	send(t, old, "frp.tunnel", "", r)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.nodes["1"].tunnelAt.IsZero() })
	s.mu.Lock()
	n := s.nodes["1"]
	at := n.tunnelAt
	if !n.metricsAt.IsZero() || !n.frpDetailAt.IsZero() {
		t.Error("tunnel refreshed other observations")
	}
	s.mu.Unlock()
	metrics := fixture(t, "report-first").Report
	metrics.SessionID = "tunnel-old"
	metrics.Sequence = 3
	send(t, old, "report", "", metrics)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return n.sequence == 3 })
	s.mu.Lock()
	if n.tunnelAt != at {
		t.Error("host metrics refreshed tunnel observation")
	}
	s.mu.Unlock()
	for _, value := range []any{s.snapshotFor(time.Now(), false), s.adminSnapshot()} {
		data, _ := json.Marshal(value)
		if strings.Contains(string(data), r.Snapshot.ProcessEpoch) || strings.Contains(string(data), "first_sequence") {
			t.Fatal("private tunnel payload entered snapshot/SSE projection")
		}
	}
	_ = dialTunnel(t, s, token, "tunnel-new")
	if s.acceptTunnel("1", old, time.Now(), tunnelReport("tunnel-old", 100)) {
		t.Fatal("old socket replaced newer session")
	}
	s.mu.Lock()
	if n.tunnel != nil || !n.tunnelAt.IsZero() {
		t.Error("new session inherited old tunnel snapshot")
	}
	s.mu.Unlock()
}

func TestTunnelRejectsUnnegotiatedWrongSessionAndReplay(t *testing.T) {
	for _, mode := range []string{"unnegotiated", "wrong-session", "replay"} {
		t.Run(mode, func(t *testing.T) {
			s, token := testMonitor(t)
			var c *websocket.Conn
			if mode == "unnegotiated" {
				c = dial(t, s, token, "owner")
			} else {
				c = dialTunnel(t, s, token, "owner")
			}
			r := tunnelReport("owner", 2)
			if mode == "wrong-session" {
				r.SessionID = "other"
			}
			if mode == "replay" {
				send(t, c, "frp.tunnel", "", r)
				eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].sequence == 2 })
			}
			send(t, c, "frp.tunnel", "", r)
			c.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatal("unauthorized/replayed tunnel frame accepted")
			}
		})
	}
}
