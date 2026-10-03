package control

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type TunnelFilter struct {
	NodeID     string `json:"node_id"`
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	InstanceID string `json:"instance_id"`
	FromMS     int64  `json:"from_ms"`
	ToMS       int64  `json:"to_ms"`
}
type TunnelOperationLink struct {
	OperationID string                  `json:"operation_id"`
	State       string                  `json:"state"`
	Relation    string                  `json:"relation"`
	CreatedAtMS int64                   `json:"created_at_ms"`
	Changes     []ConfigOperationChange `json:"changes"`
}
type TunnelPage struct {
	Items          []Tunnel `json:"items"`
	NextCursor     string   `json:"next_cursor"`
	RetainedFromMS int64    `json:"retained_from_ms"`
	EventsPruned   bool     `json:"events_pruned"`
}
type TunnelInstancePage struct {
	Items          []TunnelInstance `json:"items"`
	NextCursor     string           `json:"next_cursor"`
	RetainedFromMS int64            `json:"retained_from_ms"`
	EventsPruned   bool             `json:"events_pruned"`
}
type TunnelEventPage struct {
	Items          []TunnelHistoryEvent  `json:"items"`
	NextCursor     string                `json:"next_cursor"`
	RetainedFromMS int64                 `json:"retained_from_ms"`
	EventsPruned   bool                  `json:"events_pruned"`
	OperationLinks []TunnelOperationLink `json:"operation_links"`
}
type tunnelCursor struct {
	Version int    `json:"v"`
	High    string `json:"h"`
	Last    string `json:"l"`
	Hash    string `json:"f"`
}

func tunnelPageInput(filter TunnelFilter, cursor string, limit int, key string) (*tunnelCursor, error) {
	if limit < 1 || limit > 200 || filter.NodeID != "" && !validID(filter.NodeID) || filter.InstanceID != "" && !validID(filter.InstanceID) || filter.Kind != "" && filter.Kind != "proxy" && filter.Kind != "visitor" || filter.Source != "" && filter.Source != "server" && filter.Source != "client" || filter.FromMS < 0 || filter.ToMS < 0 || filter.ToMS != 0 && (filter.ToMS <= filter.FromMS || filter.ToMS-filter.FromMS > 31*86400000) {
		return nil, ErrInvalid
	}
	c := &tunnelCursor{Version: 1, Hash: tunnelHash(struct {
		Key    string
		Filter TunnelFilter
	}{key, filter})}
	if cursor == "" {
		return c, nil
	}
	if len(cursor) > 1024 {
		return nil, ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrInvalid
	}
	var x tunnelCursor
	if json.Unmarshal(data, &x) != nil || x.Version != 1 || !validID(x.High) || !validID(x.Last) || x.Hash != c.Hash {
		return nil, ErrInvalid
	}
	canonical, _ := json.Marshal(x)
	if string(canonical) != string(data) {
		return nil, ErrInvalid
	}
	h, _ := strconv.ParseInt(x.High, 10, 64)
	l, _ := strconv.ParseInt(x.Last, 10, 64)
	if l > h {
		return nil, ErrInvalid
	}
	return &x, nil
}
func tunnelNext(c *tunnelCursor, last string) string {
	c.Last = last
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func tunnelRetention(tx *sql.Tx) (int64, bool, error) {
	var at, n int64
	err := tx.QueryRow(`SELECT pruned_through_ms,pruned_events FROM tunnel_retention WHERE id=1`).Scan(&at, &n)
	return at, n > 0, err
}
func tunnelWatermark(tx *sql.Tx, table, column string, c *tunnelCursor) error {
	if c.High != "" {
		return nil
	}
	return tx.QueryRow("SELECT CAST(COALESCE(max(" + column + "),0) AS TEXT) FROM " + table).Scan(&c.High)
}

const tunnelInstanceColumns = `instance_id,tunnel_id,native_instance_id,source,process_epoch,generation,object_json,last_observed_at_ms`

type tunnelScanner interface{ Scan(...any) error }

func scanTunnelInstance(row tunnelScanner) (TunnelInstance, error) {
	var out TunnelInstance
	var data string
	err := row.Scan(&out.InstanceID, &out.TunnelID, &out.NativeInstanceID, &out.Source, &out.ProcessEpoch, &out.Generation, &data, &out.LastObservedAtMS)
	if err != nil {
		return out, err
	}
	var o tunnelStoredObject
	if json.Unmarshal([]byte(data), &o) != nil {
		return out, ErrInvalid
	}
	out.Protocol = o.Identity.Protocol
	out.State = o.State
	out.Counters = o.Counters
	out.Accounting = o.Counters.Accounting
	out.ByteScope = o.Counters.ByteScope
	out.ServiceID = o.ServiceID
	out.CreatedAtMS = o.CreatedAtMS
	out.ClosedAtMS = o.ClosedAtMS
	out.Freshness = "stale"
	return out, nil
}

func (s *Store) ListTunnels(ctx context.Context, f TunnelFilter, cursor string, limit int) (*TunnelPage, error) {
	if limit > 100 || f.Source != "" || f.InstanceID != "" || f.FromMS != 0 || f.ToMS != 0 {
		return nil, ErrInvalid
	}
	c, err := tunnelPageInput(f, cursor, limit, "tunnels")
	if err != nil {
		return nil, err
	}
	p := &TunnelPage{Items: []Tunnel{}}
	err = s.call(ctx, func(tx *sql.Tx) error {
		if e := tunnelWatermark(tx, "tunnels", "tunnel_id", c); e != nil {
			return e
		}
		where := "tunnel_id<=?"
		args := []any{c.High}
		if c.Last != "" {
			where += " AND tunnel_id<?"
			args = append(args, c.Last)
		}
		if f.NodeID != "" {
			where += " AND node_id=?"
			args = append(args, f.NodeID)
		}
		if f.Kind != "" {
			where += " AND kind=?"
			args = append(args, f.Kind)
		}
		args = append(args, limit+1)
		rows, e := tx.Query(`SELECT tunnel_id,node_id,binding_epoch,server_id,user_name,raw_client_id,kind,raw_name,identity_quality FROM tunnels WHERE `+where+` ORDER BY tunnel_id DESC LIMIT ?`, args...)
		if e != nil {
			return e
		}
		for rows.Next() {
			var t Tunnel
			if e = rows.Scan(&t.TunnelID, &t.NodeID, &t.BindingEpoch, &t.ServerID, &t.User, &t.RawClientID, &t.Kind, &t.RawName, &t.IdentityQuality); e != nil {
				rows.Close()
				return e
			}
			t.CurrentInstances = []TunnelInstance{}
			p.Items = append(p.Items, t)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(p.Items) > limit {
			p.Items = p.Items[:limit]
			p.NextCursor = tunnelNext(c, p.Items[limit-1].TunnelID)
		}
		p.RetainedFromMS, p.EventsPruned, e = tunnelRetention(tx)
		if e != nil {
			return e
		}
		for i := range p.Items {
			t := &p.Items[i]
			t.RetainedFromMS = p.RetainedFromMS
			rows, e = tx.Query(`SELECT `+tunnelInstanceColumns+` FROM tunnel_instances i WHERE tunnel_id=? AND generation=(SELECT max(generation) FROM tunnel_instances newest WHERE newest.tunnel_id=i.tunnel_id AND newest.source=i.source) ORDER BY source`, t.TunnelID)
			if e != nil {
				return e
			}
			for rows.Next() {
				v, e := scanTunnelInstance(rows)
				if e != nil {
					rows.Close()
					return e
				}
				t.CurrentInstances = append(t.CurrentInstances, v)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
		}
		return nil
	})
	return p, err
}

func (s *Store) ListTunnelInstances(ctx context.Context, id string, f TunnelFilter, cursor string, limit int) (*TunnelInstancePage, error) {
	if !validID(id) || f.NodeID != "" || f.Kind != "" || f.InstanceID != "" || f.FromMS != 0 || f.ToMS != 0 {
		return nil, ErrInvalid
	}
	c, err := tunnelPageInput(f, cursor, limit, "instances:"+id)
	if err != nil {
		return nil, err
	}
	p := &TunnelInstancePage{Items: []TunnelInstance{}}
	err = s.call(ctx, func(tx *sql.Tx) error {
		if e := requireTunnel(tx, id); e != nil {
			return e
		}
		if e := tunnelWatermark(tx, "tunnel_instances", "instance_id", c); e != nil {
			return e
		}
		where := "tunnel_id=? AND instance_id<=?"
		args := []any{id, c.High}
		if c.Last != "" {
			where += " AND instance_id<?"
			args = append(args, c.Last)
		}
		if f.Source != "" {
			where += " AND source=?"
			args = append(args, f.Source)
		}
		args = append(args, limit+1)
		rows, e := tx.Query(`SELECT `+tunnelInstanceColumns+` FROM tunnel_instances WHERE `+where+` ORDER BY instance_id DESC LIMIT ?`, args...)
		if e != nil {
			return e
		}
		for rows.Next() {
			v, e := scanTunnelInstance(rows)
			if e != nil {
				rows.Close()
				return e
			}
			p.Items = append(p.Items, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(p.Items) > limit {
			p.Items = p.Items[:limit]
			p.NextCursor = tunnelNext(c, p.Items[limit-1].InstanceID)
		}
		p.RetainedFromMS, p.EventsPruned, e = tunnelRetention(tx)
		return e
	})
	return p, err
}
func requireTunnel(tx *sql.Tx, id string) error {
	var n int
	err := tx.QueryRow(`SELECT 1 FROM tunnels WHERE tunnel_id=?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
func (s *Store) GetTunnelInstance(ctx context.Context, tid, id string) (TunnelInstance, error) {
	var out TunnelInstance
	if !validID(tid) || !validID(id) {
		return out, ErrInvalid
	}
	err := s.call(ctx, func(tx *sql.Tx) error {
		var e error
		out, e = scanTunnelInstance(tx.QueryRow(`SELECT `+tunnelInstanceColumns+` FROM tunnel_instances WHERE tunnel_id=? AND instance_id=?`, tid, id))
		if errors.Is(e, sql.ErrNoRows) {
			return ErrNotFound
		}
		return e
	})
	return out, err
}

func (s *Store) ListTunnelEvents(ctx context.Context, id string, f TunnelFilter, cursor string, limit int) (*TunnelEventPage, error) {
	if !validID(id) || f.NodeID != "" || f.Kind != "" || f.Source != "" {
		return nil, ErrInvalid
	}
	c, err := tunnelPageInput(f, cursor, limit, "events:"+id)
	if err != nil {
		return nil, err
	}
	p := &TunnelEventPage{Items: []TunnelHistoryEvent{}, OperationLinks: []TunnelOperationLink{}}
	err = s.call(ctx, func(tx *sql.Tx) error {
		if e := requireTunnel(tx, id); e != nil {
			return e
		}
		if e := tunnelWatermark(tx, "tunnel_events", "event_id", c); e != nil {
			return e
		}
		// A source-wide gap is shown for any tunnel observed from that same source
		// and process. It is never expanded into hundreds of duplicate event rows.
		where := `(e.tunnel_id=? OR(e.tunnel_id IS NULL AND EXISTS(SELECT 1 FROM tunnel_instances x WHERE x.tunnel_id=? AND x.process_epoch=e.process_epoch AND (x.source_key=e.source_key OR x.source_key LIKE e.source_key||'::%')))) AND e.event_id<=?`
		args := []any{id, id, c.High}
		if c.Last != "" {
			where += " AND e.event_id<?"
			args = append(args, c.Last)
		}
		if f.InstanceID != "" {
			where += ` AND (e.instance_id=? OR (e.instance_id IS NULL AND EXISTS(SELECT 1 FROM tunnel_instances x WHERE x.tunnel_id=? AND x.instance_id=? AND x.process_epoch=e.process_epoch AND(x.source_key=e.source_key OR x.source_key LIKE e.source_key||'::%'))))`
			args = append(args, f.InstanceID, id, f.InstanceID)
		}
		if f.FromMS != 0 {
			where += " AND e.received_at_ms>=?"
			args = append(args, f.FromMS)
		}
		if f.ToMS != 0 {
			where += " AND e.received_at_ms<?"
			args = append(args, f.ToMS)
		}
		args = append(args, limit+1)
		rows, e := tx.Query(`SELECT e.event_id,e.instance_id,i.generation,COALESCE(i.source,CASE WHEN e.source_key='server' THEN 'server' ELSE 'client' END),e.code,e.state_json,e.time_basis,e.occurred_at_ms,e.received_at_ms FROM tunnel_events e LEFT JOIN tunnel_instances i ON i.instance_id=e.instance_id WHERE `+where+` ORDER BY e.event_id DESC LIMIT ?`, args...)
		if e != nil {
			return e
		}
		for rows.Next() {
			var v TunnelHistoryEvent
			var state sql.NullString
			if e = rows.Scan(&v.EventID, &v.InstanceID, &v.Generation, &v.Source, &v.Code, &state, &v.TimeBasis, &v.OccurredAtMS, &v.ReceivedAtMS); e != nil {
				rows.Close()
				return e
			}
			v.OperationRelation = "none"
			if state.Valid {
				if json.Unmarshal([]byte(state.String), &v.State) != nil {
					rows.Close()
					return ErrInvalid
				}
			}
			p.Items = append(p.Items, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(p.Items) > limit {
			p.Items = p.Items[:limit]
			p.NextCursor = tunnelNext(c, p.Items[limit-1].EventID)
		}
		p.RetainedFromMS, p.EventsPruned, e = tunnelRetention(tx)
		if e != nil {
			return e
		}
		// Only declared object changes within this immutable binding interval and
		// actual managed service identity can be associated. This is not causality.
		var objectKind, objectName string
		if e = tx.QueryRow(`SELECT kind,raw_name FROM tunnels WHERE tunnel_id=?`, id).Scan(&objectKind, &objectName); e != nil {
			return e
		}
		rows, e = tx.Query(`SELECT DISTINCT o.operation_id,o.state,o.created_at_ms,a.changes_json FROM tunnels t JOIN tunnel_binding_epochs b ON b.binding_epoch=t.binding_epoch JOIN tunnel_instances i ON i.tunnel_id=t.tunnel_id AND i.source='client' JOIN config_operations o ON o.node_id=t.node_id AND o.service_id=json_extract(i.origin_json,'$.service_id') AND o.created_at_ms>=b.started_at_ms AND(b.ended_at_ms IS NULL OR o.created_at_ms<b.ended_at_ms) JOIN config_audit_entries a ON a.operation_id=o.operation_id JOIN config_audit_objects obj ON obj.audit_id=a.audit_id AND obj.kind=t.kind AND obj.name=t.raw_name WHERE t.tunnel_id=? ORDER BY o.created_at_ms DESC LIMIT 50`, id)
		if e != nil {
			return e
		}
		seen := map[string]bool{}
		for rows.Next() {
			var l TunnelOperationLink
			var data string
			if e = rows.Scan(&l.OperationID, &l.State, &l.CreatedAtMS, &data); e != nil {
				rows.Close()
				return e
			}
			if seen[l.OperationID] {
				continue
			}
			seen[l.OperationID] = true
			l.Relation = "declared_change"
			var changes []ConfigOperationChange
			if json.Unmarshal([]byte(data), &changes) != nil {
				rows.Close()
				return ErrInvalid
			}
			l.Changes = []ConfigOperationChange{}
			for _, c := range changes {
				if c.Kind == objectKind && c.Name == objectName {
					l.Changes = append(l.Changes, c)
				}
			}
			p.OperationLinks = append(p.OperationLinks, l)
		}
		e = rows.Err()
		rows.Close()
		return e
	})
	return p, err
}

// Keep the persisted projection identical to the independently validated wire.
type tunnelStoredObject = shared.TunnelObject
