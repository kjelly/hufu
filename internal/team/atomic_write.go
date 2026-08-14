package team

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// SyncDir performs an fsync operation on the specified directory to ensure directory entry changes are committed to disk.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// AtomicWriteFile safely writes data to path by writing to a temporary file,
// flushing to disk via Sync(), renaming atomically into place, and fsyncing the directory.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("atomic write mkdir: %w", err)
	}

	randBytes := make([]byte, 4)
	_, _ = rand.Read(randBytes)
	tmpPath := fmt.Sprintf("%s.tmp.%s", path, hex.EncodeToString(randBytes))

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("atomic write open temp: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic write write temp: %w", err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic write sync temp: %w", err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic write close temp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic write rename: %w", err)
	}

	_ = SyncDir(dir)
	return nil
}

// AtomicCreateFile publishes a fully-synced new file without replacing an
// existing path. The hard-link step is atomic and fails when target already
// exists, which closes the check/write race for promotion-created skills.
func AtomicCreateFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("atomic create mkdir: %w", err)
	}
	randBytes := make([]byte, 4)
	_, _ = rand.Read(randBytes)
	tmpPath := fmt.Sprintf("%s.tmp.%s", path, hex.EncodeToString(randBytes))
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("atomic create open temp: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("atomic create write temp: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("atomic create sync temp: %w", err)
	}
	if err = f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomic create close temp: %w", err)
	}
	if err = os.Link(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("atomic create publish: %w", err)
	}
	cleanup()
	_ = SyncDir(dir)
	return nil
}
