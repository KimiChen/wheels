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

type restoreProviderFunc func(context.Context, shared.RestoreCommand) shared.RestoreResult

func (f restoreProviderFunc) HandleRestore(ctx context.Context, c shared.RestoreCommand) shared.RestoreResult {
	return f(ctx, c)
}
func restoreCommand(session string, sequence uint64) shared.RestoreCommand {
	c := managementCommand(session, sequence)
	return shared.RestoreCommand{Meta: c.Meta, RequestID: c.RequestID, ServiceID: c.ServiceID, Action: "inspect", DeadlineAtMS: c.DeadlineAtMS}
}
func restoreReply(c shared.RestoreCommand) shared.RestoreResult {
	return shared.RestoreResult{Meta: c.Meta, RequestID: c.RequestID, ServiceID: managementServiceID, Action: c.Action, Code: "ok", Restore: &shared.RestoreInfo{State: "none"}}
}

func TestRestoreNegotiationAndSharedUplink(t *testing.T) {
	for _, tt := range []struct {
		name, header                      string
		enabled, provider, accepted, want bool
	}{
		{name: "old-server", enabled: true, provider: true},
		{name: "management-only", header: shared.ConfigManageCapability, enabled: true, provider: true},
		{name: "lookalike", header: shared.ConfigRestoreCapability + "0", enabled: true, provider: true},
		{name: "disabled", header: shared.ConfigRestoreCapability, provider: true},
		{name: "no-provider", header: shared.ConfigRestoreCapability, enabled: true},
		{name: "not-accepted", header: shared.ConfigRestoreCapability, enabled: true, provider: true, want: true},
		{name: "negotiated", header: shared.ConfigRestoreCapability, enabled: true, provider: true, accepted: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			completed := make(chan error, 2)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, http.Header{shared.CapabilitiesHeader: []string{tt.header}})
				if err != nil {
					return
				}
				defer c.Close()
				err = func() error {
					c.SetReadDeadline(time.Now().Add(4 * time.Second))
					_, data, err := c.ReadMessage()
					if err != nil {
						return err
					}
					f, err := shared.DecodeFrame(data)
					if err != nil || f.Hello == nil {
						return fmt.Errorf("hello: %v", err)
					}
					h := f.Hello
					offered := false
					for _, cap := range h.Capabilities {
						offered = offered || cap == shared.ConfigRestoreCapability
					}
					if offered != tt.want {
						return fmt.Errorf("wrong restore offer: %t", offered)
					}
					caps := []string{"metrics.v1", "frp.v1"}
					if tt.accepted {
						caps = append(caps, shared.ConfigRestoreCapability)
					}
					if err := c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": shared.HelloResult{Schema: 1, SessionID: h.SessionID, Capabilities: caps, ReportInterval: 1}}); err != nil {
						return err
					}
					if tt.accepted {
						if err := writeFrame(c, "config.restore.command", "", restoreCommand(h.SessionID, 2)); err != nil {
							return err
						}
					}
					sequence := uint64(1)
					reports, replies := 0, 0
					for reports < 1 || (tt.accepted && replies < 1) {
						_, data, err := c.ReadMessage()
						if err != nil {
							return err
						}
						f, err := shared.DecodeFrame(data)
						if err != nil {
							return err
						}
						var meta shared.Meta
						switch {
						case f.Report != nil:
							reports++
							meta = f.Report.Meta
						case f.RestoreResult != nil && tt.accepted:
							replies++
							meta = f.RestoreResult.Meta
							if f.RestoreResult.Code != "ok" {
								return fmt.Errorf("unexpected result")
							}
						default:
							return fmt.Errorf("unexpected frame")
						}
						if meta.Sequence != sequence+1 || meta.SessionID != h.SessionID {
							return fmt.Errorf("unshared sequence")
						}
						sequence = meta.Sequence
					}
					return nil
				}()
				completed <- err
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			cfg := testConfig(t)
			cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
			cfg.ConfigManagement = &shared.ConfigManagementConfig{Enabled: tt.enabled, Root: t.TempDir()}
			var provider shared.RestoreProvider
			if tt.provider {
				provider = restoreProviderFunc(func(_ context.Context, c shared.RestoreCommand) shared.RestoreResult {
					calls.Add(1)
					return restoreReply(c)
				})
			}
			svc, err := startWithProviders(context.Background(), cfg, nil, fixtures(t), nil, nil, provider)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("negotiation stalled")
			}
			expected := int32(0)
			if tt.accepted {
				expected = 1
			}
			if calls.Load() != expected {
				t.Fatal("provider invocation without negotiation")
			}
		})
	}
}

func TestRestoreAndConfigShareWorkerAcrossReconnect(t *testing.T) {
	for _, hungRestore := range []bool{true, false} {
		t.Run(fmt.Sprint(hungRestore), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release, started := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			var calls atomic.Int32
			svc := &Service{ctx: ctx, configJobs: make(chan configJob, 1)}
			svc.restoreProvider = restoreProviderFunc(func(_ context.Context, c shared.RestoreCommand) shared.RestoreResult {
				calls.Add(1)
				started <- struct{}{}
				<-release
				return restoreReply(c)
			})
			svc.configProvider = managementProvider(func(_ context.Context, c shared.ConfigCommand) shared.ConfigResult {
				calls.Add(1)
				started <- struct{}{}
				<-release
				return managementResult(c)
			})
			svc.wg.Add(1)
			go svc.guarded(svc.configLoop)
			oldCtx, oldCancel := context.WithCancel(ctx)
			defer oldCancel()
			oldRestore, oldConfig := make(chan shared.RestoreResult, 2), make(chan shared.ConfigResult, 2)
			c := restoreCommand("old", 2)
			if hungRestore {
				svc.dispatchRestore(configJob{ctx: oldCtx, restore: &c, restoreResults: oldRestore})
			} else {
				svc.dispatchConfig(configJob{ctx: oldCtx, command: managementCommand("old", 2), results: oldConfig})
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("provider did not start")
			}
			oldCancel()
			newRestore, newConfig := make(chan shared.RestoreResult, 1), make(chan shared.ConfigResult, 1)
			if hungRestore {
				svc.dispatchConfig(configJob{ctx: ctx, command: managementCommand("new", 2), results: newConfig})
				select {
				case r := <-newConfig:
					if r.Code != "busy" || r.SessionID != "new" {
						t.Fatal(r)
					}
				case <-time.After(time.Second):
					t.Fatal("config did not share gate")
				}
			} else {
				next := restoreCommand("new", 2)
				svc.dispatchRestore(configJob{ctx: ctx, restore: &next, restoreResults: newRestore})
				select {
				case r := <-newRestore:
					if r.Code != "busy" || r.SessionID != "new" {
						t.Fatal(r)
					}
				case <-time.After(time.Second):
					t.Fatal("restore did not share gate")
				}
			}
			if calls.Load() != 1 || len(oldRestore) != 0 || len(oldConfig) != 0 {
				t.Fatal("late work crossed session")
			}
			cancel()
			svc.wg.Wait()
		})
	}
}
func TestRestoreProviderInvalidPanicAndExpiry(t *testing.T) {
	for _, mode := range []string{"panic", "wrong-request", "wrong-service", "expired"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			svc := &Service{ctx: ctx, configJobs: make(chan configJob, 1), restoreProvider: restoreProviderFunc(func(_ context.Context, c shared.RestoreCommand) shared.RestoreResult {
				calls.Add(1)
				if mode == "panic" {
					panic("private material")
				}
				r := restoreReply(c)
				if mode == "wrong-request" {
					r.RequestID = "00000000-0000-4000-8000-000000000099"
				}
				if mode == "wrong-service" {
					r.ServiceID = "00000000-0000-4000-8000-000000000099"
				}
				return r
			})}
			c := restoreCommand("session", 2)
			expected := "internal_error"
			if mode == "expired" {
				c.CollectedAt = time.Now().Add(-2 * time.Second).Format(time.RFC3339Nano)
				c.DeadlineAtMS = time.Now().Add(-time.Second).UnixMilli()
				expected = "expired"
			}
			results := make(chan shared.RestoreResult, 1)
			svc.configBusy.Store(true)
			svc.configJobs <- configJob{ctx: ctx, restore: &c, restoreResults: results}
			svc.wg.Add(1)
			go svc.guarded(svc.configLoop)
			select {
			case r := <-results:
				if r.Code != expected || r.Restore != nil {
					t.Fatal(r)
				}
			case <-time.After(time.Second):
				t.Fatal("worker stalled")
			}
			cancel()
			svc.wg.Wait()
			if mode == "expired" && calls.Load() != 0 {
				t.Fatal("expired provider invoked")
			}
		})
	}
}

func writeRestore(c *websocket.Conn, command shared.RestoreCommand) error {
	return writeFrame(c, "config.restore.command", "", command)
}
func restoreHello(c *websocket.Conn, accept bool) (*shared.Hello, error) {
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		return nil, err
	}
	f, err := shared.DecodeFrame(data)
	if err != nil || f.Hello == nil {
		return nil, fmt.Errorf("invalid hello: %v", err)
	}
	caps := []string{"metrics.v1", "frp.v1"}
	if accept {
		caps = append(caps, shared.ConfigRestoreCapability)
	}
	if err := c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": shared.HelloResult{Schema: 1, SessionID: f.Hello.SessionID, Capabilities: caps, ReportInterval: 1}}); err != nil {
		return nil, err
	}
	return f.Hello, nil
}
func TestRestoreHungOrPanickingProviderKeepsMetricsAndBoundedCalls(t *testing.T) {
	for _, mode := range []string{"hung", "panic"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			var calls atomic.Int32
			provider := restoreProviderFunc(func(_ context.Context, c shared.RestoreCommand) shared.RestoreResult {
				calls.Add(1)
				if mode == "panic" {
					panic("synthetic-private-provider-error")
				}
				<-release
				return restoreReply(c)
			})
			completed := make(chan error, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, http.Header{shared.CapabilitiesHeader: []string{shared.ConfigRestoreCapability}})
				if err != nil {
					return
				}
				defer c.Close()
				err = func() error {
					hello, err := restoreHello(c, true)
					if err != nil {
						return err
					}
					cmd := restoreCommand(hello.SessionID, 2)
					cmd.DeadlineAtMS = time.Now().Add(200 * time.Millisecond).UnixMilli()
					if err := writeRestore(c, cmd); err != nil {
						return err
					}
					reports, replies := 0, 0
					for reports < 2 || replies < 1 || (mode == "hung" && replies < 2) {
						_, data, err := c.ReadMessage()
						if err != nil {
							return err
						}
						if strings.Contains(string(data), "synthetic-private-provider-error") {
							return fmt.Errorf("provider panic leaked")
						}
						f, err := shared.DecodeFrame(data)
						if err != nil {
							return err
						}
						if f.Report != nil {
							reports++
							continue
						}
						if f.RestoreResult == nil {
							return fmt.Errorf("unexpected stream frame")
						}
						replies++
						want := "internal_error"
						if mode == "hung" {
							want = "timeout"
							if replies == 2 {
								want = "busy"
							}
						}
						if f.RestoreResult.Code != want {
							return fmt.Errorf("expected %s, received %s", want, f.RestoreResult.Code)
						}
						if mode == "hung" && replies == 1 {
							if err := writeRestore(c, restoreCommand(hello.SessionID, 3)); err != nil {
								return err
							}
						}
					}
					return nil
				}()
				completed <- err
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			cfg := testConfig(t)
			cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
			cfg.ConfigManagement = &shared.ConfigManagementConfig{Enabled: true, Root: t.TempDir()}
			svc, err := startWithProviders(context.Background(), cfg, nil, fixtures(t), nil, nil, provider)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("provider blocked telemetry")
			}
			if calls.Load() != 1 {
				t.Fatal("hung provider accumulated calls", calls.Load())
			}
		})
	}
}

func TestRestoreRejectsUnnegotiatedAndReplayedDownlink(t *testing.T) {
	for _, mode := range []string{"not-negotiated", "replayed", "wrong-session"} {
		t.Run(mode, func(t *testing.T) {
			completed := make(chan error, 2)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, http.Header{shared.CapabilitiesHeader: []string{shared.ConfigRestoreCapability}})
				if err != nil {
					return
				}
				defer c.Close()
				h, err := restoreHello(c, mode != "not-negotiated")
				if err != nil {
					completed <- err
					return
				}
				command := restoreCommand(h.SessionID, 2)
				if mode == "replayed" {
					if err := writeRestore(c, command); err != nil {
						completed <- err
						return
					}
					for {
						_, data, err := c.ReadMessage()
						if err != nil {
							completed <- err
							return
						}
						f, err := shared.DecodeFrame(data)
						if err != nil {
							completed <- err
							return
						}
						if f.RestoreResult != nil {
							break
						}
					}
				}
				if mode == "wrong-session" {
					command.SessionID = "old-session"
				}
				if err := writeRestore(c, command); err != nil {
					completed <- err
					return
				}
				for {
					_, data, err := c.ReadMessage()
					if err != nil {
						completed <- nil
						return
					}
					f, err := shared.DecodeFrame(data)
					if err != nil || f.RestoreResult != nil {
						completed <- fmt.Errorf("invalid downlink invoked restore")
						return
					}
				}
			}))
			defer server.Close()
			cfg := testConfig(t)
			cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
			cfg.ConfigManagement = &shared.ConfigManagementConfig{Enabled: true, Root: t.TempDir()}
			svc, err := startWithProviders(context.Background(), cfg, nil, fixtures(t), nil, nil, restoreProviderFunc(func(_ context.Context, c shared.RestoreCommand) shared.RestoreResult {
				calls.Add(1)
				return restoreReply(c)
			}))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("invalid frame was not rejected")
			}
			svc.Close()
			want := int32(0)
			if mode == "replayed" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatal("unnegotiated or replayed invocation", calls.Load())
			}
		})
	}
}
