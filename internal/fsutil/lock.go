package fsutil

import "errors"

// ErrWouldBlock reports that another holder already owns an exclusive lock.
// TryLockExclusive never waits, so callers can fail fast instead of risking
// lock-order deadlocks between independent lock files.
var ErrWouldBlock = errors.New("file lock is held by another owner")

// FileLock is a held non-blocking exclusive interprocess lock. The zero value
// is not usable; obtain one from TryLockExclusive.
type FileLock struct {
	handle *fileLockHandle
}

// TryLockExclusive creates path when missing, restricts it to the owner, and
// takes an exclusive lock without waiting. The lock file's content carries no
// meaning and is truncated once the lock is held.
func TryLockExclusive(path string) (*FileLock, error) {
	handle, err := tryLockExclusive(path)
	if err != nil {
		return nil, err
	}
	return &FileLock{handle: handle}, nil
}

// Close releases the lock. It is safe to call more than once.
func (l *FileLock) Close() error {
	if l == nil || l.handle == nil {
		return nil
	}
	return l.handle.close()
}
