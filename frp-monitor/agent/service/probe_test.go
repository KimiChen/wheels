package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

func TestProbeNegotiationAndSharedReportSequence(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	completed := make(chan error, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(4 * time.Second))
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
		ping := false
		for _, value := range hello.Hello.Capabilities {
			ping = ping || value == "ping.v1"
		}
		if !ping {
			completed <- fmt.Errorf("enabled probe did not advertise ping.v1")
			return
		}
		session := hello.Hello.SessionID
		if err = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": hello.ID, "result": shared.HelloResult{Schema: 1, SessionID: session, Capabilities: []string{"metrics.v1", "ping.v1"}, ReportInterval: 1}}); err != nil {
			completed <- err
			return
		}
		list := shared.PingTasks{Meta: shared.Meta{Schema: 1, SessionID: session, Sequence: 1, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Version: 1, Tasks: []shared.PingTask{}}
		for i := 0; i < 4; i++ {
			list.Tasks = append(list.Tasks, shared.PingTask{ID: fmt.Sprintf("probe-%d", i), Target: listener.Addr().String(), Interval: 5})
		}
		if err = writeFrame(c, "ping.tasks", "", list); err != nil {
			completed <- err
			return
		}
		sequence := uint64(1)
		probes, reports := 0, 0
		seen := map[string]bool{}
		for probes < 4 || reports < 2 {
			_, data, err = c.ReadMessage()
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
			if frame.Report != nil {
				reports++
				meta = frame.Report.Meta
			} else if frame.PingResult != nil {
				result := frame.PingResult
				if result.TaskVersion != 1 || result.LatencyMS < 0 || seen[result.TaskID] {
					completed <- fmt.Errorf("invalid probe result: %+v", result)
					return
				}
				seen[result.TaskID] = true
				probes++
				meta = result.Meta
			} else {
				completed <- fmt.Errorf("unexpected frame %q", frame.Method)
				return
			}
			if meta.Sequence != sequence+1 || meta.SessionID != session {
				completed <- fmt.Errorf("sequence/session not shared: %+v after %d", meta, sequence)
				return
			}
			sequence = meta.Sequence
		}
		completed <- nil
		for {
			if _, _, err = c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	cfg := testConfig(t)
	cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/ws"
	cfg.ProbeEnabled, cfg.ProbeAllowPrivate = true, true
	s, err := start(context.Background(), cfg, nil, fixtures(t))
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
		t.Fatal("probe/report stream incomplete")
	}
}

func TestInvalidOrUnnegotiatedProbeTasksCloseSession(t *testing.T) {
	for _, name := range []string{"unnegotiated", "unrequested-capability", "wrong-session", "duplicate-sequence", "duplicate-version", "wrong-method", "task-flood"} {
		t.Run(name, func(t *testing.T) {
			closed := make(chan error, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, data, err := c.ReadMessage()
				if err != nil {
					return
				}
				hello, err := shared.DecodeFrame(data)
				if err != nil || hello.Hello == nil {
					return
				}
				caps := []string{"metrics.v1"}
				if name != "unnegotiated" {
					caps = append(caps, "ping.v1")
				}
				if name == "unrequested-capability" {
					for _, capability := range hello.Hello.Capabilities {
						if capability == "ping.v1" {
							closed <- fmt.Errorf("disabled probe advertised ping.v1")
							return
						}
					}
				}
				_ = c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": hello.ID, "result": shared.HelloResult{Schema: 1, SessionID: hello.Hello.SessionID, Capabilities: caps, ReportInterval: 1}})
				list := shared.PingTasks{Meta: shared.Meta{Schema: 1, SessionID: hello.Hello.SessionID, Sequence: 1, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Version: 1, Tasks: []shared.PingTask{}}
				if name != "unrequested-capability" {
					_ = writeFrame(c, "ping.tasks", "", list)
					list.Sequence, list.Version = 2, 2
					switch name {
					case "wrong-session":
						list.SessionID = "other-session"
					case "duplicate-sequence":
						list.Sequence = 1
					case "duplicate-version":
						list.Version = 1
					}
					method := "ping.tasks"
					if name == "wrong-method" {
						method = "report"
					}
					_ = writeFrame(c, method, "", list)
					if name == "task-flood" {
						for i := uint64(3); i <= 8; i++ {
							list.Version, list.Sequence = i, i
							_ = writeFrame(c, "ping.tasks", "", list)
						}
					}
				}
				for {
					_, _, err = c.ReadMessage()
					if err != nil {
						if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
							closed <- fmt.Errorf("invalid task did not close session")
							return
						}
						closed <- nil
						return
					}
				}
			}))
			defer server.Close()
			cfg := testConfig(t)
			cfg.Endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent/v1/ws"
			cfg.ProbeEnabled = name != "unrequested-capability"
			s, err := start(context.Background(), cfg, nil, fixtures(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("invalid session remained open")
			}
		})
	}
}
