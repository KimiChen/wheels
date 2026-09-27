package monitor

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func publicHardwareSnapshot(t *testing.T, facts *shared.Facts, metrics *shared.Metrics) (PublicSnapshot, []byte) {
	t.Helper()
	n := &node{credential: credential{AgentID: "test-node-1", Name: "Test node"}, facts: facts, metrics: metrics}
	s := &Service{cfg: shared.MonitorConfig{ReportIntervalSeconds: 1}, nodes: map[string]*node{"test-node-1": n}}
	snapshot := s.snapshot(time.Now())
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, data
}

func jsonKeys(object map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestPublicHardwareExplicitAllowlist(t *testing.T) {
	facts := fixture(t, "hello").Hello.Facts
	metrics := fixture(t, "report-first").Report.Metrics
	snapshot, encoded := publicHardwareSnapshot(t, &facts, metrics)
	var data struct {
		Nodes []map[string]json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	wantNodeKeys := []string{"freshness", "frp", "hardware", "id", "interval_seconds", "last_seen", "metrics", "metrics_at", "name", "session"}
	if got := jsonKeys(data.Nodes[0]); !reflect.DeepEqual(got, wantNodeKeys) {
		t.Fatalf("public node keys: %v", got)
	}
	var hardware map[string]json.RawMessage
	if err := json.Unmarshal(data.Nodes[0]["hardware"], &hardware); err != nil {
		t.Fatal(err)
	}
	wantHardwareKeys := []string{"agent_version", "arch", "cpu_cores", "cpu_name", "os", "virt"}
	if got := jsonKeys(hardware); !reflect.DeepEqual(got, wantHardwareKeys) {
		t.Fatalf("public hardware keys: %v", got)
	}
	want := &PublicHardware{OS: facts.OS, Arch: facts.Arch, Virt: facts.Virt, CPUName: facts.CPUName, CPUCores: facts.CPUCores, AgentVersion: facts.AgentVersion}
	if !reflect.DeepEqual(snapshot.Nodes[0].Hardware, want) {
		t.Fatalf("hardware values changed: %+v", snapshot.Nodes[0].Hardware)
	}
	for _, private := range []string{"hostname", "ipv4", "ipv6", "kernel", "boot_id", "iface", "raw_client_id", "local_target", "token", "example-node", "192.0.2.10", "6.0-example", "example-boot-id", "example-interface-hash"} {
		if strings.Contains(string(encoded), private) {
			t.Errorf("public hardware snapshot leaks %q", private)
		}
	}
}

func TestPublicHardwareMissingFactsIsNull(t *testing.T) {
	snapshot, encoded := publicHardwareSnapshot(t, nil, nil)
	n := snapshot.Nodes[0]
	if n.Hardware != nil || n.Metrics != nil || n.Session != "waiting" {
		t.Fatalf("waiting node fabricated observations: %+v", n)
	}
	if !strings.Contains(string(encoded), `"hardware":null`) {
		t.Fatalf("missing hardware must be explicit null: %s", encoded)
	}
}

func TestPublicHardwarePreservesQualityAndRealMetricZero(t *testing.T) {
	facts := fixture(t, "hello").Hello.Facts
	facts.OS = shared.Field[string]{Quality: shared.QualityUnavailable, Reason: "read_error"}
	facts.Arch = shared.Field[string]{Quality: shared.QualityUnsupported}
	facts.Virt = shared.Field[string]{Quality: shared.QualityWarmingUp, Reason: "no_baseline"}
	facts.CPUCores = shared.Field[uint32]{Quality: shared.QualityUnavailable, Reason: "read_error"}
	if err := facts.Validate(); err != nil {
		t.Fatal(err)
	}
	metrics := fixture(t, "report-first").Report.Metrics
	zero := float64(0)
	metrics.CPU = shared.Field[float64]{Value: &zero, Quality: shared.QualityOK}
	snapshot, encoded := publicHardwareSnapshot(t, &facts, metrics)
	n := snapshot.Nodes[0]
	if !reflect.DeepEqual(n.Hardware.OS, facts.OS) || !reflect.DeepEqual(n.Hardware.Arch, facts.Arch) || !reflect.DeepEqual(n.Hardware.Virt, facts.Virt) || !reflect.DeepEqual(n.Hardware.CPUCores, facts.CPUCores) {
		t.Fatalf("hardware quality was changed: %+v", n.Hardware)
	}
	if n.Metrics.CPU.Value == nil || *n.Metrics.CPU.Value != 0 || n.Metrics.CPU.Quality != shared.QualityOK {
		t.Fatal("valid zero CPU was treated as missing")
	}
	if n.Metrics.DiskUsed.Value == nil || *n.Metrics.DiskUsed.Value != "0" || n.Metrics.DiskUsed.Quality != shared.QualityOK {
		t.Fatal("valid zero byte count was treated as missing")
	}
	if n.Metrics.NetRX.Value != nil || n.Metrics.NetRX.Quality != shared.QualityWarmingUp || n.Metrics.NetRX.Reason != "no_baseline" {
		t.Fatal("warming-up metric became a fabricated zero")
	}
	for _, required := range []string{`"os":{"value":null,"quality":"unavailable","reason":"read_error"}`, `"arch":{"value":null,"quality":"unsupported"}`, `"cpu":{"value":0,"quality":"ok"}`, `"disk_used":{"value":"0","quality":"ok"}`} {
		if !strings.Contains(string(encoded), required) {
			t.Errorf("missing serialized field %s", required)
		}
	}
}
