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

const managementServiceID = "00000000-0000-4000-8000-000000000001"

type managementProvider func(context.Context, shared.ConfigCommand) shared.ConfigResult

func (f managementProvider) HandleConfig(ctx context.Context, c shared.ConfigCommand) shared.ConfigResult {
	return f(ctx, c)
}
func managementCommand(session string, sequence uint64) shared.ConfigCommand {
	now := time.Now().UTC()
	return shared.ConfigCommand{Meta: shared.Meta{Schema: 1, SessionID: session, Sequence: sequence, CollectedAt: now.Format(time.RFC3339Nano)}, RequestID: fmt.Sprintf("00000000-0000-4000-8000-%012x", sequence), ServiceID: managementServiceID, Action: "inspect", DeadlineAtMS: now.Add(time.Second).UnixMilli(), Changes: []shared.ConfigChange{}}
}
func managementResult(c shared.ConfigCommand) shared.ConfigResult {
	return shared.ConfigResult{Meta: c.Meta, RequestID: c.RequestID, ServiceID: managementServiceID, Action: c.Action, OperationID: c.OperationID, Code: "ok", Inventory: &shared.ConfigInventory{Revision: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), State: "ready", Objects: []shared.ConfigObjectView{}, Dependencies: []shared.ConfigDependency{}, Issues: []shared.ConfigIssue{}}}
}
func writeManagement(c *websocket.Conn, command shared.ConfigCommand) error {
	return c.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "config.command", "params": command})
}
func managementHello(c *websocket.Conn, accept bool) (*shared.Hello, error) {
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		return nil, err
	}
	frame, err := shared.DecodeFrame(data)
	if err != nil || frame.Hello == nil {
		return nil, fmt.Errorf("invalid hello: %v", err)
	}
	caps := []string{"metrics.v1", "frp.v1"}
	if accept {
		caps = append(caps, shared.ConfigManageCapability)
	}
	if err := c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": shared.HelloResult{Schema: 1, SessionID: frame.Hello.SessionID, Capabilities: caps, ReportInterval: 1}}); err != nil {
		return nil, err
	}
	return frame.Hello, nil
}
func TestConfigNegotiationNeedsHeaderProviderAndLocalOptIn(t *testing.T) {
	for _, tt := range []struct {
		name, header                    string
		enabled, provider, accept, want bool
	}{
		{name: "old-server", enabled: true, provider: true},
		{name: "lookalike", header: shared.ConfigManageCapability + "0", enabled: true, provider: true},
		{name: "local-disabled", header: shared.ConfigManageCapability, provider: true},
		{name: "no-provider", header: shared.ConfigManageCapability, enabled: true},
		{name: "not-accepted", header: shared.ConfigManageCapability, enabled: true, provider: true, want: true},
		{name: "negotiated", header: shared.ConfigManageCapability, enabled: true, provider: true, accept: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			completed := make(chan error, 4)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, http.Header{shared.CapabilitiesHeader: []string{tt.header}})
				if err != nil {
					return
				}
				defer c.Close()
				err = func() error {
					hello, err := managementHello(c, tt.accept)
					if err != nil {
						return err
					}
					offered := false
					for _, capability := range hello.Capabilities {
						offered = offered || capability == shared.ConfigManageCapability
					}
					if offered != tt.want {
						return fmt.Errorf("unexpected management offer %t", offered)
					}
					if tt.accept {
						if err := writeManagement(c, managementCommand(hello.SessionID, 2)); err != nil {
							return err
						}
					}
					reports, replies := 0, 0
					sequence := uint64(1)
					for reports < 1 || (tt.accept && replies < 1) {
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
						case f.ConfigResult != nil && tt.accept:
							replies++
							meta = f.ConfigResult.Meta
							if f.ConfigResult.Code != "ok" {
								return fmt.Errorf("unexpected configuration result")
							}
						default:
							return fmt.Errorf("unexpected frame")
						}
						if meta.Sequence != sequence+1 || meta.SessionID != hello.SessionID {
							return fmt.Errorf("uplink does not share sequence/session")
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
			var provider shared.ConfigProvider
			if tt.provider {
				provider = managementProvider(func(_ context.Context, c shared.ConfigCommand) shared.ConfigResult {
					calls.Add(1)
					return managementResult(c)
				})
			}
			svc, err := startWithProviders(context.Background(), cfg, nil, fixtures(t), nil, provider)
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
				t.Fatal("management negotiation stalled")
			}
			if got := calls.Load(); (tt.accept && got != 1) || (!tt.accept && got != 0) {
				t.Fatal("unauthorized or missing provider call", got)
			}
		})
	}
}
func TestConfigHungOrPanickingProviderKeepsMetricsAndBoundedCalls(t *testing.T) {
	for _, mode := range []string{"hung", "panic"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			var calls atomic.Int32
			provider := managementProvider(func(_ context.Context, c shared.ConfigCommand) shared.ConfigResult {
				calls.Add(1)
				if mode == "panic" {
					panic("synthetic-private-provider-error")
				}
				<-release
				return managementResult(c)
			})
			completed := make(chan error, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, http.Header{shared.CapabilitiesHeader: []string{shared.ConfigManageCapability}})
				if err != nil {
					return
				}
				defer c.Close()
				err = func() error {
					hello, err := managementHello(c, true)
					if err != nil {
						return err
					}
					cmd := managementCommand(hello.SessionID, 2)
					cmd.DeadlineAtMS = time.Now().Add(200 * time.Millisecond).UnixMilli()
					if err := writeManagement(c, cmd); err != nil {
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
						if f.ConfigResult == nil {
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
						if f.ConfigResult.Code != want {
							return fmt.Errorf("expected %s, received %s", want, f.ConfigResult.Code)
						}
						if mode == "hung" && replies == 1 {
							if err := writeManagement(c, managementCommand(hello.SessionID, 3)); err != nil {
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
			svc, err := startWithProviders(context.Background(), cfg, nil, fixtures(t), nil, provider)
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
func TestConfigOutstandingCallSurvivesSessionChangeWithoutDuplicateWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	svc := &Service{ctx: ctx, configJobs: make(chan configJob, 1), configProvider: managementProvider(func(_ context.Context, c shared.ConfigCommand) shared.ConfigResult {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return managementResult(c)
	})}
	svc.wg.Add(1)
	go svc.guarded(svc.configLoop)
	oldCtx, oldCancel := context.WithCancel(ctx)
	oldResults := make(chan shared.ConfigResult, 4)
	svc.dispatchConfig(configJob{ctx: oldCtx, command: managementCommand("old", 2), results: oldResults})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	oldCancel()
	nextResults := make(chan shared.ConfigResult, 4)
	svc.dispatchConfig(configJob{ctx: ctx, command: managementCommand("new", 2), results: nextResults})
	select {
	case result := <-nextResults:
		if result.Code != "busy" || result.SessionID != "new" {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("new session did not get bounded backpressure")
	}
	if calls.Load() != 1 || len(oldResults) != 0 {
		t.Fatal("session change replayed or published old work")
	}
}

func TestConfigWorkerRechecksExpiryBeforeInvokingProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	svc := &Service{ctx: ctx, configJobs: make(chan configJob, 1), configProvider: managementProvider(func(_ context.Context, c shared.ConfigCommand) shared.ConfigResult {
		calls.Add(1)
		return managementResult(c)
	})}
	command := managementCommand("session", 2)
	command.CollectedAt = time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano)
	command.DeadlineAtMS = time.Now().Add(-time.Second).UnixMilli()
	results := make(chan shared.ConfigResult, 1)
	svc.configBusy.Store(true)
	svc.configJobs <- configJob{ctx: ctx, command: command, results: results}
	svc.wg.Add(1)
	go svc.guarded(svc.configLoop)
	select {
	case result := <-results:
		if result.Code != "expired" {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("expired queued command stalled")
	}
	cancel()
	svc.wg.Wait()
	if calls.Load() != 0 {
		t.Fatal("worker executed command after expiry")
	}
}

func TestConfigAndProbeDownlinkShareSequenceAndAllowBoundedBurst(t *testing.T) {
	completed := make(chan error, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, http.Header{shared.CapabilitiesHeader: []string{shared.ConfigManageCapability}})
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
			hello, err := shared.DecodeFrame(data)
			if err != nil || hello.Hello == nil {
				return fmt.Errorf("invalid hello")
			}
			session := hello.Hello.SessionID
			caps := []string{"metrics.v1", "frp.v1", "ping.v1", shared.ConfigManageCapability}
			if err = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": hello.ID, "result": shared.HelloResult{Schema: 1, SessionID: session, Capabilities: caps, ReportInterval: 1}}); err != nil {
				return err
			}
			tasks := shared.PingTasks{Meta: shared.Meta{Schema: 1, SessionID: session, Sequence: 1, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Version: 1, Tasks: []shared.PingTask{}}
			if err = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "ping.tasks", "params": tasks}); err != nil {
				return err
			}
			previous := uint64(1)
			for sequence := uint64(2); sequence < 6; sequence++ {
				command := managementCommand(session, sequence)
				if err := writeManagement(c, command); err != nil {
					return err
				}
				for {
					_, data, err := c.ReadMessage()
					if err != nil {
						return err
					}
					f, err := shared.DecodeFrame(data)
					if err != nil {
						return err
					}
					var meta shared.Meta
					if f.Report != nil {
						meta = f.Report.Meta
					} else if f.ConfigResult != nil {
						meta = f.ConfigResult.Meta
					} else {
						return fmt.Errorf("unexpected uplink")
					}
					if meta.Sequence <= previous {
						return fmt.Errorf("uplink sequence replay")
					}
					previous = meta.Sequence
					if f.ConfigResult != nil {
						if f.ConfigResult.RequestID != command.RequestID || f.ConfigResult.Code != "ok" {
							return fmt.Errorf("configuration burst lost correlation")
						}
						break
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
	cfg.ProbeEnabled = true
	cfg.ConfigManagement = &shared.ConfigManagementConfig{Enabled: true, Root: t.TempDir()}
	svc, err := startWithProviders(context.Background(), cfg, nil, fixtures(t), nil, managementProvider(func(_ context.Context, c shared.ConfigCommand) shared.ConfigResult { return managementResult(c) }))
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
		t.Fatal("probe plus control burst stalled")
	}
}
