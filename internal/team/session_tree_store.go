package team

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/utils"
)

// storedLabelsKey keys the labels' hash among the branch hashes a tree
// remembers; no branch ID contains a NUL byte.
const storedLabelsKey = "\x00labels"

// LoadSessionTree loads the session tree from workspace's runtime.sqlite
// without creating it. A workspace without a saved tree gets a fresh
// SessionTree with the default "main" branch.
func LoadSessionTree(workspace string) (*SessionTree, error) {
	return LoadSessionTreeContext(context.Background(), workspace)
}

// LoadSessionTreeContext is the cancellable read-only session-tree reader.
func LoadSessionTreeContext(ctx context.Context, workspace string) (*SessionTree, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if workspace == "" {
		return NewSessionTree(), nil
	}
	st, exists, err := loadStoredSessionTreeContext(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if !exists {
		return NewSessionTree(), nil
	}
	if len(st.Branches) == 0 {
		st.Branches["main"] = &SessionBranch{
			ID:        "main",
			Name:      "main",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	if st.ActiveBranch == "" || st.Branches[st.ActiveBranch] == nil {
		st.ActiveBranch = "main"
		if st.Branches["main"] == nil {
			st.Branches["main"] = &SessionBranch{
				ID:        "main",
				Name:      "main",
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			}
		}
	}

	return st, nil
}

// SaveSessionTree writes the session tree to workspace's runtime.sqlite in
// one transaction. A tree from LoadSessionTree writes only the branches and
// labels that changed since it was loaded.
func SaveSessionTree(workspace string, st *SessionTree) error {
	if st == nil {
		return fmt.Errorf("save session tree: tree is nil")
	}
	if workspace == "" {
		return fmt.Errorf("save session tree: empty workspace")
	}
	var written map[string]string
	err := withRuntimeStore(workspace, func(store *runtimestore.Store) error {
		return store.WithTx(context.Background(), func(tx *sql.Tx) error {
			var err error
			written, err = saveSessionTreeTx(context.Background(), tx, st)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("save session tree: %w", err)
	}
	st.markStored(written)
	return nil
}

// loadStoredSessionTree reads the saved tree exactly as stored, without
// LoadSessionTree's defaults, and reports false when none was saved. It never
// creates runtime.sqlite.
func loadStoredSessionTree(workspace string) (*SessionTree, bool, error) {
	return loadStoredSessionTreeContext(context.Background(), workspace)
}

func loadStoredSessionTreeContext(ctx context.Context, workspace string) (*SessionTree, bool, error) {
	store, err := runtimestore.OpenRead(ctx, workspace)
	if errors.Is(err, runtimestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load session tree: %w", err)
	}
	defer func() { _ = store.Close() }()
	st, exists, err := loadSessionTreeFrom(ctx, store.DB())
	if err != nil {
		return nil, false, fmt.Errorf("load session tree: %w", err)
	}
	return st, exists, nil
}

// LoadSavedSessionTree returns the saved tree exactly as stored, without
// LoadSessionTree's defaults, for inspectors that must report a damaged
// tree. The boolean is false when no tree was saved. It never creates
// runtime.sqlite.
func LoadSavedSessionTree(workspace string) (*SessionTree, bool, error) {
	return loadStoredSessionTree(workspace)
}

// HasSessionTree reports whether workspace has a saved session tree. It never
// creates runtime.sqlite.
func HasSessionTree(workspace string) (bool, error) {
	_, exists, err := loadStoredSessionTree(workspace)
	return exists, err
}

// markStored records the hashes of the rows runtime.sqlite now holds for st,
// so the next save writes only what changed after this point.
func (st *SessionTree) markStored(hashes map[string]string) {
	if st != nil {
		st.stored = hashes
	}
}

// sessionTreeHash fingerprints a branch or the labels as held in memory.
// Saves compare it with the hash recorded at load to skip unchanged rows.
func sessionTreeHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// loadSessionTreeFrom reads the session tree tables. It reports false when no
// tree was ever saved.
func loadSessionTreeFrom(ctx context.Context, q runtimestore.Queryer) (*SessionTree, bool, error) {
	st := &SessionTree{Branches: make(map[string]*SessionBranch), Labels: make(map[string]string), stored: make(map[string]string)}
	err := q.QueryRowContext(ctx, `SELECT active_branch FROM session_tree_head WHERE id = 1`).Scan(&st.ActiveBranch)
	hasHead := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("read active branch: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT id, name, parent_id, fork_event_id, created_at, state FROM session_branches`)
	if err != nil {
		return nil, false, fmt.Errorf("read session branches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		branch := &SessionBranch{}
		var state []byte
		if err := rows.Scan(&branch.ID, &branch.Name, &branch.ParentID, &branch.ForkEventID, &branch.CreatedAt, &state); err != nil {
			return nil, false, fmt.Errorf("decode session branch: %w", err)
		}
		if err := json.Unmarshal(state, &branch.State); err != nil {
			return nil, false, fmt.Errorf("decode state of session branch %q: %w", branch.ID, err)
		}
		hash, err := sessionTreeHash(branch)
		if err != nil {
			return nil, false, fmt.Errorf("fingerprint session branch %q: %w", branch.ID, err)
		}
		st.Branches[branch.ID] = branch
		st.stored[branch.ID] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read session branches: %w", err)
	}
	labelRows, err := q.QueryContext(ctx, `SELECT name, target FROM session_labels`)
	if err != nil {
		return nil, false, fmt.Errorf("read session labels: %w", err)
	}
	defer func() { _ = labelRows.Close() }()
	for labelRows.Next() {
		var name, target string
		if err := labelRows.Scan(&name, &target); err != nil {
			return nil, false, fmt.Errorf("decode session label: %w", err)
		}
		st.Labels[name] = target
	}
	if err := labelRows.Err(); err != nil {
		return nil, false, fmt.Errorf("read session labels: %w", err)
	}
	if !hasHead && len(st.Branches) == 0 {
		return nil, false, nil
	}
	labelsHash, err := sessionTreeHash(st.Labels)
	if err != nil {
		return nil, false, fmt.Errorf("fingerprint session labels: %w", err)
	}
	st.stored[storedLabelsKey] = labelsHash
	return st, true, nil
}

// saveSessionTreeTx writes st through q and returns the hashes of the rows it
// now holds. A tree that was loaded writes only the branches and labels that
// changed since, and deletes only the branches it dropped; a tree built in
// memory replaces every stored branch.
func saveSessionTreeTx(ctx context.Context, q runtimestore.Queryer, st *SessionTree) (map[string]string, error) {
	written := make(map[string]string, len(st.Branches)+1)
	for id, branch := range st.Branches {
		if branch == nil {
			continue
		}
		hash, err := sessionTreeHash(branch)
		if err != nil {
			return nil, fmt.Errorf("fingerprint session branch %q: %w", id, err)
		}
		written[id] = hash
		if st.stored != nil && st.stored[id] == hash {
			continue
		}
		if err := putSessionBranch(ctx, q, id, branch); err != nil {
			return nil, err
		}
	}
	if err := deleteDroppedBranches(ctx, q, st, written); err != nil {
		return nil, err
	}
	labelsHash, err := sessionTreeHash(st.Labels)
	if err != nil {
		return nil, fmt.Errorf("fingerprint session labels: %w", err)
	}
	written[storedLabelsKey] = labelsHash
	if st.stored == nil || st.stored[storedLabelsKey] != labelsHash {
		if err := putSessionLabels(ctx, q, st.Labels); err != nil {
			return nil, err
		}
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO session_tree_head(id, active_branch) VALUES(1, ?)
ON CONFLICT(id) DO UPDATE SET active_branch = excluded.active_branch`, st.ActiveBranch); err != nil {
		return nil, fmt.Errorf("write active branch: %w", err)
	}
	return written, nil
}

func deleteDroppedBranches(ctx context.Context, q runtimestore.Queryer, st *SessionTree, kept map[string]string) error {
	var dropped []string
	if st.stored != nil {
		for id := range st.stored {
			if _, ok := kept[id]; !ok && id != storedLabelsKey {
				dropped = append(dropped, id)
			}
		}
	} else {
		rows, err := q.QueryContext(ctx, `SELECT id FROM session_branches`)
		if err != nil {
			return fmt.Errorf("read session branch ids: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("decode session branch id: %w", err)
			}
			if _, ok := kept[id]; !ok {
				dropped = append(dropped, id)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("read session branch ids: %w", err)
		}
	}
	for _, id := range dropped {
		if _, err := q.ExecContext(ctx, `DELETE FROM session_branches WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete session branch %q: %w", id, err)
		}
	}
	return nil
}

// putSessionBranch upserts one redacted branch row. The ID is the tree's key
// and is never rewritten by redaction.
func putSessionBranch(ctx context.Context, q runtimestore.Queryer, id string, branch *SessionBranch) error {
	var redacted SessionBranch
	if err := redactInto(branch, &redacted); err != nil {
		return fmt.Errorf("redact session branch %q: %w", id, err)
	}
	state, err := json.Marshal(redacted.State)
	if err != nil {
		return fmt.Errorf("marshal state of session branch %q: %w", id, err)
	}
	_, err = q.ExecContext(ctx, `INSERT INTO session_branches(id, name, parent_id, fork_event_id, created_at, state)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name = excluded.name, parent_id = excluded.parent_id,
  fork_event_id = excluded.fork_event_id, created_at = excluded.created_at, state = excluded.state`,
		id, redacted.Name, redacted.ParentID, redacted.ForkEventID, redacted.CreatedAt, state)
	if err != nil {
		return fmt.Errorf("write session branch %q: %w", id, err)
	}
	return nil
}

func putSessionLabels(ctx context.Context, q runtimestore.Queryer, labels map[string]string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM session_labels`); err != nil {
		return fmt.Errorf("replace session labels: %w", err)
	}
	if len(labels) == 0 {
		return nil
	}
	redacted := make(map[string]string, len(labels))
	if err := redactInto(labels, &redacted); err != nil {
		return fmt.Errorf("redact session labels: %w", err)
	}
	for name, target := range redacted {
		if _, err := q.ExecContext(ctx, `INSERT INTO session_labels(name, target) VALUES(?, ?)`, name, target); err != nil {
			return fmt.Errorf("write session label %q: %w", name, err)
		}
	}
	return nil
}

// redactInto round-trips value through the JSON redactor into target.
func redactInto(value, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data, err = utils.RedactJSON(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// updateActiveBranchState applies mutate to the active branch's state and
// writes that one row, so a checkpoint never rewrites the rest of the tree.
// A workspace whose tree was never saved falls back to a whole-tree save.
func updateActiveBranchState(workspace string, mutate func(*BranchState)) error {
	fallback := false
	err := withRuntimeStore(workspace, func(store *runtimestore.Store) error {
		ctx := context.Background()
		return store.WithTx(ctx, func(tx *sql.Tx) error {
			var active string
			var state []byte
			err := tx.QueryRowContext(ctx, `SELECT h.active_branch, b.state FROM session_tree_head h
JOIN session_branches b ON b.id = h.active_branch WHERE h.id = 1`).Scan(&active, &state)
			if errors.Is(err, sql.ErrNoRows) {
				fallback = true
				return nil
			}
			if err != nil {
				return fmt.Errorf("read active branch state: %w", err)
			}
			var current BranchState
			if err := json.Unmarshal(state, &current); err != nil {
				return fmt.Errorf("decode active branch state: %w", err)
			}
			mutate(&current)
			var redacted BranchState
			if err := redactInto(current, &redacted); err != nil {
				return fmt.Errorf("redact active branch state: %w", err)
			}
			body, err := json.Marshal(redacted)
			if err != nil {
				return fmt.Errorf("marshal active branch state: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE session_branches SET state = ? WHERE id = ?`, body, active); err != nil {
				return fmt.Errorf("write active branch state: %w", err)
			}
			return nil
		})
	})
	if err != nil || !fallback {
		return err
	}
	st, err := LoadSessionTree(workspace)
	if err != nil {
		return err
	}
	branch := st.Branches[st.ActiveBranch]
	if branch == nil {
		return nil
	}
	mutate(&branch.State)
	return SaveSessionTree(workspace, st)
}
