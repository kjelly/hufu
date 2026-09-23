//go:build windows

package versionstore

import "os"

// Workspace versioning is disabled on windows (PlatformSupported); this stub
// only keeps the package compiling.
func openRegularNoFollow(root *os.Root, rel string) (*os.File, error) {
	return root.Open(rel)
}
