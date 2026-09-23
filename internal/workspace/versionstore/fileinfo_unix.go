//go:build !windows

package versionstore

import (
	"os"
	"syscall"
)

func platformFileIdentity(info os.FileInfo) (ctimeNs int64, inode uint64) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return stat.Ctim.Sec*1e9 + stat.Ctim.Nsec, stat.Ino
}

// openRegularNoFollow opens rel for reading without following a final
// symlink and without blocking on a FIFO swapped in after Lstat.
func openRegularNoFollow(root *os.Root, rel string) (*os.File, error) {
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
