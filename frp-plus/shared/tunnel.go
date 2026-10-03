package shared

import (
	"encoding/json"
	"errors"
	"strconv"
)

const (
	TunnelCapability = "frp.tunnel.v1"
	MaxTunnelObjects = 512
	MaxTunnelEvents  = 128
	MaxTunnelBytes   = 192 * 1024
)

var ErrTunnelTooLarge = errors.New("tunnel observation exceeds limit")

// TunnelProvider supplies private observations, never configuration or secrets.
// It must return immediately when a native lock is busy. Callers isolate a
// faulty provider and must not infer removals from an incomplete inventory.
type TunnelProvider func() TunnelSnapshot

type TunnelReport struct {
	Meta
	Snapshot TunnelSnapshot `json:"snapshot"`
}

// Sequence bounds describe the included replay window, not an acknowledgement.
// A receiver detects missing events using its last committed source sequence.
// Epochs identify process instances; they never authenticate a client or node.
type TunnelSnapshot struct {
	State         string         `json:"state"`
	Source        string         `json:"source"`
	ProcessEpoch  string         `json:"process_epoch"`
	CollectedAtMS int64          `json:"collected_at_ms"`
	FirstSequence string         `json:"first_sequence"`
	LastSequence  string         `json:"last_sequence"`
	DroppedEvents string         `json:"dropped_events"`
	Objects       []TunnelObject `json:"objects"`
	Events        []TunnelEvent  `json:"events"`
}

type TunnelIdentity struct {
	ServerID    string  `json:"server_id"`
	User        string  `json:"user"`
	RawClientID *string `json:"raw_client_id"`
	RawName     string  `json:"raw_name"`
	Kind        string  `json:"kind"`
	Protocol    string  `json:"protocol"`
	Quality     string  `json:"quality"`
}

type TunnelState struct {
	Status        string `json:"status"`
	LocalState    string `json:"local_state"`
	RemoteState   string `json:"remote_state"`
	P2PState      string `json:"p2p_state"`
	FallbackState string `json:"fallback_state"`
	ErrorCode     string `json:"error_code"`
}

// RX is bytes recorded from the server toward frpc; TX is the reverse.
// Connection-close accounting does not measure instantaneous bandwidth. SUDP
// bytes are unsupported: matching codecs count framed streams while mixed
// codecs count decoded datagrams, and one proxy may use both simultaneously.
type TunnelCounters struct {
	Quality            string  `json:"quality"` // Byte quality; connection quality is independent.
	ConnectionsQuality string  `json:"connections_quality"`
	Epoch              string  `json:"epoch"`
	Accounting         string  `json:"accounting"`
	ByteScope          string  `json:"byte_scope"`
	Connections        *string `json:"connections"`
	RXBytes            *string `json:"rx_bytes"`
	TXBytes            *string `json:"tx_bytes"`
}

type TunnelObject struct {
	InstanceID        string         `json:"instance_id"`
	Identity          TunnelIdentity `json:"identity"`
	State             TunnelState    `json:"state"`
	Counters          TunnelCounters `json:"counters"`
	CreatedAtMS       int64          `json:"created_at_ms"`
	ClosedAtMS        *int64         `json:"closed_at_ms"`
	ServiceID         string         `json:"service_id"`
	OperationID       string         `json:"operation_id"`
	OperationRelation string         `json:"operation_relation"`
}

type TunnelEvent struct {
	Sequence     string       `json:"sequence"`
	TimeBasis    string       `json:"time_basis"`
	OccurredAtMS int64        `json:"occurred_at_ms"`
	Code         string       `json:"code"`
	Object       TunnelObject `json:"object"`
}

func EmptyTunnelSnapshot(source, state string) TunnelSnapshot {
	return TunnelSnapshot{Source: source, State: state, FirstSequence: "0", LastSequence: "0", DroppedEvents: "0", Objects: []TunnelObject{}, Events: []TunnelEvent{}}
}

func (r TunnelReport) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return err
	}
	if r.Sequence < 2 || r.Snapshot.Source != "client" {
		return errors.New("invalid tunnel report")
	}
	return r.Snapshot.Validate()
}

func tunnelUint(value string) (uint64, bool) {
	v, err := strconv.ParseUint(value, 10, 64)
	return v, err == nil && strconv.FormatUint(v, 10) == value
}

func tunnelTime(value int64) bool { return value >= 946684800000 && value <= 9214646400000 }

func (s TunnelSnapshot) Validate() error {
	if !detailEnum(s.State, "ready", "unavailable", "truncated") || !detailEnum(s.Source, "server", "client") || s.Objects == nil || s.Events == nil {
		return errors.New("invalid tunnel snapshot")
	}
	if len(s.Objects) > MaxTunnelObjects || len(s.Events) > MaxTunnelEvents {
		return ErrTunnelTooLarge
	}
	first, f := tunnelUint(s.FirstSequence)
	last, l := tunnelUint(s.LastSequence)
	_, d := tunnelUint(s.DroppedEvents)
	if !f || !l || !d || first > last {
		return errors.New("invalid tunnel sequence bounds")
	}
	if s.ProcessEpoch == "" {
		if s.State != "unavailable" || s.CollectedAtMS != 0 || first != 0 || last != 0 || s.DroppedEvents != "0" || len(s.Objects) != 0 || len(s.Events) != 0 {
			return errors.New("invalid unavailable tunnel snapshot")
		}
		return nil
	}
	if !ValidConfigOperationID(s.ProcessEpoch) || !tunnelTime(s.CollectedAtMS) || (s.State != "ready" && len(s.Objects) != 0) {
		return errors.New("invalid tunnel epoch or inventory")
	}
	seen := map[string]TunnelObject{}
	for _, o := range s.Objects {
		if _, exists := seen[o.InstanceID]; o.Validate(s.Source) != nil || exists {
			return errors.New("invalid or duplicate tunnel object")
		}
		seen[o.InstanceID] = o
	}
	previous := uint64(0)
	for i, e := range s.Events {
		seq, ok := tunnelUint(e.Sequence)
		if !ok || seq == 0 || (i > 0 && (previous == ^uint64(0) || seq != previous+1)) || e.Object.Validate(s.Source) != nil || !detailEnum(e.TimeBasis, "native", "observed") || !tunnelTime(e.OccurredAtMS) || !detailEnum(e.Code, "created", "registered", "state_changed", "closed", "settled", "counter_invalid") {
			return errors.New("invalid tunnel event")
		}
		if i == 0 && seq != first {
			return errors.New("invalid first tunnel sequence")
		}
		if prior, exists := seen[e.Object.InstanceID]; exists && !SameTunnelOrigin(prior, e.Object) {
			return errors.New("tunnel instance changed immutable identity")
		}
		seen[e.Object.InstanceID] = e.Object
		previous = seq
	}
	if len(s.Events) == 0 && (first != 0 || last != 0) || len(s.Events) > 0 && previous != last {
		return errors.New("invalid tunnel replay window")
	}
	data, err := json.Marshal(s)
	if err != nil {
		return errors.New("invalid tunnel snapshot")
	}
	if len(data) > MaxTunnelBytes {
		return ErrTunnelTooLarge
	}
	return nil
}

// SameTunnelOrigin compares immutable native instance metadata. A collector
// must repeat this check across frames before accepting a reused instance ID.
func SameTunnelOrigin(a, b TunnelObject) bool {
	x, y := a.Identity, b.Identity
	clientEqual := x.RawClientID == nil && y.RawClientID == nil || x.RawClientID != nil && y.RawClientID != nil && *x.RawClientID == *y.RawClientID
	return a.InstanceID == b.InstanceID && x.ServerID == y.ServerID && x.User == y.User && clientEqual && x.RawName == y.RawName && x.Kind == y.Kind && x.Protocol == y.Protocol && x.Quality == y.Quality && a.CreatedAtMS == b.CreatedAtMS && a.Counters.Epoch == b.Counters.Epoch && a.Counters.Accounting == b.Counters.Accounting && a.Counters.ByteScope == b.Counters.ByteScope && a.ServiceID == b.ServiceID && a.OperationID == b.OperationID && a.OperationRelation == b.OperationRelation
}

func (o TunnelObject) Validate(source string) error {
	i, s, c := o.Identity, o.State, o.Counters
	if !ValidConfigOperationID(o.InstanceID) || !detailText(i.ServerID, 128, false) || !detailText(i.User, 128, true) || !detailText(i.RawName, 256, false) || !configObjectType(i.Kind, i.Protocol) || !detailEnum(i.Quality, "stable", "instance_only") || !tunnelTime(o.CreatedAtMS) || (o.ClosedAtMS != nil && !tunnelTime(*o.ClosedAtMS)) {
		return errors.New("invalid tunnel identity")
	}
	if i.RawClientID != nil && !detailText(*i.RawClientID, 128, false) || i.Quality == "stable" && i.RawClientID == nil {
		return errors.New("invalid stable tunnel identity")
	}
	if !detailEnum(s.Status, "starting", "registered", "running", "error", "closed", "unknown") || !detailEnum(s.LocalState, "unknown", "configured", "starting", "listening", "error", "closed") || !detailEnum(s.RemoteState, "unknown", "connecting", "connected", "error", "closed") || !detailEnum(s.P2PState, "unknown", "connecting", "connected", "failed") || !detailEnum(s.FallbackState, "unknown", "active", "failed") || !detailEnum(s.ErrorCode, "", "start_failed", "listen_failed", "plugin_failed", "remote_failed", "p2p_failed", "fallback_failed", "connection_failed", "health_check_failed", "counter_invalid") {
		return errors.New("invalid tunnel state")
	}
	if source == "server" && (i.Kind != "proxy" || s.LocalState != "unknown" || s.RemoteState != "unknown" || s.P2PState != "unknown" || s.FallbackState != "unknown") {
		return errors.New("invalid server tunnel state")
	}
	if !detailEnum(c.Quality, "ok", "unknown", "unsupported") || !detailEnum(c.ConnectionsQuality, "ok", "unknown", "unsupported") || !detailEnum(c.Accounting, "connection_close", "datagram", "not_observed") || !detailEnum(c.ByteScope, "forwarded_stream", "datagram_payload", "not_observed") {
		return errors.New("invalid tunnel counter scope")
	}
	validateValue := func(quality string, value *string) bool {
		if quality == "ok" {
			if value == nil {
				return false
			}
			_, ok := tunnelUint(*value)
			return ok
		}
		return value == nil
	}
	if !validateValue(c.Quality, c.RXBytes) || !validateValue(c.Quality, c.TXBytes) || !validateValue(c.ConnectionsQuality, c.Connections) {
		return errors.New("invalid tunnel counter value or quality")
	}
	if c.Quality == "unsupported" && c.ConnectionsQuality == "unsupported" {
		if c.Epoch != "" {
			return errors.New("unsupported counters cannot have an epoch")
		}
	} else if !ValidConfigOperationID(c.Epoch) {
		return errors.New("observed counters require an epoch")
	}
	if c.ConnectionsQuality != "unsupported" && (source != "server" || i.Protocol == "udp" || i.Protocol == "xtcp") {
		return errors.New("connection count is not observed")
	}
	if c.Quality == "unsupported" {
		if c.Accounting != "not_observed" || c.ByteScope != "not_observed" {
			return errors.New("unsupported bytes require unknown accounting")
		}
	} else {
		if source != "server" || i.Protocol == "xtcp" || i.Protocol == "sudp" {
			return errors.New("byte counters are not observed uniformly")
		}
		if i.Protocol == "udp" {
			if c.Accounting != "datagram" || c.ByteScope != "datagram_payload" {
				return errors.New("invalid datagram accounting")
			}
		} else if c.Accounting != "connection_close" || c.ByteScope != "forwarded_stream" {
			return errors.New("invalid stream accounting")
		}
	}
	if o.ServiceID != "" && !ValidConfigOperationID(o.ServiceID) || !detailEnum(o.OperationRelation, "none", "local_apply", "local_rollback") {
		return errors.New("invalid tunnel operation relation")
	}
	if o.OperationRelation == "none" {
		if o.OperationID != "" {
			return errors.New("unattributed tunnel operation")
		}
	} else if source != "client" || !ValidConfigOperationID(o.ServiceID) || !ValidConfigOperationID(o.OperationID) {
		return errors.New("invalid tunnel operation identity")
	}
	return nil
}
