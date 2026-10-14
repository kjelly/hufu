package team

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// LegacyRuntimeBackupDir is where a conversion keeps a workspace's original
// runtime files, relative to the workspace.
const LegacyRuntimeBackupDir = "legacy-runtime-files"

// ErrNotFileBased reports a workspace with no file-based runtime state to
// convert.
var ErrNotFileBased = errors.New("workspace has no file-based runtime state")

// legacyRuntimeDocuments maps retired runtime files to the runtime documents
// that replaced them. Their bytes are kept verbatim.
var legacyRuntimeDocuments = []struct {
	path     string
	document string
}{
	{"session.json", runtimestore.DocumentSession},
	{"conversation_compaction_state.json", runtimestore.DocumentConversationCompactionState},
	{"session_history.json", runtimestore.DocumentSessionHistory},
	{filepath.Join(logsDir, "terminal_sessions.json"), runtimestore.DocumentTerminalSessions},
	{filepath.Join(logsDir, "reliability_eval.json"), runtimestore.DocumentReliabilityEval},
}

var (
	legacyEventLog      = filepath.Join(logsDir, "event_store.jsonl")
	legacySessionTree   = "session_tree.json"
	legacyTaskJournal   = filepath.Join(logsDir, "task_journal.jsonl")
	legacyDecisionIndex = filepath.Join(logsDir, decisionsDir, "index.jsonl")
)

// legacyRuntimeFiles lists every retired runtime file, relative to the
// workspace.
func legacyRuntimeFiles() []string {
	files := []string{legacyEventLog, legacySessionTree, legacyTaskJournal, legacyDecisionIndex}
	for _, document := range legacyRuntimeDocuments {
		files = append(files, document.path)
	}
	return files
}

// WorkspaceConversion reports what a conversion imported.
type WorkspaceConversion struct {
	Workspace         string   `json:"workspace"`
	DryRun            bool     `json:"dry_run"`
	Events            int      `json:"events"`
	Documents         []string `json:"documents"`
	SessionBranches   int      `json:"session_branches"`
	TaskJournalRows   int      `json:"task_journal_rows"`
	DecisionIndexRows int      `json:"decision_index_rows"`
	// MovedFiles are the original files moved (or, in a dry run, to be
	// moved) into BackupDir.
	MovedFiles []string `json:"moved_files"`
	BackupDir  string   `json:"backup_dir"`
	// Resumed reports that an earlier conversion had already committed
	// runtime.sqlite and only the original files were left to move.
	Resumed bool `json:"resumed,omitempty"`
}

// ConvertFileBasedWorkspace imports a file-based workspace's runtime state
// into runtime.sqlite. The event log is imported with full chain validation;
// documents, task journal records, and decision index rows are kept verbatim.
// Everything is built and verified in a staging database first, which is then
// renamed into place, so a failure leaves the workspace as it was. The
// original files are moved, not deleted, into LegacyRuntimeBackupDir. A dry
// run imports and verifies in staging and changes nothing. The caller must
// hold the workspace's lock so no run uses it meanwhile.
func ConvertFileBasedWorkspace(ctx context.Context, workspace string, dryRun bool) (WorkspaceConversion, error) {
	result := WorkspaceConversion{Workspace: workspace, DryRun: dryRun, BackupDir: filepath.Join(workspace, LegacyRuntimeBackupDir)}
	present, err := presentLegacyFiles(workspace)
	if err != nil {
		return result, err
	}
	if len(present) == 0 {
		return result, fmt.Errorf("%w: %s", ErrNotFileBased, workspace)
	}
	result.MovedFiles = present
	if runtimestore.Exists(workspace) {
		// runtime.sqlite is only ever created next to these files by a
		// conversion that committed but did not finish moving them.
		result.Resumed = true
		if dryRun {
			return result, nil
		}
		return result, moveLegacyFiles(workspace, present)
	}

	staging, err := os.MkdirTemp(workspace, ".runtime-conversion-")
	if err != nil {
		return result, fmt.Errorf("convert workspace: create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := importLegacyRuntimeState(ctx, workspace, staging, &result); err != nil {
		return result, fmt.Errorf("convert workspace %s: %w", workspace, err)
	}
	if err := verifyConvertedRuntimeState(staging, result); err != nil {
		return result, fmt.Errorf("convert workspace %s: verify: %w", workspace, err)
	}
	if dryRun {
		return result, nil
	}
	for _, sidecar := range runtimestore.Files[1:4] {
		if _, err := os.Lstat(filepath.Join(staging, sidecar)); err == nil {
			return result, fmt.Errorf("convert workspace %s: staging database left %s behind", workspace, sidecar)
		}
	}
	if err := os.Rename(runtimestore.Path(staging), runtimestore.Path(workspace)); err != nil {
		return result, fmt.Errorf("convert workspace %s: install runtime.sqlite: %w", workspace, err)
	}
	return result, moveLegacyFiles(workspace, present)
}

func presentLegacyFiles(workspace string) ([]string, error) {
	var present []string
	for _, name := range legacyRuntimeFiles() {
		info, err := os.Lstat(filepath.Join(workspace, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("convert workspace: inspect %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("convert workspace: %s is not a regular file", name)
		}
		present = append(present, name)
	}
	return present, nil
}

// importLegacyRuntimeState builds staging's runtime.sqlite from workspace's
// files.
func importLegacyRuntimeState(ctx context.Context, workspace, staging string, result *WorkspaceConversion) error {
	if events, err := os.Open(filepath.Join(workspace, legacyEventLog)); err == nil {
		result.Events, err = ImportEventLog(ctx, staging, events)
		_ = events.Close()
		if err != nil {
			return fmt.Errorf("event log: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("event log: %w", err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, legacySessionTree)); err == nil {
		var tree SessionTree
		if err := json.Unmarshal(data, &tree); err != nil {
			return fmt.Errorf("session tree is not valid JSON: %w", err)
		}
		if err := SaveSessionTree(staging, &tree); err != nil {
			return fmt.Errorf("session tree: %w", err)
		}
		result.SessionBranches = len(tree.Branches)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session tree: %w", err)
	}
	return withRuntimeStore(staging, func(store *runtimestore.Store) error {
		return store.WithTx(ctx, func(tx *sql.Tx) error {
			for _, document := range legacyRuntimeDocuments {
				data, err := os.ReadFile(filepath.Join(workspace, document.path))
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return fmt.Errorf("%s: %w", document.path, err)
				}
				if err := runtimestore.PutDocument(ctx, tx, document.document, data); err != nil {
					return err
				}
				result.Documents = append(result.Documents, document.document)
			}
			var err error
			if result.TaskJournalRows, err = importLegacyLines(filepath.Join(workspace, legacyTaskJournal), func(line []byte) error {
				return insertTaskJournalRecord(ctx, tx, line)
			}); err != nil {
				return fmt.Errorf("task journal: %w", err)
			}
			if result.DecisionIndexRows, err = importLegacyLines(filepath.Join(workspace, legacyDecisionIndex), func(line []byte) error {
				var row struct {
					DecisionID string `json:"decision_id"`
				}
				_ = json.Unmarshal(line, &row)
				return insertDecisionIndexRow(ctx, tx, row.DecisionID, line)
			}); err != nil {
				return fmt.Errorf("decision index: %w", err)
			}
			return nil
		})
	})
}

// importLegacyLines passes each non-blank line of a JSONL file to insert,
// verbatim: readers skip lines they cannot decode, as they did in the file.
func importLegacyLines(path string, insert func([]byte) error) (int, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(file)
	count := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			if err := insert(trimmed); err != nil {
				return count, err
			}
			count++
		}
		if errors.Is(readErr, io.EOF) {
			return count, nil
		}
		if readErr != nil {
			return count, readErr
		}
	}
}

// verifyConvertedRuntimeState reopens the staged database read-only, as a run
// would, and checks it holds what was imported.
func verifyConvertedRuntimeState(staging string, result WorkspaceConversion) error {
	events, err := OpenEventStoreReadOnly(staging)
	if err != nil {
		return err
	}
	cached, err := events.ReadEvents()
	closeErr := events.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if len(cached) != result.Events {
		return fmt.Errorf("journal holds %d events, imported %d", len(cached), result.Events)
	}
	tree, exists, err := LoadSavedSessionTree(staging)
	if err != nil {
		return err
	}
	if result.SessionBranches > 0 && (!exists || len(tree.Branches) != result.SessionBranches) {
		return fmt.Errorf("session tree holds %d branches, imported %d", len(tree.Branches), result.SessionBranches)
	}
	for _, document := range result.Documents {
		if _, ok, err := workspaceDocs(staging).getDocument(document); err != nil || !ok {
			return errors.Join(fmt.Errorf("document %s is missing", document), err)
		}
	}
	return nil
}

// moveLegacyFiles moves the converted files into the backup directory,
// keeping their relative paths. A file already in the backup is never
// overwritten.
func moveLegacyFiles(workspace string, present []string) error {
	backup := filepath.Join(workspace, LegacyRuntimeBackupDir)
	for _, name := range present {
		target := filepath.Join(backup, name)
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("convert workspace: %s already exists", target)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("convert workspace: create backup directory: %w", err)
		}
		if err := os.Rename(filepath.Join(workspace, name), target); err != nil {
			return fmt.Errorf("convert workspace: move %s: %w", name, err)
		}
	}
	marker := filepath.Join(backup, "CONVERTED")
	note := fmt.Sprintf("Converted into runtime.sqlite on %s. Hufu no longer reads these files.\n", time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(marker, []byte(note), 0o600); err != nil {
		return fmt.Errorf("convert workspace: write backup note: %w", err)
	}
	return nil
}
