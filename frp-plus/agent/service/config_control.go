package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type configJob struct {
	ctx            context.Context
	command        shared.ConfigCommand
	results        chan shared.ConfigResult
	restore        *shared.RestoreCommand
	restoreResults chan shared.RestoreResult
}

func (s *Service) configEnabled() bool {
	return s.cfg.ConfigManagement != nil && s.cfg.ConfigManagement.Enabled && s.configProvider != nil
}
func (s *Service) configError(command shared.ConfigCommand, code string) (shared.ConfigResult, bool) {
	service := command.ServiceID
	if service == "" {
		s.mu.Lock()
		service = s.configServiceID
		s.mu.Unlock()
	}
	if !shared.ValidConfigOperationID(service) {
		return shared.ConfigResult{}, false
	}
	return shared.ConfigResult{Meta: command.Meta, RequestID: command.RequestID, ServiceID: service, Action: command.Action, OperationID: command.OperationID, Code: code}, true
}
func offerConfigResult(ctx context.Context, results chan shared.ConfigResult, result shared.ConfigResult) {
	if ctx.Err() != nil {
		return
	}
	select {
	case results <- result:
	default:
	}
}
func (s *Service) dispatchConfig(job configJob) {
	if job.command.ValidateAt(time.Now()) != nil {
		if result, ok := s.configError(job.command, "expired"); ok {
			offerConfigResult(job.ctx, job.results, result)
		}
		return
	}
	if !s.configBusy.CompareAndSwap(false, true) {
		if result, ok := s.configError(job.command, "busy"); ok {
			offerConfigResult(job.ctx, job.results, result)
		}
		return
	}
	select {
	case s.configJobs <- job:
	default:
		s.configBusy.Store(false)
		if result, ok := s.configError(job.command, "busy"); ok {
			offerConfigResult(job.ctx, job.results, result)
		}
	}
}

// One worker and at most one outstanding provider call exist for the entire
// Service, including reconnects. A provider ignoring cancellation keeps the
// gate closed but never stalls telemetry or creates an unbounded goroutine set.
func (s *Service) configLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.configJobs:
			if job.restore != nil {
				s.runRestoreJob(job)
				continue
			}
			if job.command.ValidateAt(time.Now()) != nil {
				if fallback, ok := s.configError(job.command, "expired"); ok {
					offerConfigResult(job.ctx, job.results, fallback)
				}
				s.configBusy.Store(false)
				continue
			}
			if job.ctx.Err() != nil {
				s.configBusy.Store(false)
				continue
			}
			ctx, cancel := context.WithDeadline(job.ctx, time.UnixMilli(job.command.DeadlineAtMS))
			finished := make(chan shared.ConfigResult, 1)
			go func() {
				result := shared.ConfigResult{}
				func() {
					defer func() {
						if recover() != nil {
							result = shared.ConfigResult{}
						}
					}()
					result = s.configProvider.HandleConfig(ctx, job.command)
				}()
				finished <- result
			}()
			var result shared.ConfigResult
			timedOut := false
			select {
			case result = <-finished:
			case <-ctx.Done():
				timedOut = true
				if fallback, ok := s.configError(job.command, "timeout"); ok {
					offerConfigResult(job.ctx, job.results, fallback)
				}
			}
			cancel()
			if timedOut {
				// A late provider result cannot be attached to a new connection/session.
				select {
				case <-finished:
				case <-s.ctx.Done():
					return
				}
			} else {
				result.Meta = job.command.Meta
				valid := result.RequestID == job.command.RequestID && result.Action == job.command.Action && result.OperationID == job.command.OperationID && (job.command.ServiceID == "" || result.ServiceID == job.command.ServiceID || (result.Code == "service_mismatch" && result.Operation == nil)) && result.Validate() == nil
				if result.Operation != nil && (result.Operation.BaseRevision != job.command.BaseRevision || (job.command.ContextRevision != "" && result.Operation.ContextRevision != job.command.ContextRevision) || (job.command.CandidateDigest != "" && result.Operation.CandidateDigest != job.command.CandidateDigest)) {
					valid = false
				}
				if job.command.Action == "secret" && result.Code == "ok" && result.SecretReference != job.command.Secret.Reference {
					valid = false
				}
				if valid {
					// Detach any slices owned by a native provider before publishing.
					data, err := json.Marshal(result)
					var detached shared.ConfigResult
					if err == nil && json.Unmarshal(data, &detached) == nil {
						s.mu.Lock()
						s.configServiceID = detached.ServiceID
						s.mu.Unlock()
						offerConfigResult(job.ctx, job.results, detached)
					}
				} else if fallback, ok := s.configError(job.command, "internal_error"); ok {
					offerConfigResult(job.ctx, job.results, fallback)
				}
			}
			s.configBusy.Store(false)
		}
	}
}
