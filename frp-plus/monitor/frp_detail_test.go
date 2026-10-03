package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

func dialDetail(t *testing.T, s *Service, token, session string) *websocket.Conn {
	t.Helper()
	c, response, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if response.Header.Get(shared.CapabilitiesHeader) != shared.FRPDetailCapability {
		t.Fatal("authenticated upgrade did not advertise detail capability")
	}
	hello := fixture(t, "hello").Hello
	hello.SessionID = session
	hello.Capabilities = []string{"metrics.v1", "frp.v1", shared.FRPDetailCapability}
	send(t, c, "hello", "detail-hello", hello)
	var answer struct {
		Result shared.HelloResult `json:"result"`
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	if err = c.ReadJSON(&answer); err != nil || !hasCapability(answer.Result.Capabilities, shared.FRPDetailCapability) {
		t.Fatalf("detail negotiation: %v %+v", err, answer)
	}
	return c
}

func detailReport(session string, sequence uint64) shared.FRPDetailReport {
	return shared.FRPDetailReport{
		Meta:   shared.Meta{Schema: 1, SessionID: session, Sequence: sequence, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		Detail: shared.EmptyFRPDetail("unavailable"),
	}
}

func TestFRPDetailSharesSequenceButNotMetricsFreshness(t *testing.T) {
	s, token := testMonitor(t)
	c := dialDetail(t, s, token, "detail-session")
	report := detailReport("detail-session", 2)
	send(t, c, "frp.detail", "", report)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.nodes["1"].frpDetailAt.IsZero() })
	s.mu.Lock()
	n := s.nodes["1"]
	at := n.frpDetailAt
	metricsAt := n.metricsAt
	s.mu.Unlock()
	if !metricsAt.IsZero() {
		t.Fatal("detail report must not refresh host resource samples")
	}
	metrics := fixture(t, "report-first").Report
	metrics.SessionID, metrics.Sequence = "detail-session", 3
	send(t, c, "report", "", metrics)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].sequence == 3 })
	s.mu.Lock()
	if !n.frpDetailAt.Equal(at) {
		t.Error("host report refreshed detail freshness")
	}
	s.mu.Unlock()
	row := s.adminSnapshot().Nodes[0]
	if row.FRPDetailState != "unavailable" || row.FRPDetailAt == nil {
		t.Fatalf("failed adapter state must remain visible: %+v", row)
	}
	report.Sequence = 3
	send(t, c, "frp.detail", "", report)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("detail duplicated a metrics sequence")
	}
}

func TestFRPDetailRequiresNegotiatedCapabilityAndSession(t *testing.T) {
	for _, mode := range []string{"unnegotiated", "wrong-session"} {
		t.Run(mode, func(t *testing.T) {
			s, token := testMonitor(t)
			var c *websocket.Conn
			if mode == "unnegotiated" {
				c = dial(t, s, token, "owner")
			} else {
				c = dialDetail(t, s, token, "owner")
			}
			session := "owner"
			if mode == "wrong-session" {
				session = "other-session"
			}
			send(t, c, "frp.detail", "", detailReport(session, 2))
			c.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatal("unauthorized detail frame accepted")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.nodes["1"].frpDetail != nil {
				t.Fatal("rejected frame changed detail")
			}
		})
	}
}

func TestFRPDetailFreshnessPrivacyAndSessionReplacement(t *testing.T) {
	s, token := testMonitor(t)
	c := dialDetail(t, s, token, "detail-owner")
	now := time.Now()
	private := shared.EmptyFRPDetail("ready")
	private.Association = fixture(t, "hello").Hello.Extensions.FRP.Association
	private.ServiceID = "private-service-identifier"
	report := detailReport("detail-owner", 2)
	report.Detail = private
	send(t, c, "frp.detail", "", report)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].frpDetail != nil })
	s.mu.Lock()
	n := s.nodes["1"]
	_, state, at := privateDetail(n, now.Add(11*time.Second), 1)
	s.mu.Unlock()
	if state != "stale" || at == nil {
		t.Fatal("detail did not expire independently")
	}
	row := s.adminSnapshot().Nodes[0]
	detail, ok := s.frpDetailSnapshot("1", time.Now())
	if row.FRPDetailState != "ready" || !ok || detail.Detail == nil || detail.Detail.ServiceID != private.ServiceID {
		t.Fatal("private detail missing")
	}
	list, _ := json.Marshal(s.adminSnapshot())
	if strings.Contains(string(list), private.ServiceID) {
		t.Fatal("list snapshot broadcast private detail payload")
	}
	data, err := json.Marshal(s.snapshot(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"frp_detail", private.ServiceID} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("public detail leak: %s", forbidden)
		}
	}
	second := dial(t, s, token, "legacy-owner")
	_ = second
	if s.acceptFRPDetail("1", c, now, detailReport("detail-owner", 9)) {
		t.Fatal("old connection overwrote new owner")
	}
	row = s.adminSnapshot().Nodes[0]
	if row.FRPDetailState != "unsupported" || row.FRPDetailAt != nil {
		t.Fatal("new legacy session inherited old private detail")
	}
}

func TestFRPDetailEndpointRequiresAdministrator(t *testing.T) {
	s, _, token := testAdmin(t)
	path := "/api/admin/v1/nodes/1/frp-detail"
	response := adminRequest(t, s, "GET", path, "", nil, "", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal(response.StatusCode)
	}
	cookie, session := login(t, s, token)
	response = adminRequest(t, s, "GET", path, "", cookie, "", nil)
	var value privateFRPDetail
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&value) != nil || value.NodeID != "1" || value.State != "unsupported" {
		t.Fatal("invalid private detail response", response.StatusCode, value)
	}
	if got := adminRequest(t, s, "POST", path, "{}", cookie, session.CSRF, nil).StatusCode; got != http.StatusMethodNotAllowed {
		t.Fatal(got)
	}
	if got := adminRequest(t, s, "GET", "/api/admin/v1/nodes/999/frp-detail", "", cookie, "", nil).StatusCode; got != http.StatusNotFound {
		t.Fatal(got)
	}
}

func TestFRPDetailHTTPReadyAuthorizationAndPublicSSE(t *testing.T) {
	s, agentToken, adminToken := testAdmin(t)
	cookie, session := login(t, s, adminToken)
	c := dialDetail(t, s, agentToken, "http-ready-session")
	report := detailReport("http-ready-session", 2)
	report.Detail = shared.EmptyFRPDetail("ready")
	report.Detail.Association = fixture(t, "hello").Hello.Extensions.FRP.Association
	report.Detail.ServiceID = "private-http-detail-service"
	report.Detail.Proxies = []shared.FRPProxyDetail{{Name: "private-detail-proxy", Type: "http", Source: "file", SourceState: "active", Status: "running", Enabled: true, Endpoints: []shared.FRPEndpoint{{Kind: "http", Source: "configured", Host: "private-detail.example.invalid"}}}}
	send(t, c, "frp.detail", "", report)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].sequence == 2 })
	path := "/api/admin/v1/nodes/1/frp-detail"
	response := adminRequest(t, s, "GET", path, "", cookie, "", nil)
	defer response.Body.Close()
	var value privateFRPDetail
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&value) != nil || value.NodeID != "1" || value.State != "ready" || value.ReceivedAt == nil || value.Detail == nil || value.Detail.ServiceID != report.Detail.ServiceID || len(value.Detail.Proxies) != 1 || value.Detail.Proxies[0].Endpoints[0].Host != "private-detail.example.invalid" {
		t.Fatalf("ready HTTP detail did not preserve the private observation: status=%d value=%+v", response.StatusCode, value)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatal("private detail response lacks non-cacheable JSON headers")
	}
	for _, headers := range []http.Header{
		{"Origin": []string{"https://other.example.invalid"}},
		{"Sec-Fetch-Site": []string{"cross-site"}},
	} {
		denied := adminRequest(t, s, "GET", path, "", cookie, "", headers)
		body, err := io.ReadAll(denied.Body)
		denied.Body.Close()
		if err != nil || denied.StatusCode != http.StatusForbidden || strings.Contains(string(body), report.Detail.ServiceID) {
			t.Fatal("cross-origin request with a valid administrator cookie exposed detail", denied.StatusCode, err)
		}
	}

	// Exercise the actual anonymous SSE response after receiving private data,
	// rather than only checking the shared public snapshot projection.
	s.publishSnapshot(time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+"/events/public", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || stream.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("public SSE request failed", stream.StatusCode)
	}
	reader := bufio.NewReader(stream.Body)
	event, err := reader.ReadString('\n')
	if err != nil || event != "event: snapshot\n" {
		t.Fatal("public SSE snapshot missing", err)
	}
	data, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(data, "data: {") {
		t.Fatal("public SSE payload missing", err)
	}
	for _, private := range []string{"frp_detail", "service_id", report.Detail.ServiceID, "private-detail-proxy", "private-detail.example.invalid"} {
		if strings.Contains(data, private) {
			t.Fatalf("public SSE leaked private detail field %s", private)
		}
	}
	stream.Body.Close()

	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/logout", "", cookie, session.CSRF, nil), http.StatusNoContent)
	denied := adminRequest(t, s, "GET", path, "", cookie, "", nil)
	body, err := io.ReadAll(denied.Body)
	denied.Body.Close()
	if err != nil || denied.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), report.Detail.ServiceID) {
		t.Fatal("revoked administrator session retained detail access", denied.StatusCode, err)
	}
}

func TestFRPDetailHTTPStateTransitionsAndNodeDeletion(t *testing.T) {
	s, agentToken, adminToken := testAdmin(t)
	cookie, session := login(t, s, adminToken)
	path := "/api/admin/v1/nodes/1/frp-detail"
	for _, state := range []string{"stale", "busy", "unavailable", "truncated", "waiting", "unsupported"} {
		t.Run(state, func(t *testing.T) {
			sessionID := "http-state-" + state
			c := dialDetail(t, s, agentToken, sessionID)
			report := detailReport(sessionID, 2)
			report.Detail = shared.EmptyFRPDetail("ready")
			report.Detail.Association = fixture(t, "hello").Hello.Extensions.FRP.Association
			report.Detail.ServiceID = "previous-private-service"
			send(t, c, "frp.detail", "", report)
			eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].sequence == 2 })
			switch state {
			case "stale":
				s.mu.Lock()
				s.nodes["1"].frpDetailAt = time.Now().Add(-max(10*time.Second, time.Duration(s.cfg.ReportIntervalSeconds)*3*time.Second) - time.Second)
				s.mu.Unlock()
			case "busy", "unavailable", "truncated":
				report.Sequence = 3
				report.Detail = shared.EmptyFRPDetail(state)
				send(t, c, "frp.detail", "", report)
				eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].sequence == 3 })
			case "waiting":
				dialDetail(t, s, agentToken, sessionID+"-replacement")
			case "unsupported":
				dial(t, s, agentToken, sessionID+"-legacy")
			}
			response := adminRequest(t, s, "GET", path, "", cookie, "", nil)
			defer response.Body.Close()
			var value privateFRPDetail
			if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&value) != nil || value.NodeID != "1" || value.State != state || value.Detail != nil {
				t.Fatalf("HTTP state transition retained old ready data: status=%d value=%+v", response.StatusCode, value)
			}
			withoutTimestamp := state == "waiting" || state == "unsupported"
			if (value.ReceivedAt == nil) != withoutTimestamp {
				t.Fatalf("incorrect observation timestamp for %s", state)
			}
		})
	}

	// The same authenticated request must cease to resolve once the actual
	// administrator node deletion commits and invalidates cached list snapshots.
	expectStatus(t, adminRequest(t, s, "DELETE", "/api/admin/v1/nodes/1", "", cookie, session.CSRF, nil), http.StatusNoContent)
	deleted := adminRequest(t, s, "GET", path, "", cookie, "", nil)
	if deleted.StatusCode != http.StatusNotFound {
		t.Fatal("deleted node retained private detail endpoint", deleted.StatusCode)
	}
	list := adminRequest(t, s, "GET", "/api/admin/v1/nodes", "", cookie, "", nil)
	defer list.Body.Close()
	var snapshot adminSnapshot
	if list.StatusCode != http.StatusOK || json.NewDecoder(list.Body).Decode(&snapshot) != nil || len(snapshot.Nodes) != 0 {
		t.Fatal("deleted node retained list detail metadata", list.StatusCode)
	}
}
