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

func dialRestore(t *testing.T, s *Service, token, session string, management, probe bool) *websocket.Conn {
	t.Helper()
	c, r, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if !strings.Contains(r.Header.Get(shared.CapabilitiesHeader), shared.ConfigRestoreCapability) {
		t.Fatal("restore advertisement absent")
	}
	h := fixture(t, "hello").Hello
	h.SessionID = session
	h.Capabilities = []string{"metrics.v1", shared.ConfigRestoreCapability}
	if management {
		h.Capabilities = append(h.Capabilities, shared.ConfigManageCapability)
	}
	if probe {
		h.Capabilities = append(h.Capabilities, "ping.v1")
	}
	send(t, c, "hello", "restore-hello", h)
	var answer struct {
		Result shared.HelloResult `json:"result"`
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c.ReadJSON(&answer); err != nil || !hasCapability(answer.Result.Capabilities, shared.ConfigRestoreCapability) {
		t.Fatal("restore negotiation", err)
	}
	return c
}
func readRestore(t *testing.T, c *websocket.Conn) *shared.RestoreCommand {
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
		if f.RestoreCommand != nil {
			return f.RestoreCommand
		}
		if f.PingTasks == nil {
			t.Fatal("unexpected downlink", f.Method)
		}
	}
}
func restoreLinkReply(c *shared.RestoreCommand, sequence uint64) shared.RestoreResult {
	return shared.RestoreResult{Meta: shared.Meta{Schema: 1, SessionID: c.SessionID, Sequence: sequence, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, RequestID: c.RequestID, ServiceID: configTestService, Action: c.Action, Code: "ok", Restore: &shared.RestoreInfo{State: "none"}}
}

type restoreCallResult struct {
	result shared.RestoreResult
	err    error
}

func restoreCall(s *Service, ctx context.Context, c shared.RestoreCommand) <-chan restoreCallResult {
	done := make(chan restoreCallResult, 1)
	go func() { r, err := s.RestoreCommand(ctx, "1", c); done <- restoreCallResult{r, err} }()
	return done
}
func awaitRestore(t *testing.T, done <-chan restoreCallResult) restoreCallResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("restore call stalled")
		return restoreCallResult{}
	}
}

func TestRestoreLinkSharesSequenceGateRateAndPrivateState(t *testing.T) {
	s, token := testMonitor(t)
	c := dialRestore(t, s, token, "restore-session", true, true)
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	first, err := shared.DecodeFrame(data)
	if err != nil || first.PingTasks == nil {
		t.Fatal("no initial tasks", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := restoreCall(s, ctx, shared.RestoreCommand{Action: "inspect"})
	command := readRestore(t, c)
	if command.Sequence <= first.PingTasks.Sequence {
		t.Fatal("restore ignored shared downlink sequence")
	}
	if _, err := s.ConfigCommand(ctx, "1", shared.ConfigCommand{Action: "inspect"}); !errors.Is(err, ErrConfigBusy) || !errors.Is(err, ErrConfigNotSent) {
		t.Fatal("configuration bypassed pending restore", err)
	}
	send(t, c, "config.restore.result", "", restoreLinkReply(command, 2))
	if r := awaitRestore(t, done); r.err != nil {
		t.Fatal(r.err)
	}
	configDone := configCall(s, ctx, shared.ConfigCommand{Action: "inspect"})
	config := readConfig(t, c)
	if config.Sequence <= command.Sequence {
		t.Fatal("configuration reset sequence")
	}
	if _, err := s.RestoreCommand(ctx, "1", shared.RestoreCommand{Action: "inspect"}); !errors.Is(err, ErrConfigBusy) || !errors.Is(err, ErrConfigNotSent) {
		t.Fatal("restore bypassed pending configuration", err)
	}
	send(t, c, "config.result", "", configReply(config, 3))
	if r := awaitConfig(t, configDone); r.err != nil {
		t.Fatal(r.err)
	}
	// Two more restore calls exhaust the same four-token budget used by config.
	for seq := uint64(4); seq <= 5; seq++ {
		d := restoreCall(s, ctx, shared.RestoreCommand{Action: "inspect"})
		cmd := readRestore(t, c)
		send(t, c, "config.restore.result", "", restoreLinkReply(cmd, seq))
		if r := awaitRestore(t, d); r.err != nil {
			t.Fatal(r.err)
		}
	}
	if _, err := s.ConfigCommand(ctx, "1", shared.ConfigCommand{Action: "inspect"}); !errors.Is(err, ErrConfigBusy) {
		t.Fatal("separate capability rate budgets", err)
	}
	s.mu.Lock()
	n := s.nodes["1"]
	noFreshness := n.metricsAt.IsZero() && n.frpDetailAt.IsZero()
	s.mu.Unlock()
	if !noFreshness {
		t.Fatal("restore refreshed observation facts")
	}
	for _, snapshot := range []any{s.snapshot(time.Now()), s.adminSnapshot()} {
		data, _ := json.Marshal(snapshot)
		for _, private := range []string{configTestService, command.RequestID, "manifest_digest", "acknowledgement_id", "backup_service_id"} {
			if strings.Contains(string(data), private) {
				t.Fatal("restore leaked into snapshot", private)
			}
		}
	}
	if _, err := s.RestoreCommand(ctx, "1", shared.RestoreCommand{Action: "inspect", ServiceID: "00000000-0000-4000-8000-000000000099"}); !errors.Is(err, ErrConfigServiceMismatch) {
		t.Fatal("known identity drift accepted", err)
	}
	// A duplicate reply is ignored within its own bounded method namespace.
	send(t, c, "config.restore.result", "", restoreLinkReply(command, 6))
	report := fixture(t, "report-first").Report
	report.SessionID = "restore-session"
	report.Sequence = 7
	send(t, c, "report", "", report)
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.nodes["1"].sequence == 7 && !s.nodes["1"].metricsAt.IsZero()
	})
}
func TestRestoreLinkCapabilityIsolation(t *testing.T) {
	for _, kind := range []string{"legacy", "config-only", "restore-only"} {
		t.Run(kind, func(t *testing.T) {
			s, token := testMonitor(t)
			switch kind {
			case "legacy":
				dial(t, s, token, "legacy")
			case "config-only":
				dialConfig(t, s, token, "config", false)
			case "restore-only":
				dialRestore(t, s, token, "restore", false, false)
			}
			var err error
			if kind == "restore-only" {
				_, err = s.ConfigCommand(context.Background(), "1", shared.ConfigCommand{Action: "inspect"})
			} else {
				_, err = s.RestoreCommand(context.Background(), "1", shared.RestoreCommand{Action: "inspect"})
			}
			if !errors.Is(err, ErrConfigUnavailable) || !errors.Is(err, ErrConfigNotSent) {
				t.Fatal("unnegotiated command admitted", err)
			}
		})
	}
}
func TestRestoreLinkTimeoutReplacementAndMismatchedResult(t *testing.T) {
	s, token := testMonitor(t)
	c := dialRestore(t, s, token, "old-restore", true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	done := restoreCall(s, ctx, shared.RestoreCommand{Action: "inspect"})
	old := readRestore(t, c)
	if r := awaitRestore(t, done); !errors.Is(r.err, context.DeadlineExceeded) || errors.Is(r.err, ErrConfigNotSent) {
		t.Fatal("uncertain timeout marked not-sent", r.err)
	}
	send(t, c, "config.restore.result", "", restoreLinkReply(old, 2))
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["1"].sequence == 2 })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	pending := restoreCall(s, ctx2, shared.RestoreCommand{Action: "inspect"})
	old = readRestore(t, c)
	replacement := dialRestore(t, s, token, "new-restore", true, false)
	if r := awaitRestore(t, pending); !errors.Is(r.err, ErrConfigUnavailable) || errors.Is(r.err, ErrConfigNotSent) {
		t.Fatal("replacement retained pending operation", r.err)
	}
	fresh := restoreCall(s, ctx2, shared.RestoreCommand{Action: "inspect"})
	cmd := readRestore(t, replacement)
	if cmd.SessionID == old.SessionID || cmd.RequestID == old.RequestID {
		t.Fatal("old request reused")
	}
	reply := restoreLinkReply(cmd, 2)
	reply.RequestID = old.RequestID
	send(t, replacement, "config.restore.result", "", reply)
	if r := awaitRestore(t, fresh); r.err == nil {
		t.Fatal("old-session request satisfied new invocation")
	}
}
func TestRestoreLinkRejectsDifferentAcknowledgement(t *testing.T) {
	s, token := testMonitor(t)
	c := dialRestore(t, s, token, "ack-session", true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	input := shared.RestoreCommand{Action: "acknowledge", ServiceID: configTestService, Epoch: configTestService, ManifestDigest: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), StoreDigest: strings.Repeat("c", 64), AcknowledgementID: configTestService}
	done := restoreCall(s, ctx, input)
	cmd := readRestore(t, c)
	reply := restoreLinkReply(cmd, 2)
	reply.Restore = &shared.RestoreInfo{State: "acknowledged", Epoch: input.Epoch, BackupServiceID: configTestService, ManifestDigest: input.ManifestDigest, ContextRevision: input.ContextRevision, StoreDigest: input.StoreDigest, AcknowledgementID: "00000000-0000-4000-8000-000000000099", RuntimeLoaded: true, ResourcesReady: true}
	send(t, c, "config.restore.result", "", reply)
	if r := awaitRestore(t, done); r.err == nil {
		t.Fatal("another acknowledgement accepted")
	}
}

func TestRestoreLinkSuccessfulAcknowledgementAndConflictingFacts(t *testing.T) {
	s, token := testMonitor(t)
	c := dialRestore(t, s, token, "confirmed-session", true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	input := shared.RestoreCommand{Action: "acknowledge", ServiceID: configTestService, Epoch: configTestService, ManifestDigest: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), StoreDigest: strings.Repeat("c", 64), AcknowledgementID: configTestService}
	for sequence, code := range []string{"ok", "conflict"} {
		done := restoreCall(s, ctx, input)
		cmd := readRestore(t, c)
		reply := restoreLinkReply(cmd, uint64(sequence+2))
		reply.Code = code
		reply.Restore = &shared.RestoreInfo{State: "confirmed", Epoch: input.Epoch, BackupServiceID: configTestService, ManifestDigest: input.ManifestDigest, ContextRevision: input.ContextRevision, StoreDigest: input.StoreDigest, AcknowledgementID: input.AcknowledgementID, RuntimeLoaded: true, ResourcesReady: true, OperationsCount: 3}
		if code == "conflict" {
			reply.Restore.Epoch = "00000000-0000-4000-8000-000000000099"
		}
		send(t, c, "config.restore.result", "", reply)
		got := awaitRestore(t, done)
		if got.err != nil || got.result.Code != code || got.result.Restore == nil || *got.result.Restore != *reply.Restore {
			t.Fatal("restore result facts lost", got.err)
		}
	}
}
