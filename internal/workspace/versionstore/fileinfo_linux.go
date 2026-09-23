//go:build linux

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
