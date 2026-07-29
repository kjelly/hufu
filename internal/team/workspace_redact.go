package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/anomalyco/hufu/internal/utils"
)

// RedactWorkspaceManagedRecords removes recognizable credentials from hufu's
// own durable records. It deliberately never walks arbitrary workspace files:
// user artifacts such as vaults remain untouched. Event-store hashes are
// recomputed because redacting a payload necessarily changes the chain.
func RedactWorkspaceManagedRecords(workspace string) error {
	if workspace == "" {
		return fmt.Errorf("redact workspace: empty workspace")
	}
	if err := redactEventStore(workspace); err != nil {
		return err
	}
	roots := []string{
		filepath.Join(workspace, logsDir, "audit"),
		filepath.Join(workspace, llmLogsDir),
		filepath.Join(workspace, tasksDir),
		filepath.Join(workspace, statusDir),
		filepath.Join(workspace, historyDir),
	}
	for _, root := range roots {
		if err := redactTree(root); err != nil {
			return err
		}
	}
	for _, name := range []string{sessionFile, historyFile, stmFile, "chat_history.md", "context-stm.md", "context-ltm.md", filepath.Join(logsDir, taskJournalFile)} {
		if err := redactFile(filepath.Join(workspace, name)); err != nil {
			return err
		}
	}
	entries, _ := filepath.Glob(filepath.Join(workspace, "ltm-*.md"))
	for _, path := range entries {
		if err := redactFile(path); err != nil {
			return err
		}
	}
	return nil
}

func redactTree(root string) error {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".json" && ext != ".jsonl" && ext != ".md" && ext != ".log" && ext != ".yml" && ext != ".yaml" {
			return nil
		}
		return redactFile(path)
	})
}

func redactFile(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	redacted := []byte(utils.RedactSecrets(string(data)))
	if bytes.Equal(data, redacted) {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return AtomicWriteFile(path, redacted, info.Mode().Perm())
}

func redactEventStore(workspace string) error {
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var events []RunEvent
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event RunEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("redact event store: decode event: %w", err)
		}
		events = append(events, event)
	}
	var out bytes.Buffer
	var previousID, previousHash string
	for i := range events {
		e := &events[i]
		e.Payload = json.RawMessage(utils.RedactSecrets(string(e.Payload)))
		e.PreviousID, e.PreviousHash = previousID, previousHash
		e.Hash = ComputeEventHash(e.PreviousHash, e.ID, e.Type, e.Timestamp, e.Payload)
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		out.Write(line)
		out.WriteByte('\n')
		previousID, previousHash = e.ID, e.Hash
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return AtomicWriteFile(path, out.Bytes(), info.Mode().Perm())
}

func OpenAndVerifyEventStore(workspace string) (retErr error) {
	store, err := OpenEventStore(workspace)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil && retErr == nil {
			retErr = cerr
		}
	}()
	return store.VerifyHashChain()
}
