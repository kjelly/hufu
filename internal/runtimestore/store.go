// Package runtimestore owns runtime.sqlite, the authoritative per-workspace
// store for hufu's event journal and runtime state. Memory stays in
// context.sqlite. A writable open creates and migrates the database; a
// read-only open never creates a file, runs a migration, or enables WAL.
// VACUUM, PRAGMA optimize, and checkpoints are never run
// (docs/architecture/sqlite-maintenance-policy.md).
package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kjelly/hufu/internal/sqlmigrate"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// File is the database file name at the workspace root.
const File = "runtime.sqlite"

// Files lists the database and every SQLite sidecar that may sit next to it.
// Snapshot, export, and tool-ignore lists exclude all of them.
var Files = []string{File, File + "-wal", File + "-shm", File + "-journal"}

var (
	// ErrNotFound reports that a read-only open found no database. It wraps
	// os.ErrNotExist so callers that treat a missing log as empty keep working.
	ErrNotFound = fmt.Errorf("runtime store not found: %w", os.ErrNotExist)
	// ErrLegacyWorkspace reports a workspace written by a hufu that kept its
	// event journal and runtime state in files. Such workspaces are not
	// migrated; remove it with `hufu workspace delete` and start a new one.
	ErrLegacyWorkspace = errors.New("workspace uses the retired file-based event log and runtime state; remove it with `hufu workspace delete` and start a new workspace")
	// ErrReplaced reports that the database path no longer names the file a
	// store opened. Writes through the open connection would reach a file no
	// later reader sees.
	ErrReplaced = errors.New("runtime store file was replaced or removed")
)

// legacyMarkers are files only a file-based workspace contains.
var legacyMarkers = []string{filepath.Join("logs", "event_store.jsonl"), "session.json"}

// Path returns the database path of workspace.
func Path(workspace string) string {
	return filepath.Join(workspace, File)
}

// Store is an open runtime.sqlite handle.
type Store struct {
	db       *sql.DB
	path     string
	readOnly bool
	shared   *sharedHandle
}

// sharedHandle lets every writable open of one workspace in a process share
// a single connection, so the event journal and runtime documents serialize
// on it instead of contending as separate writers.
type sharedHandle struct {
	db   *sql.DB
	refs int
	// file identifies the database file the connection opened.
	file os.FileInfo
	// retired marks a connection closed because its file was replaced or
	// removed; its remaining users fail on their next statement.
	retired bool
}

var (
	sharedMu sync.Mutex
	handles  = make(map[string]*sharedHandle)
)

// Open opens workspace's runtime.sqlite for writing, creating and migrating
// it when missing. Callers must Close the store.
func Open(ctx context.Context, workspace string) (*Store, error) {
	path, err := resolvePath(workspace)
	if err != nil {
		return nil, err
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if handle := currentHandleLocked(path); handle != nil {
		handle.refs++
		return &Store{db: handle.db, path: path, shared: handle}, nil
	}
	if err := refuseLegacy(workspace, path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("open runtime store: create workspace: %w", err)
	}
	if err := createPrivate(path); err != nil {
		return nil, fmt.Errorf("open runtime store: %w", err)
	}
	db, err := sql.Open("sqlite", writableDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open runtime store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlmigrate.Apply(ctx, db, "runtime store", migrations, nil); err != nil {
		return nil, errors.Join(fmt.Errorf("open runtime store: %w", err), db.Close())
	}
	if err := restrictSidecars(path); err != nil {
		return nil, errors.Join(fmt.Errorf("open runtime store: %w", err), db.Close())
	}
	file, err := os.Stat(path)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open runtime store: %w", err), db.Close())
	}
	handle := &sharedHandle{db: db, refs: 1, file: file}
	handles[path] = handle
	return &Store{db: db, path: path, shared: handle}, nil
}

// createPrivate makes sure the database file exists with mode 0600 before
// SQLite opens it. SQLite creates the -wal, -shm, and -journal files with
// the database file's mode, so they stay private too regardless of umask.
func createPrivate(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return file.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict database permissions: %w", err)
	}
	return nil
}

// restrictSidecars tightens sidecars a previous connection left with a wider
// mode; SQLite reuses an existing sidecar's permissions.
func restrictSidecars(path string) error {
	for _, name := range Files[1:] {
		sidecar := filepath.Join(filepath.Dir(path), name)
		if err := os.Chmod(sidecar, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restrict %s permissions: %w", name, err)
		}
	}
	return nil
}

// currentHandleLocked returns the shared handle for path when path still
// names the file it opened. A handle whose file was replaced or removed is
// retired first. sharedMu must be held.
func currentHandleLocked(path string) *sharedHandle {
	handle := handles[path]
	if handle == nil {
		return nil
	}
	if current, err := os.Stat(path); err == nil && os.SameFile(handle.file, current) {
		return handle
	}
	retireLocked(path, handle)
	return nil
}

// retireLocked stops sharing handle and closes its connection at once. The
// stale connection must close before any connection opens the path again:
// SQLite removes the WAL file by name when its last connection closes, which
// would discard a newer connection's uncheckpointed commits. sharedMu must be
// held.
func retireLocked(path string, handle *sharedHandle) {
	if handles[path] == handle {
		delete(handles, path)
	}
	if !handle.retired {
		handle.retired = true
		_ = handle.db.Close()
	}
}

// OpenReadOnly opens an existing runtime.sqlite without creating, migrating,
// or otherwise changing any file. A missing database returns ErrNotFound, or
// ErrLegacyWorkspace for a file-based workspace.
func OpenReadOnly(ctx context.Context, workspace string) (*Store, error) {
	path, err := resolvePath(workspace)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if legacyErr := refuseLegacy(workspace, path); legacyErr != nil {
			return nil, legacyErr
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open read-only runtime store: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("open read-only runtime store: %q is not a regular file", path)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open read-only runtime store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlmigrate.Verify(ctx, db, "runtime store", migrations); err != nil {
		return nil, errors.Join(fmt.Errorf("open read-only runtime store: %w", err), db.Close())
	}
	return &Store{db: db, path: path, readOnly: true}, nil
}

// OpenRead opens workspace's runtime.sqlite for reading without creating or
// changing any file. It shares this process's writable connection when one is
// open, so reads see that connection's commits without another connection,
// and otherwise opens read-only. Writes through the returned store fail.
func OpenRead(ctx context.Context, workspace string) (*Store, error) {
	path, err := resolvePath(workspace)
	if err != nil {
		return nil, err
	}
	sharedMu.Lock()
	if handle := currentHandleLocked(path); handle != nil {
		handle.refs++
		sharedMu.Unlock()
		return &Store{db: handle.db, path: path, readOnly: true, shared: handle}, nil
	}
	sharedMu.Unlock()
	return OpenReadOnly(ctx, workspace)
}

// Exists reports whether workspace has a runtime.sqlite.
func Exists(workspace string) bool {
	info, err := os.Lstat(Path(workspace))
	return err == nil && info.Mode().IsRegular()
}

// DB returns the underlying handle. Writable handles open transactions with
// BEGIN IMMEDIATE, so busy_timeout applies to a contended write.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the database path.
func (s *Store) Path() string { return s.path }

// CheckCurrent fails with ErrReplaced when the database path no longer names
// the file this store's shared connection opened. The stale connection is
// then retired, so the next Open reaches the file the path names. Stores with
// their own read-only connection always pass. Every write entry point checks
// it: a write through a stale connection would report success for data no
// later reader sees.
func (s *Store) CheckCurrent() error {
	if s.shared == nil {
		return nil
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if s.shared.retired {
		return fmt.Errorf("%w: %s", ErrReplaced, s.path)
	}
	current, err := os.Stat(s.path)
	if err == nil && os.SameFile(s.shared.file, current) {
		return nil
	}
	retireLocked(s.path, s.shared)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrReplaced, err)
	}
	return fmt.Errorf("%w: %s", ErrReplaced, s.path)
}

// ReadOnly reports whether the store was opened read-only.
func (s *Store) ReadOnly() bool { return s.readOnly }

// Close releases the store. The shared writable connection closes with its
// last user.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.shared == nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	handle := s.shared
	s.db, s.shared = nil, nil
	handle.refs--
	if handle.refs > 0 {
		return nil
	}
	if handles[s.path] == handle {
		delete(handles, s.path)
	}
	if handle.retired {
		return nil
	}
	return handle.db.Close()
}

func resolvePath(workspace string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", fmt.Errorf("open runtime store: empty workspace")
	}
	path, err := filepath.Abs(Path(workspace))
	if err != nil {
		return "", fmt.Errorf("open runtime store: resolve path: %w", err)
	}
	return path, nil
}

// refuseLegacy fails for a workspace that has file-based state but no
// database. It runs only when the database does not exist yet.
func refuseLegacy(workspace, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	for _, marker := range legacyMarkers {
		if _, err := os.Lstat(filepath.Join(workspace, marker)); err == nil {
			return fmt.Errorf("%w (%s)", ErrLegacyWorkspace, workspace)
		}
	}
	return nil
}

// writableDSN applies the durability pragmas to every pooled connection:
// WAL so read-only CLIs can read during a run, synchronous=FULL so a commit
// is on disk, and IMMEDIATE transactions so busy_timeout covers writes.
func writableDSN(path string) string {
	uri := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := uri.Query()
	query.Set("mode", "rwc")
	query.Set("_txlock", "immediate")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	uri.RawQuery = query.Encode()
	return uri.String()
}

// readOnlyDSN never writes. A read-only open of a WAL database creates an
// empty -wal when none exists; immutable mode prevents that in the quiescent
// case, while a present WAL keeps normal locking so committed records that
// were not checkpointed stay visible.
func readOnlyDSN(path string) string {
	uri := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := uri.Query()
	query.Set("mode", "ro")
	if _, err := os.Stat(path + "-wal"); errors.Is(err, os.ErrNotExist) {
		query.Set("immutable", "1")
	}
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	uri.RawQuery = query.Encode()
	return uri.String()
}
