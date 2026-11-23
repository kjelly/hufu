package team

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"

	"github.com/kjelly/hufu/internal/media"
)

const codexProjectionConsumerPrefix = ".consumer-"
const maxCodexProjectionConsumers = 1024

// A consumer owns an independent kernel lock, not a process-local reference
// count. Only the workspace lock may publish/remove consumer files or images.
// Closing the process releases the kernel lock, leaving a reclaimable marker.
type codexProjectionConsumer struct {
	name string
	file *os.File
	info os.FileInfo
}

func validCodexProjectionConsumerName(name string) bool {
	suffix, ok := strings.CutPrefix(name, codexProjectionConsumerPrefix)
	if !ok || len(suffix) < 26 || len(suffix) > 64 {
		return false
	}
	for _, char := range suffix {
		if (char < 'A' || char > 'Z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func validCodexProjectionConsumerInfo(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && info.Size() == 0
}

func removeCodexProjectionFile(root *os.Root, name string, original os.FileInfo) error {
	current, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if original == nil || !os.SameFile(original, current) {
		return media.Reject(media.AccessDenied, "Codex projection file changed before reclamation")
	}
	return root.Remove(name)
}

// The caller holds the workspace lock. No legitimate waiter opens a consumer
// file outside that lock, so closing and unlinking an unlocked marker cannot
// split kernel lock ownership across two inodes.
func pruneCodexProjectionConsumers(ctx context.Context, root *os.Root, maxImages int) (int, error) {
	entries, err := readCodexProjectionEntries(root, maxImages+maxCodexProjectionConsumers)
	if err != nil {
		return 0, err
	}
	active := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if !strings.HasPrefix(entry.Name(), codexProjectionConsumerPrefix) {
			continue
		}
		live, err := inspectCodexProjectionConsumer(root, entry.Name())
		if err != nil {
			return 0, err
		}
		if live {
			active++
		}
	}
	return active, nil
}

func inspectCodexProjectionConsumer(root *os.Root, name string) (bool, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return false, err
	}
	if !validCodexProjectionConsumerName(name) || !validCodexProjectionConsumerInfo(info) {
		return false, media.Reject(media.AccessDenied, "unsafe Codex projection consumer lease")
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !os.SameFile(info, opened) || !validCodexProjectionConsumerInfo(opened) {
		return false, media.Reject(media.AccessDenied, "Codex projection consumer lease changed")
	}
	if err := lockExclusiveFile(file); errors.Is(err, errFileLockHeld) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	// Close before unlink for platforms that cannot delete an open lock file.
	if err := file.Close(); err != nil {
		return false, err
	}
	return false, removeCodexProjectionFile(root, name, info)
}

func newCodexProjectionConsumer(ctx context.Context, root *os.Root, maxImages int) (*codexProjectionConsumer, error) {
	active, err := pruneCodexProjectionConsumers(ctx, root, maxImages)
	if err != nil {
		return nil, err
	}
	if active >= maxCodexProjectionConsumers {
		return nil, media.Reject(media.LimitExceeded, "Codex projection consumer count exceeds its budget")
	}
	name := codexProjectionConsumerPrefix + rand.Text()
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = file.Close()
			_ = removeCodexProjectionFile(root, name, info)
		}
	}()
	// Normalize only the newly created inode, including restrictive umasks.
	if err := file.Chmod(0o600); err != nil {
		return nil, err
	}
	if err := lockExclusiveFile(file); err != nil {
		return nil, err
	}
	success = true
	return &codexProjectionConsumer{name: name, file: file, info: info}, nil
}

// All consumer processes must drain before calling close. Failure to acquire
// the bounded cleanup lock retains copies for quota accounting and later reuse.
// It never permits deletion outside the workspace lock.
type codexProjectionLease struct {
	base          *os.Root
	root          *os.Root
	directory     string
	directoryInfo os.FileInfo
	lockPath      string
	consumer      *codexProjectionConsumer
	files         map[string]os.FileInfo
	maxImages     int
}

func (lease *codexProjectionLease) close(ctx context.Context) error {
	defer func() { _ = lease.root.Close(); _ = lease.base.Close() }()
	if lease.consumer != nil {
		defer func() { _ = lease.consumer.file.Close() }()
	}
	unlock, err := acquireCodexStateFileLock(ctx, lease.lockPath)
	if err != nil {
		return err
	}
	defer unlock()
	if lease.consumer != nil {
		if err := lease.consumer.file.Close(); err != nil {
			return err
		}
		if err := removeCodexProjectionFile(lease.root, lease.consumer.name, lease.consumer.info); err != nil {
			return err
		}
	}
	active, err := pruneCodexProjectionConsumers(ctx, lease.root, lease.maxImages)
	if err != nil || active != 0 {
		return err
	}
	for name, info := range lease.files {
		if err := removeCodexProjectionFile(lease.root, name, info); err != nil {
			return err
		}
	}
	if err := lease.root.Close(); err != nil {
		return err
	}
	return removeCodexProjectionFile(lease.base, lease.directory, lease.directoryInfo)
}
