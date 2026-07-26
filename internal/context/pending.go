package context

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// PendingWrite is a durable record of a canonical Append that could not be
// committed because the store was unavailable. It carries enough of the
// original item to be replayed later, without ever retaining the write in a
// less-redacted form than the canonical store itself would.
type PendingWrite struct {
	Item  ContextItem `json:"item"`
	Cause string      `json:"cause"`
}

var pendingFileMu sync.Mutex

// AppendPendingWrite durably records a failed shadow write to path so a
// later Repair call can replay it. It never blocks or fails the caller's
// legacy operation: write errors are returned for logging only.
//
// The failure cause is redacted with the same RedactSecrets pass as the
// item content: a driver/repository error can embed the rejected value
// verbatim (e.g. a constraint error quoting the offending row), so the
// cause is never trusted to be safe on its own. The write is followed by
// Sync before Close so a crash immediately after this call cannot lose the
// only durable record of the failed write — matching the write/sync/close
// discipline atomicWrite already uses for projections.
func AppendPendingWrite(path string, item ContextItem, cause error) error {
	item.Content = RedactSecrets(strings.ReplaceAll(strings.TrimSpace(item.Content), "\r\n", "\n"))
	pw := PendingWrite{Item: item, Cause: RedactSecrets(cause.Error())}
	line, err := json.Marshal(pw)
	if err != nil {
		return err
	}
	pendingFileMu.Lock()
	defer pendingFileMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// RepairPendingWrites replays every record in path into repo, atomically
// rewriting the file to contain only the records that still fail so the
// operation is safe to retry and idempotent: a record is only dropped once
// repo.Append has actually accepted it. Unparseable lines are preserved
// rather than silently discarded.
func RepairPendingWrites(ctx context.Context, repo Repository, path string) (recovered, remaining int, err error) {
	pendingFileMu.Lock()
	defer pendingFileMu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var keep []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var pw PendingWrite
		if e := json.Unmarshal([]byte(line), &pw); e != nil {
			keep = append(keep, line)
			continue
		}
		if e := repo.Append(ctx, pw.Item); e != nil {
			pw.Cause = RedactSecrets(e.Error())
			b, _ := json.Marshal(pw)
			keep = append(keep, string(b))
			remaining++
			continue
		}
		recovered++
	}
	content := ""
	if len(keep) > 0 {
		content = strings.Join(keep, "\n") + "\n"
	}
	if err := atomicWrite(path, content); err != nil {
		return recovered, remaining, err
	}
	return recovered, remaining, nil
}
