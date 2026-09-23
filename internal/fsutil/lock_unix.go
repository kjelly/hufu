//go:build !windows

package fsutil

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type fileLockHandle struct {
	file *os.File
}

func tryLockExclusive(path string) (*fileLockHandle, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrWouldBlock
		}
		return nil, err
	}
	if err = file.Truncate(0); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	return &fileLockHandle{file: file}, nil
}

func (h *fileLockHandle) close() error {
	if h == nil || h.file == nil {
		return nil
	}
	err := unix.Flock(int(h.file.Fd()), unix.LOCK_UN)
	err = errors.Join(err, h.file.Close())
	h.file = nil
	return err
}
