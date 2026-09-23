//go:build windows

package versionstore

import "os"

// Workspace versioning is disabled on windows (PlatformSupported); these
// stubs only keep the package compiling.
func platformFileIdentity(os.FileInfo) (ctimeNs int64, inode uint64) { return 0, 0 }

func openRegularNoFollow(root *os.Root, rel string) (*os.File, error) {
	return root.Open(rel)
}
