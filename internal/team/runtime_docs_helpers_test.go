package team

import (
	"context"
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
