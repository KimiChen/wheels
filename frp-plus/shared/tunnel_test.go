package shared

import (
	"encoding/json"
	"strings"
	"testing"
)

func validTunnelSnapshot() TunnelSnapshot {
	s := EmptyTunnelSnapshot("client", "ready")
	s.ProcessEpoch = "11111111-1111-4111-8111-111111111111"
	s.CollectedAtMS = 1790989200000
	s.Objects = []TunnelObject{{InstanceID: "22222222-2222-4222-8222-222222222222", Identity: TunnelIdentity{ServerID: "server", User: "team", RawClientID: detailPtr("client"), RawName: "private-tunnel", Kind: "proxy", Protocol: "tcp", Quality: "stable"}, State: TunnelState{Status: "running", LocalState: "unknown", RemoteState: "unknown", P2PState: "unknown", FallbackState: "unknown"}, Counters: TunnelCounters{Quality: "unsupported", ConnectionsQuality: "unsupported", Accounting: "not_observed", ByteScope: "not_observed"}, CreatedAtMS: s.CollectedAtMS, OperationRelation: "none"}}
	s.Events = []TunnelEvent{{Sequence: "9007199254740993", TimeBasis: "native", OccurredAtMS: s.CollectedAtMS, Code: "created", Object: s.Objects[0]}}
	s.FirstSequence = s.Events[0].Sequence
	s.LastSequence = s.FirstSequence
	return s
}

func TestTunnelWireRoundTripAndLegacyIsolation(t *testing.T) {
	s := validTunnelSnapshot()
	r := TunnelReport{Meta: detailReport(validDetail()).Meta, Snapshot: s}
	frame, err := DecodeFrame(encodeFrame(t, "frp.tunnel", r))
	if err != nil {
		t.Fatal(err)
	}
	if frame.Tunnel == nil || frame.Tunnel.Snapshot.Events[0].Sequence != "9007199254740993" || frame.Report != nil || frame.FRPDetail != nil {
		t.Fatal("private frame changed identity or displaced legacy frame")
	}
	for _, name := range []string{"hello", "report-first", "ping-tasks", "ping-result"} {
		if fixtureFrame(t, name).Tunnel != nil {
			t.Fatal("legacy frame acquired private tunnels")
		}
	}
	for _, raw := range []string{
		`"raw_name":"private-tunnel","secret_key":"private"`,
		`"raw_name":"private-tunnel","password":"private"`,
		`"raw_name":"private-tunnel","native":{"token":"private"}`,
		`"raw_name":"private-tunnel","RawName":"changed"`,
	} {
		data := strings.Replace(string(encodeFrame(t, "frp.tunnel", r)), `"raw_name":"private-tunnel"`, raw, 1)
		if _, err := DecodeFrame([]byte(data)); err == nil {
			t.Fatal("unknown or aliased private field accepted")
		}
	}
}

func TestTunnelRejectsAmbiguousIncompleteAndInvalidObservations(t *testing.T) {
	for name, mutate := range map[string]func(*TunnelSnapshot){
		"missing_epoch":       func(s *TunnelSnapshot) { s.ProcessEpoch = "" },
		"partial_inventory":   func(s *TunnelSnapshot) { s.State = "truncated" },
		"duplicate_instance":  func(s *TunnelSnapshot) { s.Objects = append(s.Objects, s.Objects[0]) },
		"unstable_identity":   func(s *TunnelSnapshot) { s.Objects[0].Identity.RawClientID = nil },
		"changed_event_owner": func(s *TunnelSnapshot) { s.Events[0].Object.Identity.RawName = "other" },
		"unknown_kind":        func(s *TunnelSnapshot) { s.Objects[0].Identity.Kind = "native" },
		"native_error":        func(s *TunnelSnapshot) { s.Objects[0].State.ErrorCode = "dial secret.example.invalid failed" },
		"fake_counter":        func(s *TunnelSnapshot) { s.Objects[0].Counters.RXBytes = detailPtr("0") },
		"fake_operation":      func(s *TunnelSnapshot) { s.Objects[0].OperationID = "33333333-3333-4333-8333-333333333333" },
		"event_gap": func(s *TunnelSnapshot) {
			e := s.Events[0]
			e.Sequence = "9007199254740995"
			s.Events = append(s.Events, e)
			s.LastSequence = e.Sequence
		},
		"numeric_padding": func(s *TunnelSnapshot) { s.DroppedEvents = "00" },
		"secret_service":  func(s *TunnelSnapshot) { s.Objects[0].ServiceID = "raw-local-path" },
		"null_array":      func(s *TunnelSnapshot) { s.Events = nil },
	} {
		t.Run(name, func(t *testing.T) {
			s := validTunnelSnapshot()
			mutate(&s)
			if s.Validate() == nil {
				t.Fatal("invalid observation accepted")
			}
		})
	}
	s := validTunnelSnapshot()
	s.State = "truncated"
	s.Objects = []TunnelObject{}
	if err := s.Validate(); err != nil {
		t.Fatal("bounded event window lost with unavailable inventory")
	}
	if EmptyTunnelSnapshot("client", "unavailable").Validate() != nil {
		t.Fatal("unavailable observation rejected")
	}
	s = validTunnelSnapshot()
	s.Source = "server"
	s.Objects[0].Counters = TunnelCounters{Quality: "ok", ConnectionsQuality: "ok", Epoch: s.Objects[0].InstanceID, Accounting: "connection_close", ByteScope: "forwarded_stream", Connections: detailPtr("1"), RXBytes: detailPtr("18446744073709551615"), TXBytes: detailPtr("9007199254740993")}
	s.Events = []TunnelEvent{}
	s.FirstSequence = "0"
	s.LastSequence = "0"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(s)
	if !strings.Contains(string(data), `"18446744073709551615"`) {
		t.Fatal("uint64 lost precision")
	}
	s.Objects[0].Counters.RXBytes = detailPtr("18446744073709551616")
	if s.Validate() == nil {
		t.Fatal("overflow accepted")
	}
}
