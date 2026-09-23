package versionstore

import "encoding/hex"

// ObjectKind names one CAS namespace.
type ObjectKind string

const (
	ObjectBlob ObjectKind = "blob"
	ObjectTree ObjectKind = "tree"
	ObjectLink ObjectKind = "link"
)

func (k ObjectKind) valid() bool {
	return k == ObjectBlob || k == ObjectTree || k == ObjectLink
}

// validHash reports whether hash is a lowercase hex SHA-256 digest.
func validHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}
