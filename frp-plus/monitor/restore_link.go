package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
	"time"
)

type restorePending struct {
	expected shared.RestoreCommand
	result   chan shared.RestoreResult
}

// RestoreCommand is an authenticated-admin-only transport. It shares admission,
// rate budgets and current service identity with ordinary configuration commands.
// Timeout or disconnect never proves that acknowledgement did not happen.
func (s *Service) RestoreCommand(ctx context.Context, nodeID string, input shared.RestoreCommand) (shared.RestoreResult, error) {
	queued := false
	fail := func(err error) (shared.RestoreResult, error) {
		if !queued {
			err = fmt.Errorf("%w: %w", ErrConfigNotSent, err)
		}
		return shared.RestoreResult{}, err
	}
	if !validNodeID(nodeID) {
		return fail(ErrConfigInvalid)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	s.mu.Lock()
	n := s.nodes[nodeID]
	if n == nil || n.conn == nil || n.configLink == nil || !n.configLink.restoreEnabled || s.credentialError.Load() {
		s.mu.Unlock()
		return fail(ErrConfigUnavailable)
	}
	link, conn, session := n.configLink, n.conn, n.sessionID
	s.mu.Unlock()
	now := time.Now().UTC()
	deadline := now.Add(shared.MaxRestoreCommandDuration)
	if at, ok := ctx.Deadline(); ok && at.Before(deadline) {
		deadline = at
	}
	id, err := shared.NewConfigOperationID()
	if err != nil {
		return fail(ErrConfigUnavailable)
	}
	input.Meta = shared.Meta{Schema: shared.SchemaVersion, SessionID: session, Sequence: 2, CollectedAt: now.Format(time.RFC3339Nano)}
	input.RequestID, input.DeadlineAtMS = id, deadline.UnixMilli()
	if input.ValidateAt(now) != nil {
		return fail(ErrConfigInvalid)
	}
	// Detach the validated request before enqueueing.
	data, err := json.Marshal(input)
	if err != nil {
		return fail(ErrConfigInvalid)
	}
	var command shared.RestoreCommand
	if json.Unmarshal(data, &command) != nil {
		return fail(ErrConfigInvalid)
	}
	expected := command
	pending := &restorePending{expected: expected, result: make(chan shared.RestoreResult, 1)}
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
	link.restorePending = pending
	select {
	case link.commands <- configOutgoing{restore: &command}:
		link.tokens--
		queued = true
	default:
		link.restorePending = nil
		s.mu.Unlock()
		return fail(ErrConfigBusy)
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if link.restorePending == pending {
			link.restorePending = nil
		}
		link.remember("restore:" + id)
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

func (s *Service) sendRestoreCommand(nodeID string, conn *websocket.Conn, link *configLink, command shared.RestoreCommand, sequence *uint64) bool {
	s.mu.Lock()
	n := s.nodes[nodeID]
	current := n != nil && n.conn == conn && n.configLink == link && n.sessionID == command.SessionID && link.restorePending != nil && link.restorePending.expected.RequestID == command.RequestID
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
		JSONRPC string                `json:"jsonrpc"`
		Method  string                `json:"method"`
		Params  shared.RestoreCommand `json:"params"`
	}{"2.0", "config.restore.command", command}) == nil
}
func (s *Service) acceptRestoreResult(nodeID string, conn *websocket.Conn, received time.Time, result shared.RestoreResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[nodeID]
	if n == nil || n.conn != conn || n.configLink == nil || !n.configLink.restoreEnabled || n.sessionID != result.SessionID || result.Sequence <= n.sequence {
		return false
	}
	link := n.configLink
	pending := link.restorePending
	if pending == nil || pending.expected.RequestID != result.RequestID {
		for _, id := range link.recent {
			if id == "restore:"+result.RequestID {
				n.sequence = result.Sequence
				n.lastSeen = received
				return true
			}
		}
		return false
	}
	if !shared.MatchesRestoreResult(pending.expected, result) {
		return false
	}
	if result.Code == "ok" && result.Action == "inspect" {
		link.serviceID = result.ServiceID
	}
	n.sequence = result.Sequence
	n.lastSeen = received
	link.restorePending = nil
	link.remember("restore:" + result.RequestID)
	select {
	case pending.result <- result:
	default:
		return false
	}
	return true
}
