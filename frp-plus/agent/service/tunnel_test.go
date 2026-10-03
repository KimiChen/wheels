package service

import (
	"context"
	"fmt"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readyTunnel() shared.TunnelSnapshot {
	s := shared.EmptyTunnelSnapshot("client", "ready")
	s.ProcessEpoch = "11111111-1111-4111-8111-111111111111"
	s.CollectedAtMS = time.Now().UnixMilli()
	return s
}
func TestTunnelNegotiationPreservesLegacyFrames(t *testing.T) {
	for _, tt := range []struct {
		name       string
		header     string
		provider   bool
		accept     bool
		wantOffer  bool
		wantTunnel bool
		snapshot   shared.TunnelProvider
		wantState  string
	}{
		{name: "old-server", provider: true},
		{name: "old-call-without-provider", header: shared.TunnelCapability},
		{name: "advertised-not-accepted", header: shared.TunnelCapability, provider: true, wantOffer: true},
		{name: "lookalike-is-not-capability", header: shared.TunnelCapability + "0", provider: true},
		{name: "negotiated", header: "unknown.v1, " + shared.TunnelCapability, provider: true, accept: true, wantOffer: true, wantTunnel: true},
		{name: "panic-is-unavailable", header: shared.TunnelCapability, provider: true, accept: true, wantOffer: true, wantTunnel: true,
			snapshot: func() shared.TunnelSnapshot { panic("private adapter failure") }, wantState: "unavailable"},
		{name: "oversize-is-truncated", header: shared.TunnelCapability, provider: true, accept: true, wantOffer: true, wantTunnel: true,
			snapshot: func() shared.TunnelSnapshot {
				tunnel := readyTunnel()
				tunnel.Objects = make([]shared.TunnelObject, shared.MaxTunnelObjects+1)
				return tunnel
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
					offered = offered || capability == shared.TunnelCapability
				}
				if offered != tt.wantOffer || strings.Contains(string(data), `"tunnel"`) {
					completed <- fmt.Errorf("unexpected tunnel offer or tunnel data in hello")
					return
				}
				caps := []string{"metrics.v1", "frp.v1"}
				if tt.accept {
					caps = append(caps, shared.TunnelCapability)
				}
				session := hello.Hello.SessionID
				if err := c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": hello.ID, "result": shared.HelloResult{Schema: 1, SessionID: session, Capabilities: caps, ReportInterval: 1}}); err != nil {
					completed <- err
					return
				}
				sequence, reports, tunnels := uint64(1), 0, 0
				for reports < 2 || (tt.wantTunnel && tunnels < 2) {
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
						if frame.Report.Metrics == nil || strings.Contains(string(data), `"tunnel"`) {
							completed <- fmt.Errorf("tunnel displaced or altered the host report")
							return
						}
					case frame.Tunnel != nil && tt.wantTunnel:
						tunnels++
						meta = frame.Tunnel.Meta
						if reports != tunnels || frame.Tunnel.Snapshot.State != wantState {
							completed <- fmt.Errorf("tunnel was not independent or did not follow its host report")
							return
						}
					default:
						completed <- fmt.Errorf("unnegotiated or unexpected frame %q", frame.Method)
						return
					}
					if meta.SessionID != session || meta.Sequence != sequence+1 {
						completed <- fmt.Errorf("report/tunnel sequence or session mismatch")
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
			var provider shared.TunnelProvider
			if tt.provider {
				provider = readyTunnel
				if tt.snapshot != nil {
					provider = tt.snapshot
				}
			}
			s, err := startWithAllProviders(context.Background(), cfg, nil, fixtures(t), nil, nil, nil, provider)
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
				t.Fatal("report/tunnel stream incomplete")
			}
		})
	}
}

func TestTunnelProviderHangAndPanicRemainBounded(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	defer close(release)
	s := &Service{ctx: context.Background(), tunnelProvider: func() shared.TunnelSnapshot { calls.Add(1); <-release; return readyTunnel() }}
	for i := 0; i < 20; i++ {
		if got := s.tunnelSnapshot(); got.State != "unavailable" {
			t.Fatal("pending provider looked fresh")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("hung provider spawned extra workers")
	}
	if got := collectTunnel(func() shared.TunnelSnapshot { panic("private secret") }); got.State != "unavailable" {
		t.Fatal("provider panic escaped")
	}
}
