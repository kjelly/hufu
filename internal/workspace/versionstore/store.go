package versionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kjelly/hufu/internal/sqlmigrate"

	_ "modernc.org/sqlite"
)

// Options configures Open.
type Options struct {
	// StateDir is Project.StateDir; the store lives in StateDir/workspace-versions.
	StateDir string
	// ReadOnly opens an existing store with query_only connections and never
	// creates files.
	ReadOnly bool
	// Now and Random are injectable for deterministic tests.
	Now    func() time.Time
	Random io.Reader
}

// Store is the SQLite + CAS workspace version store for one project.
type Store struct {
	layout   layout
	db       *sql.DB
	readOnly bool
	now      func() time.Time
	random   io.Reader

	// mu serializes mutating operations inside one process; the project
	// lock (lock.go) serializes processes.
	mu sync.Mutex

	// treeReads and statCacheHits are test counters (nil in production).
	treeReads     *atomic.Int64
	statCacheHits *atomic.Int64
}

// Open opens (and, unless read-only, creates and migrates) the store.
func Open(ctx context.Context, options Options) (*Store, error) {
	if !PlatformSupported() {
		return nil, ErrUnsupportedPlatform
	}
	if options.StateDir == "" {
		return nil, fmt.Errorf("open workspace version store: %w: empty state dir", ErrVersionedWorkspaceUnresolved)
	}
	root, err := filepath.Abs(StorageRoot(options.StateDir))
	if err != nil {
		return nil, fmt.Errorf("resolve workspace version store: %w", err)
	}
	store := &Store{layout: layout{root: root}, readOnly: options.ReadOnly, now: options.Now, random: options.Random}
	if store.now == nil {
		store.now = time.Now
	}
	if options.ReadOnly {
		if _, statErr := os.Stat(store.layout.dbPath()); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, ErrStoreNotFound
			}
			return nil, fmt.Errorf("stat workspace version store: %w", statErr)
		}
	} else if err = store.layout.ensure(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", store.dsn())
	if err != nil {
		return nil, fmt.Errorf("open workspace version store: %w", err)
	}
	db.SetMaxOpenConns(1)
	store.db = db
	if options.ReadOnly {
		err = sqlmigrate.Verify(ctx, db, "workspace versions", migrations)
	} else {
		err = sqlmigrate.Apply(ctx, db, "workspace versions", migrations, store.now)
		if err == nil {
			err = os.Chmod(store.layout.dbPath(), 0o600)
		}
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("prepare workspace version store: %w", err), db.Close())
	}
	return store, nil
}

// dsn applies the durability-first pragmas of the workspace registry to every
// pooled connection. VACUUM, PRAGMA optimize, and checkpoints are never run
// (docs/architecture/sqlite-maintenance-policy.md).
func (s *Store) dsn() string {
	uri := &url.URL{Scheme: "file", Path: filepath.ToSlash(s.layout.dbPath())}
	query := uri.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	if s.readOnly {
		query.Set("mode", "ro")
		query.Add("_pragma", "query_only(1)")
	} else {
		query.Set("mode", "rwc")
		query.Add("_pragma", "journal_mode(DELETE)")
		query.Add("_pragma", "synchronous(FULL)")
	}
	uri.RawQuery = query.Encode()
	return uri.String()
}

// Root returns the store directory.
func (s *Store) Root() string { return s.layout.root }

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) newSnapshotID() (SnapshotID, error) {
	id, err := newID(s.random, "wsv")
	return SnapshotID(id), err
}

func (s *Store) requireWritable() error {
	if s.readOnly {
		return errors.New("workspace version store is read-only")
	}
	return nil
}

// withTx runs fn in one transaction and commits only when fn succeeds.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workspace version transaction: %w", err)
	}
	if err = fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit workspace version transaction: %w", err)
	}
	return nil
}
