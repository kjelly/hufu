package context

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// migrationDef is an immutable, versioned schema change. Definitions must
// never be edited once released: schema_migrations records a checksum of
// the SQL text and every open verifies it, so an edited definition looks
// identical to a tampered database and OpenSQLite refuses to proceed.
type migrationDef struct {
	version int
	name    string
	sql     string
}

var migrations = []migrationDef{
	{1, "initial_context_store", `CREATE TABLE context_items (id TEXT PRIMARY KEY, kind TEXT NOT NULL, content TEXT NOT NULL, content_hash TEXT NOT NULL, project_id TEXT NOT NULL, team_id TEXT, session_id TEXT, agent_id TEXT, task_id TEXT, attempt_id TEXT, authority TEXT NOT NULL, trust_level TEXT NOT NULL, priority INTEGER NOT NULL, must_keep INTEGER NOT NULL DEFAULT 0, pinned INTEGER NOT NULL DEFAULT 0, confidence REAL NOT NULL DEFAULT 1.0, source_json TEXT NOT NULL, evidence_json TEXT NOT NULL DEFAULT '[]', tags_json TEXT NOT NULL DEFAULT '[]', metadata_json TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, valid_from INTEGER, valid_until INTEGER, expires_at INTEGER, superseded_by TEXT, embedding_state TEXT NOT NULL DEFAULT 'pending', embedding_model TEXT); CREATE INDEX idx_context_scope ON context_items(project_id, team_id, session_id, agent_id, task_id); CREATE INDEX idx_context_kind ON context_items(project_id, kind); CREATE INDEX idx_context_created ON context_items(project_id, created_at DESC); CREATE INDEX idx_context_hash ON context_items(project_id, content_hash); CREATE INDEX idx_context_validity ON context_items(project_id, valid_until, expires_at); CREATE TABLE context_edges (from_id TEXT NOT NULL, relation TEXT NOT NULL, to_id TEXT NOT NULL, metadata_json TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL, PRIMARY KEY(from_id, relation, to_id)); CREATE TABLE context_events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, event_type TEXT NOT NULL, item_id TEXT, scope_json TEXT NOT NULL, payload_json TEXT NOT NULL, created_at INTEGER NOT NULL); CREATE VIRTUAL TABLE context_items_fts USING fts5(id UNINDEXED, content, kind, tags, tokenize='unicode61');`},
	{2, "context_events_type_index", `CREATE INDEX IF NOT EXISTS idx_context_events_type ON context_events(event_type);`},
	{3, "branch_id_lifecycle_schema", `ALTER TABLE context_items ADD COLUMN branch_id TEXT; ALTER TABLE context_items ADD COLUMN lifecycle TEXT NOT NULL DEFAULT 'confirmed'; DROP INDEX idx_context_scope; CREATE INDEX idx_context_scope ON context_items(project_id, team_id, session_id, branch_id, agent_id, task_id);`},
	{4, "outcome_driven_experience", `CREATE TABLE experience_aggregates (context_item_id TEXT NOT NULL, policy_version TEXT NOT NULL, positive_weight REAL NOT NULL DEFAULT 0, negative_weight REAL NOT NULL DEFAULT 0, exposure_count INTEGER NOT NULL DEFAULT 0, consulted_count INTEGER NOT NULL DEFAULT 0, applied_count INTEGER NOT NULL DEFAULT 0, rejected_count INTEGER NOT NULL DEFAULT 0, verified_support_count INTEGER NOT NULL DEFAULT 0, causal_failure_count INTEGER NOT NULL DEFAULT 0, independent_task_count INTEGER NOT NULL DEFAULT 0, independent_project_count INTEGER NOT NULL DEFAULT 0, utility_lower_bound REAL NOT NULL DEFAULT 0, last_observed_at INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(context_item_id, policy_version)); CREATE TABLE experience_processed_events (idempotency_key TEXT PRIMARY KEY, processed_at INTEGER NOT NULL); CREATE TABLE experience_observation_sources (context_item_id TEXT NOT NULL, policy_version TEXT NOT NULL, task_id TEXT NOT NULL, project_id TEXT NOT NULL, PRIMARY KEY(context_item_id, policy_version, task_id, project_id)); CREATE TABLE memory_policy_versions (policy_version TEXT PRIMARY KEY, snapshot_json TEXT NOT NULL, revision_hash TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL, adopted_at INTEGER);`},
	{5, "memory_consolidation_proposals", `CREATE TABLE consolidation_proposals (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, team_id TEXT, candidate_context_item_id TEXT NOT NULL, source_ids_json TEXT NOT NULL, source_revisions_json TEXT NOT NULL, aggregate_revisions_json TEXT NOT NULL, status TEXT NOT NULL, reason TEXT, created_at INTEGER NOT NULL, reviewed_at INTEGER); CREATE INDEX idx_consolidation_project_status ON consolidation_proposals(project_id,status,created_at DESC);`},
	{6, "ltm_promotion", `CREATE TABLE promotion_proposals (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, team_id TEXT NOT NULL, type TEXT NOT NULL, agent_id TEXT, target_path TEXT NOT NULL, target_base_hash TEXT NOT NULL DEFAULT '', draft TEXT NOT NULL, draft_hash TEXT NOT NULL, policy_version TEXT NOT NULL, status TEXT NOT NULL, metrics_json TEXT NOT NULL, rejection_reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, applied_at INTEGER); CREATE INDEX idx_promotion_scope_status ON promotion_proposals(project_id,team_id,status,created_at DESC); CREATE TABLE promotion_sources (proposal_id TEXT NOT NULL REFERENCES promotion_proposals(id) ON DELETE CASCADE, context_item_id TEXT NOT NULL, content_hash TEXT NOT NULL, aggregate_revision INTEGER NOT NULL, PRIMARY KEY(proposal_id,context_item_id)); CREATE TABLE promotion_event_outbox (idempotency_key TEXT PRIMARY KEY, event_type TEXT NOT NULL, payload_json TEXT NOT NULL, created_at INTEGER NOT NULL, delivered_at INTEGER);`},
	{7, "typed_context_activation_outcomes", `CREATE TABLE context_activation (context_item_id TEXT PRIMARY KEY REFERENCES context_items(id) ON DELETE CASCADE, phases TEXT NOT NULL DEFAULT '', triggers TEXT NOT NULL DEFAULT '', roles TEXT NOT NULL DEFAULT '', capabilities TEXT NOT NULL DEFAULT '', tools TEXT NOT NULL DEFAULT '', error_classes TEXT NOT NULL DEFAULT '', environment TEXT NOT NULL DEFAULT ''); INSERT INTO context_activation(context_item_id,phases,triggers,roles,capabilities,tools,error_classes,environment) SELECT id,COALESCE(json_extract(metadata_json,'$."activation.phases"'),''),COALESCE(json_extract(metadata_json,'$."activation.triggers"'),''),COALESCE(json_extract(metadata_json,'$."activation.roles"'),''),COALESCE(json_extract(metadata_json,'$."activation.capabilities"'),''),COALESCE(json_extract(metadata_json,'$."activation.tools"'),''),COALESCE(json_extract(metadata_json,'$."activation.error_classes"'),''),COALESCE(json_extract(metadata_json,'$."activation.environment"'),'') FROM context_items; CREATE INDEX idx_context_activation_phase_trigger ON context_activation(phases,triggers); CREATE INDEX idx_context_activation_role_environment ON context_activation(roles,environment); CREATE TRIGGER context_activation_insert AFTER INSERT ON context_items BEGIN INSERT OR REPLACE INTO context_activation(context_item_id,phases,triggers,roles,capabilities,tools,error_classes,environment) VALUES(NEW.id,COALESCE(json_extract(NEW.metadata_json,'$."activation.phases"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.triggers"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.roles"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.capabilities"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.tools"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.error_classes"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.environment"'),'')); END; CREATE TRIGGER context_activation_update AFTER UPDATE OF metadata_json ON context_items BEGIN INSERT OR REPLACE INTO context_activation(context_item_id,phases,triggers,roles,capabilities,tools,error_classes,environment) VALUES(NEW.id,COALESCE(json_extract(NEW.metadata_json,'$."activation.phases"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.triggers"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.roles"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.capabilities"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.tools"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.error_classes"'),''),COALESCE(json_extract(NEW.metadata_json,'$."activation.environment"'),'')); END; CREATE TABLE context_outcome_observations (idempotency_key TEXT PRIMARY KEY, context_item_id TEXT NOT NULL, phase TEXT NOT NULL, trigger TEXT NOT NULL, agent_role TEXT NOT NULL, environment TEXT NOT NULL, outcome TEXT NOT NULL, policy_revision TEXT NOT NULL, observed_at INTEGER NOT NULL); CREATE INDEX idx_context_outcome_dimensions ON context_outcome_observations(context_item_id,phase,trigger,agent_role,environment,outcome);`},
	// Additive execution linkage keeps outcome rows auditable without parsing
	// opaque idempotency keys and remains safe for already-created databases.
	{8, "context_outcome_execution_linkage", `ALTER TABLE context_outcome_observations ADD COLUMN request_id TEXT NOT NULL DEFAULT ''; ALTER TABLE context_outcome_observations ADD COLUMN manifest_fingerprint TEXT NOT NULL DEFAULT ''; ALTER TABLE context_outcome_observations ADD COLUMN run_id TEXT NOT NULL DEFAULT ''; ALTER TABLE context_outcome_observations ADD COLUMN task_id TEXT NOT NULL DEFAULT ''; ALTER TABLE context_outcome_observations ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0; ALTER TABLE context_outcome_observations ADD COLUMN model_execution_id TEXT NOT NULL DEFAULT ''; ALTER TABLE context_outcome_observations ADD COLUMN verification_outcome TEXT NOT NULL DEFAULT ''; ALTER TABLE context_outcome_observations ADD COLUMN acceptance_outcome TEXT NOT NULL DEFAULT ''; ALTER TABLE context_outcome_observations ADD COLUMN judge_outcome TEXT NOT NULL DEFAULT ''; ALTER TABLE context_outcome_observations ADD COLUMN skeptic_outcome TEXT NOT NULL DEFAULT ''; CREATE INDEX idx_context_outcome_execution ON context_outcome_observations(run_id,task_id,attempt,model_execution_id,manifest_fingerprint);`},
}

func migrationChecksum(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

type SQLiteRepository struct {
	db   *sql.DB
	path string
}

func OpenSQLite(path string) (*SQLiteRepository, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite supports concurrent processes; serialize this handle's writers.
	r := &SQLiteRepository{db: db, path: path}
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, err
	}
	if err = r.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

func (r *SQLiteRepository) Close() error { return r.db.Close() }

// migrate applies any not-yet-applied migrationDef in order, verifying the
// checksum of every already-applied one first so a silently edited migration
// is treated as tampering rather than replayed. It creates a file-level
// backup before applying a new migration to a store that already has data,
// per the spec's "confirm a recoverable copy exists before migrating"
// requirement; a brand-new, still-empty store has nothing to protect and
// skips the backup.
func (r *SQLiteRepository) migrate(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL, checksum TEXT NOT NULL)`); err != nil {
		return err
	}
	applied := map[int]string{}
	rows, err := r.db.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		var sum string
		if err = rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return err
		}
		applied[v] = sum
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	var pending []migrationDef
	for _, m := range migrations {
		want := migrationChecksum(m.sql)
		if got, ok := applied[m.version]; ok {
			if got != want {
				return fmt.Errorf("schema_migrations checksum mismatch for version %d (%s): the applied migration appears to have been modified after being committed", m.version, m.name)
			}
			continue
		}
		pending = append(pending, m)
	}
	if len(pending) == 0 {
		return nil
	}
	if len(applied) > 0 {
		if err = r.backupBeforeMigration(); err != nil {
			return fmt.Errorf("creating recovery backup before migration: %w", err)
		}
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range pending {
		if _, err = tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("applying migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,applied_at,checksum) VALUES(?,?,?,?)", m.version, m.name, time.Now().UnixMilli(), migrationChecksum(m.sql)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// backupBeforeMigration copies the database file (and any WAL/SHM sidecar
// files) to a timestamped path before schema changes are applied, so a
// failed or bad migration has a recoverable copy to restore from.
func (r *SQLiteRepository) backupBeforeMigration() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	backupPath := fmt.Sprintf("%s.bak-%d", r.path, time.Now().UnixNano())
	if err = os.WriteFile(backupPath, data, 0o600); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		side, err := os.ReadFile(r.path + suffix)
		if err != nil {
			continue // sidecar files are optional depending on checkpoint state
		}
		if err = os.WriteFile(backupPath+suffix, side, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func normalize(item *ContextItem) error {
	item.Content = RedactSecrets(strings.ReplaceAll(strings.TrimSpace(item.Content), "\r\n", "\n"))
	if item.Content == "" || item.Scope.ProjectID == "" {
		return errors.New("context item content and project scope are required")
	}
	if item.ID == "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", item.Scope.ProjectID, item.Kind, item.Content)))
		item.ID = "ctx-" + hex.EncodeToString(sum[:12])
	}
	sum := sha256.Sum256([]byte(item.Content))
	item.ContentHash = hex.EncodeToString(sum[:])
	// Confidence is preserved verbatim: an explicit 0 is a legitimate low-trust
	// value and must not be rewritten to the default. Callers that intend a
	// default set it explicitly before Append.
	if item.EmbeddingState == "" {
		item.EmbeddingState = "pending"
	}
	if item.Lifecycle == "" {
		item.Lifecycle = LifecycleConfirmed
	}
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	return nil
}
func millis(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.UnixMilli()
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func (r *SQLiteRepository) Append(ctx context.Context, items ...ContextItem) error {
	return r.withBusyRetry(ctx, func() error { return r.appendOnce(ctx, items...) })
}

// UpsertCandidate preserves a candidate's canonical identity while allowing a
// later run to refresh an unconfirmed/rejected duplicate with its own trusted
// run and evidence metadata. Confirmed records are immutable knowledge: a
// duplicate proposal returns the confirmed record instead of reopening it.
func (r *SQLiteRepository) UpsertCandidate(ctx context.Context, item ContextItem) (ContextItem, error) {
	if item.Lifecycle != LifecycleCandidate {
		return ContextItem{}, errors.New("upsert candidate requires candidate lifecycle")
	}
	var stored ContextItem
	err := r.withBusyRetry(ctx, func() error {
		if err := normalize(&item); err != nil {
			return err
		}
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		var existingID string
		err = tx.QueryRowContext(ctx, `SELECT id FROM context_items WHERE project_id=? AND kind=? AND content_hash=? AND COALESCE(team_id,'')=? AND COALESCE(session_id,'')=? AND COALESCE(branch_id,'')=? AND COALESCE(agent_id,'')=? AND COALESCE(task_id,'')=? AND COALESCE(attempt_id,'')=? LIMIT 1`, item.Scope.ProjectID, item.Kind, item.ContentHash, item.Scope.TeamID, item.Scope.SessionID, item.Scope.BranchID, item.Scope.AgentID, item.Scope.TaskID, item.Scope.AttemptID).Scan(&existingID)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, "INSERT INTO context_items ("+itemColumns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", 30), ",")+")", item.ID, item.Kind, item.Content, item.ContentHash, item.Scope.ProjectID, nilIfEmpty(item.Scope.TeamID), nilIfEmpty(item.Scope.SessionID), nilIfEmpty(item.Scope.BranchID), nilIfEmpty(item.Scope.AgentID), nilIfEmpty(item.Scope.TaskID), nilIfEmpty(item.Scope.AttemptID), item.Authority, item.TrustLevel, item.Priority, boolInt(item.MustKeep), boolInt(item.Pinned), item.Confidence, mustJSON(item.Source), mustJSON(item.Evidence), mustJSON(item.Tags), mustJSON(item.Metadata), item.CreatedAt.UnixMilli(), item.UpdatedAt.UnixMilli(), millis(item.ValidFrom), millis(item.ValidUntil), millis(item.ExpiresAt), nilIfEmpty(item.SupersededBy), string(item.Lifecycle), item.EmbeddingState, nilIfEmpty(item.EmbeddingModel))
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO context_items_fts(id,content,kind,tags) VALUES(?,?,?,?)", item.ID, item.Content, item.Kind, strings.Join(item.Tags, " ")); err != nil {
				return err
			}
			if err = insertEvent(ctx, tx, "candidate_append", item.ID, item.Scope, map[string]string{"lifecycle": string(LifecycleCandidate)}); err != nil {
				return err
			}
			stored = item
			return tx.Commit()
		}
		if err != nil {
			return err
		}
		existing, err := scanItem(tx.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE id=?", existingID))
		if err != nil {
			return err
		}
		if existing.Lifecycle == LifecycleConfirmed {
			stored = existing
			if err = insertEvent(ctx, tx, "candidate_duplicate_confirmed", existing.ID, existing.Scope, map[string]string{"candidate_id": item.ID}); err != nil {
				return err
			}
			return tx.Commit()
		}
		if _, err = tx.ExecContext(ctx, "UPDATE context_items SET source_json=?,evidence_json=?,metadata_json=?,confidence=?,lifecycle=?,updated_at=? WHERE id=?", mustJSON(item.Source), mustJSON(item.Evidence), mustJSON(item.Metadata), item.Confidence, string(LifecycleCandidate), item.UpdatedAt.UnixMilli(), existing.ID); err != nil {
			return err
		}
		if err = insertEvent(ctx, tx, "candidate_refresh", existing.ID, existing.Scope, map[string]string{"lifecycle": string(LifecycleCandidate)}); err != nil {
			return err
		}
		stored = existing
		stored.Source, stored.Evidence, stored.Metadata, stored.Confidence = item.Source, item.Evidence, item.Metadata, item.Confidence
		stored.Lifecycle, stored.UpdatedAt = LifecycleCandidate, item.UpdatedAt
		return tx.Commit()
	})
	return stored, err
}

func (r *SQLiteRepository) appendOnce(ctx context.Context, items ...ContextItem) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range items {
		if err = normalize(&items[i]); err != nil {
			return err
		}
		it := items[i]
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT id FROM context_items WHERE project_id=? AND kind=? AND content_hash=? AND COALESCE(team_id,'')=? AND COALESCE(session_id,'')=? AND COALESCE(branch_id,'')=? AND COALESCE(agent_id,'')=? AND COALESCE(task_id,'')=? AND COALESCE(attempt_id,'')=? LIMIT 1`, it.Scope.ProjectID, it.Kind, it.ContentHash, it.Scope.TeamID, it.Scope.SessionID, it.Scope.BranchID, it.Scope.AgentID, it.Scope.TaskID, it.Scope.AttemptID).Scan(&existing)
		if err == nil {
			if _, err = tx.ExecContext(ctx, "UPDATE context_items SET updated_at=?, source_json=? WHERE id=?", it.UpdatedAt.UnixMilli(), mustJSON(it.Source), existing); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES(?,?,?,?,?)", "deduplicate", existing, mustJSON(it.Scope), "{}", it.UpdatedAt.UnixMilli()); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO context_items ("+itemColumns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", 30), ",")+")", it.ID, it.Kind, it.Content, it.ContentHash, it.Scope.ProjectID, nilIfEmpty(it.Scope.TeamID), nilIfEmpty(it.Scope.SessionID), nilIfEmpty(it.Scope.BranchID), nilIfEmpty(it.Scope.AgentID), nilIfEmpty(it.Scope.TaskID), nilIfEmpty(it.Scope.AttemptID), it.Authority, it.TrustLevel, it.Priority, boolInt(it.MustKeep), boolInt(it.Pinned), it.Confidence, mustJSON(it.Source), mustJSON(it.Evidence), mustJSON(it.Tags), mustJSON(it.Metadata), it.CreatedAt.UnixMilli(), it.UpdatedAt.UnixMilli(), millis(it.ValidFrom), millis(it.ValidUntil), millis(it.ExpiresAt), nilIfEmpty(it.SupersededBy), string(it.Lifecycle), it.EmbeddingState, nilIfEmpty(it.EmbeddingModel))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO context_items_fts(id,content,kind,tags) VALUES(?,?,?,?)", it.ID, it.Content, it.Kind, strings.Join(it.Tags, " ")); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES(?,?,?,?,?)", "append", it.ID, mustJSON(it.Scope), "{}", it.UpdatedAt.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AppendReducer is the shared working-memory reducer's idempotent append. It
// deduplicates on execution identity (run/task/attempt from metadata) plus
// kind and content hash, so two tasks reporting the same finding keep distinct
// provenance instead of collapsing. On a duplicate it merges immutable
// evidence refs and refreshes metadata rather than overwriting provenance.
func (r *SQLiteRepository) AppendReducer(ctx context.Context, items ...ContextItem) error {
	return r.withBusyRetry(ctx, func() error { return r.appendReducerOnce(ctx, items...) })
}

func (r *SQLiteRepository) appendReducerOnce(ctx context.Context, items ...ContextItem) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range items {
		if err = normalize(&items[i]); err != nil {
			return err
		}
		it := items[i]
		runID := it.Metadata["run_id"]
		taskID := it.Metadata["task_id"]
		attempt := it.Metadata["attempt"]
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT id FROM context_items WHERE project_id=? AND kind=? AND content_hash=? AND COALESCE(team_id,'')=? AND COALESCE(session_id,'')=? AND COALESCE(branch_id,'')=? AND COALESCE(agent_id,'')=? AND COALESCE(task_id,'')=? AND COALESCE(attempt_id,'')=? AND json_extract(metadata_json,'$.run_id')=? AND json_extract(metadata_json,'$.task_id')=? AND json_extract(metadata_json,'$.attempt')=? LIMIT 1`,
			it.Scope.ProjectID, it.Kind, it.ContentHash, it.Scope.TeamID, it.Scope.SessionID, it.Scope.BranchID, it.Scope.AgentID, it.Scope.TaskID, it.Scope.AttemptID, runID, taskID, attempt).Scan(&existing)
		if err == nil {
			// Duplicate from the same execution identity: merge immutable
			// evidence refs and refresh metadata instead of overwriting the
			// original provenance.
			existingItem, err := scanItem(tx.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE id=?", existing))
			if err != nil {
				return err
			}
			mergedEvidence := append([]EvidenceRef(nil), existingItem.Evidence...)
			for _, ev := range it.Evidence {
				found := false
				for _, have := range mergedEvidence {
					if have.ItemID == ev.ItemID && have.Type == ev.Type && have.Ref == ev.Ref {
						found = true
						break
					}
				}
				if !found {
					mergedEvidence = append(mergedEvidence, ev)
				}
			}
			mergedMetadata := existingItem.Metadata
			if mergedMetadata == nil {
				mergedMetadata = make(map[string]string, len(it.Metadata))
			}
			for k, v := range it.Metadata {
				mergedMetadata[k] = v
			}
			if _, err = tx.ExecContext(ctx, "UPDATE context_items SET evidence_json=?,metadata_json=?,updated_at=? WHERE id=?", mustJSON(mergedEvidence), mustJSON(mergedMetadata), it.UpdatedAt.UnixMilli(), existing); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES(?,?,?,?,?)", "reducer_deduplicate", existing, mustJSON(it.Scope), "{}", it.UpdatedAt.UnixMilli()); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// The deterministic ID from normalize() is content-derived, so two items
		// with the same content but different execution identity would collide.
		// Ensure a unique ID before inserting.
		id := it.ID
		for {
			var probe string
			probeErr := tx.QueryRowContext(ctx, "SELECT id FROM context_items WHERE id=?", id).Scan(&probe)
			if errors.Is(probeErr, sql.ErrNoRows) {
				break
			}
			if probeErr != nil {
				return probeErr
			}
			id = it.ID + "-" + hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))[:8]
		}
		it.ID = id
		_, err = tx.ExecContext(ctx, "INSERT INTO context_items ("+itemColumns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", 30), ",")+")", it.ID, it.Kind, it.Content, it.ContentHash, it.Scope.ProjectID, nilIfEmpty(it.Scope.TeamID), nilIfEmpty(it.Scope.SessionID), nilIfEmpty(it.Scope.BranchID), nilIfEmpty(it.Scope.AgentID), nilIfEmpty(it.Scope.TaskID), nilIfEmpty(it.Scope.AttemptID), it.Authority, it.TrustLevel, it.Priority, boolInt(it.MustKeep), boolInt(it.Pinned), it.Confidence, mustJSON(it.Source), mustJSON(it.Evidence), mustJSON(it.Tags), mustJSON(it.Metadata), it.CreatedAt.UnixMilli(), it.UpdatedAt.UnixMilli(), millis(it.ValidFrom), millis(it.ValidUntil), millis(it.ExpiresAt), nilIfEmpty(it.SupersededBy), string(it.Lifecycle), it.EmbeddingState, nilIfEmpty(it.EmbeddingModel))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO context_items_fts(id,content,kind,tags) VALUES(?,?,?,?)", it.ID, it.Content, it.Kind, strings.Join(it.Tags, " ")); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES(?,?,?,?,?)", "reducer_append", it.ID, mustJSON(it.Scope), "{}", it.UpdatedAt.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// withBusyRetry handles short SQLite writer contention across hufu processes.
// It always honours cancellation instead of turning a cancelled task into a
// background retry loop.
func (r *SQLiteRepository) withBusyRetry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = fn(); err == nil || (!strings.Contains(strings.ToLower(err.Error()), "database is locked") && !strings.Contains(strings.ToLower(err.Error()), "database is busy")) {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanItem(row interface{ Scan(...any) error }) (ContextItem, error) {
	var i ContextItem
	var team, session, branch, agent, task, attempt, super, model, lifecycle sql.NullString
	var source, evidence, tags, metadata string
	var created, updated int64
	var vf, vu, ex sql.NullInt64
	var keep, pinned int
	err := row.Scan(&i.ID, &i.Kind, &i.Content, &i.ContentHash, &i.Scope.ProjectID, &team, &session, &branch, &agent, &task, &attempt, &i.Authority, &i.TrustLevel, &i.Priority, &keep, &pinned, &i.Confidence, &source, &evidence, &tags, &metadata, &created, &updated, &vf, &vu, &ex, &super, &lifecycle, &i.EmbeddingState, &model)
	if err != nil {
		return i, err
	}
	i.Scope.TeamID = team.String
	i.Scope.SessionID = session.String
	i.Scope.BranchID = branch.String
	i.Scope.AgentID = agent.String
	i.Scope.TaskID = task.String
	i.Scope.AttemptID = attempt.String
	i.SupersededBy = super.String
	i.Lifecycle = ContextLifecycle(lifecycle.String)
	i.EmbeddingModel = model.String
	i.MustKeep = keep != 0
	i.Pinned = pinned != 0
	i.CreatedAt = time.UnixMilli(created).UTC()
	i.UpdatedAt = time.UnixMilli(updated).UTC()
	if vf.Valid {
		v := time.UnixMilli(vf.Int64).UTC()
		i.ValidFrom = &v
	}
	if vu.Valid {
		v := time.UnixMilli(vu.Int64).UTC()
		i.ValidUntil = &v
	}
	if ex.Valid {
		v := time.UnixMilli(ex.Int64).UTC()
		i.ExpiresAt = &v
	}
	_ = json.Unmarshal([]byte(source), &i.Source)
	_ = json.Unmarshal([]byte(evidence), &i.Evidence)
	_ = json.Unmarshal([]byte(tags), &i.Tags)
	_ = json.Unmarshal([]byte(metadata), &i.Metadata)
	return i, nil
}

const itemColumns = `id,kind,content,content_hash,project_id,team_id,session_id,branch_id,agent_id,task_id,attempt_id,authority,trust_level,priority,must_keep,pinned,confidence,source_json,evidence_json,tags_json,metadata_json,created_at,updated_at,valid_from,valid_until,expires_at,superseded_by,lifecycle,embedding_state,embedding_model`

func (r *SQLiteRepository) Get(ctx context.Context, id string) (ContextItem, error) {
	return scanItem(r.db.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE id=?", id))
}
func (r *SQLiteRepository) GetMany(ctx context.Context, ids []string) ([]ContextItem, error) {
	out := make([]ContextItem, 0, len(ids))
	for _, id := range ids {
		i, e := r.Get(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, i)
	}
	return out, nil
}

// scopeAuthorize builds the scope predicate for a retrieval path. It
// replaces the pre-WP-1 scopeWhere function and enforces explicit visibility
// semantics so an empty child field is no longer a wildcard.
//
//   - VisibilityAncestors (runtime default): a non-empty request field
//     matches the same value OR a NULL ancestor (shared). An empty request
//     field matches ONLY NULL — the fix for the wildcard that let a
//     coordinator-level query see every agent's private records.
//   - VisibilityExact: non-empty fields match exactly; empty fields require
//     NULL. Used by the shared-only projection query.
//   - VisibilitySubtree: non-empty fields match same-or-NULL; empty fields
//     are omitted (wildcard). Maintenance/CLI only.
//
// prefix is a table alias (e.g. "c.") for queries that join context_items.
func scopeAuthorize(prefix string, scope Scope, visibility ScopeVisibility, args *[]any) []string {
	if visibility == "" {
		visibility = VisibilityAncestors
	}
	*args = append(*args, scope.ProjectID)
	where := []string{prefix + "project_id=?"}
	for _, p := range []struct{ n, v string }{
		{"team_id", scope.TeamID},
		{"session_id", scope.SessionID},
		{"branch_id", scope.BranchID},
		{"agent_id", scope.AgentID},
		{"task_id", scope.TaskID},
		{"attempt_id", scope.AttemptID},
	} {
		switch visibility {
		case VisibilitySubtree:
			if p.v != "" {
				where = append(where, "("+prefix+p.n+" IS NULL OR "+prefix+p.n+"=?)")
				*args = append(*args, p.v)
			}
		case VisibilityExact:
			if p.v != "" {
				where = append(where, prefix+p.n+"=?")
				*args = append(*args, p.v)
			} else {
				where = append(where, prefix+p.n+" IS NULL")
			}
		default: // VisibilityAncestors
			if p.v != "" {
				where = append(where, "("+prefix+p.n+" IS NULL OR "+prefix+p.n+"=?)")
				*args = append(*args, p.v)
			} else {
				where = append(where, prefix+p.n+" IS NULL")
			}
		}
	}
	return where
}

func (r *SQLiteRepository) Query(ctx context.Context, q RepositoryQuery) ([]ContextItem, error) {
	if q.Scope.ProjectID == "" {
		return nil, errors.New("project scope is required")
	}
	var args []any
	where := scopeAuthorize("", q.Scope, q.Visibility, &args)
	if !q.IncludeSuperseded {
		where = append(where, "superseded_by IS NULL")
	}
	if !q.IncludeExpired {
		where = append(where, "(expires_at IS NULL OR expires_at>?)")
		args = append(args, time.Now().UnixMilli())
	}
	if !q.IncludeCandidates {
		where = append(where, "lifecycle='confirmed'")
	}
	now := time.Now().UnixMilli()
	where = append(where, "(valid_from IS NULL OR valid_from<=?)", "(valid_until IS NULL OR valid_until>?)")
	args = append(args, now, now)
	if len(q.Kinds) > 0 {
		ps := make([]string, len(q.Kinds))
		for x, k := range q.Kinds {
			ps[x] = "?"
			args = append(args, k)
		}
		where = append(where, "kind IN ("+strings.Join(ps, ",")+")")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit)
	rows, e := r.db.QueryContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE "+strings.Join(where, " AND ")+" ORDER BY priority DESC, created_at DESC, id ASC LIMIT ?", args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []ContextItem
	for rows.Next() {
		i, e := scanItem(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// QuerySharedProjection returns only shared-scope items (agent_id, task_id,
// branch_id, and attempt_id are all NULL) for the given project/team/session.
// It is the canonical source for rebuilding legacy STM/LTM Markdown
// projections so private records never leak into shared prompt files.
func (r *SQLiteRepository) QuerySharedProjection(ctx context.Context, scope Scope) ([]ContextItem, error) {
	sharedScope := Scope{ProjectID: scope.ProjectID, TeamID: scope.TeamID, SessionID: scope.SessionID}
	return r.Query(ctx, RepositoryQuery{
		Scope:             sharedScope,
		Visibility:        VisibilityExact,
		IncludeSuperseded: true,
		IncludeExpired:    true,
		IncludeCandidates: true,
		Limit:             100000,
	})
}

// QuerySharedSessionProjection returns prompt-eligible shared knowledge for
// one session only. Persistent records have session_id NULL and therefore do
// not satisfy this exact scope.
func (r *SQLiteRepository) QuerySharedSessionProjection(ctx context.Context, scope Scope) ([]ContextItem, error) {
	if strings.TrimSpace(scope.SessionID) == "" {
		// A maintenance rebuild may be requested with project/team scope only.
		// It has no unambiguous session STM to render, while persistent LTM is
		// still well-defined, so render STM as empty rather than broadening the
		// query to every session.
		return nil, nil
	}
	return r.Query(ctx, RepositoryQuery{
		Scope:      Scope{ProjectID: scope.ProjectID, TeamID: scope.TeamID, SessionID: scope.SessionID},
		Visibility: contextVisibilityExact(),
		Limit:      100000,
	})
}

// QuerySharedPersistentProjection returns prompt-eligible shared knowledge
// that deliberately has no session, branch, agent, task, or attempt scope.
func (r *SQLiteRepository) QuerySharedPersistentProjection(ctx context.Context, scope Scope) ([]ContextItem, error) {
	return r.Query(ctx, RepositoryQuery{
		Scope:      Scope{ProjectID: scope.ProjectID, TeamID: scope.TeamID},
		Visibility: contextVisibilityExact(),
		Limit:      100000,
	})
}

// contextVisibilityExact exists only to keep the two projection constructors
// visually symmetric and avoid future callers accidentally selecting the
// runtime ancestors mode for a projection.
func contextVisibilityExact() ScopeVisibility { return VisibilityExact }

// itemScope fetches an item's scope for event provenance. It returns a zero
// Scope on error (e.g. the item does not exist yet) rather than failing the
// caller's mutation: recording an event with an incomplete scope is better
// than losing the revision bump entirely.
func (r *SQLiteRepository) itemScope(ctx context.Context, tx *sql.Tx, id string) Scope {
	var s Scope
	var team, session, branch, agent, task, attempt sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT project_id, team_id, session_id, branch_id, agent_id, task_id, attempt_id FROM context_items WHERE id=?", id).Scan(&s.ProjectID, &team, &session, &branch, &agent, &task, &attempt); err != nil {
		return Scope{}
	}
	s.TeamID, s.SessionID, s.BranchID, s.AgentID, s.TaskID, s.AttemptID = team.String, session.String, branch.String, agent.String, task.String, attempt.String
	return s
}

// insertEvent appends a context_events row inside tx. Every mutation that
// changes canonical state (append, supersede, edge, expiry) MUST insert one
// of these so Revision reflects the true repository state and can be used
// for cache/prefetch invalidation.
func insertEvent(ctx context.Context, tx *sql.Tx, eventType, itemID string, scope Scope, payload any) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES(?,?,?,?,?)", eventType, itemID, mustJSON(scope), mustJSON(payload), time.Now().UnixMilli())
	return err
}

func (r *SQLiteRepository) MarkSuperseded(ctx context.Context, old []string, newID string) error {
	if len(old) == 0 {
		return nil
	}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, id := range old {
		scope := r.itemScope(ctx, tx, id)
		if _, e = tx.ExecContext(ctx, "UPDATE context_items SET superseded_by=?,updated_at=? WHERE id=?", newID, time.Now().UnixMilli(), id); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO context_edges(from_id,relation,to_id,metadata_json,created_at) VALUES(?,?,?,?,?)", id, "supersedes", newID, "{}", time.Now().UnixMilli()); e != nil {
			return e
		}
		if e = insertEvent(ctx, tx, "supersede", id, scope, map[string]string{"superseded_by": newID}); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// UpdateLifecycle changes explicitly selected records and emits an event for
// each change.  Scope checks deliberately live with the higher-level caller:
// lifecycle mutation is also used by maintenance operations, while runtime
// promotion first selects candidates with an authorised exact scope.
func (r *SQLiteRepository) UpdateLifecycle(ctx context.Context, ids []string, lifecycle ContextLifecycle) error {
	if len(ids) == 0 {
		return nil
	}
	if lifecycle != LifecycleCandidate && lifecycle != LifecycleConfirmed && lifecycle != LifecycleRejected {
		return fmt.Errorf("invalid context lifecycle %q", lifecycle)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		scope := r.itemScope(ctx, tx, id)
		result, err := tx.ExecContext(ctx, "UPDATE context_items SET lifecycle=?,updated_at=? WHERE id=?", string(lifecycle), time.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return sql.ErrNoRows
		}
		if err := insertEvent(ctx, tx, "lifecycle", id, scope, map[string]string{"lifecycle": string(lifecycle)}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BindCandidates merges sealed evidence into explicitly selected candidate
// records. It refuses to bind confirmed/rejected items so an evidence receipt
// cannot be retrofitted onto knowledge that has already reached a terminal
// lifecycle state.
func (r *SQLiteRepository) BindCandidates(ctx context.Context, ids []string, binding CandidateBinding) error {
	if len(ids) == 0 {
		return nil
	}
	if strings.TrimSpace(binding.Evidence.Type) == "" || strings.TrimSpace(binding.Evidence.Ref) == "" {
		return errors.New("candidate binding requires evidence type and ref")
	}
	return r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				continue
			}
			item, err := scanItem(tx.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE id=?", id))
			if err != nil {
				return err
			}
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
			found := false
			for _, existing := range evidence {
				if existing.ItemID == binding.Evidence.ItemID && existing.Type == binding.Evidence.Type && existing.Ref == binding.Evidence.Ref {
					found = true
					break
				}
			}
			if !found {
				evidence = append(evidence, binding.Evidence)
			}
			now := time.Now().UnixMilli()
			if _, err = tx.ExecContext(ctx, "UPDATE context_items SET evidence_json=?,metadata_json=?,updated_at=? WHERE id=?", mustJSON(evidence), mustJSON(metadata), now, id); err != nil {
				return err
			}
			if err = insertEvent(ctx, tx, "candidate_bind", id, item.Scope, map[string]string{"evidence_type": binding.Evidence.Type, "evidence_ref": binding.Evidence.Ref}); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// ConfirmCandidates is the atomic candidate-to-knowledge transition.  A
// candidate may carry a newline-separated supersedes_ids metadata value that
// was authorized by the caller at proposal time.  The new record is only made
// visible when every referenced current record can be superseded in the same
// transaction, so a crash can never leave two conflicting current truths or
// hide an old truth behind a rejected candidate.
func (r *SQLiteRepository) ConfirmCandidates(ctx context.Context, ids []string, binding CandidateBinding) error {
	if len(ids) == 0 {
		return nil
	}
	if strings.TrimSpace(binding.Evidence.Type) == "" || strings.TrimSpace(binding.Evidence.Ref) == "" {
		return errors.New("candidate binding requires evidence type and ref")
	}
	return r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		seenOld := make(map[string]string)
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				continue
			}
			item, err := scanItem(tx.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE id=?", id))
			if err != nil {
				return err
			}
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
			if _, err = tx.ExecContext(ctx, "UPDATE context_items SET evidence_json=?,metadata_json=?,lifecycle=?,updated_at=? WHERE id=?", mustJSON(evidence), mustJSON(metadata), string(LifecycleConfirmed), now, id); err != nil {
				return err
			}
			if err = insertEvent(ctx, tx, "candidate_bind", id, item.Scope, map[string]string{"evidence_type": binding.Evidence.Type, "evidence_ref": binding.Evidence.Ref}); err != nil {
				return err
			}
			if err = insertEvent(ctx, tx, "lifecycle", id, item.Scope, map[string]string{"lifecycle": string(LifecycleConfirmed)}); err != nil {
				return err
			}
			for _, oldID := range strings.Fields(metadata["supersedes_ids"]) {
				if prior, duplicate := seenOld[oldID]; duplicate && prior != id {
					return fmt.Errorf("superseded item %q is proposed by multiple candidates", oldID)
				}
				seenOld[oldID] = id
				old, err := scanItem(tx.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE id=?", oldID))
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
		}
		return tx.Commit()
	})
}

// UpdateEmbeddingState records rebuild progress in canonical storage. The
// vector index is disposable, so callers use this state to retry documents
// whose embedding failed without ever deleting their canonical records.
func (r *SQLiteRepository) UpdateEmbeddingState(ctx context.Context, id, state, model string) error {
	if id == "" || state == "" || model == "" {
		return errors.New("embedding item ID, state, and model are required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	scope := r.itemScope(ctx, tx, id)
	result, err := tx.ExecContext(ctx, "UPDATE context_items SET embedding_state=?,embedding_model=?,updated_at=? WHERE id=?", state, model, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	if err := insertEvent(ctx, tx, "embedding_state", id, scope, map[string]string{"state": state, "model": model}); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) AddEdges(ctx context.Context, edges ...ContextEdge) error {
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, x := range edges {
		if x.CreatedAt.IsZero() {
			x.CreatedAt = time.Now()
		}
		if _, e = tx.ExecContext(ctx, "INSERT OR REPLACE INTO context_edges VALUES(?,?,?,?,?)", x.FromID, x.Relation, x.ToID, mustJSON(x.Metadata), x.CreatedAt.UnixMilli()); e != nil {
			return e
		}
		scope := r.itemScope(ctx, tx, x.FromID)
		if e = insertEvent(ctx, tx, "edge", x.FromID, scope, map[string]string{"relation": x.Relation, "to_id": x.ToID}); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (r *SQLiteRepository) SearchExact(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if req.Scope.ProjectID == "" {
		return nil, errors.New("project scope is required")
	}
	needle := strings.ToLower(strings.TrimSpace(req.Query))
	if needle == "" {
		return nil, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	args := []any{needle}
	where := scopeAuthorize("", req.Scope, req.Visibility, &args)
	now := time.Now().UnixMilli()
	where = append(where, "superseded_by IS NULL", "(expires_at IS NULL OR expires_at>?)", "(valid_from IS NULL OR valid_from<=?)", "(valid_until IS NULL OR valid_until>?)")
	if !req.IncludeCandidates {
		where = append(where, "lifecycle='confirmed'")
	}
	args = append(args, now, now, now)
	appendSearchFilters("", &where, &args, req)
	args = append(args, limit)
	rows, e := r.db.QueryContext(ctx, "SELECT "+itemColumns+" FROM context_items WHERE instr(lower(content), ?) > 0 AND "+strings.Join(where, " AND ")+" ORDER BY priority DESC, created_at DESC, id ASC LIMIT ?", args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		i, e := scanItem(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, SearchResult{Item: i, Score: 1})
	}
	return out, rows.Err()
}
func (r *SQLiteRepository) SearchLexical(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if req.Scope.ProjectID == "" {
		return nil, errors.New("project scope is required")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	columns := "c." + strings.ReplaceAll(itemColumns, ",", ",c.")
	// Reuse the same scope predicate as Query so lexical retrieval can never
	// surface another team/session/agent/task's context (it previously only
	// filtered by project_id).
	args := []any{ftsQuery(req.Query)}
	where := append([]string{}, scopeAuthorize("c.", req.Scope, req.Visibility, &args)...)
	now := time.Now().UnixMilli()
	where = append(where, "c.superseded_by IS NULL", "(c.expires_at IS NULL OR c.expires_at>?)", "(c.valid_from IS NULL OR c.valid_from<=?)", "(c.valid_until IS NULL OR c.valid_until>?)")
	if !req.IncludeCandidates {
		where = append(where, "c.lifecycle='confirmed'")
	}
	args = append(args, now, now, now)
	appendSearchFilters("c.", &where, &args, req)
	args = append(args, limit)
	rows, e := r.db.QueryContext(ctx, "SELECT "+columns+", bm25(context_items_fts) FROM context_items_fts JOIN context_items c ON c.id=context_items_fts.id WHERE context_items_fts MATCH ? AND "+strings.Join(where, " AND ")+" ORDER BY bm25(context_items_fts) LIMIT ?", args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var score float64
		cols := &scanWithScore{rows: &rows, score: &score}
		i, e := scanItem(cols)
		if e != nil {
			return nil, e
		}
		out = append(out, SearchResult{Item: i, Score: -score})
	}
	return out, rows.Err()
}

func appendSearchFilters(prefix string, where *[]string, args *[]any, req SearchRequest) {
	if len(req.Kinds) > 0 {
		placeholders := make([]string, 0, len(req.Kinds))
		for _, kind := range req.Kinds {
			placeholders = append(placeholders, "?")
			*args = append(*args, kind)
		}
		*where = append(*where, prefix+"kind IN ("+strings.Join(placeholders, ",")+")")
	}
	if req.MinConfidence != nil {
		*where = append(*where, prefix+"confidence>=?")
		*args = append(*args, *req.MinConfidence)
	}
}

// RebuildLexical recreates the FTS5 projection from canonical rows. It is
// safe to run repeatedly and never changes canonical context records.
func (r *SQLiteRepository) RebuildLexical(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM context_items_fts"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO context_items_fts(id,content,kind,tags) SELECT id,content,kind,tags_json FROM context_items"); err != nil {
		return err
	}
	return tx.Commit()
}

// ftsQuery turns operational identifiers (paths, commands, punctuation) into
// plain FTS tokens so an exact-matchable path cannot make the lexical stage
// fail with an FTS syntax error.
func ftsQuery(query string) string {
	terms := strings.FieldsFunc(query, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_'
	})
	return strings.Join(terms, " ")
}

type scanWithScore struct {
	rows  **sql.Rows
	score *float64
}

func (s *scanWithScore) Scan(dest ...any) error {
	dest = append(dest, s.score)
	return (*s.rows).Scan(dest...)
}
func (r *SQLiteRepository) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, "SELECT id, project_id, team_id, session_id, branch_id, agent_id, task_id, attempt_id FROM context_items WHERE expires_at IS NOT NULL AND expires_at<?", before.UnixMilli())
	if e != nil {
		return 0, e
	}
	type expired struct {
		id    string
		scope Scope
	}
	var toDelete []expired
	for rows.Next() {
		var x expired
		var team, session, branch, agent, task, attempt sql.NullString
		if e = rows.Scan(&x.id, &x.scope.ProjectID, &team, &session, &branch, &agent, &task, &attempt); e != nil {
			rows.Close()
			return 0, e
		}
		x.scope.TeamID, x.scope.SessionID, x.scope.BranchID, x.scope.AgentID, x.scope.TaskID, x.scope.AttemptID = team.String, session.String, branch.String, agent.String, task.String, attempt.String
		toDelete = append(toDelete, x)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return 0, e
	}
	rows.Close()
	for _, x := range toDelete {
		// Delete the FTS row in the same transaction as the canonical row so
		// the lexical index never keeps an orphan pointing at a dropped item.
		if _, e = tx.ExecContext(ctx, "DELETE FROM context_items_fts WHERE id=?", x.id); e != nil {
			return 0, e
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM context_items WHERE id=?", x.id); e != nil {
			return 0, e
		}
		if e = insertEvent(ctx, tx, "expire", x.id, x.scope, map[string]string{}); e != nil {
			return 0, e
		}
	}
	if e = tx.Commit(); e != nil {
		return 0, e
	}
	return int64(len(toDelete)), nil
}
func (r *SQLiteRepository) Revision(ctx context.Context) (int64, error) {
	var n int64
	e := r.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM context_events").Scan(&n)
	return n, e
}
