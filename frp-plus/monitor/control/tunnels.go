package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const MaxTunnelInstances = 10000
const MaxTunnelEvents = 100000

var ErrTunnelBusy = errors.New("tunnel history busy")
var ErrTunnelCapacity = fmt.Errorf("%w: tunnel history capacity exceeded", ErrConflict)

type Tunnel struct {
	TunnelID         string           `json:"tunnel_id"`
	NodeID           *string          `json:"node_id"`
	BindingEpoch     string           `json:"binding_epoch"`
	ServerID         string           `json:"server_id"`
	User             string           `json:"user"`
	RawClientID      *string          `json:"raw_client_id"`
	Kind             string           `json:"kind"`
	RawName          string           `json:"raw_name"`
	IdentityQuality  string           `json:"identity_quality"`
	CurrentInstances []TunnelInstance `json:"current_instances"`
	RetainedFromMS   int64            `json:"retained_from_ms"`
}

type TunnelInstance struct {
	InstanceID       string                `json:"instance_id"`
	TunnelID         string                `json:"tunnel_id"`
	NativeInstanceID string                `json:"native_instance_id"`
	Source           string                `json:"source"`
	ProcessEpoch     string                `json:"process_epoch"`
	Generation       string                `json:"generation"`
	Protocol         string                `json:"protocol"`
	State            shared.TunnelState    `json:"state"`
	Counters         shared.TunnelCounters `json:"counters"`
	Accounting       string                `json:"accounting"`
	ByteScope        string                `json:"byte_scope"`
	ServiceID        string                `json:"service_id"`
	CreatedAtMS      int64                 `json:"created_at_ms"`
	ClosedAtMS       *int64                `json:"closed_at_ms"`
	LastObservedAtMS int64                 `json:"last_observed_at_ms"`
	Freshness        string                `json:"freshness"`
}

type TunnelHistoryEvent struct {
	EventID           string              `json:"event_id"`
	InstanceID        *string             `json:"instance_id"`
	Generation        *string             `json:"generation"`
	Source            string              `json:"source"`
	Code              string              `json:"code"`
	State             *shared.TunnelState `json:"state"`
	TimeBasis         string              `json:"time_basis"`
	OccurredAtMS      *int64              `json:"occurred_at_ms"`
	ReceivedAtMS      int64               `json:"received_at_ms"`
	OperationID       string              `json:"operation_id"`
	OperationRelation string              `json:"operation_relation"`
}

type TunnelBatch struct {
	// NodeID and TokenSHA256 come from the authenticated connection, not wire.
	NodeID, TokenSHA256, CollectorEpoch string
	ReceivedAtMS                        int64
	Snapshot                            shared.TunnelSnapshot
}

type TunnelMapping struct{ NativeInstanceID, InstanceID, TunnelID, Generation, SourceKey string }

func tunnelHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func tunnelStateKey(o shared.TunnelObject) string {
	return tunnelHash(struct {
		State              shared.TunnelState
		Closed             *int64
		Bytes, Connections string
	}{o.State, o.ClosedAtMS, o.Counters.Quality, o.Counters.ConnectionsQuality})
}
func tunnelSameBinding(b *shared.FRPBinding, i shared.TunnelIdentity) bool {
	return b != nil && i.Quality == "stable" && i.RawClientID != nil && b.ServerID == i.ServerID && b.User == i.User && b.RawClientID == *i.RawClientID
}

type tunnelOwner struct {
	node    *string
	epoch   string
	binding *shared.FRPBinding
}

func tunnelOwners(tx *sql.Tx, b TunnelBatch) (map[string]tunnelOwner, tunnelOwner, error) {
	owners := map[string]tunnelOwner{}
	var agent tunnelOwner
	rows, err := tx.Query(`SELECT n.id,n.token_sha256,e.binding_epoch,e.binding_json FROM nodes n JOIN tunnel_binding_epochs e ON e.node_id=n.id AND e.ended_at_ms IS NULL`)
	if err != nil {
		return nil, agent, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, token, epoch string
		var data sql.NullString
		if err = rows.Scan(&id, &token, &epoch, &data); err != nil {
			return nil, agent, err
		}
		o := tunnelOwner{node: &id, epoch: epoch}
		if data.Valid {
			o.binding = new(shared.FRPBinding)
			if json.Unmarshal([]byte(data.String), o.binding) != nil {
				return nil, agent, ErrInvalid
			}
			owners[tunnelHash(o.binding)] = o
		}
		if id == b.NodeID {
			if token != b.TokenSHA256 {
				return nil, agent, ErrConflict
			}
			agent = o
		}
	}
	if err = rows.Err(); err != nil {
		return nil, agent, err
	}
	if b.Snapshot.Source == "client" && agent.node == nil {
		return nil, agent, ErrNotFound
	}
	return owners, agent, nil
}

// IngestTunnels is deliberately low priority. It never queues behind an existing
// control request, and is called outside socket/Service locks. Only lifecycle
// transitions are written: ordinary byte samples belong in the optional TSDB.
// IngestTunnels is the full bounded import used by local test/bootstrap callers.
func (s *Store) IngestTunnels(ctx context.Context, b TunnelBatch) ([]TunnelMapping, error) {
	r, err := s.IngestTunnelSlice(ctx, b, 0, shared.MaxTunnelObjects+shared.MaxTunnelEvents)
	return r.Mappings, err
}

type TunnelSlice struct {
	Mappings   []TunnelMapping
	NextObject int
	Complete   bool
}

// IngestTunnelSlice allows the live collector to limit each transaction to 64
// object/event updates. The durable source sequence is advanced only as far as
// committed events; remaining inventory is resumed without declaring a close.
func (s *Store) IngestTunnelSlice(ctx context.Context, b TunnelBatch, offset, budget int) (TunnelSlice, error) {
	result := TunnelSlice{Mappings: []TunnelMapping{}, NextObject: offset}
	if offset < 0 || offset > len(b.Snapshot.Objects) || budget < 1 || budget > shared.MaxTunnelObjects+shared.MaxTunnelEvents {
		return result, ErrInvalid
	}
	if b.Snapshot.Validate() != nil || !shared.ValidConfigOperationID(b.CollectorEpoch) || b.ReceivedAtMS <= 0 || b.Snapshot.Source == "client" && (!validID(b.NodeID) || validateHash(b.TokenSHA256) != nil) || b.Snapshot.Source == "server" && (b.NodeID != "" || b.TokenSHA256 != "") {
		return result, ErrInvalid
	}
	if b.Snapshot.ProcessEpoch == "" {
		result.Complete = true
		return result, nil
	}
	if len(s.queue) > 0 {
		return result, ErrTunnelBusy
	}
	out := []TunnelMapping{}
	remaining := budget
	err := s.maintenanceCall(ctx, func(tx *sql.Tx) error {
		owners, agent, err := tunnelOwners(tx, b)
		if err != nil {
			return err
		}
		sourceKey := "server"
		if agent.node != nil {
			sourceKey = "client:" + *agent.node + ":" + agent.epoch
		}
		snap := b.Snapshot
		var rowsCount int
		if err = tx.QueryRow(`SELECT count(*) FROM tunnel_events`).Scan(&rowsCount); err != nil {
			return err
		}
		if rowsCount >= MaxTunnelEvents+256 {
			return ErrTunnelCapacity
		}
		var lastText, dropped, collector, oldState string
		var sourceUpdated int64
		err = tx.QueryRow(`SELECT last_sequence,dropped_events,collector_epoch,snapshot_state,updated_at_ms FROM tunnel_sources WHERE source_key=? AND process_epoch=?`, sourceKey, snap.ProcessEpoch).Scan(&lastText, &dropped, &collector, &oldState, &sourceUpdated)
		first := errors.Is(err, sql.ErrNoRows)
		if first {
			var count int
			if e := tx.QueryRow(`SELECT count(*) FROM tunnel_sources`).Scan(&count); e != nil {
				return e
			}
			if count >= 2*MaxTunnelInstances {
				return ErrTunnelCapacity
			}
		}
		if err != nil && !first {
			return err
		}
		last, _ := strconv.ParseUint(lastText, 10, 64)
		next, _ := strconv.ParseUint(snap.LastSequence, 10, 64)
		wantedLast := next
		processedLast := last
		if !first && next < last && (snap.State == "ready" || next != 0) {
			return ErrConflict
		}
		if !first && next == 0 && snap.State != "ready" {
			snap.LastSequence = lastText
		}
		gap := func(code string) error {
			_, e := tx.Exec(`INSERT INTO tunnel_events(source_key,process_epoch,code,time_basis,received_at_ms) VALUES(?,?,?,'observed',?)`, sourceKey, snap.ProcessEpoch, code, b.ReceivedAtMS)
			return e
		}
		if first || collector != b.CollectorEpoch {
			if err = gap("collector_gap"); err != nil {
				return err
			}
		}
		if len(snap.Events) > 0 {
			start, _ := strconv.ParseUint(snap.FirstSequence, 10, 64)
			if start > last && start-last > 1 {
				if err = gap("event_gap"); err != nil {
					return err
				}
			}
		}
		if snap.DroppedEvents != "0" && snap.DroppedEvents != dropped {
			if err = gap("source_overflow"); err != nil {
				return err
			}
		}
		if snap.State != "ready" && snap.State != oldState {
			if err = gap("source_" + snap.State); err != nil {
				return err
			}
		}
		mappings := map[string]TunnelMapping{}
		upsert := func(o shared.TunnelObject) (TunnelMapping, error) {
			var owner tunnelOwner
			if snap.Source == "client" {
				owner = agent
			} else if o.Identity.RawClientID != nil {
				owner = owners[tunnelHash(&shared.FRPBinding{ServerID: o.Identity.ServerID, User: o.Identity.User, RawClientID: *o.Identity.RawClientID})]
			}
			keyIdentity := o.Identity
			keyIdentity.Protocol = ""
			quality := o.Identity.Quality
			// Unbound or mismatching Agent claims remain scoped to this native instance.
			isolated := ""
			if snap.Source == "client" && !tunnelSameBinding(owner.binding, o.Identity) || quality != "stable" {
				quality = "instance_only"
				isolated = sourceKey + ":" + snap.ProcessEpoch + ":" + o.InstanceID
			}
			epoch := owner.epoch
			if epoch == "" {
				epoch = "0"
			}
			logical := tunnelHash(struct {
				Epoch    string
				Identity shared.TunnelIdentity
				Isolated string
			}{epoch, keyIdentity, isolated})
			var tid string
			err := tx.QueryRow(`SELECT tunnel_id FROM tunnels WHERE logical_key=?`, logical).Scan(&tid)
			if errors.Is(err, sql.ErrNoRows) {
				var count int
				if err = tx.QueryRow(`SELECT count(*) FROM tunnel_instances`).Scan(&count); err != nil {
					return TunnelMapping{}, err
				}
				if count >= MaxTunnelInstances {
					return TunnelMapping{}, ErrTunnelCapacity
				}
				var node any
				if owner.node != nil {
					node = *owner.node
				}
				res, e := tx.Exec(`INSERT INTO tunnels(logical_key,node_id,binding_epoch,server_id,user_name,raw_client_id,kind,raw_name,identity_quality,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?)`, logical, node, epoch, o.Identity.ServerID, o.Identity.User, o.Identity.RawClientID, o.Identity.Kind, o.Identity.RawName, quality, b.ReceivedAtMS)
				if e != nil {
					return TunnelMapping{}, e
				}
				id, _ := res.LastInsertId()
				tid = strconv.FormatInt(id, 10)
			} else if err != nil {
				return TunnelMapping{}, err
			}
			// Binding epochs are part of the source scope on both sides. A controller
			// rebind ends association, not native forwarding; it must never move history.
			instanceSource := sourceKey
			if snap.Source == "server" {
				instanceSource += "::" + epoch
			}
			var iid, generation, origin, stateKey string
			err = tx.QueryRow(`SELECT instance_id,generation,origin_json,state_key FROM tunnel_instances WHERE source_key=? AND process_epoch=? AND native_instance_id=?`, instanceSource, snap.ProcessEpoch, o.InstanceID).Scan(&iid, &generation, &origin, &stateKey)
			data, _ := json.Marshal(o)
			key := tunnelStateKey(o)
			if errors.Is(err, sql.ErrNoRows) {
				var count int
				if err = tx.QueryRow(`SELECT count(*) FROM tunnel_instances`).Scan(&count); err != nil {
					return TunnelMapping{}, err
				}
				if count >= MaxTunnelInstances {
					return TunnelMapping{}, ErrTunnelCapacity
				}
				var gen int64
				if err = tx.QueryRow(`INSERT INTO tunnel_generations(tunnel_id,source,generation) VALUES(?,?,1) ON CONFLICT(tunnel_id,source) DO UPDATE SET generation=generation+1 RETURNING generation`, tid, snap.Source).Scan(&gen); err != nil {
					return TunnelMapping{}, err
				}
				res, e := tx.Exec(`INSERT INTO tunnel_instances(tunnel_id,source_key,source,process_epoch,native_instance_id,generation,origin_json,object_json,state_key,created_at_ms,closed_at_ms,last_observed_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, tid, instanceSource, snap.Source, snap.ProcessEpoch, o.InstanceID, gen, string(data), string(data), key, o.CreatedAtMS, o.ClosedAtMS, b.ReceivedAtMS)
				if e != nil {
					return TunnelMapping{}, e
				}
				id, _ := res.LastInsertId()
				iid = strconv.FormatInt(id, 10)
				generation = strconv.FormatInt(gen, 10)
			} else if err != nil {
				return TunnelMapping{}, err
			} else {
				var prior shared.TunnelObject
				if json.Unmarshal([]byte(origin), &prior) != nil || !shared.SameTunnelOrigin(prior, o) {
					return TunnelMapping{}, ErrConflict
				}
				if key != stateKey {
					if _, err = tx.Exec(`UPDATE tunnel_instances SET object_json=?,state_key=?,closed_at_ms=?,last_observed_at_ms=? WHERE instance_id=?`, string(data), key, o.ClosedAtMS, b.ReceivedAtMS, iid); err != nil {
						return TunnelMapping{}, err
					}
				}
			}
			m := TunnelMapping{NativeInstanceID: o.InstanceID, InstanceID: iid, TunnelID: tid, Generation: generation, SourceKey: sourceKey}
			mappings[o.InstanceID] = m
			return m, nil
		}
		for _, e := range snap.Events {
			seq, _ := strconv.ParseUint(e.Sequence, 10, 64)
			if seq <= last {
				continue
			}
			if remaining == 0 {
				break
			}
			remaining--
			m, err := upsert(e.Object)
			if err != nil {
				return err
			}
			processedLast = seq
			state, _ := json.Marshal(e.Object.State)
			if _, err = tx.Exec(`INSERT INTO tunnel_events(tunnel_id,instance_id,source_key,process_epoch,native_sequence,code,state_json,time_basis,occurred_at_ms,received_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?)`, m.TunnelID, m.InstanceID, sourceKey, snap.ProcessEpoch, e.Sequence, e.Code, string(state), e.TimeBasis, e.OccurredAtMS, b.ReceivedAtMS); err != nil {
				return err
			}
			if e.Code == "settled" {
				data, _ := json.Marshal(e.Object)
				if _, err = tx.Exec(`UPDATE tunnel_instances SET object_json=?,last_observed_at_ms=? WHERE instance_id=?`, string(data), b.ReceivedAtMS, m.InstanceID); err != nil {
					return err
				}
			}
		}
		if wantedLast == 0 || processedLast >= wantedLast {
			for i := offset; i < len(snap.Objects) && remaining > 0; i++ {
				remaining--
				if _, err = upsert(snap.Objects[i]); err != nil {
					return err
				}
				result.NextObject = i + 1
			}
		}
		snap.LastSequence = strconv.FormatUint(processedLast, 10)
		result.Complete = (wantedLast == 0 || processedLast >= wantedLast) && result.NextObject == len(snap.Objects)
		if first || lastText != snap.LastSequence || dropped != snap.DroppedEvents || collector != b.CollectorEpoch || oldState != snap.State || b.ReceivedAtMS-sourceUpdated >= 60000 {
			if _, err = tx.Exec(`INSERT INTO tunnel_sources(source_key,process_epoch,collector_epoch,last_sequence,dropped_events,snapshot_state,updated_at_ms) VALUES(?,?,?,?,?,?,?) ON CONFLICT(source_key,process_epoch) DO UPDATE SET collector_epoch=excluded.collector_epoch,last_sequence=excluded.last_sequence,dropped_events=excluded.dropped_events,snapshot_state=excluded.snapshot_state,updated_at_ms=excluded.updated_at_ms`, sourceKey, snap.ProcessEpoch, b.CollectorEpoch, snap.LastSequence, snap.DroppedEvents, snap.State, b.ReceivedAtMS); err != nil {
				return err
			}
		}
		for _, m := range mappings {
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Mappings = out
	return result, nil
}

// PruneTunnels deletes a bounded oldest event batch. Current instance metadata
// survives, and IDs are never reused. The retention boundary remains visible.
func (s *Store) PruneTunnels(ctx context.Context) error {
	if len(s.queue) > 0 {
		return ErrTunnelBusy
	}
	return s.maintenanceCall(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM tunnel_events`).Scan(&count); err != nil {
			return err
		}
		limit := min(256, max(0, count-MaxTunnelEvents))
		cutoff := s.cfg.Now().Add(-30 * 24 * time.Hour).UnixMilli()
		rows, err := tx.Query(`SELECT event_id,received_at_ms FROM tunnel_events WHERE received_at_ms<? OR event_id IN(SELECT event_id FROM tunnel_events ORDER BY event_id LIMIT ?) ORDER BY event_id LIMIT 256`, cutoff, limit)
		if err != nil {
			return err
		}
		ids := []int64{}
		last := int64(0)
		for rows.Next() {
			var id, at int64
			if err = rows.Scan(&id, &at); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			last = max(last, at)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err = tx.Exec(`DELETE FROM tunnel_events WHERE event_id=?`, id); err != nil {
				return err
			}
		}
		if len(ids) > 0 {
			if _, err = tx.Exec(`UPDATE tunnel_retention SET pruned_through_ms=max(pruned_through_ms,?),pruned_events=pruned_events+? WHERE id=1`, last, len(ids)); err != nil {
				return err
			}
		}
		metadataCutoff := s.cfg.Now().Add(-time.Duration(max(30, s.cfg.TunnelRetentionDays)) * 24 * time.Hour).UnixMilli()
		// Never evict an observed live source merely because its state has not
		// changed. Source heartbeats are persisted at most once per minute.
		if _, err = tx.Exec(`DELETE FROM tunnel_instances WHERE instance_id IN(SELECT i.instance_id FROM tunnel_instances i WHERE (i.closed_at_ms<? OR NOT EXISTS(SELECT 1 FROM tunnel_sources s WHERE s.process_epoch=i.process_epoch AND(s.source_key=i.source_key OR i.source_key LIKE s.source_key||'::%') AND s.updated_at_ms>=?)) AND i.last_observed_at_ms<? AND NOT EXISTS(SELECT 1 FROM tunnel_events e WHERE e.instance_id=i.instance_id) ORDER BY i.instance_id LIMIT 64)`, metadataCutoff, metadataCutoff, metadataCutoff); err != nil {
			return err
		}
		// Remove the generation ledger only together with the whole retired
		// logical tunnel; an existing tunnel never reuses a generation number.
		_, err = tx.Exec(`DELETE FROM tunnel_generations WHERE tunnel_id IN(SELECT t.tunnel_id FROM tunnels t WHERE t.created_at_ms<? AND NOT EXISTS(SELECT 1 FROM tunnel_instances i WHERE i.tunnel_id=t.tunnel_id) AND NOT EXISTS(SELECT 1 FROM tunnel_events e WHERE e.tunnel_id=t.tunnel_id) ORDER BY t.tunnel_id LIMIT 64)`, metadataCutoff)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM tunnels WHERE tunnel_id IN(SELECT t.tunnel_id FROM tunnels t WHERE t.created_at_ms<? AND NOT EXISTS(SELECT 1 FROM tunnel_instances i WHERE i.tunnel_id=t.tunnel_id) AND NOT EXISTS(SELECT 1 FROM tunnel_events e WHERE e.tunnel_id=t.tunnel_id) ORDER BY t.tunnel_id LIMIT 64)`, metadataCutoff)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM tunnel_sources WHERE rowid IN(SELECT s.rowid FROM tunnel_sources s WHERE s.updated_at_ms<? AND NOT EXISTS(SELECT 1 FROM tunnel_instances i WHERE i.process_epoch=s.process_epoch AND(i.source_key=s.source_key OR i.source_key LIKE s.source_key||'::%')) AND NOT EXISTS(SELECT 1 FROM tunnel_events e WHERE e.process_epoch=s.process_epoch AND e.source_key=s.source_key) ORDER BY s.updated_at_ms LIMIT 64)`, metadataCutoff)
		return err
	})
}
