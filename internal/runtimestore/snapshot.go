package runtimestore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

var errSnapshotChanged = errors.New("runtime store changed while capturing its read-only snapshot")

type snapshotFile struct {
	path string
	info os.FileInfo
}

// snapshotRuntimeStore copies both the database and its WAL, including when
// no WAL was present at the first observation. Before accepting the copy it
// checks source identity, metadata and contents again. A writer starting,
// appending, checkpointing, closing, or replacing files invalidates the copy.
// SQLite only opens the accepted private files, never the mutable source.
func snapshotRuntimeStore(ctx context.Context, path string) (string, func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	files, err := runtimeSnapshotFiles(path)
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, runtimeSnapshotTimeout(files))
	defer cancel()
	for attempt := range 3 {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		dir, err := os.MkdirTemp("", "hufu-runtime-readonly-")
		if err != nil {
			return "", nil, err
		}
		cleanup := func() error { return os.RemoveAll(dir) }
		err = copyRuntimeSnapshot(ctx, path, dir, copySnapshotFile)
		if err == nil {
			return filepath.Join(dir, File), cleanup, nil
		}
		if cleanupErr := cleanup(); cleanupErr != nil {
			return "", nil, errors.Join(err, cleanupErr)
		}
		if !errors.Is(err, errSnapshotChanged) || attempt == 2 {
			return "", nil, err
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return "", nil, errSnapshotChanged
}

// runtimeSnapshotTimeout budgets both the copy and the verification read at
// 8 MiB/s, plus startup time. Large histories get a bounded chance to finish;
// a caller's earlier cancellation or deadline still takes precedence.
func runtimeSnapshotTimeout(files []snapshotFile) time.Duration {
	const bytesPerSecond = int64(4 << 20) // Two passes at 8 MiB/s.
	const maximum = 2 * time.Minute
	budget := 5 * time.Second
	for _, file := range files {
		if file.info == nil || file.info.Size() <= 0 {
			continue
		}
		seconds := file.info.Size() / bytesPerSecond
		if file.info.Size()%bytesPerSecond != 0 {
			seconds++
		}
		if seconds >= int64((maximum-budget)/time.Second) {
			return maximum
		}
		budget += time.Duration(seconds) * time.Second
	}
	return budget
}

type snapshotCopier func(context.Context, string, string, int64) ([sha256.Size]byte, error)

func copyRuntimeSnapshot(ctx context.Context, path, dir string, copyFile snapshotCopier) error {
	files, err := runtimeSnapshotFiles(path)
	if err != nil {
		return err
	}
	digests := make([][sha256.Size]byte, len(files))
	for i, file := range files {
		if file.info == nil {
			continue
		}
		digests[i], err = copyFile(ctx, file.path, filepath.Join(dir, File+file.path[len(path):]), file.info.Size())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return errors.Join(errSnapshotChanged, err)
			}
			return fmt.Errorf("copy %s: %w", filepath.Base(file.path), err)
		}
	}
	if err := verifySnapshotFiles(files); err != nil {
		return err
	}
	// Re-read each source rather than relying on timestamp resolution alone.
	// This also rejects modifications that preserve file size and timestamps.
	for i, file := range files {
		if file.info == nil {
			continue
		}
		digest, err := hashSnapshotFile(ctx, file.path, file.info.Size(), io.Discard)
		if err != nil {
			return errors.Join(errSnapshotChanged, fmt.Errorf("verify %s: %w", filepath.Base(file.path), err))
		}
		if digest != digests[i] {
			return errSnapshotChanged
		}
	}
	return verifySnapshotFiles(files)
}

func runtimeSnapshotFiles(path string) ([]snapshotFile, error) {
	files := make([]snapshotFile, 0, 3)
	for _, suffix := range []string{"", "-wal", "-journal"} {
		name := path + suffix
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			info, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
		if info != nil && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%q is not a regular file", name)
		}
		// Immutable mode must never bypass rollback-journal recovery. Runtime
		// writers use WAL; refuse an unfinished rollback transaction instead.
		if suffix == "-journal" && info != nil && info.Size() > 0 {
			return nil, errSnapshotChanged
		}
		files = append(files, snapshotFile{path: name, info: info})
	}
	return files, nil
}

func verifySnapshotFiles(files []snapshotFile) error {
	for _, file := range files {
		current, err := os.Lstat(file.path)
		if file.info == nil && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || file.info == nil || !current.Mode().IsRegular() ||
			!os.SameFile(file.info, current) || file.info.Size() != current.Size() ||
			!file.info.ModTime().Equal(current.ModTime()) {
			return errors.Join(errSnapshotChanged, err)
		}
	}
	return nil
}

func copySnapshotFile(ctx context.Context, source, target string, size int64) ([sha256.Size]byte, error) {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	digest, copyErr := hashSnapshotFile(ctx, source, size, file)
	return digest, errors.Join(copyErr, file.Close())
}

func hashSnapshotFile(ctx context.Context, path string, size int64, target io.Writer) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	writer := io.MultiWriter(target, hash)
	buffer := make([]byte, 1<<20)
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, fmt.Errorf("read stopped at byte %d of %d: %w", size-remaining, size, err)
		}
		n := min(remaining, int64(len(buffer)))
		if _, err := io.ReadFull(file, buffer[:n]); err != nil {
			return [sha256.Size]byte{}, err
		}
		if _, err := writer.Write(buffer[:n]); err != nil {
			return [sha256.Size]byte{}, err
		}
		remaining -= n
	}
	if err := ctx.Err(); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("read stopped after %d bytes: %w", size, err)
	}
	return [sha256.Size]byte(hash.Sum(nil)), nil
}
