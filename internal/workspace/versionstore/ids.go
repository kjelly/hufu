package versionstore

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// newID uses the same "<kind>_<32 hex>" format as internal/workspace's
// IDGenerator without importing that package.
func newID(reader io.Reader, kind string) (string, error) {
	if reader == nil {
		reader = rand.Reader
	}
	value := make([]byte, 16)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", fmt.Errorf("generate %s id: %w", kind, err)
	}
	return kind + "_" + hex.EncodeToString(value), nil
}

func validID(value, kind string) bool {
	rest, ok := strings.CutPrefix(value, kind+"_")
	if !ok || len(rest) != 32 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil && strings.ToLower(rest) == rest
}

// ValidSnapshotID reports whether id has the snapshot ID format.
func ValidSnapshotID(id SnapshotID) bool { return validID(string(id), "wsv") }
