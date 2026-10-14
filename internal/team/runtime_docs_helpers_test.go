package team

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// putRuntimeDocument writes body verbatim as document name, bypassing the
// typed savers, the way a broken writer or a tamperer would.
func putRuntimeDocument(t *testing.T, workspace, name string, body []byte) {
	t.Helper()
	store, err := runtimestore.Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.PutDocument(context.Background(), name, body); err != nil {
		t.Fatal(err)
	}
}

// getRuntimeDocument reads document name without creating runtime.sqlite.
func getRuntimeDocument(t *testing.T, workspace, name string) ([]byte, bool) {
	t.Helper()
	body, exists, err := workspaceDocs(workspace).getDocument(name)
	if err != nil {
		t.Fatal(err)
	}
	return body, exists
}

// rawSessionBranch is one session_branches row; state is stored verbatim.
type rawSessionBranch struct {
	id, name, parent, state string
}

// storeRawSessionTree writes session tree rows verbatim, bypassing
// SaveSessionTree's defaults, so tests can build damaged trees.
func storeRawSessionTree(t *testing.T, workspace, active string, branches ...rawSessionBranch) {
	t.Helper()
	withJournal(t, workspace, func(q runtimestore.Queryer) {
		ctx := context.Background()
		if _, err := q.ExecContext(ctx, `INSERT INTO session_tree_head(id, active_branch) VALUES(1, ?)
ON CONFLICT(id) DO UPDATE SET active_branch = excluded.active_branch`, active); err != nil {
			t.Fatal(err)
		}
		for _, branch := range branches {
			state := branch.state
			if state == "" {
				state = "{}"
			}
			if _, err := q.ExecContext(ctx, `INSERT OR REPLACE INTO session_branches(id, name, parent_id, fork_event_id, created_at, state)
VALUES(?, ?, ?, '', '', ?)`, branch.id, branch.name, branch.parent, []byte(state)); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// readTaskJournalLog returns workspace's task journal as JSONL, the bytes the
// retired task_journal.jsonl held.
func readTaskJournalLog(workspace string) ([]byte, error) {
	var log bytes.Buffer
	err := ExportTaskJournal(context.Background(), workspace, &log)
	return log.Bytes(), err
}

// writeTaskJournalLog appends each JSONL line of data as one task journal
// record, verbatim.
func writeTaskJournalLog(workspace string, data []byte) error {
	store, err := runtimestore.Open(context.Background(), workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := insertTaskJournalRecord(context.Background(), store.DB(), line); err != nil {
			return err
		}
	}
	return nil
}

// readDecisionIndexLog returns index's stored rows as JSONL in insertion
// order, the bytes the retired logs/decisions/index.jsonl held.
func readDecisionIndexLog(index *DecisionIndex) ([]byte, error) {
	rows, err := readDecisionIndexRows(index.workspace)
	if err != nil {
		return nil, err
	}
	var log []byte
	for _, row := range rows {
		log = append(append(log, row...), '\n')
	}
	return log, nil
}

// writeDecisionIndexLog replaces index's rows with the JSONL lines of data,
// verbatim, the way an out-of-band edit of the index would.
func writeDecisionIndexLog(index *DecisionIndex, data []byte) error {
	store, err := runtimestore.Open(context.Background(), index.workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if _, err := store.DB().Exec(`DELETE FROM decision_index`); err != nil {
		return err
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var row struct {
			DecisionID string `json:"decision_id"`
		}
		_ = json.Unmarshal(line, &row)
		if err := insertDecisionIndexRow(context.Background(), store.DB(), row.DecisionID, line); err != nil {
			return err
		}
	}
	return nil
}
