package runtimestore

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Diagnostics are content-free facts about a runtime store.
type Diagnostics struct {
	SchemaVersion     int64  `json:"schema_version"`
	PageCount         int64  `json:"page_count"`
	FreelistCount     int64  `json:"freelist_count"`
	PageSize          int64  `json:"page_size"`
	JournalMode       string `json:"journal_mode"`
	Events            int64  `json:"events"`
	EventRewrites     int64  `json:"event_rewrites"`
	Documents         int64  `json:"documents"`
	SessionBranches   int64  `json:"session_branches"`
	TaskJournalRows   int64  `json:"task_journal_rows"`
	DecisionIndexRows int64  `json:"decision_index_rows"`
}

// Diagnose reads Diagnostics through an open store. It runs no checkpoint,
// VACUUM, or optimize.
func (s *Store) Diagnose(ctx context.Context) (Diagnostics, error) {
	var d Diagnostics
	queries := []struct {
		query string
		dest  any
	}{
		{`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`, &d.SchemaVersion},
		{`PRAGMA page_count`, &d.PageCount},
		{`PRAGMA freelist_count`, &d.FreelistCount},
		{`PRAGMA page_size`, &d.PageSize},
		{`PRAGMA journal_mode`, &d.JournalMode},
		{`SELECT COUNT(*) FROM events`, &d.Events},
		{`SELECT rewrites FROM event_journal_meta WHERE id = 1`, &d.EventRewrites},
		{`SELECT COUNT(*) FROM runtime_documents`, &d.Documents},
		{`SELECT COUNT(*) FROM session_branches`, &d.SessionBranches},
		{`SELECT COUNT(*) FROM task_journal`, &d.TaskJournalRows},
		{`SELECT COUNT(*) FROM decision_index`, &d.DecisionIndexRows},
	}
	for _, q := range queries {
		if err := s.db.QueryRowContext(ctx, q.query).Scan(q.dest); err != nil {
			return Diagnostics{}, fmt.Errorf("diagnose runtime store: %s: %w", q.query, err)
		}
	}
	if s.readOnly && s.shared == nil {
		// An immutable read-only connection ignores the WAL and reports a
		// rollback journal; the header records the mode the writer uses.
		mode, err := journalModeFromHeader(s.snapshotPath)
		if err != nil {
			return Diagnostics{}, fmt.Errorf("diagnose runtime store: %w", err)
		}
		d.JournalMode = mode
	}
	return d, nil
}

// journalModeFromHeader reads the database header: SQLite sets the file
// format versions at offsets 18 and 19 to 2 for a WAL database. Rollback
// journal modes are not distinguished there.
func journalModeFromHeader(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read database header: %w", err)
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil {
		return "", fmt.Errorf("read database header: %w", err)
	}
	if header[18] == 2 && header[19] == 2 {
		return "wal", nil
	}
	return "rollback", nil
}
