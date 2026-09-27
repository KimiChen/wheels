package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

func p2Config(t *testing.T) (shared.MonitorConfig, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	digest := sha256.Sum256([]byte(token))
	path := filepath.Join(dir, "credentials.json")
	data, _ := json.Marshal([]credential{{"test-node-1", "Public node", hex.EncodeToString(digest[:]), nil}})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return shared.MonitorConfig{Enabled: true, BindAddr: "127.0.0.1", ServerID: "example", CredentialsFile: path, ReportIntervalSeconds: 1, DatabaseFile: filepath.Join(dir, "history.sqlite"), RetentionDays: 7, ProbeTasksFile: filepath.Join(dir, "probes.json")}, token
}
func putTasks(t *testing.T, path string, version uint64, target string) {
	t.Helper()
	tasks := []configuredProbe{}
	if target != "" {
		tasks = append(tasks, configuredProbe{ID: "probe-one", Name: "公开标签", Target: target, Interval: 5})
	}
	data, _ := json.Marshal(map[string]any{"version": version, "nodes": []any{map[string]any{"agent_id": "test-node-1", "tasks": tasks}}})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func dialProbes(t *testing.T, s *Service, token string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	h := fixture(t, "hello").Hello
	h.SessionID = "p2-session"
	h.Capabilities = []string{"metrics.v1", "frp.v1", "ping.v1"}
	send(t, c, "hello", "hello-1", h)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	f, err := shared.DecodeFrame(data)
	if err != nil || f.PingTasks == nil || f.PingTasks.Version != 1 || len(f.PingTasks.Tasks) != 1 {
		t.Fatalf("missing tasks: %v %s", err, data)
	}
	return c
}
func historyResponse(t *testing.T, s *Service) publicHistory {
	t.Helper()
	res, err := http.Get("http://" + s.Address() + "/api/public/v1/nodes/test-node-1/history?window=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var h publicHistory
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&h) != nil {
		t.Fatal("invalid history response")
	}
	return h
}
func flushStore(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s.store == nil {
		t.Fatal("store unavailable")
	}
	if err := s.store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestP2TasksHistoryAndRestart(t *testing.T) {
	cfg, token := p2Config(t)
	putTasks(t, cfg.ProbeTasksFile, 1, "192.0.2.21:443")
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	c := dialProbes(t, s, token)
	p := shared.PingResult{Meta: shared.Meta{Schema: 1, SessionID: "p2-session", Sequence: 2, CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}, TaskVersion: 1, TaskID: "probe-one", LatencyMS: 12}
	send(t, c, "ping.result", "", p)
	r := fixture(t, "report-first").Report
	r.SessionID = "p2-session"
	r.Sequence = 3
	rx, tx := uint64(9007199254741000), uint64(9007199254742000)
	r.Metrics.NetRXTotal = shared.Field[uint64]{Value: &rx, Quality: shared.QualityOK}
	r.Metrics.NetTXTotal = shared.Field[uint64]{Value: &tx, Quality: shared.QualityOK}
	send(t, c, "report", "", r)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["test-node-1"].sequence == 3 })
	rx2, tx2 := rx+17, tx+23
	r.Sequence = 4
	r.Metrics.NetRXTotal.Value = &rx2
	r.Metrics.NetTXTotal.Value = &tx2
	send(t, c, "report", "", r)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["test-node-1"].sequence == 4 })
	flushStore(t, s)
	h := historyResponse(t, s)
	if h.Storage.State != "ready" || len(h.Probes) != 1 || h.Probes[0].Samples != 1 || *h.Probes[0].FailureRate != 0 {
		t.Fatalf("probe summary: %+v", h)
	}
	if h.Traffic.RXBytes == nil || *h.Traffic.RXBytes != "17" || *h.Traffic.TXBytes != "23" {
		t.Fatalf("traffic precision: %+v", h.Traffic)
	}
	encoded, _ := json.Marshal(h)
	for _, secret := range []string{"192.0.2.21", "target", "hostname", "boot_id", "iface", "raw_client_id", "token", "local_target"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("public history leaks %s", secret)
		}
	}
	putTasks(t, cfg.ProbeTasksFile, 2, "192.0.2.22:443")
	s.reloadTasks()
	p.Sequence = 5
	send(t, c, "ping.result", "", p) // Old in-flight result must not contaminate the replacement.
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["test-node-1"].sequence == 5 })
	flushStore(t, s)
	if historyResponse(t, s).Probes[0].Samples != 0 {
		t.Fatal("new target inherited old result")
	}
	p.Sequence = 6
	p.TaskVersion = 2
	p.LatencyMS = -1
	send(t, c, "ping.result", "", p)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.nodes["test-node-1"].sequence == 6 })
	flushStore(t, s)
	if got := historyResponse(t, s).Probes[0]; got.Samples != 1 || got.Failures != 1 || *got.FailureRate != 100 {
		t.Fatalf("failure summary: %+v", got)
	}
	c.Close()
	s.Close()
	restarted, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.snapshot(time.Now()).Nodes[0].Session != "waiting" {
		t.Fatal("persisted history became online")
	}
	if got := historyResponse(t, restarted); got.Probes[0].Samples != 1 || *got.Traffic.RXBytes != "17" {
		t.Fatal("history did not survive restart")
	}
	putTasks(t, cfg.ProbeTasksFile, 3, "")
	restarted.reloadTasks()
	if len(historyResponse(t, restarted).Probes) != 0 {
		t.Fatal("empty full list did not clear tasks")
	}
	putTasks(t, cfg.ProbeTasksFile, 2, "192.0.2.22:443")
	restarted.reloadTasks()
	if !restarted.taskError.Load() || restarted.tasks.Load().Version != 3 {
		t.Fatal("version rollback replaced valid tasks")
	}
}
func TestHistoryRoutesAndStoreFailureIsolation(t *testing.T) {
	cfg, _ := p2Config(t)
	cfg.ProbeTasksFile = ""
	publicDir := filepath.Join(filepath.Dir(cfg.DatabaseFile), "public")
	os.Mkdir(publicDir, 0755)
	cfg.DatabaseFile = filepath.Join(publicDir, "invalid.sqlite")
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("DB failure stopped listener: %v", err)
	}
	defer s.Close()
	if historyResponse(t, s).Storage.State != "degraded" {
		t.Fatal("database failure hidden")
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "unknown-node/history", 404}, {"GET", "test-node-1/history?window=forever", 400}, {"GET", "test-node-1/history?window=1h&window=7d", 400}, {"POST", "test-node-1/history", 405}} {
		req, _ := http.NewRequest(tc.method, "http://"+s.Address()+"/api/public/v1/nodes/"+tc.path, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Errorf("%s: %d", tc.path, res.StatusCode)
		}
	}
	for i := 0; i < cap(s.queries); i++ {
		s.queries <- struct{}{}
	}
	res, err := http.Get("http://" + s.Address() + "/api/public/v1/nodes/test-node-1/history")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatal("history query concurrency unbounded")
	}
	for i := 0; i < cap(s.queries); i++ {
		<-s.queries
	}
}
