package shared

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func fixtureFrame(t *testing.T, name string) *Frame {
	t.Helper()
	frame, err := DecodeFrame(fixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return frame
}
func encodeFrame(t *testing.T, method string, params any) []byte {
	t.Helper()
	m := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if method == "hello" {
		m["id"] = "test-id"
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func ok[T any](v T) Field[T]                           { return Field[T]{Value: &v, Quality: QualityOK} }
func missing[T any](q Quality, reason string) Field[T] { return Field[T]{Quality: q, Reason: reason} }

func TestGoldenFrames(t *testing.T) {
	for _, name := range []string{"hello", "report-first", "ping-tasks", "ping-result"} {
		t.Run(name, func(t *testing.T) { fixtureFrame(t, name) })
	}
	report := fixtureFrame(t, "report-first").Report
	if report.Metrics.CPU.Value != nil || report.Metrics.CPU.Quality != QualityWarmingUp {
		t.Fatal("first CPU sample must be unknown")
	}
	if *report.Metrics.NetRXTotal.Value != math.MaxUint64 {
		t.Fatal("uint64 precision lost")
	}
	if *report.Metrics.NetTXTotal.Value != 9007199254740993 {
		t.Fatal("integer above JS safe limit changed")
	}
}

func TestEnvelopeRejection(t *testing.T) {
	hello := string(fixture(t, "hello"))
	report := string(fixture(t, "report-first"))
	tests := []struct{ name, data string }{
		{"empty", ""}, {"batch", "[" + hello + "]"}, {"trailing", hello + " {}"},
		{"wrong_rpc", strings.Replace(hello, `"2.0"`, `"1.0"`, 1)},
		{"numeric_id", strings.Replace(hello, `"id": "hello-1"`, `"id": 1`, 1)},
		{"null_id", strings.Replace(hello, `"id": "hello-1"`, `"id": null`, 1)},
		{"missing_id", strings.Replace(hello, `"id": "hello-1",`, "", 1)},
		{"id_on_notification", strings.Replace(report, `"jsonrpc":`, `"id":"bad","jsonrpc":`, 1)},
		{"unknown_method", strings.Replace(hello, `"method": "hello"`, `"method": "exec"`, 1)},
		{"unknown_envelope", strings.Replace(hello, `"jsonrpc":`, `"extra":true,"jsonrpc":`, 1)},
		{"duplicate_key", strings.Replace(hello, `"schema": 1`, `"schema":1,"schema":1`, 1)},
		{"case_alias", strings.Replace(hello, `"schema": 1`, `"Schema":1`, 1)},
		{"unicode_alias_only", strings.Replace(report, `"sequence": 2`, `"ſequence":9`, 1)},
		{"unicode_alias_overwrite", strings.Replace(report, `"sequence": 2`, `"sequence":2,"ſequence":9`, 1)},
		{"identity_in_body", strings.Replace(hello, `"schema": 1`, `"agent_id":"spoofed","schema":1`, 1)},
		{"invalid_utf8", strings.Replace(hello, "example-node", string([]byte{255}), 1)},
		{"null_params", `{"jsonrpc":"2.0","method":"hello","id":"x","params":null}`},
		{"response", `{"jsonrpc":"2.0","id":"x","result":{}}`},
		{"null_user", strings.Replace(hello, `"user": ""`, `"user":null`, 1)},
		{"missing_quality", strings.Replace(hello, `"quality": "ok"`, `"unused":"ok"`, 1)},
		{"missing_null_value", strings.Replace(report, "\"value\": null,", "", 1)},
		{"negative_uint", strings.Replace(report, `18446744073709551615`, `-1`, 1)},
		{"uint_overflow", strings.Replace(report, `18446744073709551615`, `18446744073709551616`, 1)},
		{"fractional_uint", strings.Replace(report, `18446744073709551615`, `0.5`, 1)},
		{"nonfinite_json", strings.Replace(report, `18446744073709551615`, `NaN`, 1)},
		{"long_string", strings.Replace(hello, "example-node", strings.Repeat("a", MaxStringBytes+1), 1)},
		{"large_frame", strings.Repeat(" ", MaxFrameBytes+1)},
		{"deep", strings.Repeat("[", MaxDepth+2) + "0" + strings.Repeat("]", MaxDepth+2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeFrame([]byte(tt.data)); err == nil {
				t.Fatal("accepted invalid frame")
			}
		})
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Report)
	}{
		{"schema", func(r *Report) { r.Schema = 2 }},
		{"sequence_zero", func(r *Report) { r.Sequence = 0 }},
		{"sequence_hello", func(r *Report) { r.Sequence = 1 }},
		{"missing_session", func(r *Report) { r.SessionID = "" }},
		{"invalid_utf8_session", func(r *Report) { r.SessionID = string([]byte{0xff}) }},
		{"bad_time", func(r *Report) { r.CollectedAt = "yesterday" }},
		{"negative_cpu", func(r *Report) { r.Metrics.CPU = ok(-1.0) }},
		{"cpu_over_100", func(r *Report) { r.Metrics.CPU = ok(100.001) }},
		{"cpu_nan", func(r *Report) { r.Metrics.CPU = ok(math.NaN()) }},
		{"load_inf", func(r *Report) { r.Metrics.Load = ok([]float64{0, 1, math.Inf(1)}) }},
		{"load_short", func(r *Report) { r.Metrics.Load = ok([]float64{0, 1}) }},
		{"load_negative", func(r *Report) { r.Metrics.Load = ok([]float64{0, -1, 0}) }},
		{"known_missing", func(r *Report) { r.Metrics.CPU = missing[float64](QualityOK, "") }},
		{"unknown_zero", func(r *Report) { r.Metrics.CPU = ok(0.0); r.Metrics.CPU.Quality = QualityWarmingUp }},
		{"unknown_quality", func(r *Report) { r.Metrics.CPU.Quality = "stale" }},
		{"unknown_reason", func(r *Report) { r.Metrics.CPU.Reason = "exception" }},
		{"contradictory_reason", func(r *Report) { r.Metrics.CPU.Reason = "read_error" }},
		{"used_exceeds_total", func(r *Report) { r.Metrics.MemUsed = ok(uint64(math.MaxUint64)) }},
		{"scope_missing", func(r *Report) { r.Metrics.Scope = "" }},
		{"counter_scope_missing", func(r *Report) { r.Metrics.BootID = missing[string](QualityUnavailable, "read_error") }},
		{"empty_report", func(r *Report) { r.Metrics = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fixtureFrame(t, "report-first").Report
			tt.mutate(r)
			if err := r.Validate(); err == nil {
				t.Fatal("accepted invalid report")
			}
		})
	}
	r := fixtureFrame(t, "report-first").Report
	r.Schema = 99
	if !errors.Is(r.Validate(), ErrUnsupportedSchema) {
		t.Fatal("unsupported schema must be recognizable")
	}
	r = fixtureFrame(t, "report-first").Report
	r.Metrics.CPU = ok(0.0)
	r.Metrics.NetRX = ok(uint64(0))
	r.Metrics.NetTX = ok(uint64(0))
	if err := r.Validate(); err != nil {
		t.Fatalf("genuine zero must be valid: %v", err)
	}
	if _, err := DecodeFrame(encodeFrame(t, "report", r)); err != nil {
		t.Fatal(err)
	}
	r.Metrics.CPU = ok(100.0)
	if err := r.Validate(); err != nil {
		t.Fatalf("CPU 100 must be valid: %v", err)
	}
}

func TestProbeLimits(t *testing.T) {
	base := fixtureFrame(t, "ping-tasks").PingTasks
	tests := []struct {
		name   string
		mutate func(*PingTasks)
		valid  bool
	}{
		{"empty_complete_list", func(p *PingTasks) { p.Tasks = []PingTask{} }, true},
		{"null_list", func(p *PingTasks) { p.Tasks = nil }, false},
		{"64_tasks", func(p *PingTasks) {
			task := p.Tasks[0]
			for i := 1; i < 64; i++ {
				task.ID = strconv.Itoa(i)
				p.Tasks = append(p.Tasks, task)
			}
		}, true},
		{"65_tasks", func(p *PingTasks) {
			task := p.Tasks[0]
			for i := 1; i < 65; i++ {
				task.ID = strconv.Itoa(i)
				p.Tasks = append(p.Tasks, task)
			}
		}, false},
		{"duplicate", func(p *PingTasks) { p.Tasks = append(p.Tasks, p.Tasks[0]) }, false},
		{"interval_4", func(p *PingTasks) { p.Tasks[0].Interval = 4 }, false},
		{"interval_3600", func(p *PingTasks) { p.Tasks[0].Interval = 3600 }, true},
		{"interval_3601", func(p *PingTasks) { p.Tasks[0].Interval = 3601 }, false},
		{"invalid_target", func(p *PingTasks) { p.Tasks[0].Target = "https://example.invalid" }, false},
		{"whitespace_host", func(p *PingTasks) { p.Tasks[0].Target = "bad host:443" }, false},
		{"ipv6_target", func(p *PingTasks) { p.Tasks[0].Target = "[2001:db8::1]:443" }, true},
		{"zero_port", func(p *PingTasks) { p.Tasks[0].Target = "example.invalid:0" }, false},
		{"large_port", func(p *PingTasks) { p.Tasks[0].Target = "example.invalid:65536" }, false},
		{"zero_version", func(p *PingTasks) { p.Version = 0 }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := *base
			p.Tasks = append([]PingTask{}, base.Tasks...)
			tt.mutate(&p)
			_, err := DecodeFrame(encodeFrame(t, "ping.tasks", p))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
		})
	}
	for _, value := range []float64{-2, -0.5, 900.1, math.NaN(), math.Inf(1)} {
		p := fixtureFrame(t, "ping-result").PingResult
		p.LatencyMS = value
		if p.Validate() == nil {
			t.Errorf("accepted latency %v", value)
		}
	}
	for _, value := range []float64{-1, 0, 899.9, 900} {
		p := fixtureFrame(t, "ping-result").PingResult
		p.LatencyMS = value
		if err := p.Validate(); err != nil {
			t.Errorf("rejected latency %v: %v", value, err)
		}
	}
}

func TestFRPAndHelloLimits(t *testing.T) {
	h := fixtureFrame(t, "hello").Hello
	h.Extensions.FRP.Association.RawClientID = nil
	p := Proxy{Name: "example-proxy", Type: "tcp", Enabled: false, Status: "disabled"}
	h.Extensions.FRP.Proxies = []Proxy{p}
	data := encodeFrame(t, "hello", h)
	if _, err := DecodeFrame(data); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{`"enabled":null,`, ""} {
		invalid := strings.Replace(string(data), `"enabled":false,`, replacement, 1)
		if _, err := DecodeFrame([]byte(invalid)); err == nil {
			t.Fatalf("accepted enabled replacement %q", replacement)
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*Hello)
	}{
		{"wrong_first_sequence", func(h *Hello) { h.Sequence = 2 }},
		{"missing_capabilities", func(h *Hello) { h.Capabilities = nil }},
		{"duplicate_capability", func(h *Hello) { h.Capabilities = []string{"metrics.v1", "metrics.v1"} }},
		{"capability_limit", func(h *Hello) { h.Capabilities = make([]string, MaxCapabilities+1) }},
		{"invalid_ipv4", func(h *Hello) { h.Facts.IPv4 = ok("2001:db8::1") }},
		{"invalid_ipv6", func(h *Hello) { h.Facts.IPv6 = ok("192.0.2.1") }},
		{"zero_cores", func(h *Hello) { h.Facts.CPUCores = ok(uint32(0)) }},
		{"null_proxy_list", func(h *Hello) { h.Extensions.FRP.Proxies = nil }},
		{"proxy_limit", func(h *Hello) { h.Extensions.FRP.Proxies = make([]Proxy, MaxProxies+1) }},
		{"duplicate_proxy", func(h *Hello) { h.Extensions.FRP.Proxies = []Proxy{p, p} }},
		{"invalid_type", func(h *Hello) { v := p; v.Type = "shell"; h.Extensions.FRP.Proxies = []Proxy{v} }},
		{"invalid_status", func(h *Hello) { v := p; v.Status = "happy"; h.Extensions.FRP.Proxies = []Proxy{v} }},
		{"empty_stable_id", func(h *Hello) { v := ""; h.Extensions.FRP.Association.RawClientID = &v }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := fixtureFrame(t, "hello").Hello
			test.mutate(h)
			if h.Validate() == nil {
				t.Fatal("accepted invalid hello")
			}
		})
	}
}

func TestBrowserPrecision(t *testing.T) {
	m := fixtureFrame(t, "report-first").Report.Metrics
	dto, err := m.Browser()
	if err != nil {
		t.Fatal(err)
	}
	if *dto.NetRXTotal.Value != "18446744073709551615" || *dto.NetTXTotal.Value != "9007199254740993" {
		t.Fatal("browser integer precision lost")
	}
	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"value":"18446744073709551615"`) {
		t.Fatal("browser counter must be JSON string")
	}
	if dto.CPU.Value != nil || dto.NetRX.Value != nil || dto.NetRX.Quality != QualityWarmingUp {
		t.Fatal("unknown value was coerced")
	}
	f := fixtureFrame(t, "hello").Hello.Facts
	f.DiskTotal = ok(uint64(math.MaxUint64))
	facts, err := f.Browser()
	if err != nil {
		t.Fatal(err)
	}
	if *facts.DiskTotal.Value != "18446744073709551615" {
		t.Fatal("facts capacity precision lost")
	}
}

func FuzzDecodeFrame(f *testing.F) {
	for _, name := range []string{"hello", "report-first", "ping-tasks", "ping-result"} {
		data, err := os.ReadFile("testdata/" + name + ".json")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = DecodeFrame(data) })
}

func TestExactBoundaries(t *testing.T) {
	h := fixtureFrame(t, "hello").Hello
	h.Capabilities = []string{}
	for i := 0; i < MaxCapabilities; i++ {
		h.Capabilities = append(h.Capabilities, strconv.Itoa(i))
	}
	h.Facts.Hostname = ok(strings.Repeat("x", MaxStringBytes))
	h.Extensions.FRP.Proxies = []Proxy{}
	for i := 0; i < MaxProxies; i++ {
		h.Extensions.FRP.Proxies = append(h.Extensions.FRP.Proxies, Proxy{Name: strconv.Itoa(i), Type: "tcp", Enabled: true, Status: "running"})
	}
	data := encodeFrame(t, "hello", h)
	if len(data) >= MaxFrameBytes {
		t.Fatal("test fixture accidentally exceeds frame budget")
	}
	if _, err := DecodeFrame(data); err != nil {
		t.Fatalf("exact array/string limits: %v", err)
	}
	data = append(data, []byte(strings.Repeat(" ", MaxFrameBytes-len(data)))...)
	if _, err := DecodeFrame(data); err != nil {
		t.Fatalf("exact frame size: %v", err)
	}
	if _, err := DecodeFrame(append(data, ' ')); err == nil {
		t.Fatal("accepted frame above byte limit")
	}
}

func TestHelloResult(t *testing.T) {
	result := HelloResult{Schema: 1, SessionID: "example-session", Capabilities: []string{}, ReportInterval: 1}
	for _, interval := range []uint32{1, 3600} {
		result.ReportInterval = interval
		if err := result.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, interval := range []uint32{0, 3601} {
		result.ReportInterval = interval
		if result.Validate() == nil {
			t.Fatal("accepted invalid report interval")
		}
	}
	result.Schema = 2
	if !errors.Is(result.Validate(), ErrUnsupportedSchema) {
		t.Fatal("missing unsupported schema error")
	}
}
