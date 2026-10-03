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

const groupSchemaMarker = "-- Node groups (schema v5).\n"
const operationSchemaMarker = "-- Configuration operations (schema v7).\n"
const identitySchemaMarker = "-- Database identity;"

type request struct {
	sample *sample
	call   func(*sql.Tx) error
	ctx    context.Context
	done   chan error
}

type Store struct {
	db              *sql.DB
	cfg             Config
	queue           chan request
	stop            chan struct{}
	done            chan struct{}
	mu              sync.RWMutex
	closed          bool
	closeErr        error
	closeOnce       sync.Once
	migrationBackup string
	unhealthy       atomic.Bool
	// The worker owns ingestErr; it reports any failed batch through Flush.
	ingestErr error
}

func Open(cfg Config) (*Store, error) {
	return openWithMigrationBackup(cfg, backupBeforeMigration)
}

// The backup callback makes failure-before-DDL testable without injecting a
// filesystem or SQLite replacement into ordinary traffic/configuration paths.
func openWithMigrationBackup(cfg Config, backup func(context.Context, *sql.DB, string, int) (string, error)) (*Store, error) {
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
	var schemaChange, migrationBackup string
	if version == 0 && application == 0 {
		var tables int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
			return fail(err)
		}
		if tables != 0 {
			return fail(errors.New("control database must be a new empty database"))
		}
		schemaChange = Schema
	} else if application != 1179798836 || (version != 4 && version != 5 && version != 6 && version != 7) {
		return fail(errors.New("unsupported control database"))
	} else if version < 7 {
		if version == 4 {
			groups, e := schemaSection(groupSchemaMarker, operationSchemaMarker)
			if e != nil {
				return fail(e)
			}
			schemaChange += groups
		}
		if version == 4 || version == 5 {
			schemaChange += "ALTER TABLE nodes DROP COLUMN counter_scope;\n"
		}
		operations, e := schemaSection(operationSchemaMarker, identitySchemaMarker)
		if e != nil {
			return fail(e)
		}
		schemaChange += operations + "PRAGMA user_version=7;"
		migrationBackup, err = backup(ctx, db, path, version)
		if err != nil {
			return fail(fmt.Errorf("control migration backup failed: %w", err))
		}
	}
	if schemaChange != "" {
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			return fail(e)
		}
		if _, e = tx.ExecContext(ctx, schemaChange); e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
	}
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fail(err)
	}
	if mode != "wal" {
		return fail(errors.New("control WAL unavailable"))
	}
	s := &Store{db: db, cfg: cfg, queue: make(chan request, cfg.QueueCapacity), stop: make(chan struct{}), done: make(chan struct{}), migrationBackup: migrationBackup}
	go s.run()
	return s, nil
}

func schemaSection(start, end string) (string, error) {
	_, suffix, found := strings.Cut(Schema, start)
	if !found {
		return "", errors.New("missing control migration schema")
	}
	section, _, found := strings.Cut(suffix, end)
	if !found {
		return "", errors.New("missing control migration boundary")
	}
	return section, nil
}

// MigrationBackupPath identifies the private consistent pre-migration image.
// It is empty for fresh databases and databases already using the current schema.
func (s *Store) MigrationBackupPath() string { return s.migrationBackup }

// VACUUM INTO reads SQLite's consistent view, including committed WAL records;
// copying the main file would silently omit those records. Delete incomplete
// images, and durably finish a private recovery image before running any DDL.
func backupBeforeMigration(ctx context.Context, db *sql.DB, path string, version int) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+fmt.Sprintf(".pre-v7-v%d-", version)+"*.sqlite")
	if err != nil {
		return "", errors.New("cannot create private migration backup")
	}
	backupPath := f.Name()
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(backupPath)
		}
	}()
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return "", errors.New("cannot secure migration backup")
	}
	if err = f.Close(); err != nil {
		return "", errors.New("cannot close migration backup")
	}
	if _, err = db.ExecContext(ctx, "VACUUM main INTO ?", backupPath); err != nil {
		return "", errors.New("cannot create consistent migration backup")
	}
	st, err := os.Lstat(backupPath)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return "", errors.New("invalid migration backup file")
	}
	f, err = os.OpenFile(backupPath, os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("cannot open migration backup")
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return "", errors.New("cannot sync migration backup")
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return "", errors.New("cannot open migration backup directory")
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err != nil || closeErr != nil {
		return "", errors.New("cannot sync migration backup directory")
	}
	complete = true
	return backupPath, nil
}

func securePath(path string) error {
	parent := filepath.Dir(path)
	// Validate the existing ancestor chain before creating anything: MkdirAll
	// would otherwise traverse a symlink ancestor and create directories at its
	// target. The full chain is re-checked after creation.
	if err := realDirChain(parent); err != nil {
		return err
	}
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

// realDirChain requires every existing component of path to be a real
// directory, never a symlink. Missing components are left for the caller to
// create and re-check.
func realDirChain(path string) error {
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err == nil {
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return errors.New("control parent must be a real directory")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
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
			select {
			case r = <-s.queue:
			case <-s.stop:
				// Drain requests enqueued before Close. A request racing the
				// drain may stay queued; its caller unblocks through s.done or
				// its own context instead.
				for {
					select {
					case r = <-s.queue:
						s.serve(r)
					default:
						return
					}
				}
			}
		}
		if r.sample == nil {
			s.serveCall(r)
			continue
		}
		batch := []*sample{r.sample}
	collect:
		for len(batch) < 256 {
			select {
			case next := <-s.queue:
				if next.sample == nil {
					pending = &next
					break collect
				}
				batch = append(batch, next.sample)
			default:
				break collect
			}
		}
		s.apply(batch)
	}
}

// apply records one counter batch and reflects its outcome in ingest health.
func (s *Store) apply(batch []*sample) {
	err := s.applyBatch(batch)
	if err != nil {
		s.ingestErr = err
	}
	s.unhealthy.Store(err != nil)
}

// serve handles a single request during shutdown draining, without batching.
func (s *Store) serve(r request) {
	if r.sample == nil {
		s.serveCall(r)
		return
	}
	s.apply([]*sample{r.sample})
}

func (s *Store) serveCall(r request) {
	tx, err := s.db.BeginTx(r.ctx, nil)
	if err == nil {
		err = r.call(tx)
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	// database/sql can roll the transaction back on cancellation before
	// Commit runs, returning ErrTxDone instead of the context error.
	if errors.Is(err, sql.ErrTxDone) && r.ctx.Err() != nil {
		err = r.ctx.Err()
	}
	r.done <- err
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

// ingestReport wraps an earlier batch failure retold by Flush. The worker
// already updated ingest health when the failure happened, so the retelling
// must not degrade a store that has since recovered.
type ingestReport struct{ err error }

func (e ingestReport) Error() string { return e.err.Error() }
func (e ingestReport) Unwrap() error { return e.err }

func (s *Store) call(ctx context.Context, fn func(*sql.Tx) error) error {
	r := request{ctx: ctx, call: fn, done: make(chan error, 1)}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	// Never hold the lock across a blocking send: a full queue would otherwise
	// stall Close waiting for the write lock. Closing or cancellation unblocks
	// the wait, mirroring the history store's Flush.
	select {
	case s.queue <- r:
	case <-s.stop:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-r.done:
		return s.classify(err)
	case <-s.done:
		// A request the worker completed before exiting still has its real
		// result queued; only unanswered requests report closed.
		select {
		case err := <-r.done:
			return s.classify(err)
		default:
			return ErrClosed
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// classify maps SQLite constraint violations to conflicts and marks real
// storage failures unhealthy. Caller cancellation and Flush retellings
// (ingestReport) are exempt.
func (s *Store) classify(err error) error {
	var constraint interface{ Code() int }
	if errors.As(err, &constraint) && (constraint.Code() == 2067 || constraint.Code() == 1555) {
		err = fmt.Errorf("%w: duplicate configuration value", ErrConflict)
	}
	var report ingestReport
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) && !errors.As(err, &report) {
		s.unhealthy.Store(true)
	}
	return err
}

// Healthy reports durable accounting availability. A successful counter batch
// restores health after a transient failure because it reuses the stored baseline.
func (s *Store) Healthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed && !s.unhealthy.Load()
}

func (s *Store) Flush(ctx context.Context) error {
	err := s.call(ctx, func(*sql.Tx) error {
		err := s.ingestErr
		s.ingestErr = nil
		if err != nil {
			return ingestReport{err}
		}
		return nil
	})
	var report ingestReport
	if errors.As(err, &report) {
		return report.err
	}
	return err
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.stop)
		s.mu.Unlock()
		<-s.done
		s.closeErr = errors.Join(s.ingestErr, s.db.Close())
	})
	return s.closeErr
}
