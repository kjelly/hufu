package auditverify

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxBundleEntryBytes bounds any single tar entry's declared size, guarding
// against an oversized metadata entry (spec.md §53).
const maxBundleEntryBytes = 512 * 1024 * 1024 // 512 MiB

// bundleFileEntry is one in-memory file to write into a tar archive.
type bundleFileEntry struct {
	path string
	data []byte
}

// writeTarArchive writes entries into a deterministic tar stream using only
// the Go standard library (spec.md §9.1 forbids depending on an external
// shell tar binary): fixed zero mtime rather than the export wall-clock time
// (spec.md §10), and every entry is a plain regular file -- this writer never
// emits a symlink or hardlink header.
func writeTarArchive(w io.Writer, entries []bundleFileEntry) error {
	tw := tar.NewWriter(w)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.path, Typeflag: tar.TypeReg, Size: int64(len(entry.data)),
			Mode: 0o600, ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write tar header %q: %w", entry.path, err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			return fmt.Errorf("write tar data %q: %w", entry.path, err)
		}
	}
	return tw.Close()
}

// extractTarArchive extracts a tar stream into destDir, rejecting any entry
// that is unsafe (spec.md §23.1, §53): an absolute or "../"-escaping path, a
// path that resolves outside destDir, a symlink/hardlink/any non-regular-file
// entry, a duplicate path, or an oversized declared size. destDir must
// already exist.
func extractTarArchive(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	seen := make(map[string]bool)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		switch header.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir:
			continue // directories are created implicitly, below, as files need them
		default:
			return fmt.Errorf("bundle entry %q has unsupported type %d (symlinks/hardlinks are not allowed)", header.Name, header.Typeflag)
		}
		if header.Size < 0 || header.Size > maxBundleEntryBytes {
			return fmt.Errorf("bundle entry %q declares an invalid size %d", header.Name, header.Size)
		}
		if seen[header.Name] {
			return fmt.Errorf("bundle entry %q appears more than once", header.Name)
		}
		seen[header.Name] = true

		cleanPath, err := safeJoin(destDir, header.Name)
		if err != nil {
			return fmt.Errorf("bundle entry %q: %w", header.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(cleanPath), 0o700); err != nil {
			return fmt.Errorf("create directory for %q: %w", header.Name, err)
		}
		f, err := os.OpenFile(cleanPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create %q: %w", header.Name, err)
		}
		written, copyErr := io.CopyN(f, tr, header.Size)
		closeErr := f.Close()
		if copyErr != nil && copyErr != io.EOF {
			return fmt.Errorf("write %q: %w", header.Name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %q: %w", header.Name, closeErr)
		}
		if written != header.Size {
			return fmt.Errorf("bundle entry %q wrote %d bytes, want %d", header.Name, written, header.Size)
		}
	}
}

// safeJoin resolves entryPath under destDir, refusing an absolute path, a
// Windows drive-letter path, or any path -- via "../" or otherwise -- that
// would escape destDir.
func safeJoin(destDir, entryPath string) (string, error) {
	if entryPath == "" {
		return "", fmt.Errorf("empty path")
	}
	cleaned := filepath.Clean(entryPath)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("unsafe path %q", entryPath)
	}
	// A Windows drive-letter path ("C:\foo") is not filepath.IsAbs on a
	// non-Windows build; reject that shape explicitly regardless of GOOS.
	if len(cleaned) >= 2 && cleaned[1] == ':' {
		return "", fmt.Errorf("unsafe path %q", entryPath)
	}
	joined := filepath.Join(destDir, cleaned)
	destWithSep := destDir
	if !strings.HasSuffix(destWithSep, string(filepath.Separator)) {
		destWithSep += string(filepath.Separator)
	}
	if joined != destDir && !strings.HasPrefix(joined, destWithSep) {
		return "", fmt.Errorf("path %q escapes bundle root", entryPath)
	}
	return joined, nil
}
