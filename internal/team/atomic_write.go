package team

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
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

// atomicTempFilePattern matches the temp-file suffix AtomicWriteFile and
// AtomicCreateFile always use: "<path>.tmp.<8 hex chars>".
var atomicTempFilePattern = regexp.MustCompile(`\.tmp\.[0-9a-f]{8}$`)

// staleAtomicTempFileAge is how old a "*.tmp.<hex>" file must be before
// SweepStaleAtomicTempFiles treats it as orphaned rather than a write that is
// genuinely still in flight from a concurrent process.
const staleAtomicTempFileAge = 10 * time.Minute

// SweepStaleAtomicTempFiles removes leftover AtomicWriteFile/AtomicCreateFile
// temp files under root. Both writers always rename (or hard-link, then
// remove) their temp file into place immediately after a successful sync;
// the only way one survives is a process killed between that write and the
// rename, so any match older than staleAtomicTempFileAge is definitionally
// orphaned — the real content was already recovered from the file it was
// replacing (or never existed, for a create), never from the temp file
// itself. Left uncleaned, these accumulate indefinitely: a single crash mid
// event_store.jsonl rewrite has been observed leaving a multi-ten-megabyte
// orphan. Errors are intentionally ignored; this is best-effort housekeeping,
// not a correctness requirement, and must never fail workspace startup.
func SweepStaleAtomicTempFiles(root string) {
	if root == "" {
		return
	}
	cutoff := time.Now().Add(-staleAtomicTempFileAge)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !atomicTempFilePattern.MatchString(d.Name()) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || info.ModTime().After(cutoff) {
			return nil
		}
		_ = os.Remove(path)
		return nil
	})
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

	if err := SyncDir(dir); err != nil {
		return fmt.Errorf("atomic write sync directory: %w", err)
	}
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
	if err := SyncDir(dir); err != nil {
		return fmt.Errorf("atomic create sync directory: %w", err)
	}
	return nil
}
