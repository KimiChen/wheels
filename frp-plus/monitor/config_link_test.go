package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

const configTestService = "00000000-0000-4000-8000-000000000001"

func dialConfig(t *testing.T, s *Service, token, session string, probe bool) *websocket.Conn {
	t.Helper()
	c, r, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if !strings.Contains(r.Header.Get(shared.CapabilitiesHeader), shared.ConfigManageCapability) {
		t.Fatal("missing upgrade advertisement")
	}
	h := fixture(t, "hello").Hello
	h.SessionID = session
	h.Capabilities = []string{"metrics.v1", shared.ConfigManageCapability}
	if probe {
		h.Capabilities = append(h.Capabilities, "ping.v1")
	}
	send(t, c, "hello", "config-hello", h)
	var answer struct {
		Result shared.HelloResult `json:"result"`
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c.ReadJSON(&answer); err != nil || !hasCapability(answer.Result.Capabilities, shared.ConfigManageCapability) {
		t.Fatal("management negotiation", answer, err)
	}
	return c
}
func readConfig(t *testing.T, c *websocket.Conn) *shared.ConfigCommand {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		f, err := shared.DecodeFrame(data)
		if err != nil {
			t.Fatal(err)
		}
		if f.ConfigCommand != nil {
			return f.ConfigCommand
		}
		if f.PingTasks == nil {
			t.Fatal("unexpected downlink", f.Method)
		}
	}
}
func configReply(command *shared.ConfigCommand, sequence uint64) shared.ConfigResult {
	return shared.ConfigResult{Meta: shared.Meta{Schema: 1, SessionID: command.SessionID, Sequence: sequence, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, RequestID: command.RequestID, ServiceID: configTestService, Action: command.Action, OperationID: command.OperationID, Code: "ok", Inventory: &shared.ConfigInventory{Revision: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), State: "ready", Objects: []shared.ConfigObjectView{}, Dependencies: []shared.ConfigDependency{}, Issues: []shared.ConfigIssue{}}}
}

type configCallResult struct {
	result shared.ConfigResult
	err    error
}

func configCall(s *Service, ctx context.Context, command shared.ConfigCommand) <-chan configCallResult {
	done := make(chan configCallResult, 1)
	go func() { r, err := s.ConfigCommand(ctx, "1", command); done <- configCallResult{r, err} }()
	return done
}
func awaitConfig(t *testing.T, done <-chan configCallResult) configCallResult {
	t.Helper()
	select {
	case v := <-done:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("configuration call stuck")
		return configCallResult{}
	}
}
func TestConfigLinkRoundTripSequencePrivacyAndBackpressure(t *testing.T) {
	s, token := testMonitor(t)
	c := dialConfig(t, s, token, "config-session", true)
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	first, err := shared.DecodeFrame(data)
	if err != nil || first.PingTasks == nil {
		t.Fatal("missing first probe tasks", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := configCall(s, ctx, shared.ConfigCommand{Action: "inspect"})
	command := readConfig(t, c)
	if command.Sequence <= first.PingTasks.Sequence || !shared.ValidConfigOperationID(command.RequestID) {
		t.Fatal("configuration did not share downlink sequence")
	}
	if _, err := s.ConfigCommand(ctx, "1", shared.ConfigCommand{Action: "inspect"}); !errors.Is(err, ErrConfigBusy) {
		t.Fatal("multiple pending commands admitted", err)
	}
	send(t, c, "config.result", "", configReply(command, 2))
	response := awaitConfig(t, done)
	if response.err != nil || response.result.ServiceID != configTestService {
		t.Fatal(response)
	}
	s.mu.Lock()
	n := s.nodes["1"]
	noFreshness := n.metricsAt.IsZero() && n.frpDetailAt.IsZero()
	s.mu.Unlock()
	if !noFreshness {
		t.Fatal("configuration result refreshed observations")
	}
	if _, err := s.ConfigCommand(ctx, "1", shared.ConfigCommand{Action: "inspect", ServiceID: "00000000-0000-4000-8000-000000000009"}); !errors.Is(err, ErrConfigServiceMismatch) {
		t.Fatal("known service mismatch admitted", err)
	}
	for _, snapshot := range []any{s.snapshot(time.Now()), s.adminSnapshot()} {
		data, _ := json.Marshal(snapshot)
		for _, private := range []string{configTestService, command.RequestID, "candidate_digest", "secret_reference", "operation_id"} {
			if strings.Contains(string(data), private) {
				t.Fatal("configuration leaked into snapshots", private)
			}
		}
	}
	// A retransmitted reply from a completed request is boundedly discarded.
	send(t, c, "config.result", "", configReply(command, 3))
	metrics := fixture(t, "report-first").Report
	metrics.SessionID = "config-session"
	metrics.Sequence = 4
	send(t, c, "report", "", metrics)
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.nodes["1"].sequence == 4 && !s.nodes["1"].metricsAt.IsZero()
	})
}
func TestConfigLinkTimeoutLateReplyAndReplacement(t *testing.T) {
	s, token := testMonitor(t)
	c := dialConfig(t, s, token, "old-config", false)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := configCall(s, ctx, shared.ConfigCommand{Action: "inspect"})
	command := readConfig(t, c)
	if got := awaitConfig(t, done); !errors.Is(got.err, context.DeadlineExceeded) || errors.Is(got.err, ErrConfigNotSent) {
		t.Fatal("no timeout", got)
	}
	send(t, c, "config.result", "", configReply(command, 2))
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].sequence == 2 })
	nextCtx, nextCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer nextCancel()
	pending := configCall(s, nextCtx, shared.ConfigCommand{Action: "inspect"})
	old := readConfig(t, c)
	replacement := dialConfig(t, s, token, "new-config", false)
	if got := awaitConfig(t, pending); !errors.Is(got.err, ErrConfigUnavailable) || errors.Is(got.err, ErrConfigNotSent) {
		t.Fatal("replacement retained old pending command", got)
	}
	fresh := configCall(s, nextCtx, shared.ConfigCommand{Action: "inspect"})
	newCommand := readConfig(t, replacement)
	if newCommand.SessionID != "new-config" || newCommand.RequestID == old.RequestID {
		t.Fatal("old command crossed session replacement")
	}
	send(t, replacement, "config.result", "", configReply(newCommand, 2))
	if got := awaitConfig(t, fresh); got.err != nil {
		t.Fatal(got.err)
	}
}
func TestConfigLinkUnnegotiatedAndMismatchedReply(t *testing.T) {
	for _, kind := range []string{"unnegotiated", "request_id", "session", "sequence"} {
		t.Run(kind, func(t *testing.T) {
			s, token := testMonitor(t)
			if kind == "unnegotiated" {
				dial(t, s, token, "legacy")
				if _, err := s.ConfigCommand(context.Background(), "1", shared.ConfigCommand{Action: "inspect"}); !errors.Is(err, ErrConfigUnavailable) || !errors.Is(err, ErrConfigNotSent) {
					t.Fatal(err)
				}
				return
			}
			c := dialConfig(t, s, token, "private", false)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := configCall(s, ctx, shared.ConfigCommand{Action: "inspect"})
			cmd := readConfig(t, c)
			reply := configReply(cmd, 2)
			switch kind {
			case "request_id":
				reply.RequestID = "00000000-0000-4000-8000-000000000099"
			case "session":
				reply.SessionID = "old-private"
			case "sequence":
				reply.Sequence = 1
			}
			send(t, c, "config.result", "", reply)
			if got := awaitConfig(t, done); got.err == nil {
				t.Fatal("invalid result satisfied pending command")
			}
		})
	}
}
func TestConfigLinkSecretIsOnlyTransientCommandData(t *testing.T) {
	s, token := testMonitor(t)
	c := dialConfig(t, s, token, "secret-session", false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	input := shared.ConfigCommand{Action: "secret", ServiceID: configTestService, Secret: &shared.ConfigSecretInput{Reference: "00000000-0000-4000-8000-000000000003", Value: "synthetic-secret-only-in-command"}}
	done := configCall(s, ctx, input)
	command := readConfig(t, c)
	s.mu.Lock()
	pending := s.nodes["1"].configLink.pending
	retained := pending != nil && (pending.expected.Secret != nil || pending.expected.Changes != nil)
	s.mu.Unlock()
	if retained {
		t.Fatal("correlation retained secret or edits")
	}
	if command.Secret == nil || command.Secret.Value != input.Secret.Value {
		t.Fatal("secret upload lost its one authorized destination")
	}
	reply := configReply(command, 2)
	reply.Inventory = nil
	reply.SecretReference = input.Secret.Reference
	send(t, c, "config.result", "", reply)
	if got := awaitConfig(t, done); got.err != nil {
		t.Fatal(got.err)
	}
	s.mu.Lock()
	link := s.nodes["1"].configLink
	retained = link.pending != nil || len(link.commands) != 0
	s.mu.Unlock()
	if retained {
		t.Fatal("completed upload retained command")
	}
	for _, snapshot := range []any{s.snapshot(time.Now()), s.adminSnapshot()} {
		data, _ := json.Marshal(snapshot)
		if strings.Contains(string(data), input.Secret.Value) || strings.Contains(string(data), input.Secret.Reference) {
			t.Fatal("secret leaked into snapshot")
		}
	}
}

func TestConfigLinkRejectsUnboundSecretReference(t *testing.T) {
	s, token := testMonitor(t)
	c := dialConfig(t, s, token, "secret-reference", false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	command := shared.ConfigCommand{Action: "secret", ServiceID: configTestService, Secret: &shared.ConfigSecretInput{Reference: "00000000-0000-4000-8000-000000000003", Value: "synthetic-secret"}}
	done := configCall(s, ctx, command)
	sent := readConfig(t, c)
	reply := configReply(sent, 2)
	reply.Inventory = nil
	reply.SecretReference = "00000000-0000-4000-8000-000000000004"
	send(t, c, "config.result", "", reply)
	if got := awaitConfig(t, done); got.err == nil {
		t.Fatal("reply substituted an unrelated local secret reference")
	}
}
