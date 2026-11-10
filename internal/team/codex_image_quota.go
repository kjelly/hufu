package team

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/kjelly/hufu/internal/media"
)

const codexProjectionLockFile = ".hufu-projections.lock"

// Include copies left by an interrupted process. A process-local reservation
// cannot account for those bytes. The caller holds the shared projection lock
// until all app-server consumers have drained and their copies are reclaimed.
func checkCodexProjectionCopies(ctx context.Context, base, current *os.Root, refs []media.Ref, limits media.Limits) error {
	entries, err := readCodexProjectionEntries(base, limits.MaxBranchBlobs+1)
	if err != nil {
		return err
	}
	var bytes int64
	count := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := base.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if entry.Name() == codexProjectionLockFile {
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				return media.Reject(media.AccessDenied, "unsafe Codex projection quota lock")
			}
			continue
		}
		hash, ok := strings.CutPrefix(entry.Name(), "input-")
		decoded, decodeErr := hex.DecodeString(hash)
		if !ok || decodeErr != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != hash || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return media.Reject(media.AccessDenied, "unsafe Codex projection copy directory")
		}
		child, err := base.OpenRoot(entry.Name())
		if err != nil {
			return err
		}
		opened, err := child.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = child.Close()
			return media.Reject(media.AccessDenied, "Codex projection copy directory changed")
		}
		files, err := readCodexProjectionEntries(child, limits.MaxBranchBlobs)
		if err != nil {
			_ = child.Close()
			return err
		}
		for _, file := range files {
			metadata, err := child.Lstat(file.Name())
			if err != nil {
				_ = child.Close()
				return err
			}
			if !validCodexProjectionFilename(file.Name()) || !metadata.Mode().IsRegular() || metadata.Mode().Perm() != 0o400 || metadata.Size() <= 0 {
				_ = child.Close()
				return media.Reject(media.AccessDenied, "unsafe retained Codex projection copy")
			}
			if err := addCodexProjectionCopy(&count, &bytes, metadata.Size(), limits); err != nil {
				_ = child.Close()
				return err
			}
		}
		_ = child.Close()
	}
	seen := make(map[string]bool)
	for _, ref := range refs {
		if seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		_, err := current.Lstat(codexImageFilename(ref))
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := addCodexProjectionCopy(&count, &bytes, ref.ByteSize, limits); err != nil {
			return err
		}
	}
	return nil
}

func addCodexProjectionCopy(count *int, bytes *int64, size int64, limits media.Limits) error {
	if *count >= limits.MaxBranchBlobs || size <= 0 || size > limits.MaxBranchBytes-*bytes {
		return media.Reject(media.LimitExceeded, "Codex projection copies exceed the retained copy budget")
	}
	*count++
	*bytes += size
	return nil
}

func validCodexProjectionFilename(name string) bool {
	for _, suffix := range []string{".png", ".jpg", ".webp"} {
		if id, ok := strings.CutSuffix(name, suffix); ok {
			return media.IsID(id)
		}
	}
	return false
}

func readCodexProjectionEntries(root *os.Root, maximum int) ([]fs.DirEntry, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	entries, err := file.ReadDir(maximum + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maximum {
		return nil, media.Reject(media.LimitExceeded, "Codex projection directory count exceeds copy budget")
	}
	return entries, nil
}
