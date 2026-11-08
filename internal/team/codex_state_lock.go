package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// One kernel lock owner spans both process startup and drain.
func acquireCodexStateFileLock(ctx context.Context, lockPath string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openCodexStateLock(lockPath)
	if err != nil {
		return nil, fmt.Errorf("open Codex state lock %q: %w", lockPath, err)
	}
	release := func() {
		_ = unlockExclusiveFile(file)
		_ = file.Close()
	}
	for {
		if err := lockExclusiveFile(file); err == nil {
			return sync.OnceFunc(release), nil
		} else if !errors.Is(err, errFileLockHeld) {
			release()
			return nil, fmt.Errorf("lock Codex state %q: %w", lockPath, err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			release()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Only a newly created inode may have its permissions normalized. Never chmod
// a pre-existing path: it may be a symlink or belong to another lock owner.
func openCodexStateLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		// Creation modes are filtered by umask, including owner permission bits.
		// Chmod the descriptor, not the path, before publishing a usable lock.
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("unsafe Codex state lock %q", path)
	}
	file, err = os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0o600 {
		_ = file.Close()
		return nil, fmt.Errorf("codex state lock %q changed while opening", path)
	}
	return file, nil
}
