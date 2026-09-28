// Package shared defines version 1 of the independent FRP monitor protocol.
// It has no dependency on FRP's client/server packages or transport implementation.
package shared

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	SchemaVersion   = 1
	MaxFrameBytes   = 256 * 1024
	MaxStringBytes  = 1024
	MaxDepth        = 16
	MaxCapabilities = 32
	MaxProxies      = 512
	MaxProbeTasks   = 64
)

var ErrUnsupportedSchema = errors.New("unsupported schema")

// Quality distinguishes a genuine zero from the absence of a usable sample.
type Quality string

const (
	QualityOK          Quality = "ok"
	QualityWarmingUp   Quality = "warming_up"
	QualityUnavailable Quality = "unavailable"
	QualityUnsupported Quality = "unsupported"
)

// Field always carries both value and quality. A non-ok value must be null.
type Field[T any] struct {
	Value   *T      `json:"value"`
	Quality Quality `json:"quality"`
	Reason  string  `json:"reason,omitempty"`
}

// Meta identifies an observation, not an authenticated node. The receiver must
// derive node identity from the authenticated handshake and enforce session ownership.
type Meta struct {
	Schema      int    `json:"schema"`
	SessionID   string `json:"session_id"`
	Sequence    uint64 `json:"sequence"`
	CollectedAt string `json:"collected_at"`
}

type Facts struct {
	Hostname     Field[string] `json:"hostname"`
	OS           Field[string] `json:"os"`
	Kernel       Field[string] `json:"kernel"`
	Arch         Field[string] `json:"arch"`
	Virt         Field[string] `json:"virt"`
	CPUName      Field[string] `json:"cpu_name"`
	CPUCores     Field[uint32] `json:"cpu_cores"`
	AgentVersion Field[string] `json:"agent_version"`
	IPv4         Field[string] `json:"ipv4"`
	IPv6         Field[string] `json:"ipv6"`
	MemTotal     Field[uint64] `json:"mem_total"`
	SwapTotal    Field[uint64] `json:"swap_total"`
	DiskTotal    Field[uint64] `json:"disk_total"`
}

type Metrics struct {
	CPU        Field[float64]   `json:"cpu"`
	Load       Field[[]float64] `json:"load"`
	MemTotal   Field[uint64]    `json:"mem_total"`
	MemUsed    Field[uint64]    `json:"mem_used"`
	SwapTotal  Field[uint64]    `json:"swap_total"`
	SwapUsed   Field[uint64]    `json:"swap_used"`
	DiskTotal  Field[uint64]    `json:"disk_total"`
	DiskUsed   Field[uint64]    `json:"disk_used"`
	NetRX      Field[uint64]    `json:"net_rx"`
	NetTX      Field[uint64]    `json:"net_tx"`
	NetRXTotal Field[uint64]    `json:"net_rx_total"`
	NetTXTotal Field[uint64]    `json:"net_tx_total"`
	BootID     Field[string]    `json:"boot_id"`
	Iface      Field[string]    `json:"iface"`
	Uptime     Field[uint64]    `json:"uptime"`
	TCP        Field[uint64]    `json:"tcp"`
	UDP        Field[uint64]    `json:"udp"`
	Procs      Field[uint64]    `json:"procs"`
}

// FRPAssociation is a reconciliation hint, never proof of node identity.
// A nil RawClientID represents a client without a configured stable client ID.
type FRPAssociation struct {
	ServerID    string  `json:"server_id"`
	User        string  `json:"user"`
	RawClientID *string `json:"raw_client_id"`
}

type FRP struct {
	Association  FRPAssociation `json:"association"`
	Version      string         `json:"version"`
	ControlState string         `json:"control_state"`
	Proxies      []Proxy        `json:"proxies"`
}

type Proxy struct {
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	LocalTarget *string `json:"local_target"`
	Enabled     bool    `json:"enabled"`
	Status      string  `json:"status"`
}

type Extensions struct {
	FRP *FRP `json:"frp,omitempty"`
}

type Hello struct {
	Meta
	Capabilities []string    `json:"capabilities"`
	Facts        Facts       `json:"facts"`
	Extensions   *Extensions `json:"extensions,omitempty"`
}

// HelloResult is returned in a JSON-RPC success response with the request's ID.
// SessionID echoes the accepted session. Acceptance is conditional on authentication.
type HelloResult struct {
	Schema         int      `json:"schema"`
	SessionID      string   `json:"session_id"`
	Capabilities   []string `json:"capabilities"`
	ReportInterval uint32   `json:"report_interval"`
}

type Report struct {
	Meta
	Facts      *Facts      `json:"facts,omitempty"`
	Metrics    *Metrics    `json:"metrics,omitempty"`
	Extensions *Extensions `json:"extensions,omitempty"`
}

type PingTask struct {
	ID       string `json:"id"`
	Target   string `json:"target"`
	Interval uint32 `json:"interval"`
}

type PingTasks struct {
	Meta
	Version uint64     `json:"version"`
	Tasks   []PingTask `json:"tasks"`
}

type PingResult struct {
	Meta
	TaskVersion uint64  `json:"task_version"`
	TaskID      string  `json:"task_id"`
	LatencyMS   float64 `json:"latency_ms"`
}

// Frame contains exactly one of Hello, Report, PingTasks or PingResult.
// ID is present only for hello, which requires a JSON-RPC response.
type Frame struct {
	ID         string
	Method     string
	Hello      *Hello
	Report     *Report
	PingTasks  *PingTasks
	PingResult *PingResult
}

// DecodeFrame validates method frames for the v1 application profile of JSON-RPC
// 2.0. Batches, response frames, unknown fields/methods and numeric request IDs
// are outside this profile. It does not authenticate, track sessions or rate-limit.
func DecodeFrame(data []byte) (*Frame, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("frame must be UTF-8")
	}
	if len(data) > MaxFrameBytes {
		return nil, errors.New("frame exceeds byte limit")
	}
	if err := checkJSON(data); err != nil {
		return nil, err
	}
	var wire struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := decodeObject(data, &wire); err != nil {
		return nil, err
	}
	if wire.JSONRPC != "2.0" {
		return nil, errors.New("jsonrpc must be 2.0")
	}
	f := &Frame{Method: wire.Method}
	if wire.Method == "hello" {
		if len(wire.ID) == 0 || json.Unmarshal(wire.ID, &f.ID) != nil || !bounded(f.ID, 64, false) {
			return nil, errors.New("hello requires a non-empty string id of at most 64 bytes")
		}
	} else if len(wire.ID) != 0 {
		return nil, errors.New("only hello may carry id")
	}
	var err error
	switch wire.Method {
	case "hello":
		f.Hello = new(Hello)
		if err = decodeObject(wire.Params, f.Hello); err == nil {
			err = f.Hello.Validate()
		}
	case "report":
		f.Report = new(Report)
		if err = decodeObject(wire.Params, f.Report); err == nil {
			err = f.Report.Validate()
		}
	case "ping.tasks":
		f.PingTasks = new(PingTasks)
		if err = decodeObject(wire.Params, f.PingTasks); err == nil {
			err = f.PingTasks.Validate()
		}
	case "ping.result":
		f.PingResult = new(PingResult)
		if err = decodeObject(wire.Params, f.PingResult); err == nil {
			err = f.PingResult.Validate()
		}
	default:
		err = errors.New("unsupported method")
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func decodeObject(data []byte, dst any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return errors.New("expected JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := requireFields(data, reflect.TypeOf(dst).Elem()); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

// Token inspection catches duplicate keys, deep nesting, and case aliases that
// encoding/json would otherwise accept when matching struct field names.
func checkJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var visit func(int) error
	visit = func(depth int) error {
		if depth > MaxDepth {
			return errors.New("JSON nesting limit exceeded")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		if s, ok := token.(string); ok && len(s) > MaxStringBytes {
			return errors.New("JSON string limit exceeded")
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				keyToken, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || !validObjectKey(key) {
					return errors.New("invalid JSON object key")
				}
				if seen[key] {
					return errors.New("duplicate JSON object key")
				}
				seen[key] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

// Only ASCII keys are admitted because encoding/json folds Unicode case aliases.
// For example, U+017F (long s) can otherwise overwrite an ASCII "sequence" key.
func validObjectKey(key string) bool {
	if len(key) == 0 || len(key) > MaxStringBytes {
		return false
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '_' {
			return false
		}
	}
	return true
}

func bounded(s string, max int, empty bool) bool {
	return (empty || strings.TrimSpace(s) != "") && len(s) <= max && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func validateField[T any](name string, f Field[T], check func(T) bool) error {
	switch f.Reason {
	case "", "no_baseline", "read_error", "counter_reset":
	default:
		return fmt.Errorf("%s: invalid reason", name)
	}
	switch f.Quality {
	case QualityOK, QualityUnsupported:
		if f.Reason != "" {
			return fmt.Errorf("%s: reason incompatible with quality", name)
		}
	case QualityWarmingUp:
		if f.Reason != "" && f.Reason != "no_baseline" && f.Reason != "counter_reset" {
			return fmt.Errorf("%s: reason incompatible with quality", name)
		}
	case QualityUnavailable:
		if f.Reason != "" && f.Reason != "read_error" {
			return fmt.Errorf("%s: reason incompatible with quality", name)
		}
	}
	switch f.Quality {
	case QualityOK:
		if f.Value == nil || (check != nil && !check(*f.Value)) {
			return fmt.Errorf("%s: invalid ok value", name)
		}
	case QualityWarmingUp, QualityUnavailable, QualityUnsupported:
		if f.Value != nil {
			return fmt.Errorf("%s: non-ok value must be null", name)
		}
	default:
		return fmt.Errorf("%s: invalid or missing quality", name)
	}
	return nil
}
func validText(s string) bool    { return bounded(s, MaxStringBytes, false) }
func nonnegative(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }
func (m Meta) Validate() error {
	if m.Schema != SchemaVersion {
		return ErrUnsupportedSchema
	}
	if !bounded(m.SessionID, 128, false) || m.Sequence == 0 {
		return errors.New("invalid session_id or sequence")
	}
	t, err := time.Parse(time.RFC3339Nano, m.CollectedAt)
	if err != nil || t.Year() < 1970 {
		return errors.New("collected_at must be RFC3339 at or after 1970")
	}
	return nil
}
func (f Facts) Validate() error {
	for _, item := range []struct {
		name  string
		value Field[string]
	}{
		{"hostname", f.Hostname}, {"os", f.OS}, {"kernel", f.Kernel}, {"arch", f.Arch}, {"virt", f.Virt}, {"cpu_name", f.CPUName}, {"agent_version", f.AgentVersion},
	} {
		if err := validateField(item.name, item.value, validText); err != nil {
			return err
		}
	}
	if err := validateField("cpu_cores", f.CPUCores, func(n uint32) bool { return n > 0 }); err != nil {
		return err
	}
	if err := validateField("ipv4", f.IPv4, func(s string) bool { return net.ParseIP(s) != nil && net.ParseIP(s).To4() != nil }); err != nil {
		return err
	}
	if err := validateField("ipv6", f.IPv6, func(s string) bool { return net.ParseIP(s) != nil && net.ParseIP(s).To4() == nil }); err != nil {
		return err
	}
	for _, item := range []struct {
		name  string
		value Field[uint64]
	}{{"mem_total", f.MemTotal}, {"swap_total", f.SwapTotal}, {"disk_total", f.DiskTotal}} {
		if err := validateField(item.name, item.value, nil); err != nil {
			return err
		}
	}
	return nil
}
func (m Metrics) Validate() error {
	if err := validateField("cpu", m.CPU, func(n float64) bool { return nonnegative(n) && n <= 100 }); err != nil {
		return err
	}
	if err := validateField("load", m.Load, func(v []float64) bool {
		return len(v) == 3 && nonnegative(v[0]) && nonnegative(v[1]) && nonnegative(v[2])
	}); err != nil {
		return err
	}
	for _, item := range []struct {
		name  string
		value Field[uint64]
	}{
		{"mem_total", m.MemTotal}, {"mem_used", m.MemUsed}, {"swap_total", m.SwapTotal}, {"swap_used", m.SwapUsed}, {"disk_total", m.DiskTotal}, {"disk_used", m.DiskUsed},
		{"net_rx", m.NetRX}, {"net_tx", m.NetTX}, {"net_rx_total", m.NetRXTotal}, {"net_tx_total", m.NetTXTotal}, {"uptime", m.Uptime}, {"tcp", m.TCP}, {"udp", m.UDP}, {"procs", m.Procs},
	} {
		if err := validateField(item.name, item.value, nil); err != nil {
			return err
		}
	}
	if err := validateField("boot_id", m.BootID, validText); err != nil {
		return err
	}
	if err := validateField("iface", m.Iface, func(s string) bool { return bounded(s, MaxStringBytes, true) }); err != nil {
		return err
	}
	for _, pair := range [][2]Field[uint64]{{m.MemUsed, m.MemTotal}, {m.SwapUsed, m.SwapTotal}, {m.DiskUsed, m.DiskTotal}} {
		if pair[0].Value != nil && pair[1].Value != nil && *pair[0].Value > *pair[1].Value {
			return errors.New("used capacity exceeds total")
		}
	}
	if (m.NetRXTotal.Quality == QualityOK || m.NetTXTotal.Quality == QualityOK) && (m.BootID.Quality != QualityOK || m.Iface.Quality != QualityOK) {
		return errors.New("valid network counters require boot_id and iface")
	}
	return nil
}
func validateCapabilities(c []string) error {
	if c == nil || len(c) > MaxCapabilities {
		return errors.New("capabilities must be an array of at most 32 items")
	}
	seen := map[string]bool{}
	for _, value := range c {
		if !bounded(value, 64, false) || seen[value] {
			return errors.New("invalid or duplicate capability")
		}
		seen[value] = true
	}
	return nil
}
func (h Hello) Validate() error {
	if err := h.Meta.Validate(); err != nil {
		return err
	}
	if h.Sequence != 1 {
		return errors.New("hello sequence must be 1")
	}
	if err := validateCapabilities(h.Capabilities); err != nil {
		return err
	}
	if err := h.Facts.Validate(); err != nil {
		return err
	}
	return h.Extensions.Validate()
}
func (h HelloResult) Validate() error {
	if h.Schema != SchemaVersion {
		return ErrUnsupportedSchema
	}
	if !bounded(h.SessionID, 128, false) || h.ReportInterval < 1 || h.ReportInterval > 3600 {
		return errors.New("invalid hello result")
	}
	return validateCapabilities(h.Capabilities)
}
func (r Report) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return err
	}
	if r.Sequence < 2 {
		return errors.New("report must follow hello")
	}
	if r.Facts == nil && r.Metrics == nil && (r.Extensions == nil || r.Extensions.FRP == nil) {
		return errors.New("report must carry data")
	}
	if r.Facts != nil {
		if err := r.Facts.Validate(); err != nil {
			return err
		}
	}
	if r.Metrics != nil {
		if err := r.Metrics.Validate(); err != nil {
			return err
		}
	}
	return r.Extensions.Validate()
}
func (e *Extensions) Validate() error {
	if e == nil {
		return nil
	}
	if e.FRP == nil {
		return errors.New("extensions must carry frp")
	}
	f := e.FRP
	if !bounded(f.Association.ServerID, 128, false) || !bounded(f.Association.User, 128, true) || (f.Association.RawClientID != nil && !bounded(*f.Association.RawClientID, 128, false)) || !bounded(f.Version, 64, false) {
		return errors.New("invalid FRP association or version")
	}
	switch f.ControlState {
	case "unknown", "connecting", "connected", "disconnected", "error":
	default:
		return errors.New("invalid FRP control state")
	}
	if f.Proxies == nil || len(f.Proxies) > MaxProxies {
		return errors.New("proxies must be an array of at most 512 items")
	}
	seen := map[string]bool{}
	for _, p := range f.Proxies {
		if !bounded(p.Name, 256, false) || seen[p.Name] || (p.LocalTarget != nil && !bounded(*p.LocalTarget, MaxStringBytes, false)) {
			return errors.New("invalid or duplicate proxy")
		}
		seen[p.Name] = true
		switch p.Type {
		case "tcp", "udp", "http", "https", "tcpmux", "stcp", "sudp", "xtcp":
		default:
			return errors.New("invalid proxy type")
		}
		switch p.Status {
		case "unknown", "disabled", "starting", "running", "error", "closed":
		default:
			return errors.New("invalid proxy status")
		}
	}
	return nil
}
func (p PingTasks) Validate() error {
	if err := p.Meta.Validate(); err != nil {
		return err
	}
	if p.Version == 0 || p.Tasks == nil || len(p.Tasks) > MaxProbeTasks {
		return errors.New("tasks require positive version and array of at most 64 items")
	}
	seen := map[string]bool{}
	for _, t := range p.Tasks {
		if !bounded(t.ID, 128, false) || seen[t.ID] || !bounded(t.Target, 256, false) || t.Interval < 5 || t.Interval > 3600 {
			return errors.New("invalid or duplicate probe task")
		}
		seen[t.ID] = true
		host, port, err := net.SplitHostPort(t.Target)
		if err != nil || strings.TrimSpace(host) == "" || strings.ContainsAny(host, " \t\r\n/@\\") {
			return errors.New("probe target must be host:port")
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return errors.New("probe target requires port 1..65535")
		}
	}
	return nil
}
func (p PingResult) Validate() error {
	if err := p.Meta.Validate(); err != nil {
		return err
	}
	if p.Sequence < 2 || p.TaskVersion == 0 || !bounded(p.TaskID, 128, false) || (!nonnegative(p.LatencyMS) && p.LatencyMS != -1) || p.LatencyMS > 900 {
		return errors.New("invalid ping result")
	}
	return nil
}

// requireFields rejects omitted required fields (including zero-valued booleans),
// while allowing explicit null for pointer-valued observations.
func requireFields(data []byte, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return nil
		}
		typ = typ.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("required field cannot be null")
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Anonymous {
				if err := requireFields(data, f.Type); err != nil {
					return err
				}
				continue
			}
			tag := f.Tag.Get("json")
			name, options, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			value, present := fields[name]
			if !present {
				if options == "omitempty" {
					continue
				}
				return fmt.Errorf("missing field %s", name)
			}
			if err := requireFields(value, f.Type); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if typ == reflect.TypeOf(json.RawMessage{}) {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		for _, item := range items {
			if err := requireFields(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
