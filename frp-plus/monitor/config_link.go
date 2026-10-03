package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

var (
	ErrConfigUnavailable     = errors.New("configuration channel unavailable")
	ErrConfigBusy            = errors.New("configuration channel busy")
	ErrConfigInvalid         = errors.New("invalid configuration command")
	ErrConfigServiceMismatch = errors.New("configuration service changed")
	// ErrConfigNotSent proves this invocation never entered the outgoing queue.
	// It is combined with the ordinary cause; a post-queue failure never has it.
	ErrConfigNotSent = errors.New("configuration command not dispatched")
)

type configPending struct {
	expected        shared.ConfigCommand // Changes and Secret are always nil here.
	result          chan shared.ConfigResult
	secretReference string
}

type configOutgoing struct {
	config  *shared.ConfigCommand
	restore *shared.RestoreCommand
}

// Mutable fields are protected by Service.mu; socket writes have one owner.
// A replaced connection owns a distinct link, queue and response namespace.
type configLink struct {
	commands       chan configOutgoing
	restorePending *restorePending
	configEnabled  bool
	restoreEnabled bool
	closed         chan struct{}
	closeOnce      sync.Once
	pending        *configPending
	recent         []string
	serviceID      string
	tokens         float64
	replenished    time.Time
}

func newConfigLink() *configLink {
	return &configLink{commands: make(chan configOutgoing, 1), configEnabled: true, closed: make(chan struct{}), tokens: 4, replenished: time.Now()}
}
func (l *configLink) stop() {
	if l != nil {
		l.closeOnce.Do(func() { close(l.closed) })
	}
}
func (l *configLink) remember(id string) {
	for _, old := range l.recent {
		if old == id {
			return
		}
	}
	if len(l.recent) == 16 {
		copy(l.recent, l.recent[1:])
		l.recent = l.recent[:15]
	}
	l.recent = append(l.recent, id)
}

// ConfigCommand is an internal authenticated-admin transport, not an HTTP
// endpoint. The admin layer must authorize the action before calling it. A
// timeout/disconnect means no result was observed; it never proves no write.
func (s *Service) ConfigCommand(ctx context.Context, nodeID string, input shared.ConfigCommand) (shared.ConfigResult, error) {
	queued := false
	fail := func(err error) (shared.ConfigResult, error) {
		if !queued {
			err = fmt.Errorf("%w: %w", ErrConfigNotSent, err)
		}
		return shared.ConfigResult{}, err
	}
	if !validNodeID(nodeID) {
		return fail(ErrConfigInvalid)
	}
	if serverRestoreActionBlocked(s.serverRestorePending, input.Action) {
		return fail(ErrServerRestorePending)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	s.mu.Lock()
	n := s.nodes[nodeID]
	if n == nil || n.conn == nil || n.configLink == nil || !n.configLink.configEnabled || s.credentialError.Load() {
		s.mu.Unlock()
		return fail(ErrConfigUnavailable)
	}
	link, conn, session := n.configLink, n.conn, n.sessionID
	s.mu.Unlock()
	now := time.Now().UTC()
	deadline := now.Add(shared.MaxConfigCommandDuration)
	if at, ok := ctx.Deadline(); ok && at.Before(deadline) {
		deadline = at
	}
	id, err := shared.NewConfigOperationID()
	if err != nil {
		return fail(ErrConfigUnavailable)
	}
	input.Meta = shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: 2, CollectedAt: now.Format(time.RFC3339Nano)}
	input.RequestID, input.DeadlineAtMS = id, deadline.UnixMilli()
	if input.Changes == nil {
		input.Changes = []shared.ConfigChange{}
	}
	if input.ValidateAt(now) != nil {
		return fail(ErrConfigInvalid)
	}
	// Detach caller-owned raw values and the transient secret before enqueueing.
	data, err := json.Marshal(input)
	if err != nil {
		return fail(ErrConfigInvalid)
	}
	var command shared.ConfigCommand
	if json.Unmarshal(data, &command) != nil {
		return fail(ErrConfigInvalid)
	}
	expected := command
	expected.Changes, expected.Secret = nil, nil
	pending := &configPending{expected: expected, result: make(chan shared.ConfigResult, 1)}
	if command.Secret != nil {
		pending.secretReference = command.Secret.Reference
	}
	s.mu.Lock()
	if s.nodes[nodeID] != n || n.conn != conn || n.configLink != link || n.sessionID != session || s.credentialError.Load() {
		s.mu.Unlock()
		return fail(ErrConfigUnavailable)
	}
	select {
	case <-link.closed:
		s.mu.Unlock()
		return fail(ErrConfigUnavailable)
	default:
	}
	if link.serviceID != "" && command.ServiceID != "" && command.ServiceID != link.serviceID {
		s.mu.Unlock()
		return fail(ErrConfigServiceMismatch)
	}
	if link.pending != nil || link.restorePending != nil {
		s.mu.Unlock()
		return fail(ErrConfigBusy)
	}
	acceptedAt := time.Now()
	link.tokens = min(4, link.tokens+acceptedAt.Sub(link.replenished).Seconds())
	link.replenished = acceptedAt
	if link.tokens < 1 {
		s.mu.Unlock()
		return fail(ErrConfigBusy)
	}
	link.pending = pending
	select {
	case link.commands <- configOutgoing{config: &command}:
		link.tokens--
		queued = true
	default:
		link.pending = nil
		s.mu.Unlock()
		return fail(ErrConfigBusy)
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if link.pending == pending {
			link.pending = nil
		}
		link.remember("config:" + id)
		s.mu.Unlock()
	}()
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	select {
	case result := <-pending.result:
		return result, nil
	case <-waitCtx.Done():
		return fail(waitCtx.Err())
	case <-link.closed:
		return fail(ErrConfigUnavailable)
	case <-s.ctx.Done():
		return fail(ErrConfigUnavailable)
	}
}

func (s *Service) sendConfigCommand(nodeID string, conn *websocket.Conn, link *configLink, command shared.ConfigCommand, sequence *uint64) bool {
	s.mu.Lock()
	n := s.nodes[nodeID]
	current := n != nil && n.conn == conn && n.configLink == link && n.sessionID == command.SessionID && link.pending != nil && link.pending.expected.RequestID == command.RequestID
	s.mu.Unlock()
	if !current || command.ValidateAt(time.Now()) != nil {
		return true
	}
	if *sequence < 1 {
		*sequence = 1
	}
	if *sequence == ^uint64(0) {
		return false
	}
	*sequence++
	command.Sequence = *sequence
	_ = conn.SetWriteDeadline(minTime(time.Now().Add(5*time.Second), time.UnixMilli(command.DeadlineAtMS)))
	return conn.WriteJSON(struct {
		JSONRPC string               `json:"jsonrpc"`
		Method  string               `json:"method"`
		Params  shared.ConfigCommand `json:"params"`
	}{"2.0", "config.command", command}) == nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Service) acceptConfigResult(nodeID string, conn *websocket.Conn, received time.Time, result shared.ConfigResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[nodeID]
	if n == nil || n.conn != conn || n.configLink == nil || !n.configLink.configEnabled || n.sessionID != result.SessionID || result.Sequence <= n.sequence {
		return false
	}
	link := n.configLink
	pending := link.pending
	if pending == nil || pending.expected.RequestID != result.RequestID {
		for _, id := range link.recent {
			if id == "config:"+result.RequestID {
				n.sequence = result.Sequence
				n.lastSeen = received
				return true
			}
		}
		return false
	}
	expected := pending.expected
	if result.Action != expected.Action || result.OperationID != expected.OperationID {
		return false
	}
	if expected.ServiceID != "" && result.ServiceID != expected.ServiceID && !(result.Code == "service_mismatch" && result.Operation == nil) {
		return false
	}
	if result.Operation != nil && (result.Operation.BaseRevision != expected.BaseRevision || (expected.ContextRevision != "" && result.Operation.ContextRevision != expected.ContextRevision) || (expected.CandidateDigest != "" && result.Operation.CandidateDigest != expected.CandidateDigest)) {
		return false
	}
	if result.Code == "ok" && result.Action == "secret" && result.SecretReference != pending.secretReference {
		return false
	}
	if result.Code == "ok" && result.Action == "inspect" {
		link.serviceID = result.ServiceID
	}
	n.sequence = result.Sequence
	n.lastSeen = received
	link.pending = nil
	link.remember("config:" + result.RequestID)
	select {
	case pending.result <- result:
	default:
		return false
	}
	return true
}

// The management capability is only an upgrade advertisement; HTTP admin
// authentication, local opt-in and current session ownership remain mandatory.
func configCapabilitiesHeader() http.Header {
	return http.Header{shared.CapabilitiesHeader: []string{shared.TunnelCapability + ", " + shared.FRPDetailCapability + ", " + shared.ConfigManageCapability + ", " + shared.ConfigRestoreCapability}}
}
