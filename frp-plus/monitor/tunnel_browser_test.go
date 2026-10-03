package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

// Actual Handler/authentication, SQLite and VictoriaMetrics. Native lifecycle
// and counter samples are seeded; this test does not claim native forwarding.
func TestTunnelBrowserEndToEnd(t *testing.T) {
	helper, node := os.Getenv("FRP_TUNNEL_BROWSER_HELPER"), os.Getenv("FRP_CONFIG_E2E_NODE")
	if helper == "" || node == "" {
		t.Skip("tunnel browser requires helper and Node")
	}
	for _, path := range []string{helper, node} {
		info, err := os.Stat(path)
		if !filepath.IsAbs(path) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("browser paths must be absolute regular files")
		}
	}
	token, _ := randomToken()
	cfg := testControlConfig(t, "Tunnel browser fixture", token)
	secret := filepath.Join(filepath.Dir(cfg.DatabaseFile), "github.secret")
	if err := os.WriteFile(secret, []byte("synthetic-oauth-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubClientID, cfg.GitHubClientSecretFile = "synthetic-client", secret
	cfg.GitHubCallbackURL = "http://127.0.0.1:1/api/admin/v1/auth/github/callback"
	cfg.GitHubAdminUsers = []string{"operator"}
	cfg.HistoryDataPath = filepath.Join(filepath.Dir(cfg.DatabaseFile), "history")
	cfg.RetentionDays = 7
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.cfg.GitHubCallbackURL = "http://" + s.Address() + "/api/admin/v1/auth/github/callback"
	s.admin.github.callback, s.admin.github.client = s.cfg.GitHubCallbackURL, githubFixtureClient("operator")
	cookie, _ := login(t, s, "github-test")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	n, err := s.control.Get(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.control.SetBinding(ctx, "1", &shared.FRPBinding{ServerID: "example", User: "demo", RawClientID: "fixture-client"}, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	value := func(v string) *string { return &v }
	now := time.Now()
	epoch := "11111111-1111-4111-8111-111111111111"
	old := shared.TunnelObject{InstanceID: "22222222-2222-4222-8222-222222222222", Identity: shared.TunnelIdentity{ServerID: "example", User: "demo", RawClientID: value("fixture-client"), RawName: "browser-tcp", Kind: "proxy", Protocol: "tcp", Quality: "stable"}, State: shared.TunnelState{Status: "registered", LocalState: "unknown", RemoteState: "unknown", P2PState: "unknown", FallbackState: "unknown"}, Counters: shared.TunnelCounters{Quality: "ok", ConnectionsQuality: "ok", Epoch: epoch, Accounting: "connection_close", ByteScope: "forwarded_stream", Connections: value("1"), RXBytes: value("100"), TXBytes: value("50")}, CreatedAtMS: now.Add(-150 * time.Second).UnixMilli(), OperationRelation: "none"}
	closed := old
	closed.State.Status, closed.Counters.Connections = "closed", value("0")
	closedAt := now.Add(-100 * time.Second).UnixMilli()
	closed.ClosedAtMS = &closedAt
	current := old
	current.InstanceID, current.CreatedAtMS = "33333333-3333-4333-8333-333333333333", now.Add(-90*time.Second).UnixMilli()
	batch := control.TunnelBatch{CollectorEpoch: "44444444-4444-4444-8444-444444444444", ReceivedAtMS: now.UnixMilli(), Snapshot: shared.TunnelSnapshot{State: "ready", Source: "server", ProcessEpoch: epoch, CollectedAtMS: now.UnixMilli(), FirstSequence: "1", LastSequence: "3", DroppedEvents: "0", Objects: []shared.TunnelObject{current}, Events: []shared.TunnelEvent{{Sequence: "1", TimeBasis: "native", OccurredAtMS: old.CreatedAtMS, Code: "registered", Object: old}, {Sequence: "2", TimeBasis: "native", OccurredAtMS: closedAt, Code: "closed", Object: closed}, {Sequence: "3", TimeBasis: "native", OccurredAtMS: current.CreatedAtMS, Code: "registered", Object: current}}}}
	var mappings []control.TunnelMapping
	for tries := 0; tries < 20; tries++ {
		mappings, err = s.control.IngestTunnels(ctx, batch)
		if !errors.Is(err, control.ErrTunnelBusy) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil || len(mappings) != 2 || s.store == nil {
		t.Fatal("cannot seed real tunnel stores", err)
	}
	for _, mapping := range mappings {
		for index := 0; index < 4; index++ {
			c := old.Counters
			c.RXBytes, c.TXBytes = value(strconv.Itoa(100+index*100)), value(strconv.Itoa(50+index*50))
			at := now.Add(time.Duration(-75+index*2) * time.Second)
			if mapping.NativeInstanceID == old.InstanceID {
				at = at.Add(-70 * time.Second)
			}
			if !s.store.AcceptTunnel(mapping.InstanceID, at, c) {
				t.Fatal("history fixture was rejected")
			}
		}
	}
	if err = s.store.Flush(ctx); err != nil {
		t.Fatal("history fixture flush failed")
	}
	input, err := json.Marshal(map[string]any{"url": "http://" + s.Address(), "cookie": map[string]string{"name": cookie.Name, "value": cookie.Value}})
	if err != nil {
		t.Fatal("cannot encode private browser fixture")
	}
	root := t.TempDir()
	command := exec.CommandContext(ctx, node, helper)
	command.Dir, command.Stdin = root, bytes.NewReader(input)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root, "LANG=C", "LC_ALL=C", "PLAYWRIGHT_MODULE=" + os.Getenv("PLAYWRIGHT_MODULE"), "BROWSER_EXECUTABLE=" + os.Getenv("BROWSER_EXECUTABLE")}
	output, runErr := command.CombinedOutput()
	var report struct {
		OK    bool   `json:"ok"`
		Stage string `json:"stage"`
	}
	if len(output) > 1024 || json.Unmarshal(output, &report) != nil || !regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString(report.Stage) {
		t.Fatal("tunnel browser returned no bounded safe report")
	}
	if runErr != nil || !report.OK {
		t.Fatalf("tunnel browser failed at safe stage %q", report.Stage)
	}
	t.Log("actual tunnel UI/auth/SQLite/TSDB passed: stable identity, two generations, events, real curves, window switch, filters and logout")
}
