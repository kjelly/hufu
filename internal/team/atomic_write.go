package team

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/kjelly/hufu/internal/fsutil"
)

// SyncDir performs an fsync operation on the specified directory to ensure directory entry changes are committed to disk.
func SyncDir(dir string) error { return fsutil.SyncDir(dir) }

// atomicTempFilePattern matches the temp-file suffix AtomicWriteFile and
// AtomicCreateFile always use: "<path>.tmp.<8 hex chars>".
var atomicTempFilePattern = fsutil.AtomicTempFilePattern

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
	return fsutil.AtomicWriteFile(path, data, perm)
}

// AtomicCreateFile publishes a fully-synced new file without replacing an
// existing path. The hard-link step is atomic and fails when target already
// exists, which closes the check/write race for promotion-created skills.
func AtomicCreateFile(path string, data []byte, perm os.FileMode) error {
	return fsutil.AtomicCreateFile(path, data, perm)
}
