//go:build !linux

package versionstore

import "os"

// Workspace versioning is only enabled on linux (PlatformSupported); other
// platforms compile with an identity that never matches the stat cache.
func platformFileIdentity(os.FileInfo) (ctimeNs int64, inode uint64) { return 0, 0 }
