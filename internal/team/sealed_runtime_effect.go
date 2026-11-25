package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kjelly/hufu/internal/fsutil"
)

const runtimeEffectBodyFile = "body"

// Publish the body and its independent digest in one directory rename. The
// final directory is absent until both files have been synced. Once present,
// a missing member is lost evidence instead of an unstarted effect.
func createSealedRuntimeEffect(dir string, body []byte) error {
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return os.ErrExist
		}
		return err
	}
	parent := filepath.Dir(dir)
	stage, err := os.MkdirTemp(parent, ".sealing-effect-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := fsutil.AtomicCreateFile(filepath.Join(stage, runtimeEffectBodyFile), body, 0o600); err != nil {
		return err
	}
	if err := fsutil.AtomicCreateFile(filepath.Join(stage, "sha256"), []byte(sha256HexBytes(body)), 0o600); err != nil {
		return err
	}
	if err := os.Rename(stage, dir); err != nil {
		if _, statErr := os.Lstat(dir); statErr == nil {
			return os.ErrExist
		}
		return err
	}
	return fsutil.SyncDir(parent)
}

func readSealedRuntimeEffect(dir string, limit int) ([]byte, bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if replacedRuntimeEffectPath(err) {
		return nil, true, rejectResultSourceRead(errors.New("sealed runtime effect path was replaced"))
	}
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, true, rejectResultSourceRead(errors.New("sealed runtime effect is not a directory"))
	}
	digest, err := readRuntimeEffectFile(filepath.Join(dir, "sha256"), hex.EncodedLen(sha256.Size))
	if err != nil {
		return nil, true, rejectNormalizationRecoveryRead(fmt.Errorf("read sealed runtime effect digest: %w", err))
	}
	body, err := readRuntimeEffectFile(filepath.Join(dir, runtimeEffectBodyFile), limit)
	if err != nil {
		return nil, true, rejectNormalizationRecoveryRead(fmt.Errorf("read sealed runtime effect body: %w", err))
	}
	if len(body) == 0 || string(digest) != sha256HexBytes(body) {
		return nil, true, rejectResultSourceRead(errors.New("runtime effect body differs from its sealed digest"))
	}
	return body, true, nil
}

func readRuntimeEffectFile(path string, limit int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, classifyRuntimeEffectMemberError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("sealed runtime effect must be a regular file")
	}
	file, err := openBoundedArtifact(path)
	if err != nil {
		return nil, classifyRuntimeEffectMemberError(err)
	}
	defer func() { _ = file.Close() }()
	if limit >= 0 {
		return readBoundedRegularFile(context.Background(), file, limit)
	}
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("sealed runtime effect must be a regular file")
	}
	return io.ReadAll(file)
}

func classifyRuntimeEffectMemberError(err error) error {
	if replacedRuntimeEffectPath(err) {
		return errors.New("sealed runtime effect member was replaced")
	}
	return err
}

func replacedRuntimeEffectPath(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR)
}
