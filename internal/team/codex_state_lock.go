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
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
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
