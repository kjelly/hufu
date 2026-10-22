//go:build unix

package team

import (
	"os"

	"golang.org/x/sys/unix"
)

func openBoundedArtifact(path string) (*os.File, error) {
	// A raced-in FIFO must not block before Stat can reject it. Do not follow
	// final-component symlinks when opening untrusted inspection artifacts.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
