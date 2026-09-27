package context

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// queryer is the read surface shared by *sql.DB and *sql.Tx. The repository
// owns a single SQLite connection, so every read performed while a
// transaction is open must go through that transaction to avoid deadlock.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// getItemQ reads one item, including content, through q.
func getItemQ(ctx context.Context, q queryer, id string) (ContextItem, error) {
	return scanItem(q.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE id=?", id))
}

// insertItemRowTx inserts an already normalized item and its FTS row. The
// caller records the context event that describes why the row was added.
func insertItemRowTx(ctx context.Context, tx *sql.Tx, it ContextItem) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO context_items ("+itemColumns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", 30), ",")+")", it.ID, it.Kind, it.Content, it.ContentHash, it.Scope.ProjectID, nilIfEmpty(it.Scope.TeamID), nilIfEmpty(it.Scope.SessionID), nilIfEmpty(it.Scope.BranchID), nilIfEmpty(it.Scope.AgentID), nilIfEmpty(it.Scope.TaskID), nilIfEmpty(it.Scope.AttemptID), it.Authority, it.TrustLevel, it.Priority, boolInt(it.MustKeep), boolInt(it.Pinned), it.Confidence, mustJSON(it.Source), mustJSON(it.Evidence), mustJSON(it.Tags), mustJSON(it.Metadata), it.CreatedAt.UnixMilli(), it.UpdatedAt.UnixMilli(), millis(it.ValidFrom), millis(it.ValidUntil), millis(it.ExpiresAt), nilIfEmpty(it.SupersededBy), string(it.Lifecycle), it.EmbeddingState, nilIfEmpty(it.EmbeddingModel)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO context_items_fts(id,content,kind,tags) VALUES(?,?,?,?)", it.ID, it.Content, it.Kind, strings.Join(it.Tags, " "))
	return err
}

// confirmCandidateTx binds sealed evidence to one loaded candidate, confirms
// it, and applies its recorded supersession links inside tx. seenOld detects
// two candidates in one transaction superseding the same record.
func confirmCandidateTx(ctx context.Context, tx *sql.Tx, item ContextItem, binding CandidateBinding, seenOld map[string]string) error {
	id := item.ID
	if item.Lifecycle != LifecycleCandidate {
		return fmt.Errorf("context item %q is not a candidate", id)
	}
	metadata := item.Metadata
	if metadata == nil {
		metadata = make(map[string]string, len(binding.Metadata))
	}
	for key, value := range binding.Metadata {
		metadata[key] = value
	}
	evidence := append([]EvidenceRef(nil), item.Evidence...)
	bound := false
	for _, existing := range evidence {
		if existing.ItemID == binding.Evidence.ItemID && existing.Type == binding.Evidence.Type && existing.Ref == binding.Evidence.Ref {
			bound = true
			break
		}
	}
	if !bound {
		evidence = append(evidence, binding.Evidence)
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, "UPDATE context_items SET evidence_json=?,metadata_json=?,lifecycle=?,updated_at=? WHERE id=?", mustJSON(evidence), mustJSON(metadata), string(LifecycleConfirmed), now, id); err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, "candidate_bind", id, item.Scope, map[string]string{"evidence_type": binding.Evidence.Type, "evidence_ref": binding.Evidence.Ref}); err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, "lifecycle", id, item.Scope, map[string]string{"lifecycle": string(LifecycleConfirmed)}); err != nil {
		return err
	}
	for _, oldID := range strings.Fields(metadata["supersedes_ids"]) {
		if prior, duplicate := seenOld[oldID]; duplicate && prior != id {
			return fmt.Errorf("superseded item %q is proposed by multiple candidates", oldID)
		}
		seenOld[oldID] = id
		old, err := getItemQ(ctx, tx, oldID)
		if err != nil {
			return fmt.Errorf("load superseded context item %q: %w", oldID, err)
		}
		if old.Lifecycle != LifecycleConfirmed || old.SupersededBy != "" {
			return fmt.Errorf("superseded context item %q is not current confirmed knowledge", oldID)
		}
		if _, err = tx.ExecContext(ctx, "UPDATE context_items SET superseded_by=?,updated_at=? WHERE id=?", id, now, oldID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO context_edges(from_id,relation,to_id,metadata_json,created_at) VALUES(?,?,?,?,?)", oldID, "supersedes", id, "{}", now); err != nil {
			return err
		}
		if err = insertEvent(ctx, tx, "supersede", oldID, old.Scope, map[string]string{"superseded_by": id}); err != nil {
			return err
		}
	}
	return nil
}
