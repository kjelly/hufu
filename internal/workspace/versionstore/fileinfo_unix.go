//go:build !windows

package versionstore

import (
	"os"
	"syscall"
)

// openRegularNoFollow opens rel for reading without following a final
// symlink and without blocking on a FIFO swapped in after Lstat.
func openRegularNoFollow(root *os.Root, rel string) (*os.File, error) {
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
