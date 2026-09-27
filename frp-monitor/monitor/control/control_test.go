package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type fixture struct {
	t      *testing.T
	s      *Store
	clock  atomic.Int64
	cfg    Config
	serial int
}

func setup(t *testing.T, at time.Time, loc *time.Location) *fixture {
	t.Helper()
	f := &fixture{t: t}
	f.clock.Store(at.UnixMilli())
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f.cfg = Config{Path: filepath.Join(dir, "control.sqlite"), Location: loc, Now: func() time.Time { return time.UnixMilli(f.clock.Load()) }}
	f.s, err = Open(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.s.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *fixture) create(cfg NodeConfig) *Node {
	f.t.Helper()
	f.serial++
	n, err := f.s.CreateNode(context.Background(), cfg, fmt.Sprintf("%064x", f.serial), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func metrics(rx, tx uint64) shared.Metrics {
	return shared.Metrics{Scope: shared.ScopeHost,
		BootID:     shared.Field[string]{Value: ptr("boot-a"), Quality: shared.QualityOK},
		Iface:      shared.Field[string]{Value: ptr("eth0"), Quality: shared.QualityOK},
		NetRXTotal: shared.Field[uint64]{Value: &rx, Quality: shared.QualityOK},
		NetTXTotal: shared.Field[uint64]{Value: &tx, Quality: shared.QualityOK}}
}

func (f *fixture) sample(id string, at time.Time, rx, tx uint64) *Node {
	f.t.Helper()
	f.clock.Store(at.UnixMilli())
	if !f.s.Accept(id, at, metrics(rx, tx)) {
		f.t.Fatal("sample rejected")
	}
	if err := f.s.Flush(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	n, err := f.s.Get(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestAutoincrementDeletionAndProbeCleanup(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	n := f.create(DefaultNodeConfig("one"))
	if n.ID != "1" {
		t.Fatal(n.ID)
	}
	if err := f.s.WriteProbes(context.Background(), []byte(`{"version":2,"nodes":[{"agent_id":"1","tasks":[]}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteNode(context.Background(), n.ID); err != nil {
		t.Fatal(err)
	}
	n = f.create(DefaultNodeConfig("two"))
	if n.ID != "2" {
		t.Fatal("deleted ID reused", n.ID)
	}
	data, err := f.s.ReadProbes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var doc probeDocument
	if err = json.Unmarshal(data, &doc); err != nil || doc.Version != 3 || len(doc.Nodes) != 0 {
		t.Fatal(string(data), err)
	}
}

func TestWholePeriodMaxCalibrationAndModeChange(t *testing.T) {
	at := utc("2026-09-28T12:00:00Z")
	f := setup(t, at, time.UTC)
	n := f.create(DefaultNodeConfig("node"))
	f.sample(n.ID, at, 500, 500)
	n = f.sample(n.ID, at.Add(time.Second), 600, 580)
	if n.UsedBytes() != "100" {
		t.Fatal(n.UsedBytes())
	}
	var err error
	n, err = f.s.UpdateNode(context.Background(), n.ID, n.NodeConfig, ptr("150"), false, n.ConfigRevision)
	if err != nil || n.UsedBytes() != "150" || n.TrafficAdjustmentBytes != "50" {
		t.Fatal(n, err)
	}
	cfg := n.NodeConfig
	cfg.TrafficMode = "total"
	n, err = f.s.UpdateNode(context.Background(), n.ID, cfg, nil, false, n.ConfigRevision)
	if err != nil || n.UsedBytes() != "150" || n.TrafficAdjustmentBytes != "-30" {
		t.Fatal(n, err)
	}
	n = f.sample(n.ID, at.Add(2*time.Second), 610, 620)
	if n.UsedBytes() != "200" {
		t.Fatal(n.UsedBytes())
	}
	if _, err = f.s.UpdateNode(context.Background(), n.ID, cfg, nil, false, 1); !errors.Is(err, ErrConflict) {
		t.Fatal("stale config accepted", err)
	}
	if n.TrafficTodayRXBytes != "110" || n.TrafficTodayTXBytes != "120" {
		t.Fatal("calibration affected today", n)
	}
}

func TestMaxIsNotSumOfPerSampleMax(t *testing.T) {
	at := utc("2026-09-28T12:00:00Z")
	f := setup(t, at, time.UTC)
	n := f.create(DefaultNodeConfig("node"))
	f.sample(n.ID, at, 0, 0)
	f.sample(n.ID, at.Add(time.Second), 100, 0)
	n = f.sample(n.ID, at.Add(2*time.Second), 100, 100)
	if n.UsedBytes() != "100" {
		t.Fatal(n.UsedBytes())
	}
}

func TestAutomaticInterfaceSelectionPersistsAndTracksTopology(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "shared", "testdata", "report-first.json"))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := shared.DecodeFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	m := *frame.Report.Metrics
	if m.Iface.Quality != shared.QualityOK || m.Iface.Value == nil || *m.Iface.Value != "" {
		t.Fatal("fixture must use automatic interfaces")
	}
	at := utc("2026-09-28T12:00:00Z")
	f := setup(t, at, time.UTC)
	n := f.create(DefaultNodeConfig("node"))
	accept := func(offset time.Duration, rx, tx uint64) *Node {
		t.Helper()
		m.NetRXTotal.Value = ptr(rx)
		m.NetTXTotal.Value = ptr(tx)
		f.clock.Store(at.Add(offset).UnixMilli())
		if !f.s.Accept(n.ID, at.Add(offset), m) {
			t.Fatal("valid automatic interface report rejected")
		}
		if err := f.s.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		out, err := f.s.Get(context.Background(), n.ID)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	accept(0, 100, 200)
	n = accept(time.Second, 175, 225)
	if n.TrafficTodayRXBytes != "75" || n.TrafficTodayTXBytes != "25" || n.UsedBytes() != "75" || n.CounterInterface == nil || *n.CounterInterface != "" {
		t.Fatal(n)
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s, err = Open(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	n = accept(2*time.Second, 200, 250)
	if n.UsedBytes() != "100" {
		t.Fatal("empty interface baseline did not survive restart", n)
	}
	m.BootID.Value = ptr("same-boot/new-interface-set-hash")
	n = accept(3*time.Second, 10000, 10000)
	if n.UsedBytes() != "100" || !n.TrafficPeriodPartial {
		t.Fatal("topology change counted lifetime bytes", n)
	}
	m.Iface.Value = nil
	if f.s.Accept(n.ID, at.Add(4*time.Second), m) {
		t.Fatal("missing interface value accepted")
	}
}

func TestControllerDayUsesLocalTimezoneAndDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	f := setup(t, start, loc)
	n := f.create(DefaultNodeConfig("node"))
	f.sample(n.ID, start, 0, 0)
	// This local day lasts 23 hours. Only the final 15 minutes belong to Mar 9.
	end := time.Date(2026, 3, 9, 0, 15, 0, 0, loc)
	n = f.sample(n.ID, end, 5580, 2790)
	if *n.TrafficDay != "2026-03-09" || n.TrafficTodayRXBytes != "60" || n.TrafficTodayTXBytes != "30" {
		t.Fatal(n)
	}
	if !n.TrafficTodayPartial {
		t.Fatal("estimated crossing not marked partial")
	}
	if n.TrafficPeriodRXBytes != "5580" {
		t.Fatal(n.TrafficPeriodRXBytes)
	}
	// Positive UTC offset crosses its date before UTC does.
	sh, _ := time.LoadLocation("Asia/Shanghai")
	g := setup(t, utc("2026-09-28T15:59:59Z"), sh)
	p := g.create(DefaultNodeConfig("node"))
	g.sample(p.ID, utc("2026-09-28T15:59:59Z"), 100, 100)
	p = g.sample(p.ID, utc("2026-09-28T16:00:01Z"), 200, 300)
	if *p.TrafficDay != "2026-09-29" || p.TrafficTodayRXBytes != "50" || p.TrafficTodayTXBytes != "100" {
		t.Fatal(p)
	}
}

func TestMonthEndAndManualResetKeepBaseline(t *testing.T) {
	at := utc("2026-01-31T00:00:00Z")
	f := setup(t, at, time.UTC)
	cfg := DefaultNodeConfig("node")
	cfg.TrafficResetDay = 31
	n := f.create(cfg)
	f.sample(n.ID, at, 0, 0)
	n = f.sample(n.ID, utc("2026-02-28T00:00:00Z"), 1000, 500)
	if n.UsedBytes() != "0" || *n.TrafficPeriodEndAtMS != utc("2026-03-31T00:00:00Z").UnixMilli() {
		t.Fatal(n)
	}
	f.clock.Store(utc("2026-03-01T12:00:00Z").UnixMilli())
	var err error
	cfg = n.NodeConfig
	cfg.TrafficResetMode = "manual"
	n, err = f.s.UpdateNode(context.Background(), n.ID, cfg, ptr("100"), false, n.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	if n.TrafficPeriodEndAtMS != nil || n.UsedBytes() != "100" {
		t.Fatal(n)
	}
	f.clock.Store(utc("2026-03-01T12:00:10Z").UnixMilli())
	n, err = f.s.UpdateNode(context.Background(), n.ID, n.NodeConfig, nil, true, n.ConfigRevision)
	if err != nil || n.UsedBytes() != "0" || n.CounterRXBytes == nil || *n.CounterRXBytes != "1000" {
		t.Fatal(n, err)
	}
	if *n.TrafficPeriodStartAtMS != f.clock.Load() {
		t.Fatal(n)
	}
}

func TestManualResetSplitsPendingCounterDelta(t *testing.T) {
	at := utc("2026-09-28T12:00:00Z")
	f := setup(t, at, time.UTC)
	cfg := DefaultNodeConfig("node")
	cfg.TrafficResetMode = "manual"
	n := f.create(cfg)
	n = f.sample(n.ID, at, 100, 100)
	f.clock.Store(at.Add(5 * time.Second).UnixMilli())
	var err error
	n, err = f.s.UpdateNode(context.Background(), n.ID, n.NodeConfig, nil, true, n.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	n = f.sample(n.ID, at.Add(10*time.Second), 200, 200)
	if n.UsedBytes() != "50" || n.TrafficTodayRXBytes != "100" {
		t.Fatal(n)
	}
}

func TestCounterResetInvalidReportsAndRestart(t *testing.T) {
	at := utc("2026-09-28T12:00:00Z")
	f := setup(t, at, time.UTC)
	n := f.create(DefaultNodeConfig("node"))
	f.sample(n.ID, at, 100, 100)
	n = f.sample(n.ID, at.Add(time.Second), 200, 150)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = Open(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	n = f.sample(n.ID, at.Add(2*time.Second), 300, 200)
	if n.UsedBytes() != "200" {
		t.Fatal(n)
	}
	data, _ := json.Marshal(n)
	if strings.Contains(string(data), "online") || strings.Contains(string(data), "counter_boot") {
		t.Fatal("baseline leaked as live state")
	}
	m := metrics(10000, 10000)
	m.Scope = shared.ScopeNamespace
	f.clock.Store(at.Add(3 * time.Second).UnixMilli())
	if !f.s.Accept(n.ID, at.Add(3*time.Second), m) {
		t.Fatal("rejected")
	}
	if err = f.s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err = f.s.Get(context.Background(), n.ID)
	if err != nil || n.UsedBytes() != "200" {
		t.Fatal(n, err)
	}
	if f.s.Accept(n.ID, at.Add(4*time.Second), shared.Metrics{}) {
		t.Fatal("invalid report accepted")
	}
	n = f.sample(n.ID, at.Add(5*time.Second), 1, 1)
	if n.UsedBytes() != "200" || !n.TrafficPeriodPartial {
		t.Fatal(n)
	}
	if *n.CounterRXBytes != "1" {
		t.Fatal(n)
	}
}

func TestRollbackKeepsCounterAndUsageAtomic(t *testing.T) {
	at := utc("2026-09-28T12:00:00Z")
	f := setup(t, at, time.UTC)
	n := f.create(DefaultNodeConfig("node"))
	f.sample(n.ID, at, 100, 100)
	err := f.s.call(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_sample BEFORE UPDATE ON nodes WHEN NEW.counter_rx_bytes='200' BEGIN SELECT RAISE(ABORT,'test failure'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Store(at.Add(time.Second).UnixMilli())
	if !f.s.Accept(n.ID, at.Add(time.Second), metrics(200, 200)) {
		t.Fatal("rejected")
	}
	if err = f.s.Flush(context.Background()); err == nil {
		t.Fatal("failed transaction not reported")
	}
	if f.s.Healthy() {
		t.Fatal("failed accounting reported healthy")
	}
	n, err = f.s.Get(context.Background(), n.ID)
	if err != nil || *n.CounterRXBytes != "100" || n.UsedBytes() != "0" {
		t.Fatal(n, err)
	}
	err = f.s.call(context.Background(), func(tx *sql.Tx) error { _, err := tx.Exec("DROP TRIGGER reject_sample"); return err })
	if err != nil {
		t.Fatal(err)
	}
	n = f.sample(n.ID, at.Add(2*time.Second), 300, 300)
	if n.UsedBytes() != "200" {
		t.Fatal("retry duplicated/lost bytes", n)
	}
	if !f.s.Healthy() {
		t.Fatal("accounting did not recover")
	}
}

func TestValidationErrorsAreDistinctFromStorageFailures(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	cfg := DefaultNodeConfig("node")
	if _, err := f.s.CreateNode(context.Background(), cfg, "secret", nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	cfg.TrafficQuotaBytes = ptr("01")
	if _, err := f.s.CreateNode(context.Background(), cfg, strings.Repeat("a", 64), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.s.Get(context.Background(), "01"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.s.Get(context.Background(), "999"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	n := f.create(DefaultNodeConfig("node"))
	if _, err := f.s.CreateNode(context.Background(), n.NodeConfig, n.TokenSHA256, nil); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if !f.s.Healthy() {
		t.Fatal("bad administrator input degraded accounting")
	}
}

func TestBindingValidationMatchesTrustedConfiguration(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	n := f.create(DefaultNodeConfig("node"))
	for _, binding := range []*shared.FRPBinding{
		{ServerID: "server"},
		{ServerID: " ", RawClientID: "client"},
		{ServerID: "server", User: "user\n", RawClientID: "client"},
		{ServerID: "server", RawClientID: "client\r"},
	} {
		if _, err := f.s.CreateNode(context.Background(), n.NodeConfig, strings.Repeat("a", 64), binding); !errors.Is(err, ErrInvalid) {
			t.Fatalf("created invalid binding %+v: %v", binding, err)
		}
		if err := f.s.SetBinding(context.Background(), n.ID, binding, n.ConfigRevision); !errors.Is(err, ErrInvalid) {
			t.Fatalf("set invalid binding %+v: %v", binding, err)
		}
	}
	// Native FRP's default user is empty; it remains a valid trusted binding.
	binding := &shared.FRPBinding{ServerID: "server", RawClientID: "client"}
	if err := f.s.SetBinding(context.Background(), n.ID, binding, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	updated, err := f.s.Get(context.Background(), n.ID)
	if err != nil || updated.ConfigRevision != n.ConfigRevision+1 || *updated.Binding != *binding {
		t.Fatal(updated, err)
	}
	if err := f.s.SetBinding(context.Background(), n.ID, nil, updated.ConfigRevision); err != nil {
		t.Fatal("cannot clear binding", err)
	}
}

func TestNodeLimitIsTransactionalConflict(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	// Seed near the cap in one transaction; exercise the API for the boundary.
	err := f.s.call(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE ids(n) AS (
 SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<1023
) INSERT INTO nodes(name,token_sha256,created_at_ms,updated_at_ms)
 SELECT 'fixture',printf('%064x',n),0,0 FROM ids`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.s.CreateNode(context.Background(), DefaultNodeConfig("last"), strings.Repeat("a", 64), nil)
	if err != nil || n.ID != "1024" {
		t.Fatal(n, err)
	}
	if _, err = f.s.CreateNode(context.Background(), n.NodeConfig, strings.Repeat("b", 64), nil); !errors.Is(err, ErrConflict) {
		t.Fatal("limit must map to HTTP 409", err)
	}
	if err = f.s.DeleteNode(context.Background(), n.ID); err != nil {
		t.Fatal(err)
	}
	n, err = f.s.CreateNode(context.Background(), n.NodeConfig, strings.Repeat("b", 64), nil)
	if err != nil || n.ID != "1025" {
		t.Fatal("capacity did not recover without reusing an ID", n, err)
	}
}

func TestCanceledQueuedCallsDoNotReadWorkerResults(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	n := f.create(DefaultNodeConfig("node"))
	tests := []struct {
		name string
		run  func(context.Context) (bool, error)
	}{
		{"get", func(ctx context.Context) (bool, error) { v, e := f.s.Get(ctx, n.ID); return v == nil, e }},
		{"nodes", func(ctx context.Context) (bool, error) { v, e := f.s.Nodes(ctx); return v == nil, e }},
		{"probes", func(ctx context.Context) (bool, error) { v, e := f.s.ReadProbes(ctx); return v == nil, e }},
		{"create", func(ctx context.Context) (bool, error) {
			v, e := f.s.CreateNode(ctx, DefaultNodeConfig("queued"), strings.Repeat("c", 64), nil)
			return v == nil, e
		}},
		{"update", func(ctx context.Context) (bool, error) {
			v, e := f.s.UpdateNode(ctx, n.ID, n.NodeConfig, nil, false, n.ConfigRevision)
			return v == nil, e
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			blockDone := make(chan error, 1)
			go func() {
				blockDone <- f.s.call(context.Background(), func(*sql.Tx) error { close(started); <-release; return nil })
			}()
			<-started
			ctx, cancel := context.WithCancel(context.Background())
			type answer struct {
				empty bool
				err   error
			}
			result := make(chan answer, 1)
			go func() { empty, err := tc.run(ctx); result <- answer{empty, err} }()
			deadline := time.Now().Add(time.Second)
			for len(f.s.queue) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if len(f.s.queue) == 0 {
				cancel()
				close(release)
				<-blockDone
				t.Fatal("call did not enter queue")
			}
			cancel()
			out := <-result
			close(release)
			if err := <-blockDone; err != nil {
				t.Fatal(err)
			}
			if !out.empty || !errors.Is(out.err, context.Canceled) {
				t.Fatalf("canceled call exposed worker result: %+v", out)
			}
			if err := f.s.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Also race cancellation against actual result production. An errored call
	// must not return even a partial slice produced by its unfinished worker.
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(time.Microsecond); cancel() }()
		rows, err := f.s.Nodes(ctx)
		if err != nil && rows != nil {
			t.Fatal("canceled read returned partial worker data")
		}
		cancel()
	}
	if err := f.s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLargeCountersAndCredentialBindingUniqueness(t *testing.T) {
	at := utc("2026-09-28T12:00:00Z")
	f := setup(t, at, time.UTC)
	n := f.create(DefaultNodeConfig("node"))
	f.sample(n.ID, at, 0, 0)
	n = f.sample(n.ID, at.Add(time.Second), ^uint64(0), ^uint64(0))
	cfg := n.NodeConfig
	cfg.TrafficMode = "total"
	n, err := f.s.UpdateNode(context.Background(), n.ID, cfg, ptr("36893488147419103230"), false, n.ConfigRevision)
	if err != nil || n.UsedBytes() != "36893488147419103230" {
		t.Fatal(n, err)
	}
	if _, err = f.s.CreateNode(context.Background(), cfg, n.TokenSHA256, nil); err == nil {
		t.Fatal("duplicate token allowed")
	}
	b := &shared.FRPBinding{ServerID: "fixture", User: "test", RawClientID: "client"}
	if err = f.s.SetBinding(context.Background(), n.ID, b, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	p := f.create(DefaultNodeConfig("two"))
	if err = f.s.SetBinding(context.Background(), p.ID, b, p.ConfigRevision); err == nil {
		t.Fatal("duplicate binding allowed")
	}
	st, err := os.Stat(f.cfg.Path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
}
