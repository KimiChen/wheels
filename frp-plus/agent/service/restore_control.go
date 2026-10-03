package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func (s *Service) restoreEnabled() bool {
	return s.cfg.ConfigManagement != nil && s.cfg.ConfigManagement.Enabled && s.restoreProvider != nil
}
func (s *Service) restoreError(c shared.RestoreCommand, code string) (shared.RestoreResult, bool) {
	service := c.ServiceID
	if service == "" {
		s.mu.Lock()
		service = s.configServiceID
		s.mu.Unlock()
	}
	if !shared.ValidConfigOperationID(service) {
		return shared.RestoreResult{}, false
	}
	return shared.RestoreResult{Meta: c.Meta, RequestID: c.RequestID, ServiceID: service, Action: c.Action, Code: code}, true
}
func offerRestoreResult(ctx context.Context, results chan shared.RestoreResult, r shared.RestoreResult) {
	if ctx.Err() != nil {
		return
	}
	select {
	case results <- r:
	default:
	}
}
func (s *Service) restoreFallback(job configJob, code string) {
	if result, ok := s.restoreError(*job.restore, code); ok {
		offerRestoreResult(job.ctx, job.restoreResults, result)
	}
}
func (s *Service) dispatchRestore(job configJob) {
	if job.restore.ValidateAt(time.Now()) != nil {
		s.restoreFallback(job, "expired")
		return
	}
	if !s.configBusy.CompareAndSwap(false, true) {
		s.restoreFallback(job, "busy")
		return
	}
	select {
	case s.configJobs <- job:
	default:
		s.configBusy.Store(false)
		s.restoreFallback(job, "busy")
	}
}

// Called by the sole configuration worker, retaining its gate across reconnects
// and cancellation until a provider that ignored cancellation finally returns.
func (s *Service) runRestoreJob(job configJob) {
	defer s.configBusy.Store(false)
	c := *job.restore
	if c.ValidateAt(time.Now()) != nil {
		s.restoreFallback(job, "expired")
		return
	}
	if job.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithDeadline(job.ctx, time.UnixMilli(c.DeadlineAtMS))
	defer cancel()
	finished := make(chan shared.RestoreResult, 1)
	go func() {
		r := shared.RestoreResult{}
		func() {
			defer func() {
				if recover() != nil {
					r = shared.RestoreResult{}
				}
			}()
			r = s.restoreProvider.HandleRestore(ctx, c)
		}()
		finished <- r
	}()
	select {
	case r := <-finished:
		r.Meta = c.Meta
		if !shared.MatchesRestoreResult(c, r) {
			s.restoreFallback(job, "internal_error")
			return
		}
		data, err := json.Marshal(r)
		var detached shared.RestoreResult
		if err != nil || json.Unmarshal(data, &detached) != nil {
			s.restoreFallback(job, "internal_error")
			return
		}
		s.mu.Lock()
		s.configServiceID = detached.ServiceID
		s.mu.Unlock()
		offerRestoreResult(job.ctx, job.restoreResults, detached)
	case <-ctx.Done():
		s.restoreFallback(job, "timeout")
		select {
		case <-finished:
		case <-s.ctx.Done():
		}
	}
}
