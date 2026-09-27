package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	_ "modernc.org/sqlite"
)

const maxPending = 16384

var ErrClosed = errors.New("history store closed")
var fieldNames = []string{"cpu", "load1", "load2", "load3", "mem_total", "mem_used", "swap_total", "swap_used", "disk_total", "disk_used", "net_rx", "net_tx", "net_rx_total", "net_tx_total", "uptime", "tcp", "udp", "procs"}

type sum struct {
	Sum      string
	N        int
	Coverage float64
	Integer  bool
}
type bucket struct {
	Samples  int
	Coverage float64
	Fields   map[string]sum
}
type key struct {
	Node string
	At   int64
	Task string
}
type baseline struct {
	Boot, Iface string
	RX, TX      uint64
	At          int64
}
type daily struct {
	RX, TX        string
	Coverage      float64
	Resets, Pairs int
}
type probe struct {
	Total             float64
	Samples, Failures int
	LastAt            int64
	LastLatency       float64
}
type observation struct {
	values   map[string]sum
	baseline *baseline
}
type event struct {
	node, task string
	at         time.Time
	sample     *observation
	latency    float64
	flush      chan error
	ctx        context.Context
}

type Store struct {
	db                       *sql.DB
	cfg                      Config
	queue                    chan event
	stop, done               chan struct{}
	mu                       sync.RWMutex
	closed                   bool
	closeErr                 error
	dropped, writes, queries atomic.Uint64
	failure                  atomic.Pointer[string]
	// All mutable aggregation state below is owned by the worker.
	metrics      map[key]*bucket
	probes       map[key]*probe
	baselines    map[string]baseline
	changed      map[string]baseline
	traffic      map[key]*daily
	lastObserved map[string]map[string]int64
}

func Open(cfg Config) (*Store, error) {
	if cfg.RetentionDays == 0 {
		cfg.RetentionDays = 7
	}
	if cfg.ReportInterval == 0 {
		cfg.ReportInterval = time.Second
	}
	if cfg.QueueCapacity == 0 {
		cfg.QueueCapacity = 4096
	}
	if cfg.RetentionDays < 1 || cfg.RetentionDays > 31 || cfg.ReportInterval < time.Second || cfg.ReportInterval > time.Hour || cfg.QueueCapacity < 1 || cfg.QueueCapacity > 65536 {
		return nil, errors.New("invalid history limits")
	}
	if cfg.Path == "" || strings.ContainsAny(cfg.Path, "\x00\r\n") {
		return nil, errors.New("invalid history path")
	}
	path, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, err
	}
	cfg.Path = path
	if err = securePath(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	for _, v := range []string{"busy_timeout(1000)", "synchronous(FULL)", "foreign_keys(ON)"} {
		q.Add("_pragma", v)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
		if err == nil {
			err = errors.New("WAL unavailable")
		}
		return fail(err)
	}
	if err = migrate(ctx, db); err != nil {
		return fail(err)
	}
	s := &Store{db: db, cfg: cfg, queue: make(chan event, cfg.QueueCapacity), stop: make(chan struct{}), done: make(chan struct{}), metrics: map[key]*bucket{}, probes: map[key]*probe{}, baselines: map[string]baseline{}, changed: map[string]baseline{}, traffic: map[key]*daily{}, lastObserved: map[string]map[string]int64{}}
	rows, err := db.QueryContext(ctx, "SELECT node,boot,iface,rx,tx,at FROM traffic_state LIMIT 1025")
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var node, rx, tx string
		var b baseline
		if err = rows.Scan(&node, &b.Boot, &b.Iface, &rx, &tx, &b.At); err != nil {
			break
		}
		b.RX, err = strconv.ParseUint(rx, 10, 64)
		if err != nil {
			break
		}
		b.TX, err = strconv.ParseUint(tx, 10, 64)
		if err != nil {
			break
		}
		s.baselines[node] = b
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return fail(err)
	}
	if len(s.baselines) > 1024 {
		return fail(errors.New("history node limit exceeded"))
	}
	go s.run()
	return s, nil
}

// Refuse symlinks and existing public files; the directory is private so WAL/SHM
// inherit a safe enclosing boundary without changing process-wide umask.
func securePath(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	for p := parent; ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errors.New("history parent must be a real directory")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	st, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("history directory must have private permissions")
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, e := os.Lstat(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			return errors.New("history files must be regular and private")
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return errors.New("unsupported history schema version")
	}
	if version == 0 {
		for _, query := range []string{
			"CREATE TABLE metrics_1m(node TEXT NOT NULL,at INTEGER NOT NULL,data TEXT NOT NULL,PRIMARY KEY(node,at)) WITHOUT ROWID",
			"CREATE TABLE probes_1m(node TEXT NOT NULL,task TEXT NOT NULL,at INTEGER NOT NULL,data TEXT NOT NULL,PRIMARY KEY(node,task,at)) WITHOUT ROWID",
			"CREATE TABLE traffic_state(node TEXT PRIMARY KEY,boot TEXT NOT NULL,iface TEXT NOT NULL,rx TEXT NOT NULL,tx TEXT NOT NULL,at INTEGER NOT NULL) WITHOUT ROWID",
			"CREATE TABLE traffic_daily(node TEXT NOT NULL,at INTEGER NOT NULL,data TEXT NOT NULL,PRIMARY KEY(node,at)) WITHOUT ROWID",
			"CREATE INDEX metrics_expiry ON metrics_1m(at)", "CREATE INDEX probes_expiry ON probes_1m(at)", "CREATE INDEX traffic_expiry ON traffic_daily(at)", "PRAGMA user_version=1",
		} {
			if _, err = tx.ExecContext(ctx, query); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func validID(s string) bool {
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
func validAt(at time.Time) bool { return !at.IsZero() && at.Year() >= 2000 && at.Year() <= 2261 }
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
func (s *Store) drop(reason string) { s.dropped.Add(1); s.failure.Store(&reason) }
func (s *Store) Accept(nodeID string, at time.Time, m shared.Metrics) bool {
	if !validID(nodeID) || !validAt(at) || m.Validate() != nil {
		s.drop("invalid_sample")
		return false
	}
	o := observation{values: map[string]sum{}}
	addFloat := func(name string, f shared.Field[float64]) {
		if f.Quality == shared.QualityOK && f.Value != nil {
			o.values[name] = sum{Sum: strconv.FormatFloat(*f.Value, 'g', -1, 64), N: 1}
		}
	}
	addUint := func(name string, f shared.Field[uint64]) {
		if f.Quality == shared.QualityOK && f.Value != nil {
			o.values[name] = sum{Sum: strconv.FormatUint(*f.Value, 10), N: 1, Integer: true}
		}
	}
	addFloat("cpu", m.CPU)
	if m.Load.Quality == shared.QualityOK && m.Load.Value != nil {
		for i, v := range *m.Load.Value {
			o.values["load"+strconv.Itoa(i+1)] = sum{Sum: strconv.FormatFloat(v, 'g', -1, 64), N: 1}
		}
	}
	for _, f := range []struct {
		name string
		f    shared.Field[uint64]
	}{{"mem_total", m.MemTotal}, {"mem_used", m.MemUsed}, {"swap_total", m.SwapTotal}, {"swap_used", m.SwapUsed}, {"disk_total", m.DiskTotal}, {"disk_used", m.DiskUsed}, {"net_rx", m.NetRX}, {"net_tx", m.NetTX}, {"net_rx_total", m.NetRXTotal}, {"net_tx_total", m.NetTXTotal}, {"uptime", m.Uptime}, {"tcp", m.TCP}, {"udp", m.UDP}, {"procs", m.Procs}} {
		addUint(f.name, f.f)
	}
	if m.BootID.Quality == shared.QualityOK && m.BootID.Value != nil && m.Iface.Quality == shared.QualityOK && m.Iface.Value != nil && m.NetRXTotal.Quality == shared.QualityOK && m.NetRXTotal.Value != nil && m.NetTXTotal.Quality == shared.QualityOK && m.NetTXTotal.Value != nil {
		o.baseline = &baseline{Boot: string(m.Scope) + ":" + *m.BootID.Value, Iface: *m.Iface.Value, RX: *m.NetRXTotal.Value, TX: *m.NetTXTotal.Value, At: at.UnixNano()}
	}
	return s.offer(event{node: nodeID, at: at.UTC(), sample: &o})
}
func (s *Store) AcceptProbe(nodeID, taskID string, at time.Time, latencyMS float64) bool {
	if !validID(nodeID) || !validID(taskID) || !validAt(at) || math.IsNaN(latencyMS) || math.IsInf(latencyMS, 0) || latencyMS < 0 && latencyMS != -1 || latencyMS > 900 {
		s.drop("invalid_probe")
		return false
	}
	return s.offer(event{node: nodeID, task: taskID, at: at.UTC(), latency: latencyMS})
}
func (s *Store) Status() Status {
	out := Status{Dropped: s.dropped.Load(), WriteErrors: s.writes.Load(), QueryErrors: s.queries.Load(), QueueDepth: len(s.queue)}
	if v := s.failure.Load(); v != nil {
		out.LastError = *v
		out.Degraded = true
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
	case s.queue <- event{flush: answer, ctx: ctx}:
	case <-s.stop:
		return ErrClosed
	case <-s.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-answer:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
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
		s.db.Close()
	}()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case e := <-s.queue:
			s.consume(e)
		case <-ticker.C:
			s.persistBounded(context.Background())
		case <-s.stop:
			for {
				select {
				case e := <-s.queue:
					s.consume(e)
				default:
					s.closeErr = s.persistBounded(context.Background())
					return
				}
			}
		}
	}
}
func (s *Store) consume(e event) {
	if e.flush != nil {
		e.flush <- s.persistBounded(e.ctx)
		return
	}
	k := key{Node: e.node, At: e.at.Truncate(time.Minute).Unix(), Task: e.task}
	if len(s.metrics)+len(s.probes)+len(s.traffic) >= maxPending {
		s.drop("pending_full")
		return
	}
	if e.sample != nil {
		if _, ok := s.lastObserved[e.node]; !ok {
			if len(s.lastObserved) >= 1024 {
				s.drop("node_limit")
				return
			}
			s.lastObserved[e.node] = map[string]int64{}
		}
		b := s.metrics[k]
		if b == nil {
			b = &bucket{Fields: map[string]sum{}}
			s.metrics[k] = b
		}
		b.Samples++
		b.Coverage = math.Min(60, b.Coverage+s.coverage(e.node, "", e.at))
		for name, v := range e.sample.values {
			v.Coverage = s.coverage(e.node, name, e.at)
			old := b.Fields[name]
			b.Fields[name] = mergeSum(old, v, 60)
		}
		if e.sample.baseline != nil {
			s.observeTraffic(e.node, e.at, *e.sample.baseline)
		}
	} else {
		p := s.probes[k]
		if p == nil {
			p = &probe{}
			s.probes[k] = p
		}
		p.Samples++
		if e.latency < 0 {
			p.Failures++
		} else {
			p.Total += e.latency
		}
		if e.at.UnixNano() >= p.LastAt {
			p.LastAt = e.at.UnixNano()
			p.LastLatency = e.latency
		}
	}
}
func mergeSum(a, b sum, cap float64) sum {
	if a.N == 0 {
		a.Integer = b.Integer
		a.Sum = "0"
	}
	if b.N == 0 {
		return a
	}
	if a.Integer {
		x, _ := new(big.Int).SetString(a.Sum, 10)
		y, _ := new(big.Int).SetString(b.Sum, 10)
		if x == nil || y == nil {
			panic("invalid integer aggregate")
		}
		a.Sum = x.Add(x, y).String()
	} else {
		x, okX := new(big.Rat).SetString(a.Sum)
		y, okY := new(big.Rat).SetString(b.Sum)
		if !okX || !okY {
			panic("invalid decimal aggregate")
		}
		a.Sum = x.Add(x, y).RatString()
	}
	a.N += b.N
	a.Coverage = math.Min(cap, a.Coverage+b.Coverage)
	return a
}
func addDecimal(a, b string) string {
	x, _ := new(big.Int).SetString(a, 10)
	y, _ := new(big.Int).SetString(b, 10)
	if x == nil || y == nil {
		panic("invalid traffic count")
	}
	return x.Add(x, y).String()
}
func (s *Store) observeTraffic(node string, at time.Time, next baseline) {
	old, exists := s.baselines[node]
	if !exists && len(s.baselines) >= 1024 {
		s.drop("node_limit")
		return
	}
	if exists && next.At <= old.At {
		return
	} // Duplicates/reordered receive clocks never move the baseline backwards.
	k := key{Node: node, At: time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC).Unix()}
	d := s.traffic[k]
	if d == nil {
		d = &daily{RX: "0", TX: "0"}
		s.traffic[k] = d
	}
	if exists {
		if old.Boot == next.Boot && old.Iface == next.Iface && next.RX >= old.RX && next.TX >= old.TX {
			d.RX = addDecimal(d.RX, strconv.FormatUint(next.RX-old.RX, 10))
			d.TX = addDecimal(d.TX, strconv.FormatUint(next.TX-old.TX, 10))
			d.Pairs++
			d.Coverage += math.Min(float64(next.At-old.At)/1e9, 2*s.cfg.ReportInterval.Seconds())
		} else {
			d.Resets++
		}
	}
	s.baselines[node] = next
	s.changed[node] = next
}
func (s *Store) persistBounded(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	err := s.persist(ctx)
	if err != nil {
		s.writes.Add(1)
		reason := "write_failed"
		s.failure.Store(&reason)
	}
	return err
}
func mergeBucket(a, b *bucket) {
	a.Samples += b.Samples
	a.Coverage = math.Min(60, a.Coverage+b.Coverage)
	if a.Fields == nil {
		a.Fields = map[string]sum{}
	}
	for k, v := range b.Fields {
		a.Fields[k] = mergeSum(a.Fields[k], v, 60)
	}
}
func readJSON(tx *sql.Tx, ctx context.Context, query string, dst any, args ...any) error {
	var raw string
	err := tx.QueryRowContext(ctx, query, args...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > 65536 {
		return errors.New("aggregate too large")
	}
	return decodeAggregate(raw, dst)
}
func writeJSON(tx *sql.Tx, ctx context.Context, query string, value any, args ...any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	args = append(args, string(raw))
	_, err = tx.ExecContext(ctx, query, args...)
	return err
}
func (s *Store) persist(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, b := range s.metrics {
		var old bucket
		if err = readJSON(tx, ctx, "SELECT data FROM metrics_1m WHERE node=? AND at=?", &old, k.Node, k.At); err != nil {
			return err
		}
		mergeBucket(&old, b)
		if err = writeJSON(tx, ctx, "INSERT OR REPLACE INTO metrics_1m(node,at,data) VALUES(?,?,?)", old, k.Node, k.At); err != nil {
			return err
		}
	}
	for k, p := range s.probes {
		var old probe
		if err = readJSON(tx, ctx, "SELECT data FROM probes_1m WHERE node=? AND task=? AND at=?", &old, k.Node, k.Task, k.At); err != nil {
			return err
		}
		old.Total += p.Total
		old.Samples += p.Samples
		old.Failures += p.Failures
		if p.LastAt >= old.LastAt {
			old.LastAt = p.LastAt
			old.LastLatency = p.LastLatency
		}
		if err = writeJSON(tx, ctx, "INSERT OR REPLACE INTO probes_1m(node,task,at,data) VALUES(?,?,?,?)", old, k.Node, k.Task, k.At); err != nil {
			return err
		}
	}
	for k, d := range s.traffic {
		old := daily{RX: "0", TX: "0"}
		if err = readJSON(tx, ctx, "SELECT data FROM traffic_daily WHERE node=? AND at=?", &old, k.Node, k.At); err != nil {
			return err
		}
		old.RX = addDecimal(old.RX, d.RX)
		old.TX = addDecimal(old.TX, d.TX)
		old.Coverage = math.Min(86400, old.Coverage+d.Coverage)
		old.Resets += d.Resets
		old.Pairs += d.Pairs
		if err = writeJSON(tx, ctx, "INSERT OR REPLACE INTO traffic_daily(node,at,data) VALUES(?,?,?)", old, k.Node, k.At); err != nil {
			return err
		}
	}
	for node, b := range s.changed {
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO traffic_state(node,boot,iface,rx,tx,at) VALUES(?,?,?,?,?,?)", node, b.Boot, b.Iface, strconv.FormatUint(b.RX, 10), strconv.FormatUint(b.TX, 10), b.At); err != nil {
			return err
		}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(s.cfg.RetentionDays) * 24 * time.Hour).Truncate(time.Minute).Unix()
	for _, table := range []string{"metrics_1m", "probes_1m", "traffic_daily"} {
		boundary := cutoff
		if table == "traffic_daily" {
			boundary = time.Unix(cutoff, 0).UTC().Truncate(24 * time.Hour).Unix()
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE at < ?", boundary); err != nil {
			return err
		}
	}
	// Baselines are tiny and persist across retention, so a long outage does not
	// silently re-count a node's all-time counters. They do not imply online state.
	if err = tx.Commit(); err != nil {
		return err
	}
	clear(s.metrics)
	clear(s.probes)
	clear(s.changed)
	clear(s.traffic)
	return nil
}

func (s *Store) queryError(err error) error {
	if err != nil {
		s.queries.Add(1)
		reason := "query_failed"
		s.failure.Store(&reason)
	}
	return err
}
func queryRange(node string, from, to time.Time, step time.Duration) (time.Time, time.Time, time.Duration, error) {
	if !validID(node) || !validAt(from) || !validAt(to) || !to.After(from) || to.Sub(from) > 31*24*time.Hour || step < 0 || step > 31*24*time.Hour {
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
func (s *Store) History(ctx context.Context, node string, from, to time.Time, step time.Duration) (HistoryResult, error) {
	from, to, step, err := queryRange(node, from, to, step)
	if err != nil {
		return HistoryResult{}, err
	}
	result := HistoryResult{StepSeconds: int64(step / time.Second), Points: []Point{}}
	count := int((to.Sub(from) + step - 1) / step)
	buckets := make([]bucket, count)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, "SELECT at,data FROM metrics_1m WHERE node=? AND at>=? AND at<? ORDER BY at", node, from.Unix(), to.Unix())
	if err != nil {
		return result, s.queryError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var raw string
		var b bucket
		if err = rows.Scan(&at, &raw); err != nil {
			break
		}
		if len(raw) > 65536 {
			err = errors.New("aggregate too large")
			break
		}
		if err = decodeAggregate(raw, &b); err != nil {
			break
		}
		idx := int((at - from.Unix()) / int64(step/time.Second))
		out := &buckets[idx]
		out.Samples += b.Samples
		out.Coverage += b.Coverage
		if out.Fields == nil {
			out.Fields = map[string]sum{}
		}
		for name, v := range b.Fields {
			out.Fields[name] = mergeSum(out.Fields[name], v, step.Seconds())
		}
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return result, s.queryError(err)
	}
	for i, b := range buckets {
		p := Point{At: from.Add(time.Duration(i) * step), Samples: b.Samples, CoverageSeconds: math.Min(step.Seconds(), b.Coverage), Fields: map[string]Field{}}
		for _, name := range fieldNames {
			a := b.Fields[name]
			f := Field{Samples: a.N, CoverageSeconds: math.Min(step.Seconds(), a.Coverage)}
			if a.N > 0 {
				var value string
				if a.Integer {
					x, ok := new(big.Int).SetString(a.Sum, 10)
					if !ok {
						return result, s.queryError(errors.New("invalid aggregate"))
					}
					value = x.Quo(x, big.NewInt(int64(a.N))).String()
				} else {
					x, ok := new(big.Rat).SetString(a.Sum)
					if !ok {
						return result, s.queryError(errors.New("invalid decimal aggregate"))
					}
					x.Quo(x, new(big.Rat).SetInt64(int64(a.N)))
					v, _ := x.Float64()
					value = strconv.FormatFloat(v, 'g', -1, 64)
				}
				f.Value = &value
			}
			p.Fields[name] = f
		}
		result.Points = append(result.Points, p)
	}
	return result, nil
}
func (s *Store) Traffic(ctx context.Context, node string, at time.Time) (TrafficResult, error) {
	result := TrafficResult{Day: at.UTC().Format("2006-01-02")}
	if !validID(node) || !validAt(at) {
		return result, errors.New("invalid traffic query")
	}
	day := time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC).Unix()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT data FROM traffic_daily WHERE node=? AND at=?", node, day).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, s.queryError(err)
	}
	var d daily
	if err = decodeAggregate(raw, &d); err != nil {
		return result, s.queryError(err)
	}
	if d.Pairs > 0 {
		result.RXBytes = &d.RX
		result.TXBytes = &d.TX
	}
	result.CoverageSeconds = d.Coverage
	result.Resets = d.Resets
	return result, nil
}
func (s *Store) ProbeHistory(ctx context.Context, node, task string, from, to time.Time, step time.Duration) (ProbeHistoryResult, error) {
	result := ProbeHistoryResult{Points: []ProbePoint{}}
	if !validID(task) {
		return result, errors.New("invalid task")
	}
	from, to, step, err := queryRange(node, from, to, step)
	if err != nil {
		return result, err
	}
	result.StepSeconds = int64(step / time.Second)
	count := int((to.Sub(from) + step - 1) / step)
	buckets := make([]probe, count)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, "SELECT at,data FROM probes_1m WHERE node=? AND task=? AND at>=? AND at<? ORDER BY at", node, task, from.Unix(), to.Unix())
	if err != nil {
		return result, s.queryError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var raw string
		var p probe
		if err = rows.Scan(&at, &raw); err != nil {
			break
		}
		if len(raw) > 65536 {
			err = errors.New("aggregate too large")
			break
		}
		if err = decodeAggregate(raw, &p); err != nil {
			break
		}
		idx := int((at - from.Unix()) / int64(step/time.Second))
		out := &buckets[idx]
		out.Samples += p.Samples
		out.Failures += p.Failures
		out.Total += p.Total
		if p.LastAt > out.LastAt {
			out.LastAt = p.LastAt
			out.LastLatency = p.LastLatency
		}
		if result.LatestAt == nil || p.LastAt > result.LatestAt.UnixNano() {
			t := time.Unix(0, p.LastAt).UTC()
			result.LatestAt = &t
			v := p.LastLatency
			result.LatencyMS = &v
		}
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return result, s.queryError(err)
	}
	for i, p := range buckets {
		out := ProbePoint{At: from.Add(time.Duration(i) * step), Samples: p.Samples, Failures: p.Failures}
		if good := p.Samples - p.Failures; good > 0 {
			v := p.Total / float64(good)
			out.LatencyMS = &v
		}
		result.Samples += p.Samples
		result.Failures += p.Failures
		result.Points = append(result.Points, out)
	}
	return result, nil
}

// String deliberately does not disclose the database path.
func (s *Store) String() string {
	return fmt.Sprintf("history store (retention=%dd)", s.cfg.RetentionDays)
}

// Validate persisted payloads before arithmetic: a damaged database produces a
// degraded query/write result, never a process panic or an invented zero.
func decodeAggregate(raw string, dst any) error {
	if len(raw) > 65536 {
		return errors.New("aggregate too large")
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return err
	}
	validCount := func(n int) bool { return n >= 0 && n <= 1000000 }
	validCoverage := func(n float64, cap float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= cap }
	validUint := func(v string) bool {
		if len(v) == 0 || len(v) > 128 {
			return false
		}
		for _, r := range v {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	switch v := dst.(type) {
	case *bucket:
		if !validCount(v.Samples) || !validCoverage(v.Coverage, 60) || len(v.Fields) > len(fieldNames) {
			return errors.New("invalid metric aggregate")
		}
		for name, a := range v.Fields {
			known := false
			for _, n := range fieldNames {
				if name == n {
					known = true
					break
				}
			}
			isInt := name != "cpu" && !strings.HasPrefix(name, "load")
			if !known || a.Integer != isInt || !validCount(a.N) || a.N > v.Samples || !validCoverage(a.Coverage, 60) {
				return errors.New("invalid field aggregate")
			}
			if a.Integer {
				if !validUint(a.Sum) {
					return errors.New("invalid integer aggregate")
				}
			} else {
				x, ok := new(big.Rat).SetString(a.Sum)
				if len(a.Sum) > 2048 || !ok || x.Sign() < 0 {
					return errors.New("invalid decimal aggregate")
				}
			}
		}
	case *probe:
		if !validCount(v.Samples) || !validCount(v.Failures) || v.Failures > v.Samples || !validCoverage(v.Total, 900*float64(v.Samples)) || v.LastLatency < -1 || v.LastLatency > 900 || math.IsNaN(v.LastLatency) || math.IsInf(v.LastLatency, 0) {
			return errors.New("invalid probe aggregate")
		}
	case *daily:
		if !validUint(v.RX) || !validUint(v.TX) || !validCoverage(v.Coverage, 86400) || !validCount(v.Pairs) || !validCount(v.Resets) {
			return errors.New("invalid traffic aggregate")
		}
	}
	return nil
}

// Coverage measures observed time, capped at one expected interval per sample.
// Fast/replayed reports cannot turn one second of reception into a full minute.
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
