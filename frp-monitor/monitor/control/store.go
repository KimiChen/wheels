package control

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	_ "modernc.org/sqlite"
)

// Schema is also used by the development initializer to build an empty database.
//
//go:embed schema.sql
var Schema string

type request struct {
	sample *sample
	call   func(*sql.Tx) error
	ctx    context.Context
	done   chan error
}

type Store struct {
	db        *sql.DB
	cfg       Config
	queue     chan request
	done      chan struct{}
	mu        sync.RWMutex
	closed    bool
	closeErr  error
	closeOnce sync.Once
	unhealthy atomic.Bool
	// The worker owns ingestErr; it reports any failed batch through Flush.
	ingestErr error
}

func Open(cfg Config) (*Store, error) {
	if cfg.ReportInterval == 0 {
		cfg.ReportInterval = time.Second
	}
	if cfg.QueueCapacity == 0 {
		cfg.QueueCapacity = 4096
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Path == "" || strings.ContainsAny(cfg.Path, "\x00\r\n") || cfg.ReportInterval < time.Second || cfg.ReportInterval > time.Hour || cfg.QueueCapacity < 1 || cfg.QueueCapacity > 65536 {
		return nil, fmt.Errorf("%w: control configuration", ErrInvalid)
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
	for _, v := range []string{"busy_timeout(5000)", "synchronous(FULL)", "foreign_keys(ON)"} {
		q.Add("_pragma", v)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version, application int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if err = db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&application); err != nil {
		return fail(err)
	}
	if version == 0 && application == 0 {
		var tables int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
			return fail(err)
		}
		if tables != 0 {
			return fail(errors.New("control database must be a new empty database"))
		}
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			return fail(e)
		}
		if _, e = tx.ExecContext(ctx, Schema); e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
	} else if version != 4 || application != 1179798836 {
		return fail(errors.New("unsupported control database"))
	}
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fail(err)
	}
	if mode != "wal" {
		return fail(errors.New("control WAL unavailable"))
	}
	s := &Store{db: db, cfg: cfg, queue: make(chan request, cfg.QueueCapacity), done: make(chan struct{})}
	go s.run()
	return s, nil
}

func securePath(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	for p := parent; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("control parent must be a real directory")
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
		return errors.New("control directory must have private permissions")
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
			return errors.New("control files must be regular and private")
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

func (s *Store) run() {
	defer close(s.done)
	var pending *request
	for {
		var r request
		if pending != nil {
			r = *pending
			pending = nil
		} else {
			var ok bool
			r, ok = <-s.queue
			if !ok {
				return
			}
		}
		if r.sample == nil {
			tx, err := s.db.BeginTx(r.ctx, nil)
			if err == nil {
				err = r.call(tx)
				if err == nil {
					err = tx.Commit()
				} else {
					tx.Rollback()
				}
			}
			r.done <- err
			continue
		}
		batch := []*sample{r.sample}
	collect:
		for len(batch) < 256 {
			select {
			case next, ok := <-s.queue:
				if !ok {
					break collect
				}
				if next.sample == nil {
					pending = &next
					break collect
				}
				batch = append(batch, next.sample)
			default:
				break collect
			}
		}
		err := s.applyBatch(batch)
		if err != nil {
			s.ingestErr = err
		}
		s.unhealthy.Store(err != nil)
	}
}

func (s *Store) applyBatch(batch []*sample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range batch {
		n, err := readNode(tx, v.id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if s.observe(n, *v) {
			if err = saveNode(tx, n); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Accept never blocks a connection handler. A dropped sample is recoverable
// through the next cumulative counter pair, with a gap marked as partial.
func (s *Store) Accept(id string, at time.Time, m shared.Metrics) bool {
	if !validID(id) {
		return false
	}
	v := observation(id, at, m)
	if v == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	select {
	case s.queue <- request{sample: v}:
		return true
	default:
		s.unhealthy.Store(true)
		return false
	}
}

func (s *Store) call(ctx context.Context, fn func(*sql.Tx) error) error {
	r := request{ctx: ctx, call: fn, done: make(chan error, 1)}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrClosed
	}
	select {
	case s.queue <- r:
		s.mu.RUnlock()
	case <-ctx.Done():
		s.mu.RUnlock()
		return ctx.Err()
	}
	select {
	case err := <-r.done:
		var constraint interface{ Code() int }
		if errors.As(err, &constraint) && (constraint.Code() == 2067 || constraint.Code() == 1555) {
			err = fmt.Errorf("%w: duplicate node credential or FRP binding", ErrConflict)
		}
		if err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
			s.unhealthy.Store(true)
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Healthy reports durable accounting availability. A successful counter batch
// restores health after a transient failure because it reuses the stored baseline.
func (s *Store) Healthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed && !s.unhealthy.Load()
}

func (s *Store) Flush(ctx context.Context) error {
	return s.call(ctx, func(*sql.Tx) error { err := s.ingestErr; s.ingestErr = nil; return err })
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.queue)
		s.mu.Unlock()
		<-s.done
		s.closeErr = errors.Join(s.ingestErr, s.db.Close())
	})
	return s.closeErr
}
