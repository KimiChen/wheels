package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

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
	m := *f.Report.Metrics
	m.NetRXTotal = field(uint64(100))
	m.NetTXTotal = field(uint64(200))
	m.BootID = field("boot-a")
	m.Iface = field("eth0")
	return m
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
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Config{Path: filepath.Join(privateDir(t), "history.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}
func flush(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func minute() time.Time { return time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute) }

func TestAverageCoverageGapsAndPrecision(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	m.CPU = field(10.0)
	m.MemUsed = field(uint64(math.MaxUint64))
	m.MemTotal = field(uint64(math.MaxUint64))
	if !s.Accept("node", at, m) {
		t.Fatal("accept")
	}
	m.CPU = field(20.0)
	m.MemUsed = field(uint64(math.MaxUint64 - 1))
	s.Accept("node", at.Add(time.Second), m)
	m.CPU = shared.Field[float64]{Quality: shared.QualityUnavailable, Reason: "read_error"}
	s.Accept("node", at.Add(2*time.Second), m)
	flush(t, s)
	h, err := s.History(context.Background(), "node", at, at.Add(3*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Points) != 3 {
		t.Fatal(h)
	}
	p := h.Points[0]
	if p.Samples != 3 || p.CoverageSeconds != 3 || p.Fields["cpu"].Samples != 2 || p.Fields["cpu"].CoverageSeconds != 2 || *p.Fields["cpu"].Value != "15" {
		t.Fatal(p)
	}
	if *p.Fields["mem_used"].Value != "18446744073709551614" {
		t.Fatal(p.Fields["mem_used"])
	}
	if h.Points[1].Fields["cpu"].Value != nil || h.Points[1].Samples != 0 {
		t.Fatal("gap filled")
	}
	// A second partial flush of the same minute merges samples, not means.
	m.CPU = field(90.0)
	s.Accept("node", at.Add(3*time.Second), m)
	flush(t, s)
	h, err = s.History(context.Background(), "node", at, at.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if *h.Points[0].Fields["cpu"].Value != "40" {
		t.Fatal(h)
	}
}
func TestDownsampleAndBounds(t *testing.T) {
	s := openTest(t)
	at := minute()
	h, err := s.History(context.Background(), "node", at.Add(-7*24*time.Hour), at, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Points) > 500 {
		t.Fatal(len(h.Points))
	}
	if _, err = s.History(context.Background(), "node", at.Add(-32*24*time.Hour), at, time.Minute); err == nil {
		t.Fatal("range unbounded")
	}
	if _, err = s.History(context.Background(), "node", at, at, -time.Second); err == nil {
		t.Fatal("negative step")
	}
}
func TestTrafficRecoveryResetsFailureAndExactness(t *testing.T) {
	dir := privateDir(t)
	cfg := Config{Path: filepath.Join(dir, "history.db")}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := minute()
	m := metrics(t)
	m.NetRXTotal = field(uint64(9007199254740993))
	m.NetTXTotal = field(uint64(math.MaxUint64 - 100))
	s.Accept("node", at, m)
	flush(t, s)
	r, _ := s.Traffic(context.Background(), "node", at)
	if r.RXBytes != nil {
		t.Fatal("first sample counted")
	}
	m.NetRXTotal = field(uint64(9007199254741004))
	m.NetTXTotal = field(uint64(math.MaxUint64 - 95))
	s.Accept("node", at.Add(time.Second), m)
	flush(t, s)
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	m.NetRXTotal = field(uint64(9007199254741008))
	m.NetTXTotal = field(uint64(math.MaxUint64 - 90))
	s.Accept("node", at.Add(2*time.Second), m)
	bad := m
	bad.NetRXTotal = shared.Field[uint64]{Quality: shared.QualityUnavailable, Reason: "read_error"}
	s.Accept("node", at.Add(3*time.Second), bad)
	m.NetRXTotal = field(uint64(9007199254741011))
	m.NetTXTotal = field(uint64(math.MaxUint64 - 80))
	s.Accept("node", at.Add(4*time.Second), m)
	m.BootID = field("boot-b")
	m.NetRXTotal = field(uint64(5))
	m.NetTXTotal = field(uint64(6))
	s.Accept("node", at.Add(5*time.Second), m)
	m.Iface = field("eth1")
	m.NetRXTotal = field(uint64(50))
	m.NetTXTotal = field(uint64(60))
	s.Accept("node", at.Add(6*time.Second), m)
	m.NetRXTotal = field(uint64(2))
	m.NetTXTotal = field(uint64(3))
	s.Accept("node", at.Add(7*time.Second), m)
	m.NetRXTotal = field(uint64(2))
	m.NetTXTotal = field(uint64(3))
	s.Accept("node", at.Add(8*time.Second), m)
	flush(t, s)
	r, err = s.Traffic(context.Background(), "node", at)
	if err != nil {
		t.Fatal(err)
	}
	if r.RXBytes == nil || *r.RXBytes != "18" || *r.TXBytes != "20" || r.Resets != 3 || r.CoverageSeconds != 5 {
		t.Fatalf("%+v", r)
	}
}
func TestTrafficCrossDayReceivingDay(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	m := metrics(t)
	s.Accept("node", at.Add(-time.Second), m)
	m.NetRXTotal = field(uint64(125))
	m.NetTXTotal = field(uint64(250))
	s.Accept("node", at.Add(time.Second), m)
	flush(t, s)
	y, _ := s.Traffic(context.Background(), "node", at.Add(-time.Second))
	d, _ := s.Traffic(context.Background(), "node", at)
	if y.RXBytes != nil || d.RXBytes == nil || *d.RXBytes != "25" || *d.TXBytes != "50" {
		t.Fatal(y, d)
	}
}
func TestProbeAverageFailureAndGap(t *testing.T) {
	s := openTest(t)
	at := minute()
	s.AcceptProbe("node", "task", at, 10)
	s.AcceptProbe("node", "task", at.Add(time.Second), -1)
	s.AcceptProbe("node", "task", at.Add(2*time.Second), 30)
	flush(t, s)
	h, err := s.ProbeHistory(context.Background(), "node", "task", at, at.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if h.Samples != 3 || h.Failures != 1 || h.LatencyMS == nil || *h.LatencyMS != 30 || *h.Points[0].LatencyMS != 20 || h.Points[1].LatencyMS != nil {
		t.Fatal(h)
	}
}
func TestNoWritesBeforeFlushAndAtomicTrafficFailure(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	s.Accept("node", at, m)
	m.NetRXTotal = field(uint64(111))
	s.Accept("node", at.Add(time.Second), m)
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM metrics_1m").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	// An abort on baseline write must also roll back the already attempted daily write.
	if _, err := s.db.Exec("CREATE TRIGGER fail_state BEFORE INSERT ON traffic_state BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if err := s.db.QueryRow("SELECT count(*) FROM traffic_daily").Scan(&n); err != nil || n != 0 {
		t.Fatal("not atomic", n, err)
	}
	if !s.Status().Degraded || s.Status().WriteErrors != 1 {
		t.Fatal(s.Status())
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_state"); err != nil {
		t.Fatal(err)
	}
	flush(t, s)
	r, _ := s.Traffic(context.Background(), "node", at)
	if r.RXBytes == nil || *r.RXBytes != "11" {
		t.Fatal(r)
	}
}
func TestQueueOverflowReturnsPromptly(t *testing.T) {
	s := &Store{queue: make(chan event, 1)}
	s.queue <- event{}
	start := time.Now()
	if s.AcceptProbe("node", "task", minute(), 1) {
		t.Fatal("queue not bounded")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("blocked")
	}
	if s.Status().Dropped != 1 || s.Status().LastError != "queue_full" {
		t.Fatal(s.Status())
	}
}
func TestConcurrentAcceptQueryClose(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Accept("node", at.Add(time.Duration(j)*time.Second), m)
				s.Status()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 4; j++ {
			s.History(context.Background(), "node", at, at.Add(time.Minute), time.Minute)
		}
	}()
	wg.Wait()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Accept("node", at, m) {
		t.Fatal("accepted after close")
	}
	if !errors.Is(s.Flush(context.Background()), ErrClosed) {
		t.Fatal("flush after close")
	}
}
func TestRetentionAndUnknownSchema(t *testing.T) {
	s := openTest(t)
	m := metrics(t)
	s.Accept("node", time.Now().UTC().Add(-8*24*time.Hour), m)
	flush(t, s)
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM metrics_1m").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	p := s.cfg.Path
	s.Close(context.Background())
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("PRAGMA user_version=999")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if next, err := Open(Config{Path: p}); err == nil {
		next.Close(context.Background())
		t.Fatal("future schema accepted")
	}
}
func TestPrivateFilesAndUnsafePaths(t *testing.T) {
	s := openTest(t)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		st, err := os.Stat(s.cfg.Path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0077 != 0 {
			t.Fatal("public database sidecar", suffix, st.Mode())
		}
	}
	dir := privateDir(t)
	target := filepath.Join(dir, "target")
	os.WriteFile(target, nil, 0600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if st, err := Open(Config{Path: link}); err == nil {
		st.Close(context.Background())
		t.Fatal("symlink accepted")
	}
	os.Chmod(target, 0644)
	if st, err := Open(Config{Path: target}); err == nil {
		st.Close(context.Background())
		t.Fatal("public db accepted")
	}
}
func TestWALCrashRecovery(t *testing.T) {
	if path := os.Getenv("FRP_MONITOR_WAL_CHILD"); path != "" {
		s, err := Open(Config{Path: path})
		if err != nil {
			panic(err)
		}
		m := metrics(t)
		s.Accept("node", minute(), m)
		m.NetRXTotal = field(uint64(123))
		s.Accept("node", minute().Add(time.Second), m)
		if err = s.Flush(context.Background()); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	path := filepath.Join(privateDir(t), "crash.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWALCrashRecovery$")
	cmd.Env = append(os.Environ(), "FRP_MONITOR_WAL_CHILD="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	st, err := os.Stat(path + "-wal")
	if err != nil || st.Size() == 0 {
		t.Fatal("child did not leave a WAL", err)
	}
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	r, err := s.Traffic(context.Background(), "node", minute())
	if err != nil || r.RXBytes == nil || *r.RXBytes != strconv.Itoa(23) {
		t.Fatal(r, err)
	}
	var integrity string
	if err = s.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
}

func TestFastReportsDoNotInventCoverage(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	m.CPU = field(10.0)
	for i := 0; i < 10; i++ {
		s.Accept("node", at.Add(time.Duration(i)*100*time.Millisecond), m)
	}
	flush(t, s)
	h, err := s.History(context.Background(), "node", at, at.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(h.Points[0].CoverageSeconds-1.9) > 1e-9 || math.Abs(h.Points[0].Fields["cpu"].CoverageSeconds-1.9) > 1e-9 {
		t.Fatal(h.Points)
	}
	s.Accept("node", at.Add(900*time.Millisecond), m)
	flush(t, s)
	h, err = s.History(context.Background(), "node", at, at.Add(time.Minute), time.Minute)
	if err != nil || math.Abs(h.Points[0].CoverageSeconds-1.9) > 1e-9 {
		t.Fatal(h, err)
	}
}
func TestCorruptAggregateReturnsDegradedError(t *testing.T) {
	s := openTest(t)
	at := minute()
	if _, err := s.db.Exec("INSERT INTO metrics_1m(node,at,data) VALUES(?,?,?)", "node", at.Unix(), `{"Samples":1,"Fields":{"mem_used":{"Sum":"bad","N":1,"Integer":true}}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.History(context.Background(), "node", at, at.Add(time.Minute), time.Minute); err == nil {
		t.Fatal("corruption accepted")
	}
	if s.Status().QueryErrors != 1 {
		t.Fatal(s.Status())
	}
	if _, err := s.db.Exec("DELETE FROM metrics_1m"); err != nil {
		t.Fatal(err)
	}
}
func TestExtremeLoadSumDoesNotOverflow(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	m.Load = field([]float64{math.MaxFloat64, math.SmallestNonzeroFloat64, 1})
	s.Accept("node", at, m)
	s.Accept("node", at.Add(time.Second), m)
	flush(t, s)
	h, err := s.History(context.Background(), "node", at, at.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if h.Points[0].Fields["load1"].Value == nil || *h.Points[0].Fields["load1"].Value != strconv.FormatFloat(math.MaxFloat64, 'g', -1, 64) {
		t.Fatal(h)
	}
}

func TestTrafficScopeChangeRebaselines(t *testing.T) {
	s := openTest(t)
	at := minute()
	m := metrics(t)
	s.Accept("node", at, m)
	m.Scope = shared.ScopeNamespace
	m.NetRXTotal = field(uint64(500))
	s.Accept("node", at.Add(time.Second), m)
	flush(t, s)
	d, err := s.Traffic(context.Background(), "node", at)
	if err != nil || d.RXBytes != nil || d.Resets != 1 {
		t.Fatal(d, err)
	}
}
func TestFlushWaitingDoesNotPreventCloseDeadline(t *testing.T) {
	s := &Store{queue: make(chan event, 1), stop: make(chan struct{}), done: make(chan struct{})}
	s.queue <- event{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- s.Flush(ctx) }()
	closeCtx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	start := time.Now()
	if err := s.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("Close blocked on Flush mutex")
	}
	if err := <-completed; !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestWorkerPanicIsContained(t *testing.T) {
	s, err := Open(Config{Path: filepath.Join(privateDir(t), "history.db")})
	if err != nil {
		t.Fatal(err)
	}
	s.offer(event{node: "node", at: minute(), sample: &observation{values: map[string]sum{"mem_used": {Sum: "corrupt", N: 1, Integer: true}}}})
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not contain panic")
	}
	if s.Status().LastError != "worker_panic" {
		t.Fatal(s.Status())
	}
	if s.AcceptProbe("node", "task", minute(), 1) {
		t.Fatal("dead worker accepted sample")
	}
	if err = s.Close(context.Background()); err == nil {
		t.Fatal("panic close did not report error")
	}
}
