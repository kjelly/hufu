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
	if item.Confidence == 0 {
		item.Confidence = 1
	}
	if item.EmbeddingState == "" {
		item.EmbeddingState = "pending"
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
		err = tx.QueryRowContext(ctx, `SELECT id FROM context_items WHERE project_id=? AND kind=? AND content_hash=? AND COALESCE(team_id,'')=? AND COALESCE(session_id,'')=? AND COALESCE(agent_id,'')=? AND COALESCE(task_id,'')=? AND COALESCE(attempt_id,'')=? LIMIT 1`, it.Scope.ProjectID, it.Kind, it.ContentHash, it.Scope.TeamID, it.Scope.SessionID, it.Scope.AgentID, it.Scope.TaskID, it.Scope.AttemptID).Scan(&existing)
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
		_, err = tx.ExecContext(ctx, "INSERT INTO context_items ("+itemColumns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", 28), ",")+")", it.ID, it.Kind, it.Content, it.ContentHash, it.Scope.ProjectID, nilIfEmpty(it.Scope.TeamID), nilIfEmpty(it.Scope.SessionID), nilIfEmpty(it.Scope.AgentID), nilIfEmpty(it.Scope.TaskID), nilIfEmpty(it.Scope.AttemptID), it.Authority, it.TrustLevel, it.Priority, boolInt(it.MustKeep), boolInt(it.Pinned), it.Confidence, mustJSON(it.Source), mustJSON(it.Evidence), mustJSON(it.Tags), mustJSON(it.Metadata), it.CreatedAt.UnixMilli(), it.UpdatedAt.UnixMilli(), millis(it.ValidFrom), millis(it.ValidUntil), millis(it.ExpiresAt), nilIfEmpty(it.SupersededBy), it.EmbeddingState, nilIfEmpty(it.EmbeddingModel))
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
	var team, session, agent, task, attempt, super, model sql.NullString
	var source, evidence, tags, metadata string
	var created, updated int64
	var vf, vu, ex sql.NullInt64
	var keep, pinned int
	err := row.Scan(&i.ID, &i.Kind, &i.Content, &i.ContentHash, &i.Scope.ProjectID, &team, &session, &agent, &task, &attempt, &i.Authority, &i.TrustLevel, &i.Priority, &keep, &pinned, &i.Confidence, &source, &evidence, &tags, &metadata, &created, &updated, &vf, &vu, &ex, &super, &i.EmbeddingState, &model)
	if err != nil {
		return i, err
	}
	i.Scope.TeamID = team.String
	i.Scope.SessionID = session.String
	i.Scope.AgentID = agent.String
	i.Scope.TaskID = task.String
	i.Scope.AttemptID = attempt.String
	i.SupersededBy = super.String
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

const itemColumns = `id,kind,content,content_hash,project_id,team_id,session_id,agent_id,task_id,attempt_id,authority,trust_level,priority,must_keep,pinned,confidence,source_json,evidence_json,tags_json,metadata_json,created_at,updated_at,valid_from,valid_until,expires_at,superseded_by,embedding_state,embedding_model`

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

// scopeWhere builds the shared project/team/session/agent/task scope
// predicate: a query for a given child scope value also matches rows where
// that column is NULL (wider, shared scope), but never matches rows scoped
// to a *different* value. prefix is a table alias prefix (e.g. "c.") for
// queries that join context_items against another table.
func scopeWhere(prefix string, scope Scope, args *[]any) []string {
	*args = append(*args, scope.ProjectID)
	where := []string{prefix + "project_id=?"}
	for _, p := range []struct{ n, v string }{{"team_id", scope.TeamID}, {"session_id", scope.SessionID}, {"agent_id", scope.AgentID}, {"task_id", scope.TaskID}, {"attempt_id", scope.AttemptID}} {
		if p.v != "" {
			where = append(where, "("+prefix+p.n+" IS NULL OR "+prefix+p.n+"=?)")
			*args = append(*args, p.v)
		}
	}
	return where
}

func (r *SQLiteRepository) Query(ctx context.Context, q RepositoryQuery) ([]ContextItem, error) {
	if q.Scope.ProjectID == "" {
		return nil, errors.New("project scope is required")
	}
	var args []any
	where := scopeWhere("", q.Scope, &args)
	if !q.IncludeSuperseded {
		where = append(where, "superseded_by IS NULL")
	}
	if !q.IncludeExpired {
		where = append(where, "(expires_at IS NULL OR expires_at>?)")
		args = append(args, time.Now().UnixMilli())
	}
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

// itemScope fetches an item's scope for event provenance. It returns a zero
// Scope on error (e.g. the item does not exist yet) rather than failing the
// caller's mutation: recording an event with an incomplete scope is better
// than losing the revision bump entirely.
func (r *SQLiteRepository) itemScope(ctx context.Context, tx *sql.Tx, id string) Scope {
	var s Scope
	var team, session, agent, task, attempt sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT project_id, team_id, session_id, agent_id, task_id, attempt_id FROM context_items WHERE id=?", id).Scan(&s.ProjectID, &team, &session, &agent, &task, &attempt); err != nil {
		return Scope{}
	}
	s.TeamID, s.SessionID, s.AgentID, s.TaskID, s.AttemptID = team.String, session.String, agent.String, task.String, attempt.String
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
	where := scopeWhere("", req.Scope, &args)
	where = append(where, "superseded_by IS NULL", "(expires_at IS NULL OR expires_at>?)")
	args = append(args, time.Now().UnixMilli(), limit)
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
	where := append([]string{}, scopeWhere("c.", req.Scope, &args)...)
	where = append(where, "c.superseded_by IS NULL")
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
	rows, e := tx.QueryContext(ctx, "SELECT id, project_id, team_id, session_id, agent_id, task_id, attempt_id FROM context_items WHERE expires_at IS NOT NULL AND expires_at<?", before.UnixMilli())
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
		var team, session, agent, task, attempt sql.NullString
		if e = rows.Scan(&x.id, &x.scope.ProjectID, &team, &session, &agent, &task, &attempt); e != nil {
			rows.Close()
			return 0, e
		}
		x.scope.TeamID, x.scope.SessionID, x.scope.AgentID, x.scope.TaskID, x.scope.AttemptID = team.String, session.String, agent.String, task.String, attempt.String
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
