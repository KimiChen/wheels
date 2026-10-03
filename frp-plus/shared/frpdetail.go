package shared

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	FRPDetailCapability = "frp.detail.v1"
	CapabilitiesHeader  = "X-Frp-Plus-Capabilities"
	MaxDetailProxies    = 512
	MaxDetailVisitors   = 512
	MaxDetailEndpoints  = 32
	// Leave space for the JSON-RPC envelope and metadata within MaxFrameBytes.
	MaxDetailBytes = 192 * 1024
)

var ErrFRPDetailTooLarge = errors.New("FRP detail exceeds size limit")

// DetailProvider returns a bounded, private observation. It never returns a
// native configuration, credentials, plugin options, headers or raw errors.
type DetailProvider func() FRPDetail

// FRPDetail is an independently negotiated administrative observation. It is
// not part of frp.v1, must never enter public JSON/SSE, and proves no identity.
// A non-ready snapshot must not present a partial object list as complete.
type FRPDetail struct {
	State         string                 `json:"state"`
	Association   FRPAssociation         `json:"association"`
	ServiceID     string                 `json:"service_id"`
	Transport     FRPTransportDetail     `json:"transport"`
	ControlError  *FRPErrorDetail        `json:"control_error"`
	Configuration FRPConfigurationDetail `json:"configuration"`
	Proxies       []FRPProxyDetail       `json:"proxies"`
	Visitors      []FRPVisitorDetail     `json:"visitors"`
}

type FRPDetailReport struct {
	Meta
	Detail FRPDetail `json:"detail"`
}

// Source distinguishes configured transport from an observed connection. A nil
// boolean is unknown; a configured TLS value does not prove a TLS handshake.
type FRPTransportDetail struct {
	Protocol     string `json:"protocol"`
	WireProtocol string `json:"wire_protocol"`
	Source       string `json:"source"`
	TLS          *bool  `json:"tls"`
	TCPMux       *bool  `json:"tcp_mux"`
}

// Revision is an opaque SHA-256 revision, never configuration text or a path.
// NativeEntry identifies an operation family, not an untrusted browser URL.
type FRPConfigurationDetail struct {
	Source           string   `json:"source"`
	SourceState      string   `json:"source_state"`
	ShadowedSources  []string `json:"shadowed_sources,omitempty"`
	Revision         *string  `json:"revision"`
	NativeEntry      string   `json:"native_entry"`
	Reloadable       *bool    `json:"reloadable"`
	ReadAt           *string  `json:"read_at"`
	DashboardEnabled *bool    `json:"dashboard_enabled"`
}

// Errors use only fixed codes. Native error text may contain tokens, paths,
// plugin options or credentials and must never be copied into this contract.
// At and RecoveredAt are event times, or nil when no event time was observed.
type FRPErrorDetail struct {
	Code        string  `json:"code"`
	Stage       string  `json:"stage"`
	At          *string `json:"at"`
	RecoveredAt *string `json:"recovered_at"`
}

// An observed listener is not necessarily a publicly reachable endpoint.
// Subdomain carries an unresolved native subDomain without inventing its DNS
// suffix. Host is empty in that case. Path excludes query strings and fragments.
type FRPEndpoint struct {
	Kind      string  `json:"kind"`
	Source    string  `json:"source"`
	Host      string  `json:"host"`
	Port      *uint16 `json:"port"`
	Path      string  `json:"path"`
	Subdomain *string `json:"subdomain"`
}

type FRPProxyDetail struct {
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	Source          string          `json:"source"`
	SourceState     string          `json:"source_state"`
	ShadowedSources []string        `json:"shadowed_sources,omitempty"`
	Status          string          `json:"status"`
	Enabled         bool            `json:"enabled"`
	LocalTarget     *string         `json:"local_target"`
	PluginType      *string         `json:"plugin_type"`
	Endpoints       []FRPEndpoint   `json:"endpoints"`
	Encryption      *bool           `json:"encryption"`
	Compression     *bool           `json:"compression"`
	Error           *FRPErrorDetail `json:"error"`
}

// Local listening, a remote visitor connection, P2P and fallback are separate
// observations. A listening socket alone must not imply remote reachability.
type FRPVisitorDetail struct {
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	Source          string          `json:"source"`
	SourceState     string          `json:"source_state"`
	ShadowedSources []string        `json:"shadowed_sources,omitempty"`
	Status          string          `json:"status"`
	Enabled         bool            `json:"enabled"`
	ServerUser      string          `json:"server_user"`
	ServerName      string          `json:"server_name"`
	BindEndpoint    *FRPEndpoint    `json:"bind_endpoint"`
	Protocol        string          `json:"protocol"`
	FallbackTo      *string         `json:"fallback_to"`
	PluginType      *string         `json:"plugin_type"`
	LocalState      string          `json:"local_state"`
	RemoteState     string          `json:"remote_state"`
	P2PState        string          `json:"p2p_state"`
	FallbackState   string          `json:"fallback_state"`
	Encryption      *bool           `json:"encryption"`
	Compression     *bool           `json:"compression"`
	Error           *FRPErrorDetail `json:"error"`
}

// EmptyFRPDetail provides an explicit unavailable/truncated observation for a
// failed or oversized provider. Callers must not use it to assert readiness.
func EmptyFRPDetail(state string) FRPDetail {
	return FRPDetail{
		State:         state,
		Transport:     FRPTransportDetail{Protocol: "unknown", WireProtocol: "unknown", Source: "unknown"},
		Configuration: FRPConfigurationDetail{Source: "unknown", SourceState: "unknown", NativeEntry: "unavailable"},
		Proxies:       []FRPProxyDetail{}, Visitors: []FRPVisitorDetail{},
	}
}

func (r FRPDetailReport) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return err
	}
	if r.Sequence < 2 {
		return errors.New("FRP detail must follow hello")
	}
	return r.Detail.Validate()
}

func (d FRPDetail) Validate() error {
	if !detailEnum(d.State, "ready", "busy", "unavailable", "truncated") {
		return errors.New("invalid FRP detail state")
	}
	knownAssociation := d.Association.ServerID != "" || d.Association.User != "" || d.Association.RawClientID != nil
	if d.State == "ready" || knownAssociation {
		if !bounded(d.Association.ServerID, 128, false) || !bounded(d.Association.User, 128, true) || (d.Association.RawClientID != nil && !bounded(*d.Association.RawClientID, 128, false)) {
			return errors.New("invalid FRP detail association")
		}
	}
	if !detailText(d.ServiceID, 128, d.State != "ready") {
		return errors.New("invalid FRP service ID")
	}
	if err := d.Transport.Validate(); err != nil {
		return err
	}
	if err := d.Configuration.Validate(); err != nil {
		return err
	}
	if err := d.ControlError.Validate(); err != nil {
		return err
	}
	if d.Proxies == nil || d.Visitors == nil {
		return errors.New("FRP detail lists must be arrays")
	}
	if len(d.Proxies) > MaxDetailProxies || len(d.Visitors) > MaxDetailVisitors {
		return ErrFRPDetailTooLarge
	}
	if d.State != "ready" && (len(d.Proxies) != 0 || len(d.Visitors) != 0) {
		return errors.New("non-ready FRP detail must not contain partial lists")
	}
	seen := map[string]bool{}
	for _, p := range d.Proxies {
		if err := p.Validate(); err != nil {
			return err
		}
		if seen[p.Name] {
			return errors.New("duplicate FRP detail proxy")
		}
		seen[p.Name] = true
	}
	seen = map[string]bool{}
	for _, v := range d.Visitors {
		if err := v.Validate(); err != nil {
			return err
		}
		if seen[v.Name] {
			return errors.New("duplicate FRP detail visitor")
		}
		seen[v.Name] = true
	}
	data, err := json.Marshal(d)
	if err != nil {
		return errors.New("invalid FRP detail encoding")
	}
	if len(data) > MaxDetailBytes {
		return ErrFRPDetailTooLarge
	}
	return nil
}

func (d FRPTransportDetail) Validate() error {
	if !detailProtocol(d.Protocol) || !detailEnum(d.WireProtocol, "unknown", "v1", "v2") || !detailEnum(d.Source, "unknown", "configured", "observed") {
		return errors.New("invalid FRP transport detail")
	}
	return nil
}

func (d FRPConfigurationDetail) Validate() error {
	if !detailSource(d.Source, d.SourceState, d.ShadowedSources) || !detailEnum(d.NativeEntry, "file", "store", "unavailable") {
		return errors.New("invalid FRP configuration detail")
	}
	if d.Revision != nil {
		decoded, err := hex.DecodeString(*d.Revision)
		if err != nil || len(decoded) != 32 || strings.ToLower(*d.Revision) != *d.Revision {
			return errors.New("invalid FRP configuration revision")
		}
	}
	if d.ReadAt != nil && !detailTime(*d.ReadAt) {
		return errors.New("invalid FRP configuration read time")
	}
	return nil
}

func (e *FRPErrorDetail) Validate() error {
	if e == nil {
		return nil
	}
	if !detailEnum(e.Code, "unknown", "unavailable", "connect_failed", "login_failed", "register_failed", "health_check_failed", "listen_failed", "peer_failed", "nat_traversal_failed", "plugin_failed", "reload_failed") || !detailEnum(e.Stage, "control", "login", "proxy", "health_check", "visitor", "transport", "configuration") {
		return errors.New("invalid FRP error detail")
	}
	for _, at := range []*string{e.At, e.RecoveredAt} {
		if at != nil && !detailTime(*at) {
			return errors.New("invalid FRP error event time")
		}
	}
	if e.At != nil && e.RecoveredAt != nil {
		at, _ := time.Parse(time.RFC3339Nano, *e.At)
		recovered, _ := time.Parse(time.RFC3339Nano, *e.RecoveredAt)
		if recovered.Before(at) {
			return errors.New("FRP error recovery precedes error")
		}
	}
	return nil
}

func (e FRPEndpoint) Validate() error {
	if !detailEnum(e.Kind, "tcp", "udp", "http", "https", "tcpmux", "visitor") || !detailEnum(e.Source, "configured", "observed", "unknown") || !detailHost(e.Host, true) || (e.Port != nil && *e.Port == 0 && e.Source == "observed") {
		return errors.New("invalid FRP endpoint")
	}
	if !detailText(e.Path, 1024, true) || (e.Path != "" && (!strings.HasPrefix(e.Path, "/") || strings.ContainsAny(e.Path, "?#"))) {
		return errors.New("invalid FRP endpoint path")
	}
	if e.Subdomain != nil && (!detailHost(*e.Subdomain, false) || strings.ContainsAny(*e.Subdomain, ".:*[]") || e.Host != "" || e.Source != "configured") {
		return errors.New("invalid unresolved FRP subdomain")
	}
	if e.Kind == "visitor" && (e.Host != "" || e.Port != nil || e.Path != "" || e.Subdomain != nil) {
		return errors.New("private FRP endpoint must not invent a public listener")
	}
	if e.Path != "" && e.Kind != "http" {
		return errors.New("FRP path is only valid for HTTP routing")
	}
	if e.Subdomain != nil && !detailEnum(e.Kind, "http", "https", "tcpmux") {
		return errors.New("FRP subdomain requires virtual hosting")
	}
	return nil
}

func (p FRPProxyDetail) Validate() error {
	if !detailText(p.Name, 256, false) || !detailEnum(p.Type, "tcp", "udp", "http", "https", "tcpmux", "stcp", "sudp", "xtcp") || !detailSource(p.Source, p.SourceState, p.ShadowedSources) || !detailEnum(p.Status, "unknown", "disabled", "starting", "running", "error", "closed") {
		return errors.New("invalid FRP proxy detail")
	}
	if p.LocalTarget != nil && !detailTarget(*p.LocalTarget) {
		return errors.New("invalid FRP local target")
	}
	// These names are the pinned upstream proxy_plugin.go whitelist. Never
	// accept an arbitrary plugin identifier or serialize its native options.
	if p.PluginType != nil && !detailEnum(*p.PluginType, "http2https", "http_proxy", "https2http", "https2https", "http2http", "socks5", "static_file", "unix_domain_socket", "tls2raw", "virtual_net") {
		return errors.New("invalid FRP proxy plugin type")
	}
	if p.Endpoints == nil {
		return errors.New("invalid FRP endpoint count")
	}
	if len(p.Endpoints) > MaxDetailEndpoints {
		return ErrFRPDetailTooLarge
	}
	for _, endpoint := range p.Endpoints {
		if err := endpoint.Validate(); err != nil {
			return err
		}
		kind := p.Type
		if detailEnum(kind, "stcp", "sudp", "xtcp") {
			kind = "visitor"
		}
		if endpoint.Kind != kind {
			return errors.New("FRP endpoint kind does not match proxy")
		}
	}
	return p.Error.Validate()
}

func (v FRPVisitorDetail) Validate() error {
	if !detailText(v.Name, 256, false) || !detailEnum(v.Type, "stcp", "sudp", "xtcp") || !detailSource(v.Source, v.SourceState, v.ShadowedSources) || !detailEnum(v.Status, "unknown", "disabled", "configured", "starting", "listening", "connected", "error", "closed") {
		return errors.New("invalid FRP visitor detail")
	}
	if !detailText(v.ServerUser, 128, true) || !detailText(v.ServerName, 256, false) || !detailProtocol(v.Protocol) || (v.FallbackTo != nil && !detailText(*v.FallbackTo, 256, false)) {
		return errors.New("invalid FRP visitor target")
	}
	// The pinned upstream visitor_plugin.go only supports virtual_net.
	if v.PluginType != nil && *v.PluginType != "virtual_net" {
		return errors.New("invalid FRP visitor plugin type")
	}
	if !detailEnum(v.LocalState, "unknown", "configured", "listening", "disabled", "error", "closed") || !detailEnum(v.RemoteState, "unknown", "connecting", "connected", "error", "closed") || !detailEnum(v.P2PState, "unknown", "unsupported", "connecting", "connected", "failed") || !detailEnum(v.FallbackState, "unknown", "disabled", "available", "active", "failed") {
		return errors.New("invalid FRP visitor observation state")
	}
	if v.BindEndpoint != nil {
		if err := v.BindEndpoint.Validate(); err != nil {
			return err
		}
		kind := "tcp"
		if v.Type == "sudp" {
			kind = "udp"
		}
		if v.BindEndpoint.Kind != kind {
			return errors.New("invalid FRP visitor listener kind")
		}
	}
	return v.Error.Validate()
}

func detailEnum(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func detailProtocol(value string) bool {
	return detailEnum(value, "unknown", "tcp", "kcp", "quic", "websocket", "wss")
}

func detailSource(source, state string, shadowed []string) bool {
	if !detailEnum(source, "file", "include", "store", "mixed", "unknown") || !detailEnum(state, "active", "overridden", "conflict", "unknown") || len(shadowed) > 3 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range shadowed {
		if !detailEnum(item, "file", "include", "store") || seen[item] {
			return false
		}
		seen[item] = true
	}
	return true
}

func detailText(value string, max int, empty bool) bool {
	if !bounded(value, max, empty) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func detailHost(host string, empty bool) bool {
	if !detailText(host, 253, empty) || strings.ContainsAny(host, " /\\@?#%") {
		return false
	}
	for _, r := range host {
		if unicode.IsSpace(r) {
			return false
		}
	}
	return !strings.ContainsAny(host, ":[]") || net.ParseIP(host) != nil
}

func detailTarget(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || !detailHost(host, false) {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n != 0
}

func detailTime(value string) bool {
	t, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && len(value) <= 64 && t.Year() >= 1970
}
