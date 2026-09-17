package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const decisionWriterRetryInterval = 25 * time.Millisecond

// DecisionWriterLease holds the cross-process exclusion for one workspace
// branch. Callers keep it from attach/preparation through invocation end.
type DecisionWriterLease struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	closed bool
}

// AcquireDecisionWriterLease waits for branch-scoped cross-process ownership
// or context cancellation. A live owner is never displaced by a TTL.
func AcquireDecisionWriterLease(ctx context.Context, workspace, branchID string) (*DecisionWriterLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if workspace == "" || !validDecisionIdentifier(branchID) {
		return nil, fmt.Errorf("acquire decision writer lease: invalid workspace or branch")
	}
	logsPath := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logsPath, 0o755); err != nil {
		return nil, fmt.Errorf("acquire decision writer lease: %w", err)
	}
	branchHash := sha256.Sum256([]byte(branchID))
	path := filepath.Join(logsPath, "decision-writer-"+hex.EncodeToString(branchHash[:16])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open decision writer lease: %w", err)
	}

	ticker := time.NewTicker(decisionWriterRetryInterval)
	defer ticker.Stop()
	for {
		if err := lockEventStoreFile(file); err == nil {
			return &DecisionWriterLease{file: file, path: path}, nil
		} else if !errors.Is(err, ErrEventStoreWriterUnavailable) {
			_ = file.Close()
			return nil, fmt.Errorf("lock decision writer lease: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("acquire decision writer lease: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Close releases the lease. It is idempotent.
func (l *DecisionWriterLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	unlockErr := unlockEventStoreFile(l.file)
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}

func (l *DecisionWriterLease) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}
