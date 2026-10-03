package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// Each source holds at most the native frame limit. There is one low-priority
// worker, not one goroutine per node/report. A busy disk cannot block telemetry.
type tunnelObserver struct {
	mu        sync.RWMutex
	collector string
	sources   map[string]*tunnelSourceState
	live      map[string]tunnelLive
	offset    int
	status    string
}
type tunnelSourceState struct {
	complete     bool
	objectOffset int
	hash         string
	mappings     map[string]control.TunnelMapping
	seen         time.Time
	epoch        string
}
type tunnelLive struct {
	object shared.TunnelObject
	at     time.Time
	source string
}
type tunnelInput struct {
	key, node, token, revision, session string
	at                                  time.Time
	snapshot                            *shared.TunnelSnapshot
}

func newTunnelObserver() *tunnelObserver {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return &tunnelObserver{status: "degraded"}
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	return &tunnelObserver{collector: id, sources: map[string]*tunnelSourceState{}, live: map[string]tunnelLive{}, status: "ready"}
}
func (s *Service) tunnelInputs() []tunnelInput {
	out := make([]tunnelInput, 0, 16)
	s.mu.Lock()
	for id, n := range s.nodes {
		if n.tunnel != nil && n.tunnelEnabled && n.conn != nil {
			binding, _ := json.Marshal(n.credential.FRPBinding)
			out = append(out, tunnelInput{key: "client:" + id, node: id, session: n.sessionID, token: n.credential.TokenSHA256, revision: string(binding), at: n.tunnelAt, snapshot: n.tunnel})
		}
	}
	s.mu.Unlock()
	if v := s.serverTunnelSnapshot.Load(); v != nil {
		at := time.UnixMilli(v.CollectedAtMS)
		// An unavailable provider has no native observation timestamp. The
		// collector records its own discovery time without inventing native time.
		if v.State != "ready" && v.CollectedAtMS == 0 {
			at = time.Now()
		}
		out = append(out, tunnelInput{key: "server", snapshot: v, at: at})
	}
	// Server association can change without a native lifecycle event. The trusted
	// credential revision is included without retaining any token in history.
	configs := s.configs.Load()
	if configs != nil {
		bindings := map[string]struct {
			Revision int64
			Binding  *shared.FRPBinding
		}{}
		for id, n := range *configs {
			bindings[id] = struct {
				Revision int64
				Binding  *shared.FRPBinding
			}{n.ConfigRevision, n.Binding}
		}
		revision, _ := json.Marshal(bindings)
		for i := range out {
			if out[i].key == "server" {
				out[i].revision = string(revision)
			} else if n := (*configs)[out[i].node]; n != nil {
				out[i].revision = fmt.Sprint(n.ConfigRevision)
			}
		}
	}
	return out
}
func tunnelInputHash(in tunnelInput) string {
	// Counters are deliberately excluded; persist only metadata/state/events.
	v := *in.snapshot
	v.CollectedAtMS = 0
	v.Objects = append([]shared.TunnelObject(nil), v.Objects...)
	for i := range v.Objects {
		v.Objects[i].Counters.Connections = nil
		v.Objects[i].Counters.RXBytes = nil
		v.Objects[i].Counters.TXBytes = nil
	}
	data, _ := json.Marshal(struct {
		Revision, Token, Session string
		Snapshot                 shared.TunnelSnapshot
	}{in.revision, in.token, in.session, v})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func (s *Service) tunnelObserveLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	gc := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.observeTunnels(now)
			gc++
			if gc >= 30 {
				gc = 0
				ctx, cancel := context.WithTimeout(s.ctx, 50*time.Millisecond)
				_ = s.control.PruneTunnels(ctx)
				cancel()
			}
		}
	}
}
func (s *Service) observeTunnels(now time.Time) {
	h := s.tunnelHistory
	if h == nil || h.collector == "" {
		return
	}
	inputs := s.tunnelInputs()
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].key < inputs[j].key })
	current := map[string]bool{}
	for _, in := range inputs {
		current[in.key] = true
	}
	if len(inputs) > 16 {
		start := h.offset % len(inputs)
		batch := make([]tunnelInput, 0, 16)
		for i := 0; i < 16; i++ {
			batch = append(batch, inputs[(start+i)%len(inputs)])
		}
		h.offset = (start + 16) % len(inputs)
		inputs = batch
	}
	for _, in := range inputs {
		current[in.key] = true
		if now.Sub(in.at) > s.tunnelTTL() || in.at.After(now.Add(10*time.Second)) {
			continue
		}
		v := in.snapshot
		if v.Validate() != nil {
			continue
		}
		hash := tunnelInputHash(in)
		h.mu.RLock()
		prior := h.sources[in.key]
		h.mu.RUnlock()
		if v.ProcessEpoch == "" && prior != nil && prior.epoch != "" {
			copy := *v
			copy.ProcessEpoch = prior.epoch
			copy.CollectedAtMS = in.at.UnixMilli()
			v = &copy
		}
		if prior == nil || prior.hash != hash || !prior.complete || now.Sub(prior.seen) >= time.Minute {
			offset := 0
			if prior != nil && prior.hash == hash {
				offset = prior.objectOffset
			}
			if prior != nil && prior.complete {
				offset = 0
			}
			ctx, cancel := context.WithTimeout(s.ctx, 50*time.Millisecond)
			batchResult, err := s.control.IngestTunnelSlice(ctx, control.TunnelBatch{NodeID: in.node, TokenSHA256: in.token, CollectorEpoch: tunnelCollector(h.collector, in.session), ReceivedAtMS: in.at.UnixMilli(), Snapshot: *v}, offset, 64)
			cancel()
			if err != nil {
				h.mu.Lock()
				if err == control.ErrTunnelCapacity {
					h.status = "capacity_limited"
				} else {
					h.status = "degraded"
				}
				h.mu.Unlock()
				continue
			}
			state := &tunnelSourceState{hash: hash, mappings: map[string]control.TunnelMapping{}, seen: now, epoch: v.ProcessEpoch, complete: batchResult.Complete, objectOffset: batchResult.NextObject}
			if prior != nil && prior.hash == hash {
				for id, m := range prior.mappings {
					state.mappings[id] = m
				}
			}
			for _, m := range batchResult.Mappings {
				state.mappings[m.NativeInstanceID] = m
			}
			h.mu.Lock()
			h.sources[in.key] = state
			h.status = "ready"
			h.mu.Unlock()
			prior = state
		}
		h.mu.Lock()
		// Ready inventory is a coherent set; unavailable/truncated inventory is not
		// evidence of a close and cannot refresh a previously observed instance.
		if v.State != "ready" {
			for id, old := range h.live {
				if old.source == in.key {
					old.at = time.Time{}
					h.live[id] = old
					if s.store != nil {
						c := old.object.Counters
						c.Quality = "unknown"
						c.RXBytes = nil
						c.TXBytes = nil
						s.store.AcceptTunnel(id, in.at, c)
					}
				}
			}
		}
		if v.State == "ready" {
			for _, o := range v.Objects {
				m, ok := prior.mappings[o.InstanceID]
				if !ok {
					continue
				}
				old, exists := h.live[m.InstanceID]
				if exists && !shared.SameTunnelOrigin(old.object, o) {
					h.status = "degraded"
					continue
				}
				if !exists && len(h.live) >= 1024 {
					h.status = "capacity_limited"
					continue
				}
				h.live[m.InstanceID] = tunnelLive{object: o, at: in.at, source: in.key}
				if s.store != nil && (!exists || in.at.After(old.at)) {
					s.store.AcceptTunnel(m.InstanceID, in.at, o.Counters)
				}
			}
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	for id, l := range h.live {
		if !current[l.source] || now.Sub(l.at) > time.Minute {
			delete(h.live, id)
		}
	}
	for key, p := range h.sources {
		if !current[key] || now.Sub(p.seen) > 10*time.Minute {
			delete(h.sources, key)
		}
	}
	h.mu.Unlock()
}
func (s *Service) overlayTunnelInstance(v *control.TunnelInstance, now time.Time) {
	v.Freshness = "stale"
	if s.tunnelHistory == nil {
		return
	}
	s.tunnelHistory.mu.RLock()
	live, ok := s.tunnelHistory.live[v.InstanceID]
	s.tunnelHistory.mu.RUnlock()
	if ok && now.Sub(live.at) <= s.tunnelTTL() && !live.at.After(now.Add(10*time.Second)) {
		v.State = live.object.State
		v.Counters = live.object.Counters
		v.ClosedAtMS = live.object.ClosedAtMS
		v.LastObservedAtMS = live.at.UnixMilli()
		v.Freshness = "fresh"
	}
}

func (s *Service) tunnelTTL() time.Duration {
	s.mu.Lock()
	interval := s.cfg.ReportIntervalSeconds
	s.mu.Unlock()
	return max(10*time.Second, 3*time.Duration(interval)*time.Second)
}

// Session rotation is a collection discontinuity even when native runtime and
// sequence have not changed. Only the opaque digest is persisted, never session.
func tunnelCollector(process, session string) string {
	if session == "" {
		return process
	}
	sum := sha256.Sum256([]byte(process + "\x00" + session))
	b := sum[:16]
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
