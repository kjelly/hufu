package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/fsutil"
)

func runtimeEffectRecordPath(root, key string) string {
	return filepath.Join(root, "runtime-effect-records", sha256HexBytes([]byte(key)))
}

// A create-only record authenticates evidence independently of the SQLite row.
func createRuntimeEffectRecord(root, key, binding string) error {
	path := runtimeEffectRecordPath(root, key)
	if err := prepareRuntimeEffectDirectory(root, path); err != nil {
		return err
	}
	return fsutil.AtomicCreateFile(path, []byte(binding), 0o600)
}

func createRuntimeEffectBody(root, key string, body []byte) error {
	path := runtimeEffectRecordPath(root, key)
	if err := prepareRuntimeEffectDirectory(root, path); err != nil {
		return err
	}
	return createSealedRuntimeEffect(path, body)
}

func prepareRuntimeEffectDirectory(root, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.SyncDir(root)
}

func readRuntimeEffectRecord(root, key string) (string, error) {
	body, err := readRuntimeEffectFile(runtimeEffectRecordPath(root, key), hex.EncodedLen(sha256.Size))
	return string(body), err
}

func readRuntimeEffectCompletion(root, key string, limit int) (body []byte, published bool, err error) {
	body, _, err = readSealedRuntimeEffect(runtimeEffectRecordPath(root, key), limit)
	if err != nil {
		return nil, false, rejectNormalizationRecoveryRead(err)
	}
	marker, err := readRuntimeEffectRecord(root, key+"/published")
	if errors.Is(err, os.ErrNotExist) {
		return body, false, nil
	}
	if err != nil {
		return nil, false, rejectNormalizationRecoveryRead(err)
	}
	if len(body) == 0 || marker != sha256HexBytes(body) {
		return nil, false, rejectResultSourceRead(errors.New("published runtime effect does not match its sealed completion"))
	}
	return body, true, nil
}

func validateRuntimeEffectProjection(body, projection []byte, published bool) error {
	if (projection != nil || published) && !bytes.Equal(body, projection) {
		return rejectResultSourceRead(errors.New("runtime effect projection differs from its sealed completion"))
	}
	return nil
}

func publishRuntimeEffectProjection(root, key, digest string) error {
	return pinRuntimeEffectDigest(root, key+"/published", digest)
}

func pinRuntimeEffectDigest(root, key, digest string) error {
	err := createRuntimeEffectRecord(root, key, digest)
	if errors.Is(err, os.ErrExist) {
		saved, readErr := readRuntimeEffectRecord(root, key)
		if readErr != nil {
			return rejectNormalizationRecoveryRead(readErr)
		}
		if saved != digest {
			return rejectResultSourceRead(errors.New("runtime effect binding differs from its seal"))
		}
		return nil
	}
	if err != nil {
		return rejectLocalResultPersistence(fmt.Errorf("persist runtime effect binding: %w", err))
	}
	return nil
}
