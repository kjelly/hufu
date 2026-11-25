//go:build linux

package golangruntime

import (
	"runtime/debug"

	"golang.org/x/sys/unix"
)

func limitNormalizerMemory() error {
	debug.SetMemoryLimit(128 << 20)
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_AS, &limit); err != nil {
		return err
	}
	limit.Cur = min(limit.Cur, 2<<30)
	limit.Max = limit.Cur
	return unix.Setrlimit(unix.RLIMIT_AS, &limit)
}
