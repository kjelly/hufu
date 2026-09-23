package versionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/fsutil"
)

// casWrite is the outcome of publishing one object.
type casWrite struct {
	Hash string
	Size int64
	// New is false when an identical object already existed.
	New bool
}

// limitedHashWriter hashes and counts bytes, failing once limit is exceeded.
type limitedHashWriter struct {
	dst   io.Writer
	limit int64
	n     int64
}

func (w *limitedHashWriter) Write(p []byte) (int, error) {
	if w.limit > 0 && w.n+int64(len(p)) > w.limit {
		return 0, fmt.Errorf("%w: object exceeds %d bytes", ErrCaptureLimitExceeded, w.limit)
	}
	n, err := w.dst.Write(p)
	w.n += int64(n)
	return n, err
}

// putObject streams r into the CAS. Bytes are written to tmp/, hashed while
// written, fsynced, and only then hard-linked into objects/ — a link never
// replaces an existing file, so a published object is immutable. When
// expectedHash is non-empty and differs from the actual digest, nothing is
// published and ErrWorkspaceChangedAfterObservation is returned. maxBytes of
// zero is unlimited.
func (s *Store) putObject(ctx context.Context, kind ObjectKind, r io.Reader, expectedHash string, maxBytes int64) (casWrite, error) {
	if err := ctx.Err(); err != nil {
		return casWrite{}, err
	}
	tmp, err := os.CreateTemp(s.layout.tmpDir(), "obj-*")
	if err != nil {
		return casWrite{}, fmt.Errorf("create object temp file: %w", err)
	}
	tmpPath := tmp.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tmpPath)
		}
	}()
	hasher := sha256.New()
	counter := &limitedHashWriter{dst: io.MultiWriter(tmp, hasher), limit: maxBytes}
	if _, err = io.Copy(counter, r); err != nil {
		_ = tmp.Close()
		return casWrite{}, fmt.Errorf("write %s object: %w", kind, err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return casWrite{}, fmt.Errorf("sync %s object: %w", kind, err)
	}
	if err = tmp.Close(); err != nil {
		return casWrite{}, fmt.Errorf("close %s object: %w", kind, err)
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	if expectedHash != "" && hash != expectedHash {
		return casWrite{}, fmt.Errorf("%w: %s object hash %s, observed %s", ErrWorkspaceChangedAfterObservation, kind, hash, expectedHash)
	}
	final, err := s.layout.objectPath(kind, hash)
	if err != nil {
		return casWrite{}, err
	}
	fanout := filepath.Dir(final)
	if err = os.MkdirAll(fanout, 0o700); err != nil {
		return casWrite{}, fmt.Errorf("create object directory: %w", err)
	}
	if err = os.Link(tmpPath, final); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return casWrite{}, fmt.Errorf("publish %s object: %w", kind, err)
		}
		if verifyErr := s.verifyExistingObject(kind, hash, counter.n); verifyErr != nil {
			return casWrite{}, verifyErr
		}
		return casWrite{Hash: hash, Size: counter.n, New: false}, nil
	}
	published = true
	_ = os.Remove(tmpPath)
	for _, dir := range []string{fanout, filepath.Dir(fanout)} {
		if err = fsutil.SyncDir(dir); err != nil {
			return casWrite{}, fmt.Errorf("sync object directory: %w", err)
		}
	}
	return casWrite{Hash: hash, Size: counter.n, New: true}, nil
}

// putBytes publishes a small in-memory object (trees and link targets).
func (s *Store) putBytes(ctx context.Context, kind ObjectKind, data []byte) (casWrite, error) {
	return s.putObject(ctx, kind, bytes.NewReader(data), "", 0)
}

// verifyExistingObject is the reuse check for an object that is already
// present. Blobs are verified by size (their name is their content hash and a
// full re-hash on every reuse would make capture O(total bytes)); small tree
// and link objects are fully re-hashed.
func (s *Store) verifyExistingObject(kind ObjectKind, hash string, size int64) error {
	path, err := s.layout.objectPath(kind, hash)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: stat existing %s object %s: %v", ErrCASCorrupt, kind, hash, err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return fmt.Errorf("%w: existing %s object %s has size %d, want %d", ErrCASCorrupt, kind, hash, info.Size(), size)
	}
	if kind == ObjectBlob {
		return nil
	}
	_, err = s.hashObjectFile(kind, hash)
	return err
}

// hasObject reports whether an object file exists (without verifying it).
func (s *Store) hasObject(kind ObjectKind, hash string) (bool, error) {
	path, err := s.layout.objectPath(kind, hash)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s object: %w", kind, err)
	}
	return info.Mode().IsRegular(), nil
}

// openObject opens an object for reading.
func (s *Store) openObject(kind ObjectKind, hash string) (*os.File, error) {
	path, err := s.layout.objectPath(kind, hash)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s %s", ErrCASObjectMissing, kind, hash)
		}
		return nil, fmt.Errorf("open %s object: %w", kind, err)
	}
	return file, nil
}

// hashObjectFile re-hashes an object and fails with ErrCASCorrupt when the
// content no longer matches its name (I4). It returns the byte size.
func (s *Store) hashObjectFile(kind ObjectKind, hash string) (int64, error) {
	file, err := s.openObject(kind, hash)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	n, err := io.Copy(hasher, file)
	if err != nil {
		return 0, fmt.Errorf("read %s object %s: %w", kind, hash, err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != hash {
		return 0, fmt.Errorf("%w: %s object %s hashes to %s", ErrCASCorrupt, kind, hash, got)
	}
	return n, nil
}

// maxSmallObjectBytes bounds tree and link objects read into memory.
const maxSmallObjectBytes = 64 << 20

// readSmallObject reads and verifies a tree or link object.
func (s *Store) readSmallObject(kind ObjectKind, hash string) ([]byte, error) {
	file, err := s.openObject(kind, hash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxSmallObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s object %s: %w", kind, hash, err)
	}
	if len(data) > maxSmallObjectBytes {
		return nil, fmt.Errorf("%w: %s object %s is too large", ErrCASCorrupt, kind, hash)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != hash {
		return nil, fmt.Errorf("%w: %s object %s hashes to %s", ErrCASCorrupt, kind, hash, got)
	}
	return data, nil
}

// putTree encodes and publishes a tree object.
func (s *Store) putTree(ctx context.Context, tree TreeObject) (casWrite, error) {
	data, hash, err := EncodeTree(tree)
	if err != nil {
		return casWrite{}, err
	}
	if s.treeWrites != nil {
		s.treeWrites.Add(1)
	}
	written, err := s.putBytes(ctx, ObjectTree, data)
	if err != nil {
		return casWrite{}, err
	}
	if written.Hash != hash {
		return casWrite{}, fmt.Errorf("%w: tree hash mismatch", ErrCASCorrupt)
	}
	return written, nil
}

// readTree reads, verifies, and decodes a tree object.
func (s *Store) readTree(hash string) (TreeObject, error) {
	data, err := s.readSmallObject(ObjectTree, hash)
	if err != nil {
		return TreeObject{}, err
	}
	tree, err := DecodeTree(data)
	if err != nil {
		return TreeObject{}, fmt.Errorf("decode tree %s: %w", hash, err)
	}
	return tree, nil
}

// readLink reads and verifies a symlink target object.
func (s *Store) readLink(hash string) (string, error) {
	data, err := s.readSmallObject(ObjectLink, hash)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
