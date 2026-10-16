package team

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/kjelly/hufu/internal/runtimestore"
)

const workspaceConversionDocument = "file_storage_conversion"

// The record is published in the same database as the imported data. Recovery
// requires both its source hashes and its logical database hash to match. File
// locations are the progress journal: each original must exist exactly once,
// either at its original path or in the backup. No separate progress write can
// get ahead of (or lag behind) a rename.
type workspaceConversionRecord struct {
	Version      int                 `json:"version"`
	SourceHashes map[string]string   `json:"source_hashes"`
	StateHash    string              `json:"state_hash"`
	Imported     WorkspaceConversion `json:"imported"`
}

func newWorkspaceConversionRecord(workspace string, present []string) (*workspaceConversionRecord, error) {
	record := &workspaceConversionRecord{Version: 1, SourceHashes: make(map[string]string)}
	for _, name := range present {
		digest, _, err := hashFileContents(filepath.Join(workspace, name))
		if err != nil {
			return nil, err
		}
		record.SourceHashes[name] = digest
	}
	return record, nil
}

func sealWorkspaceConversion(ctx context.Context, staging string, record *workspaceConversionRecord, result WorkspaceConversion) error {
	store, err := runtimestore.Open(ctx, staging)
	if err != nil {
		return err
	}
	record.Imported = result
	record.StateHash, err = conversionStateHash(ctx, store.DB())
	if err == nil {
		var body []byte
		body, err = json.Marshal(record)
		if err == nil {
			err = store.PutDocument(ctx, workspaceConversionDocument, body)
		}
	}
	if err := errors.Join(err, store.Close()); err != nil {
		return fmt.Errorf("convert workspace: save conversion record: %w", err)
	}
	// Verify the durable record and every imported table through a read-only
	// connection after all writers have closed, before publishing the file.
	_, err = loadWorkspaceConversionRecord(ctx, staging)
	return err
}

func loadWorkspaceConversionRecord(ctx context.Context, workspace string) (*workspaceConversionRecord, error) {
	store, err := runtimestore.OpenReadOnly(ctx, workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	body, ok, err := store.GetDocument(ctx, workspaceConversionDocument)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	var record workspaceConversionRecord
	if err := json.Unmarshal(body, &record, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("convert workspace: invalid conversion record: %w", err)
	}
	if record.Version != 1 || len(record.SourceHashes) == 0 || !validConversionHash(record.StateHash) {
		return nil, fmt.Errorf("convert workspace: invalid conversion record version, sources, or state hash")
	}
	for name, digest := range record.SourceHashes {
		if !slices.Contains(legacyRuntimeFiles(), name) || !validConversionHash(digest) {
			return nil, fmt.Errorf("convert workspace: invalid conversion source %q", name)
		}
	}
	digest, err := conversionStateHash(ctx, store.DB())
	if err != nil {
		return nil, err
	}
	if digest != record.StateHash {
		return nil, fmt.Errorf("convert workspace: runtime.sqlite no longer matches the verified import; originals left in place")
	}
	return &record, nil
}

func validConversionHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func resumeWorkspaceConversion(ctx context.Context, workspace string, result WorkspaceConversion, present []string) (WorkspaceConversion, error) {
	// A completed conversion (or an ordinary SQLite workspace) has no files
	// left to move and has a completion note, if it was converted. Do not
	// require an unchanged database after normal runtime use has resumed.
	markerExists, err := conversionFileExists(workspace, filepath.Join(LegacyRuntimeBackupDir, "CONVERTED"))
	if err != nil {
		return result, err
	}
	if len(present) == 0 && markerExists {
		return result, fmt.Errorf("%w: %s", ErrNotFileBased, workspace)
	}
	record, err := loadWorkspaceConversionRecord(ctx, workspace)
	if err != nil {
		return result, fmt.Errorf("convert workspace: cannot verify existing runtime.sqlite: %w", err)
	}
	if record == nil {
		if len(present) == 0 {
			return result, fmt.Errorf("%w: %s", ErrNotFileBased, workspace)
		}
		return result, fmt.Errorf("convert workspace: existing runtime.sqlite has no verified conversion record; originals left in place")
	}
	if err := verifyConversionSources(workspace, record, true); err != nil {
		return result, err
	}
	result.Events = record.Imported.Events
	result.Documents = record.Imported.Documents
	result.SessionBranches = record.Imported.SessionBranches
	result.TaskJournalRows = record.Imported.TaskJournalRows
	result.DecisionIndexRows = record.Imported.DecisionIndexRows
	result.MovedFiles = present
	result.Resumed = true
	if result.DryRun {
		return result, nil
	}
	return result, conversionPartialError(moveLegacyFiles(workspace, present, record))
}

func (record *workspaceConversionRecord) backupNote() string {
	return fmt.Sprintf("Converted into runtime.sqlite (verified import SHA-256 %s). Hufu no longer reads these files.\n", record.StateHash)
}

func conversionPartialError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("convert workspace: runtime.sqlite is installed, but archiving originals is incomplete; resolve the error and rerun convert-storage to verify and resume: %w", err)
}

// verifyConversionSources preflights every source, backup destination, and
// ancestor directory before any publication or move, also during dry runs.
func verifyConversionSources(workspace string, record *workspaceConversionRecord, resume bool) error {
	for _, name := range legacyRuntimeFiles() {
		source, err := conversionFileExists(workspace, name)
		if err != nil {
			return err
		}
		expected, imported := record.SourceHashes[name]
		if !imported {
			if source {
				return fmt.Errorf("convert workspace: %s was not part of the verified import", name)
			}
			continue
		}
		backupName := filepath.Join(LegacyRuntimeBackupDir, name)
		backup, err := conversionFileExists(workspace, backupName)
		if err != nil {
			return err
		}
		if (!resume && backup) || (source && backup) {
			return fmt.Errorf("convert workspace: backup %s already exists", backupName)
		}
		if !source && !backup {
			return fmt.Errorf("convert workspace: imported source %s is missing from both workspace and backup", name)
		}
		path := filepath.Join(workspace, name)
		if !source {
			path = filepath.Join(workspace, backupName)
		}
		digest, _, err := hashFileContents(path)
		if err != nil {
			return err
		}
		if digest != expected {
			return fmt.Errorf("convert workspace: %s does not match its verified source hash", path)
		}
	}
	return requireConversionPathAbsent(filepath.Join(workspace, LegacyRuntimeBackupDir, "CONVERTED"))
}

// conversionFileExists rejects symlinks and non-directory ancestors. Both
// legacy paths and backup paths are fixed, workspace-relative names.
func conversionFileExists(workspace, name string) (bool, error) {
	for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
		info, err := os.Lstat(filepath.Join(workspace, dir))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, fmt.Errorf("convert workspace: %s is not a directory", dir)
		}
	}
	info, err := os.Lstat(filepath.Join(workspace, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("convert workspace: %s is not a regular file", name)
	}
	return true, nil
}

func requireConversionPathAbsent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("convert workspace: %s already exists", path)
}

// conversionStateHash covers all imported data, including journal records,
// labels, branch state, and document bytes. It excludes only the conversion
// record itself and incidental write timestamps. Queries are fixed code, never
// taken from the record. Ordering and row boundaries make the hash stable.
func conversionStateHash(ctx context.Context, db *sql.DB) (string, error) {
	queries := []string{
		`SELECT * FROM events ORDER BY sequence`,
		`SELECT name, body FROM runtime_documents WHERE name <> 'file_storage_conversion' ORDER BY name`,
		`SELECT sequence, record FROM task_journal ORDER BY sequence`,
		`SELECT * FROM decision_index ORDER BY sequence`,
		`SELECT * FROM session_branches ORDER BY id`,
		`SELECT * FROM session_labels ORDER BY name`,
		`SELECT * FROM session_tree_head ORDER BY id`,
	}
	digest := sha256.New()
	for _, query := range queries {
		_, _ = io.WriteString(digest, query+"\n")
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return "", err
		}
		err = hashConversionRows(rows, digest)
		if err := errors.Join(err, rows.Close()); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func hashConversionRows(rows *sql.Rows, digest io.Writer) error {
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		data, err := json.Marshal(values)
		if err != nil {
			return err
		}
		if _, err := digest.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return rows.Err()
}
