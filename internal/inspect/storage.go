package inspect

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/runtimestore"
)

type StorageData struct {
	PageCount         int64  `json:"page_count"`
	FreelistCount     int64  `json:"freelist_count"`
	PageSize          int64  `json:"page_size"`
	JournalMode       string `json:"journal_mode"`
	WALAutoCheckpoint int64  `json:"wal_autocheckpoint"`
	SchemaVersion     int64  `json:"schema_version"`
	DatabaseBytes     int64  `json:"database_bytes"`
	WALBytes          int64  `json:"wal_bytes"`
	FTSRows           int64  `json:"fts_rows"`
	ContextRows       int64  `json:"context_rows"`
	// Runtime describes runtime.sqlite; it is absent before the workspace's
	// first run.
	Runtime *RuntimeStorageData `json:"runtime,omitempty"`
}

// RuntimeStorageData describes runtime.sqlite, which holds the event journal
// and runtime state.
type RuntimeStorageData struct {
	runtimestore.Diagnostics
	DatabaseBytes int64 `json:"database_bytes"`
	WALBytes      int64 `json:"wal_bytes"`
}

func InspectStorage(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindStorage); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: inspect storage: %w", ErrIntegrity, err)
	}
	path := filepath.Join(query.Workspace, "context.sqlite")
	snapshotPath, databaseBytes, walBytes, cleanup, err := snapshotSQLiteStorage(ctx, path, "context.sqlite")
	if err != nil {
		return nil, fmt.Errorf("%w: snapshot context database read-only: %w", ErrIntegrity, err)
	}
	defer cleanup()
	repo, err := contextstore.OpenSQLiteReadOnly(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("%w: open context database read-only: %v", ErrIntegrity, err)
	}
	defer func() { _ = repo.Close() }()
	diagnostics, err := repo.StorageDiagnostics(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect context storage: %w", ErrIntegrity, err)
	}
	data := StorageData{
		PageCount: diagnostics.PageCount, FreelistCount: diagnostics.FreelistCount, PageSize: diagnostics.PageSize,
		JournalMode: diagnostics.JournalMode, WALAutoCheckpoint: diagnostics.WALAutoCheckpoint, SchemaVersion: diagnostics.SchemaVersion,
		DatabaseBytes: databaseBytes, WALBytes: walBytes, FTSRows: diagnostics.FTSRows, ContextRows: diagnostics.ContextRows,
	}
	if data.Runtime, err = inspectRuntimeStorage(ctx, query.Workspace); err != nil {
		return nil, fmt.Errorf("%w: inspect runtime storage: %w", ErrIntegrity, err)
	}
	return envelope(KindStorage, query, "", data), nil
}

// inspectRuntimeStorage diagnoses a snapshot of runtime.sqlite, so the live
// database never gains a sidecar. A workspace without one returns nil.
func inspectRuntimeStorage(ctx context.Context, workspace string) (*RuntimeStorageData, error) {
	path := runtimestore.Path(workspace)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil, nil
	}
	snapshotPath, databaseBytes, walBytes, cleanup, err := snapshotSQLiteStorage(ctx, path, runtimestore.File)
	if err != nil {
		return nil, fmt.Errorf("snapshot read-only: %w", err)
	}
	defer cleanup()
	store, err := runtimestore.OpenReadOnly(ctx, filepath.Dir(snapshotPath))
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	diagnostics, err := store.Diagnose(ctx)
	if err != nil {
		return nil, err
	}
	return &RuntimeStorageData{Diagnostics: diagnostics, DatabaseBytes: databaseBytes, WALBytes: walBytes}, nil
}

// snapshotSQLiteStorage copies a database with a WAL, under name, into a
// temporary directory so it can be opened without touching the original. A
// database without a WAL is returned as is.
func snapshotSQLiteStorage(ctx context.Context, path, name string) (snapshotPath string, databaseBytes, walBytes int64, cleanup func(), err error) {
	databaseBytes, err = regularFileSize(path)
	if err != nil {
		return "", 0, 0, func() {}, err
	}
	walInfo, err := os.Stat(path + "-wal")
	if os.IsNotExist(err) {
		return path, databaseBytes, 0, func() {}, nil
	}
	if err != nil {
		return "", 0, 0, func() {}, err
	}
	if !walInfo.Mode().IsRegular() {
		return "", 0, 0, func() {}, fmt.Errorf("%q is not a regular file", path+"-wal")
	}
	walBytes = walInfo.Size()
	tempDir, err := os.MkdirTemp("", "hufu-inspect-storage-")
	if err != nil {
		return "", 0, 0, func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(tempDir) }
	snapshotPath = filepath.Join(tempDir, name)
	if err = copyRegularFilePrefix(ctx, path, snapshotPath, databaseBytes); err != nil {
		cleanup()
		return "", 0, 0, func() {}, err
	}
	if walBytes > 0 {
		if err = copyRegularFilePrefix(ctx, path+"-wal", snapshotPath+"-wal", walBytes); err != nil {
			cleanup()
			return "", 0, 0, func() {}, err
		}
	}
	return snapshotPath, databaseBytes, walBytes, cleanup, nil
}

func copyRegularFilePrefix(ctx context.Context, sourcePath, targetPath string, size int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()
	buffer := make([]byte, 128*1024)
	remaining := size
	for remaining > 0 {
		if err = ctx.Err(); err != nil {
			return err
		}
		chunk := int64(len(buffer))
		if remaining < chunk {
			chunk = remaining
		}
		written, copyErr := io.CopyBuffer(target, io.LimitReader(source, chunk), buffer)
		if copyErr != nil {
			return copyErr
		}
		if written != chunk {
			return io.ErrUnexpectedEOF
		}
		remaining -= written
	}
	return target.Sync()
}

func regularFileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%q is not a regular file", path)
	}
	return info.Size(), nil
}
