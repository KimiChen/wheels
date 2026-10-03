package store

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompb"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

var ErrClosed = errors.New("history store closed")
var fieldNames = []string{"cpu", "load1", "load2", "load3", "mem_total", "mem_used", "swap_total", "swap_used", "disk_total", "disk_used", "net_rx", "net_tx", "uptime", "tcp", "udp", "procs"}

// VictoriaMetrics cache settings are process global. The monitor owns one store.
var instanceMu sync.Mutex
var instanceOpen bool
var settingsOnce sync.Once
var readMemoryLimit = platformMemoryLimit

type event struct {
	tunnel     *tunnelSample
	node, task string
	at         time.Time
	values     map[string]float64
	latency    float64
	flush      chan error
}

type Store struct {
	db                       *storage.Storage
	cfg                      Config
	queue                    chan event
	stop, done               chan struct{}
	mu                       sync.RWMutex
	closed                   bool
	closeErr                 error
	queryMu                  sync.RWMutex
	dropped, writes, queries atomic.Uint64
	failure                  atomic.Pointer[string]
	// maxTracked bounds per-node coverage baselines; zero selects the default
	// 1024. Node IDs are never reused, so a full map evicts the least recently
	// seen node instead of rejecting every new node.
	maxTracked int
	// Only the worker accesses coverage baselines and its bounded write batch.
	tunnelBaselines map[string]tunnelBaseline
	lastObserved    map[string]map[string]int64
	lastSeen        map[string]int64
	rows            []storage.MetricRow
}

func memoryBudget(limit uint64) (int, error) {
	// Keep optional TSDB off when there is no reliable budget. VM fatally exits
	// if memory.allowedBytes consumes the entire physical/cgroup memory limit.
	if limit < 64<<20 {
		return 0, errors.New("history memory limit too low or unavailable")
	}
	budget := min(uint64(128<<20), limit/8)
	return int(budget), nil
}

func Open(cfg Config) (out *Store, err error) {
	if cfg.RetentionDays == 0 {
		cfg.RetentionDays = 7
	}
	if cfg.ReportInterval == 0 {
		cfg.ReportInterval = time.Second
	}
	if cfg.QueueCapacity == 0 {
		cfg.QueueCapacity = 4096
	}
	if cfg.RetentionDays < 1 || cfg.RetentionDays > 365 || cfg.ReportInterval < time.Second || cfg.ReportInterval > time.Hour || cfg.QueueCapacity < 1 || cfg.QueueCapacity > 65536 {
		return nil, errors.New("invalid history limits")
	}
	if cfg.Path == "" || strings.ContainsAny(cfg.Path, "\x00\r\n") {
		return nil, errors.New("invalid history path")
	}
	cfg.Path, err = filepath.Abs(cfg.Path)
	if err != nil {
		return nil, errors.New("invalid history path")
	}
	limit, err := readMemoryLimit()
	if err != nil {
		return nil, errors.New("history memory limit unavailable")
	}
	budget, err := memoryBudget(limit)
	if err != nil {
		return nil, err
	}
	if err = secureDirectory(cfg.Path); err != nil {
		return nil, err
	}
	instanceMu.Lock()
	defer instanceMu.Unlock()
	if instanceOpen {
		return nil, errors.New("history store already open")
	}
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("history open failed")
		}
	}()
	settingsOnce.Do(func() {
		// FRP uses Cobra; the standard flag set may never have been parsed. VM
		// requires it before initializing memory. Parse no FRP command arguments.
		if err := flag.Set("memory.allowedBytes", strconv.Itoa(budget)); err != nil {
			panic(err)
		}
		if !flag.Parsed() {
			if err := flag.CommandLine.Parse(nil); err != nil {
				panic(err)
			}
		}
		// Cache budgets, not a process-wide memory limit. No HTTP listener is started.
		// metricID_tsid has no dedicated setter; VM gives it allowedBytes/16 (at most 8 MiB).
		storage.SetTSIDCacheSize(32 << 20)
		storage.SetMetricNameCacheSize(8 << 20)
		storage.SetTagFiltersCacheSize(8 << 20)
		storage.SetMetadataStorageSize(1 << 20)
		storage.SetFreeDiskSpaceLimit(1 << 30)
		storage.SetDataFlushInterval(5 * time.Second)
		storage.SetDedupInterval(time.Millisecond)
	})
	db := storage.MustOpenStorage(cfg.Path, storage.OpenOptions{Retention: time.Duration(cfg.RetentionDays) * 24 * time.Hour})
	if db.IsReadOnly() {
		db.MustClose()
		return nil, errors.New("history disk space low")
	}
	s := &Store{db: db, cfg: cfg, queue: make(chan event, cfg.QueueCapacity), stop: make(chan struct{}), done: make(chan struct{}), maxTracked: 1024, lastObserved: map[string]map[string]int64{}, lastSeen: map[string]int64{}}
	instanceOpen = true
	go s.run()
	return s, nil
}

func secureDirectory(path string) error {
	// Validate the existing ancestor chain before creating anything: MkdirAll
	// would otherwise traverse a symlink ancestor and create directories at its
	// target. The full chain is re-checked after creation.
	if err := realDirChain(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return errors.New("cannot create history directory")
	}
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("history path must be a real directory")
		}
		if p == path && st.Mode().Perm()&0077 != 0 {
			return errors.New("history directory must have private permissions")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}

// realDirChain requires every existing component of path to be a real
// directory, never a symlink. Missing components are left for the caller to
// create and re-check.
func realDirChain(path string) error {
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err == nil {
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return errors.New("history path must be a real directory")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("cannot inspect history directory")
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

func validNode(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}
func validTask(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}
func validAt(at time.Time) bool     { return !at.IsZero() && at.Year() >= 2000 && at.Year() <= 2261 }
func (s *Store) drop(reason string) { s.dropped.Add(1); s.failure.Store(&reason) }
func (s *Store) offer(e event) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.drop("closed")
		return false
	}
	select {
	case s.queue <- e:
		return true
	default:
		s.drop("queue_full")
		return false
	}
}
func (s *Store) Accept(nodeID string, at time.Time, m shared.Metrics) bool {
	if !validNode(nodeID) || !validAt(at) || m.Validate() != nil {
		// Invalid input is a caller bug, not a storage failure: count the drop
		// without degrading the store (degraded would persist until restart).
		s.dropped.Add(1)
		return false
	}
	values := map[string]float64{}
	addFloat := func(name string, f shared.Field[float64]) {
		if f.Quality == shared.QualityOK && f.Value != nil {
			values[name] = *f.Value
		}
	}
	addUint := func(name string, f shared.Field[uint64]) {
		if f.Quality == shared.QualityOK && f.Value != nil {
			values[name] = float64(*f.Value)
		}
	}
	addFloat("cpu", m.CPU)
	if m.Load.Quality == shared.QualityOK && m.Load.Value != nil {
		for i, v := range *m.Load.Value {
			values["load"+strconv.Itoa(i+1)] = v
		}
	}
	for _, f := range []struct {
		name string
		f    shared.Field[uint64]
	}{{"mem_total", m.MemTotal}, {"mem_used", m.MemUsed}, {"swap_total", m.SwapTotal}, {"swap_used", m.SwapUsed}, {"disk_total", m.DiskTotal}, {"disk_used", m.DiskUsed}, {"net_rx", m.NetRX}, {"net_tx", m.NetTX}, {"uptime", m.Uptime}, {"tcp", m.TCP}, {"udp", m.UDP}, {"procs", m.Procs}} {
		addUint(f.name, f.f)
	}
	return s.offer(event{node: nodeID, at: at.UTC(), values: values})
}
func (s *Store) AcceptProbe(nodeID, taskID string, at time.Time, latencyMS float64) bool {
	if !validNode(nodeID) || !validTask(taskID) || !validAt(at) || math.IsNaN(latencyMS) || math.IsInf(latencyMS, 0) || latencyMS < 0 && latencyMS != -1 || latencyMS > 900 {
		// As in Accept, invalid input only counts as dropped, never degraded.
		s.dropped.Add(1)
		return false
	}
	return s.offer(event{node: nodeID, task: taskID, at: at.UTC(), latency: latencyMS})
}
func (s *Store) Status() Status {
	out := Status{Dropped: s.dropped.Load(), WriteErrors: s.writes.Load(), QueryErrors: s.queries.Load(), QueueDepth: len(s.queue)}
	if reason := s.failure.Load(); reason != nil {
		out.Degraded = true
		out.LastError = *reason
	}
	return out
}
func (s *Store) Flush(ctx context.Context) error {
	answer := make(chan error, 1)
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	select {
	case s.queue <- event{flush: answer}:
	case <-s.stop:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-answer:
		return err
	case <-s.done:
		// Shutdown may finish after the worker has already answered this
		// flush. Preserve that result when both channels are ready.
		select {
		case err := <-answer:
			return err
		default:
			return ErrClosed
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Store) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Store) run() {
	defer close(s.done)
	defer func() {
		if recover() != nil {
			reason := "worker_panic"
			s.failure.Store(&reason)
			s.closeErr = errors.New(reason)
		}
		s.mu.Lock()
		if !s.closed {
			s.closed = true
			close(s.stop)
		}
		s.mu.Unlock()
		s.queryMu.Lock()
		func() {
			defer func() {
				if recover() != nil {
					s.closeErr = errors.New("history close failed")
				}
			}()
			s.db.MustClose()
		}()
		s.queryMu.Unlock()
		instanceMu.Lock()
		instanceOpen = false
		instanceMu.Unlock()
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-s.queue:
			s.consume(e)
		case <-ticker.C:
			_ = s.persist(true)
		case <-s.stop:
			for {
				select {
				case e := <-s.queue:
					s.consume(e)
				default:
					s.closeErr = s.persist(true)
					return
				}
			}
		}
	}
}
func (s *Store) consume(e event) {
	if e.flush != nil {
		e.flush <- s.persist(true)
		return
	}
	if s.db.IsReadOnly() {
		s.drop("disk_space_low")
		return
	}
	if e.tunnel != nil {
		s.consumeTunnel(*e.tunnel)
		return
	}
	limit := s.maxTracked
	if limit <= 0 {
		limit = 1024
	}
	if _, ok := s.lastObserved[e.node]; !ok {
		if len(s.lastObserved) >= limit {
			// Node IDs are AUTOINCREMENT and never reused; without eviction a
			// full table would silently drop every new node's history and hold
			// Degraded. Evict the node unseen for the longest time instead.
			s.evictOldestNode()
		}
		s.lastObserved[e.node] = map[string]int64{}
	}
	if at := e.at.UnixNano(); s.lastSeen[e.node] < at {
		s.lastSeen[e.node] = at
	}
	if e.values == nil {
		// A probe keeps its failure as -1; it is excluded from the mean at query time.
		s.rows = append(s.rows, metricRow("probe", e.node, e.task, e.at.UnixMilli(), e.latency))
	} else {
		s.rows = append(s.rows, metricRow("reports", e.node, "", e.at.UnixMilli(), s.coverage(e.node, "", e.at)))
		for name, value := range e.values {
			s.rows = append(s.rows, metricRow(name, e.node, "", e.at.UnixMilli(), value), metricRow(name+"_coverage", e.node, "", e.at.UnixMilli(), s.coverage(e.node, name, e.at)))
		}
	}
	if len(s.rows) >= 512 {
		_ = s.persist(false)
	}
}

// evictOldestNode drops the coverage baselines of the node unseen for the
// longest time, keeping the table bounded. Stored history itself is untouched;
// if the evicted node reports again, its baselines start over like a new node.
func (s *Store) evictOldestNode() {
	var oldest string
	var seen int64
	first := true
	for node, at := range s.lastSeen {
		if first || at < seen {
			oldest, seen, first = node, at, false
		}
	}
	delete(s.lastObserved, oldest)
	delete(s.lastSeen, oldest)
}

func metricRow(field, node, task string, at int64, value float64) storage.MetricRow {
	labels := []prompb.Label{{Name: "__name__", Value: "frpmonitor_" + field}, {Name: "node_id", Value: node}}
	if task != "" {
		labels = append(labels, prompb.Label{Name: "task_id", Value: task})
	}
	return storage.MetricRow{MetricNameRaw: storage.MarshalMetricNameRaw(nil, labels), Timestamp: at, Value: value}
}
func (s *Store) coverage(node, name string, at time.Time) float64 {
	times := s.lastObserved[node]
	now := at.UnixNano()
	old, exists := times[name]
	if exists && now <= old {
		return 0
	}
	times[name] = now
	if !exists {
		return math.Min(60, s.cfg.ReportInterval.Seconds())
	}
	return math.Min(s.cfg.ReportInterval.Seconds(), float64(now-old)/1e9)
}
func (s *Store) persist(flush bool) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("history write failed")
		}
		if err != nil {
			s.writes.Add(1)
			reason := "write_failed"
			s.failure.Store(&reason)
		}
		// A failed batch is not retried: its commit outcome may be unknown.
		s.rows = nil
	}()
	if s.db.IsReadOnly() {
		return errors.New("history disk space low")
	}
	if len(s.rows) > 0 {
		s.db.AddRows(s.rows, 64)
	}
	if flush {
		s.db.DebugFlush()
	}
	return nil
}
func queryRange(node string, from, to time.Time, step time.Duration) (time.Time, time.Time, time.Duration, error) {
	if !validNode(node) || !validAt(from) || !validAt(to) || !to.After(from) || to.Sub(from) > 31*24*time.Hour || step < 0 || step > 31*24*time.Hour {
		return from, to, step, errors.New("invalid history range")
	}
	from = from.UTC().Truncate(time.Minute)
	to = to.UTC()
	if to != to.Truncate(time.Minute) {
		to = to.Truncate(time.Minute).Add(time.Minute)
	}
	if step < time.Minute {
		step = time.Minute
	}
	step = ((step + time.Minute - 1) / time.Minute) * time.Minute
	minStep := ((to.Sub(from)+499)/500 + time.Minute - 1) / time.Minute * time.Minute
	if step < minStep {
		step = minStep
	}
	return from, to, step, nil
}
func (s *Store) queryError(parent context.Context, err error) error {
	// A caller abandoning its request says nothing about storage health.
	// VM has its own timeout error; normalize it only when the caller's
	// context also expired. The store's own query budget remains a failure.
	if cause := parent.Err(); cause != nil && (errors.Is(err, cause) || errors.Is(err, storage.ErrDeadlineExceeded)) {
		return cause
	}
	if err != nil {
		s.queries.Add(1)
		reason := "query_failed"
		s.failure.Store(&reason)
	}
	return err
}

// scan streams TSDB blocks into bounded result buckets. No raw history array is
// retained. VictoriaMetrics' deadline plus per-block context checks bound work;
// an operating-system disk stall can still delay the storage library itself.
func (s *Store) scan(ctx context.Context, node, task string, from, to time.Time, visit func(string, int64, float64) error) (err error) {
	parent := ctx
	defer func() {
		if recover() != nil {
			err = errors.New("history query failed")
		}
		err = s.queryError(parent, err)
	}()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return err
	}
	s.queryMu.RLock()
	defer s.queryMu.RUnlock()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	tfs := storage.NewTagFilters()
	if err = tfs.Add([]byte("node_id"), []byte(node), false, false); err != nil {
		return err
	}
	if task != "" {
		if err = tfs.Add(nil, []byte("frpmonitor_probe"), false, false); err != nil {
			return err
		}
		if err = tfs.Add([]byte("task_id"), []byte(task), false, false); err != nil {
			return err
		}
	} else {
		if err = tfs.Add(nil, []byte("frpmonitor_probe"), true, false); err != nil {
			return err
		}
	}
	tr := storage.TimeRange{MinTimestamp: from.UnixMilli(), MaxTimestamp: to.UnixMilli() - 1}
	deadline, _ := ctx.Deadline()
	var search storage.Search
	search.Init(nil, s.db, []*storage.TagFilters{tfs}, tr, 2*len(fieldNames)+1, uint64(deadline.Unix()+1))
	defer search.MustClose()
	var timestamps []int64
	var values []float64
	seen := 0
	for search.NextMetricBlock() {
		if err = ctx.Err(); err != nil {
			return err
		}
		mbr := search.MetricBlockRef
		var mn storage.MetricName
		if err = mn.Unmarshal(mbr.MetricName); err != nil {
			return errors.New("invalid history series")
		}
		field := strings.TrimPrefix(string(mn.MetricGroup), "frpmonitor_")
		var block storage.Block
		mbr.BlockRef.MustReadBlock(&block)
		if err = block.UnmarshalData(); err != nil {
			return errors.New("invalid history block")
		}
		timestamps, values = block.AppendRowsWithTimeRangeFilter(timestamps[:0], values[:0], tr)
		seen += len(timestamps)
		if seen > 32000000 {
			return errors.New("history sample limit exceeded")
		}
		for i, at := range timestamps {
			if math.IsNaN(values[i]) || math.IsInf(values[i], 0) {
				return errors.New("invalid history value")
			}
			if err = visit(field, at, values[i]); err != nil {
				return err
			}
		}
	}
	return search.Error()
}

type average struct {
	value float64
	n     int
}

func (a *average) add(v float64) {
	a.n++
	if a.n == 1 {
		a.value = v
	} else {
		a.value = a.value*(float64(a.n-1)/float64(a.n)) + v/float64(a.n)
	}
}
func (s *Store) History(ctx context.Context, node string, from, to time.Time, step time.Duration) (HistoryResult, error) {
	from, to, step, err := queryRange(node, from, to, step)
	if err != nil {
		return HistoryResult{}, err
	}
	result := HistoryResult{StepSeconds: int64(step / time.Second), Points: []Point{}}
	count := int((to.Sub(from) + step - 1) / step)
	means := make([]map[string]*average, count)
	for i := 0; i < count; i++ {
		p := Point{At: from.Add(time.Duration(i) * step), Fields: map[string]Field{}}
		for _, name := range fieldNames {
			p.Fields[name] = Field{}
		}
		result.Points = append(result.Points, p)
		means[i] = map[string]*average{}
	}
	err = s.scan(ctx, node, "", from, to, func(name string, at int64, v float64) error {
		idx := int((at - from.UnixMilli()) / step.Milliseconds())
		p := &result.Points[idx]
		if name == "reports" {
			p.Samples++
			p.CoverageSeconds = math.Min(step.Seconds(), p.CoverageSeconds+v)
			return nil
		}
		if strings.HasSuffix(name, "_coverage") {
			key := strings.TrimSuffix(name, "_coverage")
			f, ok := p.Fields[key]
			if !ok {
				return errors.New("unknown history field")
			}
			f.CoverageSeconds = math.Min(step.Seconds(), f.CoverageSeconds+v)
			p.Fields[key] = f
			return nil
		}
		if _, ok := p.Fields[name]; !ok {
			return errors.New("unknown history field")
		}
		a := means[idx][name]
		if a == nil {
			a = &average{}
			means[idx][name] = a
		}
		a.add(v)
		return nil
	})
	if err != nil {
		return HistoryResult{}, err
	}
	for i := range result.Points {
		for name, a := range means[i] {
			f := result.Points[i].Fields[name]
			f.Samples = a.n
			v := strconv.FormatFloat(a.value, 'g', -1, 64)
			if name != "cpu" && !strings.HasPrefix(name, "load") {
				v = strconv.FormatFloat(math.Floor(a.value), 'f', 0, 64)
			}
			f.Value = &v
			result.Points[i].Fields[name] = f
		}
	}
	return result, nil
}
func (s *Store) ProbeHistory(ctx context.Context, node, task string, from, to time.Time, step time.Duration) (ProbeHistoryResult, error) {
	result := ProbeHistoryResult{Points: []ProbePoint{}}
	if !validTask(task) {
		return result, errors.New("invalid task")
	}
	from, to, step, err := queryRange(node, from, to, step)
	if err != nil {
		return result, err
	}
	result.StepSeconds = int64(step / time.Second)
	count := int((to.Sub(from) + step - 1) / step)
	means := make([]average, count)
	for i := 0; i < count; i++ {
		result.Points = append(result.Points, ProbePoint{At: from.Add(time.Duration(i) * step)})
	}
	err = s.scan(ctx, node, task, from, to, func(_ string, at int64, v float64) error {
		if v < -1 || v > 900 {
			return errors.New("invalid probe value")
		}
		idx := int((at - from.UnixMilli()) / step.Milliseconds())
		p := &result.Points[idx]
		p.Samples++
		result.Samples++
		if v < 0 {
			p.Failures++
			result.Failures++
		} else {
			means[idx].add(v)
		}
		if result.LatestAt == nil || at >= result.LatestAt.UnixMilli() {
			t := time.UnixMilli(at).UTC()
			result.LatestAt = &t
			value := v
			result.LatencyMS = &value
		}
		return nil
	})
	if err != nil {
		return ProbeHistoryResult{}, err
	}
	for i, a := range means {
		if a.n > 0 {
			v := a.value
			result.Points[i].LatencyMS = &v
		}
	}
	return result, nil
}
func (s *Store) String() string {
	return fmt.Sprintf("VictoriaMetrics history (retention=%dd)", s.cfg.RetentionDays)
}
