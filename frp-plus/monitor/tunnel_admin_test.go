package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func monitorTunnelObject() shared.TunnelObject {
	return shared.TunnelObject{InstanceID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Identity: shared.TunnelIdentity{ServerID: "example-server", User: "demo", RawClientID: nil, RawName: "private-tunnel-name", Kind: "visitor", Protocol: "stcp", Quality: "instance_only"}, State: shared.TunnelState{Status: "running", LocalState: "listening", RemoteState: "unknown", P2PState: "unknown", FallbackState: "unknown"}, Counters: shared.TunnelCounters{Quality: "unsupported", ConnectionsQuality: "unsupported", Accounting: "not_observed", ByteScope: "not_observed"}, CreatedAtMS: time.Now().Add(-time.Second).UnixMilli(), OperationRelation: "none"}
}
func TestTunnelAdminRealWSPrivateHistoryAuthenticationAndFreshness(t *testing.T) {
	s, token, secret := testAdmin(t)
	cookie, session := login(t, s, secret)
	ws := dialTunnel(t, s, token, "tunnel-admin")
	o := monitorTunnelObject()
	r := tunnelReport("tunnel-admin", 2)
	r.Snapshot = shared.TunnelSnapshot{State: "ready", Source: "client", ProcessEpoch: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", CollectedAtMS: time.Now().UnixMilli(), FirstSequence: "1", LastSequence: "1", DroppedEvents: "0", Objects: []shared.TunnelObject{o}, Events: []shared.TunnelEvent{{Sequence: "1", TimeBasis: "native", OccurredAtMS: o.CreatedAtMS, Code: "created", Object: o}}}
	send(t, ws, "frp.tunnel", "", r)
	var page control.TunnelPage
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(75 * time.Millisecond)
		ready := func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			p, e := s.control.ListTunnels(ctx, control.TunnelFilter{}, "", 50)
			if e != nil || len(p.Items) != 1 {
				return false
			}
			page = *p
			return true
		}()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			in := s.tunnelInputs()
			if len(in) == 0 {
				t.Fatal("no accepted input")
			}
			_, e := s.control.IngestTunnels(context.Background(), control.TunnelBatch{NodeID: in[0].node, TokenSHA256: in[0].token, CollectorEpoch: s.tunnelHistory.collector, ReceivedAtMS: in[0].at.UnixMilli(), Snapshot: *in[0].snapshot})
			t.Fatal("observer did not persist", e, in[0].snapshot.Validate())
		}
	}
	for _, tt := range []struct {
		cookie  *http.Cookie
		headers http.Header
		status  int
	}{{nil, nil, 401}, {cookie, http.Header{"Origin": []string{"https://other.invalid"}}, 403}, {cookie, nil, 200}} {
		res := adminRequest(t, s, "GET", "/api/admin/v1/tunnels", "", tt.cookie, "", tt.headers)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tt.status {
			t.Fatal(res.StatusCode, string(body))
		}
		if tt.status == 200 {
			var data struct {
				control.TunnelPage
				Code string `json:"code"`
			}
			if json.Unmarshal(body, &data) != nil || data.Code != "ok" || len(data.Items) != 1 || data.Items[0].CurrentInstances[0].Freshness != "fresh" {
				t.Fatal(string(body))
			}
		}
	}
	id := page.Items[0].TunnelID
	instance := page.Items[0].CurrentInstances[0].InstanceID
	for _, path := range []string{"/tunnels/" + id + "/instances", "/tunnels/" + id + "/events", "/tunnels/" + id + "/history?instance_id=" + instance} {
		res := adminRequest(t, s, "GET", "/api/admin/v1"+path, "", cookie, "", nil)
		if res.StatusCode != 200 {
			t.Fatal(path, res.StatusCode)
		}
		res.Body.Close()
	}
	for _, path := range []string{"tunnels?node_id=1&node_id=2", "tunnels?limit=201", "tunnels?unknown=x", "tunnels/" + id + "/events?from_ms=1791000000000", "tunnels/" + id + "/events?to_ms=1791000000000", "tunnels/" + id + "/history?instance_id=" + instance + "&window=1000d", "tunnels/" + id + "/events?from_ms=1&to_ms=99999999999999"} {
		res := adminRequest(t, s, "GET", "/api/admin/v1/"+path, "", cookie, "", nil)
		if res.StatusCode != 400 {
			t.Fatal(path, res.StatusCode)
		}
		res.Body.Close()
	}
	res := adminRequest(t, s, "POST", "/api/admin/v1/tunnels", "{}", cookie, session.CSRF, nil)
	if res.StatusCode != 405 {
		t.Fatal(res.StatusCode)
	}
	res.Body.Close()
	// A reconnect retains native identity/generation but leaves an explicit
	// collector gap, even if the lifecycle ring did not change while offline.
	ws2 := dialTunnel(t, s, token, "tunnel-reconnected")
	r.SessionID = "tunnel-reconnected"
	r.Sequence = 2
	r.CollectedAt = time.Now().UTC().Format(time.RFC3339Nano)
	send(t, ws2, "frp.tunnel", "", r)
	until := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(75 * time.Millisecond)
		p, e := s.control.ListTunnelEvents(context.Background(), id, control.TunnelFilter{}, "", 100)
		if e != nil {
			t.Fatal(e)
		}
		gaps := 0
		for _, event := range p.Items {
			if event.Code == "collector_gap" {
				gaps++
			}
		}
		if gaps >= 2 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("reconnect missed collection gap")
		}
	}
	for _, snapshot := range []any{s.snapshotFor(time.Now(), false), s.adminSnapshot()} {
		data, _ := json.Marshal(snapshot)
		if strings.Contains(string(data), o.Identity.RawName) || strings.Contains(string(data), o.InstanceID) {
			t.Fatal("history leaked into public/admin SSE")
		}
	}
}
func TestTunnelInputHashExcludesOnlyCountersAndNotABARevision(t *testing.T) {
	o := monitorTunnelObject()
	v := shared.TunnelSnapshot{State: "ready", Source: "client", Objects: []shared.TunnelObject{o}, Events: []shared.TunnelEvent{}}
	in := tunnelInput{revision: "1", snapshot: &v}
	first := tunnelInputHash(in)
	v.CollectedAtMS++
	value := "7"
	v.Objects[0].Counters.RXBytes = &value
	if tunnelInputHash(in) != first {
		t.Fatal("ordinary counter causes database write")
	}
	in.revision = "3"
	if tunnelInputHash(in) == first {
		t.Fatal("rebind ABA hidden")
	}
	in.revision = "1"
	v.Objects[0].State.LocalState = "error"
	if tunnelInputHash(in) == first {
		t.Fatal("state transition hidden")
	}
}

func TestTunnelUnavailableServerProviderRecordsGapWithoutNativeTimestamp(t *testing.T) {
	s, _, _ := testAdmin(t)
	o := monitorTunnelObject()
	o.Identity.Kind = "proxy"
	o.Identity.Protocol = "tcp"
	o.State.Status = "registered"
	o.State.LocalState = "unknown"
	snapshot := shared.TunnelSnapshot{State: "ready", Source: "server", ProcessEpoch: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", CollectedAtMS: time.Now().UnixMilli(), FirstSequence: "0", LastSequence: "0", DroppedEvents: "0", Objects: []shared.TunnelObject{o}, Events: []shared.TunnelEvent{}}
	s.serverTunnelSnapshot.Store(&snapshot)
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(75 * time.Millisecond)
		p, e := s.control.ListTunnels(context.Background(), control.TunnelFilter{}, "", 50)
		if e != nil {
			t.Fatal(e)
		}
		if len(p.Items) == 1 {
			id = p.Items[0].TunnelID
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server inventory missing")
		}
	}
	unavailable := shared.EmptyTunnelSnapshot("server", "unavailable")
	s.serverTunnelSnapshot.Store(&unavailable)
	deadline = time.Now().Add(5 * time.Second)
	for {
		time.Sleep(75 * time.Millisecond)
		p, e := s.control.ListTunnelEvents(context.Background(), id, control.TunnelFilter{}, "", 100)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, event := range p.Items {
			if event.Code == "source_unavailable" {
				if event.OccurredAtMS != nil || event.TimeBasis != "observed" {
					t.Fatal("invented native timestamp")
				}
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider failure was silently omitted")
		}
	}
}
