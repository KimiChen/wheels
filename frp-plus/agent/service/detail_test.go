package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

func readyDetail() shared.FRPDetail {
	detail := shared.EmptyFRPDetail("ready")
	id := "test-client"
	detail.Association = shared.FRPAssociation{ServerID: "example", RawClientID: &id}
	detail.ServiceID = "primary"
	return detail
}

func TestDetailNegotiationPreservesLegacyFrames(t *testing.T) {
	for _, tt := range []struct {
		name       string
		header     string
		provider   bool
		accept     bool
		wantOffer  bool
		wantDetail bool
		snapshot   shared.DetailProvider
		wantState  string
	}{
		{name: "old-server", provider: true},
		{name: "old-call-without-provider", header: shared.FRPDetailCapability},
		{name: "advertised-not-accepted", header: shared.FRPDetailCapability, provider: true, wantOffer: true},
		{name: "lookalike-is-not-capability", header: shared.FRPDetailCapability + "0", provider: true},
		{name: "negotiated", header: "unknown.v1, " + shared.FRPDetailCapability, provider: true, accept: true, wantOffer: true, wantDetail: true},
		{name: "panic-is-unavailable", header: shared.FRPDetailCapability, provider: true, accept: true, wantOffer: true, wantDetail: true,
			snapshot: func() shared.FRPDetail { panic("private adapter failure") }, wantState: "unavailable"},
		{name: "oversize-is-truncated", header: shared.FRPDetailCapability, provider: true, accept: true, wantOffer: true, wantDetail: true,
			snapshot: func() shared.FRPDetail {
				detail := readyDetail()
				detail.Proxies = make([]shared.FRPProxyDetail, shared.MaxDetailProxies+1)
				return detail
			}, wantState: "truncated"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wantState := tt.wantState
			if wantState == "" {
				wantState = "ready"
			}
			completed := make(chan error, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				header := http.Header{}
				if tt.header != "" {
					header.Set(shared.CapabilitiesHeader, tt.header)
				}
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, header)
				if err != nil {
					return
				}
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
				_, data, err := c.ReadMessage()
				if err != nil {
					completed <- err
					return
				}
				hello, err := shared.DecodeFrame(data)
				if err != nil || hello.Hello == nil {
					completed <- fmt.Errorf("invalid hello: %v", err)
					return
				}
				offered := false
				for _, capability := range hello.Hello.Capabilities {
					offered = offered || capability == shared.FRPDetailCapability
				}
				if offered != tt.wantOffer || strings.Contains(string(data), `"detail"`) {
					completed <- fmt.Errorf("unexpected detail offer or detail data in hello")
					return
				}
				caps := []string{"metrics.v1", "frp.v1"}
				if tt.accept {
					caps = append(caps, shared.FRPDetailCapability)
				}
				session := hello.Hello.SessionID
				if err := c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": hello.ID, "result": shared.HelloResult{Schema: 1, SessionID: session, Capabilities: caps, ReportInterval: 1}}); err != nil {
					completed <- err
					return
				}
				sequence, reports, details := uint64(1), 0, 0
				for reports < 2 || (tt.wantDetail && details < 2) {
					_, data, err := c.ReadMessage()
					if err != nil {
						completed <- err
						return
					}
					frame, err := shared.DecodeFrame(data)
					if err != nil {
						completed <- err
						return
					}
					var meta shared.Meta
					switch {
					case frame.Report != nil:
						reports++
						meta = frame.Report.Meta
						if frame.Report.Metrics == nil || strings.Contains(string(data), `"detail"`) {
							completed <- fmt.Errorf("detail displaced or altered the host report")
							return
						}
					case frame.FRPDetail != nil && tt.wantDetail:
						details++
						meta = frame.FRPDetail.Meta
						if reports != details || frame.FRPDetail.Detail.State != wantState {
							completed <- fmt.Errorf("detail was not independent or did not follow its host report")
							return
						}
					default:
						completed <- fmt.Errorf("unnegotiated or unexpected frame %q", frame.Method)
						return
					}
					if meta.SessionID != session || meta.Sequence != sequence+1 {
						completed <- fmt.Errorf("report/detail sequence or session mismatch")
						return
					}
					sequence = meta.Sequence
				}
				completed <- nil
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			cfg := testConfig(t)
			cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/ws"
			var provider shared.DetailProvider
			if tt.provider {
				provider = readyDetail
				if tt.snapshot != nil {
					provider = tt.snapshot
				}
			}
			s, err := start(context.Background(), cfg, nil, fixtures(t), provider)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("report/detail stream incomplete")
			}
		})
	}
}

func TestDetailProviderFailureDoesNotDropHostSamples(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider shared.DetailProvider
		state    string
	}{
		{name: "absent"},
		{name: "panic", provider: func() shared.FRPDetail { panic("private adapter failure") }, state: "unavailable"},
		{name: "invalid", provider: func() shared.FRPDetail { return shared.FRPDetail{State: "invalid"} }, state: "unavailable"},
		{name: "proxy-limit", provider: func() shared.FRPDetail {
			detail := readyDetail()
			detail.Proxies = make([]shared.FRPProxyDetail, shared.MaxDetailProxies+1)
			return detail
		}, state: "truncated"},
		{name: "visitor-limit", provider: func() shared.FRPDetail {
			detail := readyDetail()
			detail.Visitors = make([]shared.FRPVisitorDetail, shared.MaxDetailVisitors+1)
			return detail
		}, state: "truncated"},
		{name: "endpoint-limit", provider: func() shared.FRPDetail {
			detail := readyDetail()
			detail.Proxies = []shared.FRPProxyDetail{{Name: "web", Type: "http", Source: "unknown", SourceState: "unknown", Status: "unknown", Endpoints: make([]shared.FRPEndpoint, shared.MaxDetailEndpoints+1)}}
			return detail
		}, state: "truncated"},
		{name: "byte-limit", provider: func() shared.FRPDetail {
			detail := readyDetail()
			for i := 0; i < 400; i++ {
				detail.Proxies = append(detail.Proxies, shared.FRPProxyDetail{Name: fmt.Sprintf("web-%d", i), Type: "http", Source: "file", SourceState: "active", Status: "unknown", Endpoints: []shared.FRPEndpoint{{Kind: "http", Source: "configured", Host: "example.invalid", Path: "/" + strings.Repeat("p", 512)}}})
			}
			return detail
		}, state: "truncated"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Service{ctx: context.Background(), collector: fixtures(t), detailProvider: tt.provider, ready: make(chan struct{}), notify: make(chan struct{}, 1)}
			s.sample()
			got := s.current()
			if got.generation != 1 || got.metrics.Validate() != nil || got.facts.Validate() != nil {
				t.Fatal("detail failure prevented host sampling")
			}
			if tt.state == "" {
				if got.detail != nil {
					t.Fatal("missing provider fabricated detail")
				}
			} else if got.detail == nil || got.detail.State != tt.state || got.detail.Validate() != nil {
				t.Fatalf("expected bounded %s detail, got %+v", tt.state, got.detail)
			}
		})
	}
}

func TestDetailSnapshotOwnsProviderData(t *testing.T) {
	provided := readyDetail()
	provided.Proxies = []shared.FRPProxyDetail{{Name: "echo", Type: "tcp", Source: "unknown", SourceState: "unknown", Status: "unknown", Endpoints: []shared.FRPEndpoint{}}}
	detail := collectFRPDetail(func() shared.FRPDetail { return provided })
	if detail.State != "ready" || len(detail.Proxies) != 1 {
		t.Fatal("valid provider data was not collected")
	}
	provided.Proxies[0].Name = "changed"
	*provided.Association.RawClientID = "changed"
	if detail.Proxies[0].Name != "echo" || *detail.Association.RawClientID != "test-client" {
		t.Fatal("published detail retained provider-owned slices or pointers")
	}
}

func TestBlockedDetailProviderHasOneOutstandingCall(t *testing.T) {
	unblock := make(chan struct{})
	defer close(unblock)
	var calls atomic.Int32
	s := &Service{ctx: context.Background(), collector: fixtures(t), ready: make(chan struct{}), notify: make(chan struct{}, 1), detailProvider: func() shared.FRPDetail {
		calls.Add(1)
		<-unblock
		return readyDetail()
	}}
	started := time.Now()
	for i := 0; i < 5; i++ {
		s.sample()
	}
	got := s.current()
	if time.Since(started) > time.Second || calls.Load() != 1 || got.generation != 5 || got.detail == nil || got.detail.State != "unavailable" {
		t.Fatal("blocked detail provider stalled host metrics or created unbounded workers")
	}
}

func TestDetailCapabilitiesRequireExactAdvertisementAndAcceptance(t *testing.T) {
	for _, value := range []string{"", "frp.detail.v10", "prefix-frp.detail.v1", "FRP.DETAIL.V1", "frp.detail.v1;version=1", strings.Repeat("x", 4096) + ",frp.detail.v1"} {
		header := http.Header{}
		header.Set(shared.CapabilitiesHeader, value)
		if advertisedCapability(header, shared.FRPDetailCapability) {
			t.Fatal("non-exact or oversize header advertised detail")
		}
	}
	header := http.Header{}
	header.Add(shared.CapabilitiesHeader, "other.v1")
	header.Add(shared.CapabilitiesHeader, " \t"+shared.FRPDetailCapability+" \t")
	if !advertisedCapability(header, shared.FRPDetailCapability) {
		t.Fatal("exact capability on a separate header line was ignored")
	}
	caps := []string{"metrics.v1", shared.FRPDetailCapability}
	if capabilities(caps, false, false) || !capabilities(caps, false, true) {
		t.Fatal("server may only accept a capability the agent offered")
	}
}

func TestSessionSequenceDoesNotWrap(t *testing.T) {
	sequence := ^uint64(0) - 1
	if !nextSequence(&sequence) || sequence != ^uint64(0) {
		t.Fatal("last sequence is not usable")
	}
	if nextSequence(&sequence) || sequence != ^uint64(0) {
		t.Fatal("session sequence wrapped")
	}
}
