package runtimestore

import (
	"context"
	"errors"
	"time"

	"github.com/kjelly/hufu/internal/fsutil"
)

// lockInitialization waits only for other initializers, with the same five
// second bound as SQLite's busy timeout. The caller releases it before Open
// returns; normal database transactions rely on SQLite's own locking.
// Keep the empty lock file after release: unlinking it would let independent
// openers lock different inodes at the same path.
func lockInitialization(ctx context.Context, path string) (*fsutil.FileLock, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	retry := time.Tick(10 * time.Millisecond)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := fsutil.TryLockExclusive(path + ".init.lock")
		if !errors.Is(err, fsutil.ErrWouldBlock) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-retry:
		}
	}
}
