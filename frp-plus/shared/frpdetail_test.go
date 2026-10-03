package shared

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func detailPtr[T any](value T) *T { return &value }

func validDetail() FRPDetail {
	d := EmptyFRPDetail("ready")
	d.Association = FRPAssociation{ServerID: "server", User: "team", RawClientID: detailPtr("client")}
	d.ServiceID = "primary"
	d.Transport = FRPTransportDetail{Protocol: "quic", WireProtocol: "v2", Source: "configured", TLS: detailPtr(true), TCPMux: detailPtr(false)}
	d.Configuration = FRPConfigurationDetail{Source: "mixed", SourceState: "active", ShadowedSources: []string{"file"}, Revision: detailPtr(strings.Repeat("a", 64)), NativeEntry: "file", Reloadable: detailPtr(true)}
	d.Proxies = []FRPProxyDetail{{Name: "same-name", Type: "tcp", Source: "store", SourceState: "active", ShadowedSources: []string{"include"}, Status: "running", Enabled: true, LocalTarget: detailPtr("[::1]:8080"), Endpoints: []FRPEndpoint{{Kind: "tcp", Source: "configured", Host: "example.invalid", Port: detailPtr(uint16(0))}, {Kind: "tcp", Source: "observed", Host: "::", Port: detailPtr(uint16(18080))}}, Encryption: detailPtr(true), Compression: detailPtr(false)}}
	d.Visitors = []FRPVisitorDetail{{Name: "same-name", Type: "stcp", Source: "include", SourceState: "active", Status: "listening", Enabled: true, ServerUser: "team", ServerName: "private-service", BindEndpoint: &FRPEndpoint{Kind: "tcp", Source: "observed", Host: "127.0.0.1", Port: detailPtr(uint16(18081))}, Protocol: "tcp", LocalState: "listening", RemoteState: "unknown", P2PState: "unsupported", FallbackState: "disabled"}}
	d.ControlError = &FRPErrorDetail{Code: "connect_failed", Stage: "control", At: detailPtr("2026-10-03T01:00:00Z"), RecoveredAt: detailPtr("2026-10-03T01:01:00Z")}
	return d
}

func detailReport(d FRPDetail) FRPDetailReport {
	return FRPDetailReport{Meta: Meta{Schema: SchemaVersion, SessionID: "session", Sequence: 2, CollectedAt: "2026-10-03T01:02:00Z"}, Detail: d}
}

func TestFRPDetailRoundTripAndLegacyFixtures(t *testing.T) {
	d := validDetail()
	frame, err := DecodeFrame(encodeFrame(t, "frp.detail", detailReport(d)))
	if err != nil {
		t.Fatal(err)
	}
	if frame.FRPDetail == nil || frame.Hello != nil || frame.Report != nil || frame.PingTasks != nil || frame.PingResult != nil || frame.Method != "frp.detail" || frame.ID != "" {
		t.Fatalf("incorrect frame: %#v", frame)
	}
	got, _ := json.Marshal(frame.FRPDetail.Detail)
	want, _ := json.Marshal(d)
	if string(got) != string(want) {
		t.Fatal("detail changed during round trip")
	}
	if frame.FRPDetail.Detail.Visitors[0].RemoteState != "unknown" || *frame.FRPDetail.Detail.Proxies[0].Endpoints[0].Port != 0 {
		t.Fatal("unknown remote status or configured dynamic port was changed")
	}
	for _, name := range []string{"hello", "report-first", "ping-tasks", "ping-result"} {
		if fixtureFrame(t, name).FRPDetail != nil {
			t.Fatalf("legacy %s unexpectedly contains details", name)
		}
	}
}

func TestFRPDetailUnavailableAndTruncated(t *testing.T) {
	for _, state := range []string{"busy", "unavailable", "truncated"} {
		d := EmptyFRPDetail(state)
		if _, err := DecodeFrame(encodeFrame(t, "frp.detail", detailReport(d))); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		d.Proxies = validDetail().Proxies
		if d.Validate() == nil {
			t.Fatalf("%s accepted partial proxy list", state)
		}
	}
	if EmptyFRPDetail("ready").Validate() == nil {
		t.Fatal("empty observation asserted ready")
	}
	d := EmptyFRPDetail("ready")
	d.Association.ServerID, d.ServiceID = "server", "primary"
	if err := d.Validate(); err != nil {
		t.Fatalf("ready empty configuration with unknown fields: %v", err)
	}
}

func TestFRPDetailSemanticRejection(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*FRPDetail)
	}{
		{"unknown_state", func(d *FRPDetail) { d.State = "partial" }},
		{"missing_service", func(d *FRPDetail) { d.ServiceID = "" }},
		{"control_service", func(d *FRPDetail) { d.ServiceID = "primary\tservice" }},
		{"missing_server", func(d *FRPDetail) { d.Association.ServerID = "" }},
		{"unknown_transport", func(d *FRPDetail) { d.Transport.Protocol = "h2" }},
		{"unknown_wire", func(d *FRPDetail) { d.Transport.WireProtocol = "v3" }},
		{"unknown_transport_source", func(d *FRPDetail) { d.Transport.Source = "guessed" }},
		{"native_source_path", func(d *FRPDetail) { d.Configuration.Source = "/private/config.toml" }},
		{"native_entry_url", func(d *FRPDetail) { d.Configuration.NativeEntry = "https://user:secret@example.invalid" }},
		{"native_revision", func(d *FRPDetail) { d.Configuration.Revision = detailPtr("auth.token=secret") }},
		{"invalid_configuration_read_time", func(d *FRPDetail) { d.Configuration.ReadAt = detailPtr("now") }},
		{"uppercase_revision", func(d *FRPDetail) { d.Configuration.Revision = detailPtr(strings.Repeat("A", 64)) }},
		{"duplicate_shadowed_source", func(d *FRPDetail) { d.Configuration.ShadowedSources = []string{"file", "file"} }},
		{"unknown_source_state", func(d *FRPDetail) { d.Proxies[0].SourceState = "fresh" }},
		{"nil_proxies", func(d *FRPDetail) { d.Proxies = nil }},
		{"nil_visitors", func(d *FRPDetail) { d.Visitors = nil }},
		{"duplicate_proxy", func(d *FRPDetail) { d.Proxies = append(d.Proxies, d.Proxies[0]) }},
		{"duplicate_visitor", func(d *FRPDetail) { d.Visitors = append(d.Visitors, d.Visitors[0]) }},
		{"unknown_proxy_type", func(d *FRPDetail) { d.Proxies[0].Type = "exec" }},
		{"unknown_proxy_state", func(d *FRPDetail) { d.Proxies[0].Status = "healthy" }},
		{"invalid_local_target", func(d *FRPDetail) { d.Proxies[0].LocalTarget = detailPtr("http://user:secret@example.invalid") }},
		{"unknown_proxy_plugin", func(d *FRPDetail) { d.Proxies[0].PluginType = detailPtr("plugin-token-secret") }},
		{"empty_proxy_plugin", func(d *FRPDetail) { d.Proxies[0].PluginType = detailPtr("") }},
		{"nil_endpoints", func(d *FRPDetail) { d.Proxies[0].Endpoints = nil }},
		{"wrong_endpoint_kind", func(d *FRPDetail) { d.Proxies[0].Endpoints[0].Kind = "http" }},
		{"observed_zero_port", func(d *FRPDetail) { d.Proxies[0].Endpoints[1].Port = detailPtr(uint16(0)) }},
		{"credentials_in_host", func(d *FRPDetail) { d.Proxies[0].Endpoints[0].Host = "user:secret@example.invalid" }},
		{"host_whitespace", func(d *FRPDetail) { d.Proxies[0].Endpoints[0].Host = "example\u00a0.invalid" }},
		{"unknown_error_code", func(d *FRPDetail) { d.ControlError.Code = "token-secret-is-invalid" }},
		{"unknown_error_stage", func(d *FRPDetail) { d.ControlError.Stage = "exec" }},
		{"invalid_error_time", func(d *FRPDetail) { d.ControlError.At = detailPtr("now") }},
		{"inverted_error_time", func(d *FRPDetail) { d.ControlError.RecoveredAt = detailPtr("2026-10-03T00:59:00Z") }},
		{"unknown_visitor_type", func(d *FRPDetail) { d.Visitors[0].Type = "http" }},
		{"missing_visitor_target", func(d *FRPDetail) { d.Visitors[0].ServerName = "" }},
		{"unknown_visitor_plugin", func(d *FRPDetail) { d.Visitors[0].PluginType = detailPtr("socks5") }},
		{"unknown_local_state", func(d *FRPDetail) { d.Visitors[0].LocalState = "connected" }},
		{"unknown_remote_state", func(d *FRPDetail) { d.Visitors[0].RemoteState = "listening" }},
		{"unknown_p2p_state", func(d *FRPDetail) { d.Visitors[0].P2PState = "listening" }},
		{"unknown_fallback_state", func(d *FRPDetail) { d.Visitors[0].FallbackState = "connected" }},
		{"wrong_visitor_listener", func(d *FRPDetail) { d.Visitors[0].BindEndpoint.Kind = "udp" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validDetail()
			tt.mutate(&d)
			if d.Validate() == nil {
				t.Fatal("accepted invalid detail")
			}
		})
	}
}

func TestFRPDetailWireRejection(t *testing.T) {
	wire := string(encodeFrame(t, "frp.detail", detailReport(validDetail())))
	tests := map[string]string{
		"body_secret":               strings.Replace(wire, `"detail":{`, `"detail":{"auth_token":"secret",`, 1),
		"proxy_secret":              strings.Replace(wire, `"local_target":`, `"plugin_options":{"token":"secret"},"local_target":`, 1),
		"visitor_secret":            strings.Replace(wire, `"bind_endpoint":`, `"plugin_options":{"destination_ip":"secret"},"bind_endpoint":`, 1),
		"raw_error":                 strings.Replace(wire, `"code":"connect_failed"`, `"message":"secret","code":"connect_failed"`, 1),
		"unknown_capitalized_field": strings.Replace(wire, `"state":"ready"`, `"State":"ready"`, 1),
		"missing_state":             strings.Replace(wire, `"state":"ready",`, "", 1),
		"missing_bool":              strings.Replace(wire, `"enabled":true,`, "", 1),
		"null_bool":                 strings.Replace(wire, `"enabled":true`, `"enabled":null`, 1),
		"duplicate_state":           strings.Replace(wire, `"state":"ready"`, `"state":"ready","state":"ready"`, 1),
		"notification_id":           strings.Replace(wire, `"jsonrpc":`, `"id":"detail","jsonrpc":`, 1),
		"hello_sequence":            strings.Replace(wire, `"sequence":2`, `"sequence":1`, 1),
		"unsupported_schema":        strings.Replace(wire, `"schema":1`, `"schema":2`, 1),
		"legacy_hello_detail":       strings.Replace(string(fixture(t, "hello")), `"frp": {`, `"frp": {"detail":{},`, 1),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeFrame([]byte(data)); err == nil {
				t.Fatal("accepted invalid wire data")
			}
		})
	}
}

func TestFRPDetailPluginTypesArePinnedSummaries(t *testing.T) {
	for _, plugin := range []string{"http2https", "http_proxy", "https2http", "https2https", "http2http", "socks5", "static_file", "unix_domain_socket", "tls2raw", "virtual_net"} {
		d := validDetail()
		d.Proxies[0].PluginType = detailPtr(plugin)
		d.Proxies[0].LocalTarget = nil
		d.Visitors[0].PluginType = detailPtr("virtual_net")
		frame, err := DecodeFrame(encodeFrame(t, "frp.detail", detailReport(d)))
		if err != nil || *frame.FRPDetail.Detail.Proxies[0].PluginType != plugin || *frame.FRPDetail.Detail.Visitors[0].PluginType != "virtual_net" {
			t.Fatalf("plugin summary %s: %v", plugin, err)
		}
	}
}

func TestFRPDetailSizeLimits(t *testing.T) {
	for _, which := range []string{"proxies", "visitors", "endpoints", "bytes"} {
		t.Run(which, func(t *testing.T) {
			d := validDetail()
			switch which {
			case "proxies":
				d.Proxies = make([]FRPProxyDetail, MaxDetailProxies+1)
			case "visitors":
				d.Visitors = make([]FRPVisitorDetail, MaxDetailVisitors+1)
			case "endpoints":
				d.Proxies[0].Endpoints = make([]FRPEndpoint, MaxDetailEndpoints+1)
			case "bytes":
				p := d.Proxies[0]
				p.Type = "http"
				p.Endpoints = []FRPEndpoint{{Kind: "http", Source: "configured", Host: "example.invalid", Path: "/" + strings.Repeat("a", 1023)}}
				d.Proxies = nil
				for i := 0; i < MaxDetailProxies; i++ {
					p.Name = fmt.Sprintf("proxy-%d", i)
					d.Proxies = append(d.Proxies, p)
				}
			}
			if err := d.Validate(); !errors.Is(err, ErrFRPDetailTooLarge) {
				t.Fatalf("expected size sentinel, got %v", err)
			}
		})
	}
}

func TestFRPEndpointUnresolvedAndPrivate(t *testing.T) {
	for _, e := range []FRPEndpoint{
		{Kind: "http", Source: "configured", Subdomain: detailPtr("app"), Path: "/api"},
		{Kind: "https", Source: "configured", Host: "*.example.invalid"},
		{Kind: "visitor", Source: "configured"},
		{Kind: "tcp", Source: "observed", Host: "::1", Port: detailPtr(uint16(65535))},
	} {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []FRPEndpoint{
		{Kind: "http", Source: "observed", Subdomain: detailPtr("app")},
		{Kind: "http", Source: "configured", Host: "example.invalid", Subdomain: detailPtr("app")},
		{Kind: "http", Source: "configured", Host: "example.invalid", Path: "/api?token=secret"},
		{Kind: "http", Source: "configured", Host: "example.invalid", Path: "/api#secret"},
		{Kind: "tcp", Source: "configured", Path: "/api"},
		{Kind: "visitor", Source: "configured", Host: "example.invalid"},
	} {
		if e.Validate() == nil {
			t.Fatalf("accepted invalid endpoint %#v", e)
		}
	}
}
