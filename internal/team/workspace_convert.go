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

	"github.com/kjelly/hufu/internal/fsutil"
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
// Everything is built and verified in staging before publication. Failures
// before publication leave the workspace unchanged. After publication, errors
// report partial completion; rerunning verifies the conversion record and
// finishes archiving the originals into LegacyRuntimeBackupDir. A dry run
// performs the same validation without publishing or moving files. The caller
// must hold the workspace's lock so no run uses it meanwhile.
func ConvertFileBasedWorkspace(ctx context.Context, workspace string, dryRun bool) (WorkspaceConversion, error) {
	result := WorkspaceConversion{Workspace: workspace, DryRun: dryRun, BackupDir: filepath.Join(workspace, LegacyRuntimeBackupDir)}
	present, err := presentLegacyFiles(workspace)
	if err != nil {
		return result, err
	}
	exists, err := conversionFileExists(workspace, runtimestore.File)
	if err != nil {
		return result, err
	}
	if exists {
		return resumeWorkspaceConversion(ctx, workspace, result, present)
	}
	if len(present) == 0 {
		return result, fmt.Errorf("%w: %s", ErrNotFileBased, workspace)
	}
	result.MovedFiles = present
	// Orphan SQLite sidecars must not be attached to the imported database.
	for _, name := range runtimestore.Files[1:] {
		if err := requireConversionPathAbsent(filepath.Join(workspace, name)); err != nil {
			return result, err
		}
	}
	record, err := newWorkspaceConversionRecord(workspace, present)
	if err != nil {
		return result, err
	}
	if err := verifyConversionSources(workspace, record, false); err != nil {
		return result, err
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
	if err := sealWorkspaceConversion(ctx, staging, record, result); err != nil {
		return result, err
	}
	// Check the entire plan again before publishing anything, including in a
	// dry run. No source or backup directory has been changed at this point.
	if err := verifyConversionSources(workspace, record, false); err != nil {
		return result, err
	}
	for _, sidecar := range runtimestore.Files[1:4] {
		if err := requireConversionPathAbsent(filepath.Join(staging, sidecar)); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if dryRun {
		return result, nil
	}
	// Link publication is atomic and refuses to replace an existing database.
	// The staging name is removed by the deferred cleanup.
	if err := os.Link(runtimestore.Path(staging), runtimestore.Path(workspace)); err != nil {
		return result, fmt.Errorf("convert workspace %s: install runtime.sqlite: %w", workspace, err)
	}
	if err := fsutil.SyncDir(workspace); err != nil {
		return result, conversionPartialError(err)
	}
	return result, conversionPartialError(moveLegacyFiles(workspace, present, record))
}

func presentLegacyFiles(workspace string) ([]string, error) {
	var present []string
	for _, name := range legacyRuntimeFiles() {
		exists, err := conversionFileExists(workspace, name)
		if err != nil {
			return nil, err
		}
		if exists {
			present = append(present, name)
		}
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
	if result.SessionBranches > 0 && (!exists || tree == nil || len(tree.Branches) != result.SessionBranches) {
		return fmt.Errorf("session tree does not hold the %d imported branches", result.SessionBranches)
	}
	for _, document := range result.Documents {
		if _, ok, err := workspaceDocs(staging).getDocument(document); err != nil || !ok {
			return errors.Join(fmt.Errorf("document %s is missing", document), err)
		}
	}
	store, err := runtimestore.OpenReadOnly(context.Background(), staging)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	for _, check := range []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM task_journal`, result.TaskJournalRows},
		{`SELECT COUNT(*) FROM decision_index`, result.DecisionIndexRows},
	} {
		var count int
		if err := store.DB().QueryRow(check.query).Scan(&count); err != nil {
			return err
		}
		if count != check.want {
			return fmt.Errorf("%s: got %d records, imported %d", check.query, count, check.want)
		}
	}
	return nil
}

// moveLegacyFiles moves the converted files into the backup directory,
// keeping their relative paths. A file already in the backup is never
// overwritten.
func moveLegacyFiles(workspace string, present []string, record *workspaceConversionRecord) error {
	backup := filepath.Join(workspace, LegacyRuntimeBackupDir)
	for _, name := range present {
		target := filepath.Join(backup, name)
		if err := requireConversionPathAbsent(target); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("convert workspace: create backup directory: %w", err)
		}
		// Persist newly created ancestors before removing a source name.
		for dir := filepath.Dir(filepath.Join(LegacyRuntimeBackupDir, name)); dir != "."; dir = filepath.Dir(dir) {
			if err := fsutil.SyncDir(filepath.Join(workspace, filepath.Dir(dir))); err != nil {
				return err
			}
		}
		if err := os.Rename(filepath.Join(workspace, name), target); err != nil {
			return fmt.Errorf("convert workspace: move %s: %w", name, err)
		}
		if err := fsutil.SyncDir(filepath.Dir(target)); err != nil {
			return err
		}
		if err := fsutil.SyncDir(filepath.Dir(filepath.Join(workspace, name))); err != nil {
			return err
		}
	}
	marker := filepath.Join(backup, "CONVERTED")
	if err := fsutil.AtomicCreateFile(marker, []byte(record.backupNote()), 0o600); err != nil {
		return fmt.Errorf("convert workspace: write backup note: %w", err)
	}
	return nil
}
