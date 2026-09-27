package store

import (
	"context"
	"errors"
	"flag"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func field[T any](v T) shared.Field[T] { return shared.Field[T]{Value: &v, Quality: shared.QualityOK} }
func metrics(t *testing.T) shared.Metrics {
	t.Helper()
	raw, err := os.ReadFile("../../shared/testdata/report-first.json")
	if err != nil {
		t.Fatal(err)
	}
	f, err := shared.DecodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	return *f.Report.Metrics
}
func privateDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(p, 0700); err != nil {
		t.Fatal(err)
	}
	return p
}
func openTest(t *testing.T) *Store { t.Helper(); return openPath(t, privateDir(t), 30) }
func openPath(t *testing.T, path string, retention int) *Store {
	t.Helper()
	s, err := Open(Config{Path: path, RetentionDays: retention})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
func flush(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}
func minute() time.Time { return time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute) }

func TestActualTSDBQualityMeansAndMissingBuckets(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	m.CPU = field(10.0)
	m.MemUsed = field(uint64(100))
	m.MemTotal = field(uint64(1000))
	if !s.Accept("1", at, m) {
		t.Fatal("accept")
	}
	m.CPU = field(20.0)
	m.MemUsed = field(uint64(200))
	s.Accept("1", at.Add(time.Second), m)
	m.CPU = shared.Field[float64]{Quality: shared.QualityUnavailable, Reason: "read_error"}
	s.Accept("1", at.Add(2*time.Second), m)
	flush(t, s)
	h, err := s.History(context.Background(), "1", at, at.Add(3*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := h.Points[0]
	if len(h.Points) != 3 || p.Samples != 3 || p.CoverageSeconds != 3 || p.Fields["cpu"].Samples != 2 || p.Fields["cpu"].CoverageSeconds != 2 || *p.Fields["cpu"].Value != "15" || *p.Fields["mem_used"].Value != "166" {
		t.Fatalf("wrong aggregate: %+v / %+v", p, p.Fields)
	}
	if h.Points[1].Fields["cpu"].Value != nil || h.Points[1].Samples != 0 {
		t.Fatal("missing bucket filled")
	}
	if err = s.scan(context.Background(), "1", "", at, at.Add(time.Minute), func(name string, _ int64, _ float64) error {
		if name == "net_rx_total" || name == "net_tx_total" {
			t.Errorf("unused lifetime counter stored in TSDB: %s", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Only the real VictoriaMetrics directory layout is created, no history SQLite.
	if _, err = os.Stat(filepath.Join(s.cfg.Path, "data")); err != nil {
		t.Fatal("missing TSDB data directory", err)
	}
}
func TestProbeFailureMeanTargetIsolationAndLatest(t *testing.T) {
	s := openTest(t)
	at := minute()
	s.AcceptProbe("1", "task-a", at, 10)
	s.AcceptProbe("1", "task-a", at.Add(time.Second), 30)
	s.AcceptProbe("1", "task-a", at.Add(2*time.Second), -1)
	s.AcceptProbe("1", "task-b", at, 800)
	flush(t, s)
	h, err := s.ProbeHistory(context.Background(), "1", "task-a", at, at.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if h.Samples != 3 || h.Failures != 1 || h.LatencyMS == nil || *h.LatencyMS != -1 || *h.Points[0].LatencyMS != 20 || h.Points[1].LatencyMS != nil {
		t.Fatal(h)
	}
}
func TestReopenKeepsHistoryAndOneActiveInstance(t *testing.T) {
	path := privateDir(t)
	s := openPath(t, path, 30)
	at := minute()
	s.Accept("8", at, metrics(t))
	flush(t, s)
	if _, err := Open(Config{Path: privateDir(t)}); err == nil {
		t.Fatal("multiple TSDB instances")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := openPath(t, path, 30)
	h, err := next.History(context.Background(), "8", at, at.Add(time.Minute), time.Minute)
	if err != nil || h.Points[0].Samples != 1 {
		t.Fatal(h, err)
	}
}
func TestRetentionRemovesOldPartitionOnReopen(t *testing.T) {
	path := privateDir(t)
	s := openPath(t, path, 90)
	old := time.Now().UTC().AddDate(0, -2, 0).Truncate(time.Minute)
	s.Accept("1", old, metrics(t))
	flush(t, s)
	h, err := s.History(context.Background(), "1", old, old.Add(time.Minute), time.Minute)
	if err != nil || h.Points[0].Samples != 1 {
		t.Fatal(h, err)
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := openPath(t, path, 1)
	h, err = next.History(context.Background(), "1", old, old.Add(time.Minute), time.Minute)
	if err != nil || h.Points[0].Samples != 0 {
		t.Fatal("expired partition survived", h, err)
	}
}
func TestFastSamplesAndDownsampling(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	for i := 0; i < 10; i++ {
		s.Accept("1", at.Add(time.Duration(i)*100*time.Millisecond), m)
	}
	flush(t, s)
	h, err := s.History(context.Background(), "1", at, at.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(h.Points[0].CoverageSeconds-1.9) > 0.000001 {
		t.Fatal(h.Points[0])
	}
	h, err = s.History(context.Background(), "1", at.Add(-7*24*time.Hour), at, time.Minute)
	if err != nil || len(h.Points) > 500 || h.StepSeconds <= 60 {
		t.Fatal(h, err)
	}
}
func TestQueueBackpressureCloseAndCancellation(t *testing.T) {
	s := &Store{queue: make(chan event, 1), stop: make(chan struct{}), done: make(chan struct{})}
	e := event{node: "1"}
	if !s.offer(e) || s.offer(e) || s.Status().Dropped != 1 || !s.Status().Degraded {
		t.Fatal(s.Status())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.closed = true
	if s.offer(e) {
		t.Fatal("closed store accepted")
	}
}
func TestInvalidInputAndQueryCancellation(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	for _, id := range []string{"node", "0", "01", "-1", "1\"", "9223372036854775808"} {
		if s.Accept(id, at, m) {
			t.Fatal(id)
		}
	}
	if s.AcceptProbe("1", "task", at, math.NaN()) || s.AcceptProbe("1", "task", at, -2) {
		t.Fatal("invalid probe accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.History(ctx, "1", at, at.Add(time.Minute), time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !s.Status().Degraded || s.Status().QueryErrors == 0 {
		t.Fatal(s.Status())
	}
	if _, err := s.History(context.Background(), "1", at.Add(-32*24*time.Hour), at, time.Minute); err == nil {
		t.Fatal("unbounded query")
	}
}
func TestPrivatePathLimitsAndMaskedOpenError(t *testing.T) {
	for _, cfg := range []Config{{}, {Path: "bad\npath"}, {Path: privateDir(t), RetentionDays: 366}, {Path: privateDir(t), QueueCapacity: 65537}} {
		if _, err := Open(cfg); err == nil {
			t.Fatal(cfg)
		}
	}
	p := privateDir(t)
	if err := os.Chmod(p, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: p}); err == nil {
		t.Fatal("public directory accepted")
	}
	target := privateDir(t)
	link := filepath.Join(privateDir(t), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: link}); err == nil {
		t.Fatal("symlink accepted")
	}
	path := privateDir(t)
	if err := os.WriteFile(filepath.Join(path, "flock.lock"), []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	// Invalid control paths are never included in exported status text.
	s := openTest(t)
	s.failure.Store(&[]string{"write_failed"}[0])
	if strings.Contains(s.String(), s.cfg.Path) || strings.Contains(s.Status().LastError, s.cfg.Path) {
		t.Fatal("path leaked")
	}
}
func TestConcurrentAcceptQueryClose(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Accept("1", at.Add(time.Duration(j)*time.Second), m)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3; i++ {
			_, _ = s.History(context.Background(), "1", at, at.Add(time.Minute), time.Minute)
		}
	}()
	wg.Wait()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Accept("1", at, m) {
		t.Fatal("closed store accepted")
	}
	if err := s.Flush(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
func TestExtremeFiniteValueDoesNotOverflowMean(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	m.Load = field([]float64{math.MaxFloat64, 1, 1})
	s.Accept("1", at, m)
	s.Accept("1", at.Add(time.Second), m)
	flush(t, s)
	h, err := s.History(context.Background(), "1", at, at.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if h.Points[0].Fields["load1"].Value == nil || strings.Contains(*h.Points[0].Fields["load1"].Value, "Inf") {
		t.Fatal(h.Points[0])
	}
}

// The native FRP executable uses Cobra rather than flag.Parse. Exercise that
// initialization state in a fresh process, before VM's process-global once runs.
func TestOpenWithUnparsedStandardFlagsAndCacheBudget(t *testing.T) {
	if os.Getenv("FRP_MONITOR_TSDB_UNPARSED") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestOpenWithUnparsedStandardFlagsAndCacheBudget$")
		cmd.Env = append(os.Environ(), "FRP_MONITOR_TSDB_UNPARSED=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("unparsed-flag initialization: %v\n%s", err, output)
		}
		return
	}
	previous := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet("frp-monitor", flag.ContinueOnError)
	previous.VisitAll(func(f *flag.Flag) { flag.CommandLine.Var(f.Value, f.Name, f.Usage) })
	if flag.Parsed() {
		t.Fatal("fixture already parsed")
	}
	s := openTest(t)
	var stats storage.Metrics
	s.db.UpdateMetrics(&stats)
	if !flag.Parsed() || stats.MetricIDCacheSizeMaxBytes > 64<<20 || stats.MetricIDCacheSizeMaxBytes == 0 {
		t.Fatalf("unbounded metric ID cache: %d", stats.MetricIDCacheSizeMaxBytes)
	}
}

func TestMemoryBudget(t *testing.T) {
	for _, tt := range []struct {
		limit uint64
		want  int
	}{
		{0, 0}, {63 << 20, 0}, {64 << 20, 8 << 20}, {128 << 20, 16 << 20},
		{512 << 20, 64 << 20}, {1 << 30, 128 << 20}, {16 << 30, 128 << 20},
	} {
		got, err := memoryBudget(tt.limit)
		if tt.want == 0 {
			if err == nil {
				t.Fatalf("unsafe budget for %d", tt.limit)
			}
			continue
		}
		if err != nil || got != tt.want || uint64(got) >= tt.limit {
			t.Fatalf("limit=%d budget=%d error=%v", tt.limit, got, err)
		}
	}
}
func TestLowMemoryOpenNeverExitsProcess(t *testing.T) {
	if mode := os.Getenv("FRP_MONITOR_TSDB_MEMORY_TEST"); mode != "" {
		readMemoryLimit = func() (uint64, error) {
			if mode == "unknown" {
				return 0, errors.New("unavailable")
			}
			if mode == "too-small" {
				return 32 << 20, nil
			}
			return 128 << 20, nil
		}
		s, err := Open(Config{Path: privateDir(t)})
		if mode != "small" {
			if err == nil {
				s.Close(context.Background())
				t.Fatal("unsafe memory limit accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := flag.Lookup("memory.allowedBytes").Value.String(); got != "16777216" && got != "16MiB" {
			t.Fatalf("small-memory budget: %s", got)
		}
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, mode := range []string{"unknown", "too-small", "small"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLowMemoryOpenNeverExitsProcess$")
		cmd.Env = append(os.Environ(), "FRP_MONITOR_TSDB_MEMORY_TEST="+mode)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("memory=%s: %v\n%s", mode, err, output)
		}
	}
}
