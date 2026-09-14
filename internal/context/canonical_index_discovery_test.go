package context

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCanonicalIndexDiscovery is an opt-in, reproducible discovery harness for
// WP-9. It deliberately creates candidate indexes only in a temporary database;
// production migrations must be justified by the checked-in performance note.
func TestCanonicalIndexDiscovery(t *testing.T) {
	if os.Getenv("HUFU_SQLITE_DISCOVERY") != "1" {
		t.Skip("set HUFU_SQLITE_DISCOVERY=1 to run the canonical index review")
	}
	repo := openCanonicalIndexFixture(t, 10_000)
	defer repo.Close()

	queries := canonicalIndexQueries()
	logCanonicalIndexMeasurements(t, repo.db, "before", queries)
	beforeBytes := canonicalDatabaseBytes(t, repo.db)
	beforeWrite := measureCanonicalAppends(t, repo, "before")

	for _, candidate := range canonicalIndexCandidates() {
		if _, err := repo.db.ExecContext(t.Context(), candidate.create); err != nil {
			t.Fatal(err)
		}
		label := "after_" + candidate.name
		logCanonicalIndexMeasurements(t, repo.db, label, queries)
		candidateBytes := canonicalDatabaseBytes(t, repo.db)
		candidateWrite := measureCanonicalAppends(t, repo, label)
		t.Logf("candidate=%s storage_before_bytes=%d storage_after_bytes=%d delta_pct=%.2f", candidate.name, beforeBytes, candidateBytes, percentageDelta(beforeBytes, candidateBytes))
		t.Logf("candidate=%s append_before_median=%s append_after_median=%s delta_pct=%.2f", candidate.name, beforeWrite, candidateWrite, percentageDelta(int64(beforeWrite), int64(candidateWrite)))
		if _, err := repo.db.ExecContext(t.Context(), candidate.drop); err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkCanonicalIndexDiscovery(b *testing.B) {
	repo := openCanonicalIndexFixture(b, 10_000)
	defer repo.Close()
	if selected := os.Getenv("HUFU_SQLITE_INDEX_CANDIDATE"); selected != "" {
		found := false
		for _, candidate := range canonicalIndexCandidates() {
			if candidate.name != selected {
				continue
			}
			if _, err := repo.db.ExecContext(b.Context(), candidate.create); err != nil {
				b.Fatal(err)
			}
			found = true
			break
		}
		if !found {
			b.Fatalf("unknown candidate %q", selected)
		}
	}
	for _, query := range canonicalIndexQueries() {
		b.Run(query.name, func(b *testing.B) {
			for b.Loop() {
				rows, err := repo.db.QueryContext(b.Context(), query.sql, query.args...)
				if err != nil {
					b.Fatal(err)
				}
				for rows.Next() {
					var ignored any
					if err = rows.Scan(&ignored); err != nil {
						b.Fatal(err)
					}
				}
				if err = rows.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type canonicalIndexCandidate struct {
	name, create, drop string
}

func canonicalIndexCandidates() []canonicalIndexCandidate {
	return []canonicalIndexCandidate{
		{name: "context_dedupe", create: `CREATE INDEX candidate_context_dedupe ON context_items(project_id,kind,content_hash,COALESCE(team_id,''),COALESCE(session_id,''),COALESCE(branch_id,''),COALESCE(agent_id,''),COALESCE(task_id,''),COALESCE(attempt_id,''))`, drop: `DROP INDEX candidate_context_dedupe`},
		{name: "context_origin_run", create: `CREATE INDEX candidate_context_origin_run ON context_items(project_id,json_extract(metadata_json,'$.run_id'),lifecycle,priority DESC,created_at DESC,id)`, drop: `DROP INDEX candidate_context_origin_run`},
		{name: "experience_policy", create: `CREATE INDEX candidate_experience_policy ON experience_aggregates(policy_version,context_item_id)`, drop: `DROP INDEX candidate_experience_policy`},
		{name: "promotion_order", create: `CREATE INDEX candidate_promotion_order ON promotion_proposals(project_id,team_id,created_at DESC,id)`, drop: `DROP INDEX candidate_promotion_order`},
		{name: "memory_active", create: `CREATE INDEX candidate_memory_active ON memory_policy_versions(status,adopted_at DESC)`, drop: `DROP INDEX candidate_memory_active`},
	}
}

type canonicalIndexQuery struct {
	name string
	sql  string
	args []any
}

func canonicalIndexQueries() []canonicalIndexQuery {
	return []canonicalIndexQuery{
		{name: "canonical_query", sql: `SELECT id FROM context_items WHERE project_id=? AND lifecycle=? ORDER BY priority DESC,created_at DESC,id LIMIT 20`, args: []any{"project", LifecycleConfirmed}},
		{name: "fts_search", sql: `SELECT id FROM context_items_fts WHERE context_items_fts MATCH ? LIMIT 20`, args: []any{"needle"}},
		{name: "candidate_duplicate", sql: `SELECT id FROM context_items WHERE project_id=? AND kind=? AND content_hash=? AND COALESCE(team_id,'')=? AND COALESCE(session_id,'')=? AND COALESCE(branch_id,'')=? AND COALESCE(agent_id,'')=? AND COALESCE(task_id,'')=? AND COALESCE(attempt_id,'')=? LIMIT 1`, args: []any{"project", ContextObservation, "hash-5000", "team", "session", "branch", "worker", "task-0", "attempt"}},
		{name: "append_dedupe", sql: `SELECT id FROM context_items WHERE project_id=? AND kind=? AND content_hash=? AND COALESCE(team_id,'')=? AND COALESCE(session_id,'')=? AND COALESCE(branch_id,'')=? AND COALESCE(agent_id,'')=? AND COALESCE(task_id,'')=? AND COALESCE(attempt_id,'')=? LIMIT 1`, args: []any{"project", ContextObservation, "hash-5000", "team", "session", "branch", "worker", "task-0", "attempt"}},
		{name: "reducer_dedupe", sql: `SELECT id FROM context_items WHERE project_id=? AND kind=? AND content_hash=? AND COALESCE(team_id,'')=? AND COALESCE(session_id,'')=? AND COALESCE(branch_id,'')=? AND COALESCE(agent_id,'')=? AND COALESCE(task_id,'')=? AND COALESCE(attempt_id,'')=? AND json_extract(metadata_json,'$.run_id')=? AND json_extract(metadata_json,'$.task_id')=? AND json_extract(metadata_json,'$.attempt')=? LIMIT 1`, args: []any{"project", ContextObservation, "hash-5000", "team", "session", "branch", "worker", "task-0", "attempt", "run-0", "task-0", "1"}},
		{name: "origin_run", sql: `SELECT id FROM context_items WHERE project_id=? AND json_extract(metadata_json,'$.run_id')=? AND lifecycle=? ORDER BY priority DESC,created_at DESC,id`, args: []any{"project", "run-0", LifecycleCandidate}},
		{name: "context_outcome", sql: `SELECT COUNT(*) FROM context_outcome_observations WHERE context_item_id=? AND phase=? AND trigger=? AND agent_role=? AND environment=? AND outcome=?`, args: []any{"ctx-005000", "execute", "manual", "worker", "test", "positive"}},
		{name: "experience_aggregate", sql: `SELECT context_item_id FROM experience_aggregates WHERE policy_version=? ORDER BY context_item_id`, args: []any{"policy-1"}},
		{name: "promotion_list", sql: `SELECT id FROM promotion_proposals WHERE project_id=? AND team_id=? ORDER BY created_at DESC,id`, args: []any{"project", "team"}},
		{name: "promotion_status", sql: `SELECT id FROM promotion_proposals WHERE project_id=? AND team_id=? AND status=? ORDER BY created_at DESC,id`, args: []any{"project", "team", PromotionStatusProposed}},
		{name: "memory_policy", sql: `SELECT policy_version FROM memory_policy_versions WHERE status='active' ORDER BY adopted_at DESC LIMIT 1`},
	}
}

func openCanonicalIndexFixture(t testing.TB, cardinality int) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(t.TempDir() + "/context.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := repo.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	itemStatement := `INSERT INTO context_items(id,kind,content,content_hash,project_id,team_id,session_id,branch_id,agent_id,task_id,attempt_id,authority,trust_level,priority,source_json,evidence_json,tags_json,metadata_json,created_at,updated_at,lifecycle,embedding_state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	for index := range cardinality {
		id := fmt.Sprintf("ctx-%06d", index)
		runID := fmt.Sprintf("run-%d", index%100)
		taskID := fmt.Sprintf("task-%d", index%10)
		lifecycle := LifecycleConfirmed
		if index%100 == 0 {
			lifecycle = LifecycleCandidate
		}
		metadata := fmt.Sprintf(`{"run_id":%q,"task_id":%q,"attempt":"1"}`, runID, taskID)
		if _, err = tx.ExecContext(t.Context(), itemStatement, id, ContextObservation, "needle content "+id, fmt.Sprintf("hash-%d", index), "project", "team", "session", "branch", "worker", taskID, "attempt", AuthorityTool, TrustInternal, index%10, "{}", "[]", "[]", metadata, int64(index), int64(index), lifecycle, "pending"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(t.Context(), `INSERT INTO context_items_fts(id,content,kind,tags) VALUES(?,?,?,?)`, id, "needle content "+id, ContextObservation, ""); err != nil {
			t.Fatal(err)
		}
		if index%10 == 0 {
			if _, err = tx.ExecContext(t.Context(), `INSERT INTO context_outcome_observations(idempotency_key,context_item_id,phase,trigger,agent_role,environment,outcome,policy_revision,observed_at) VALUES(?,?,?,?,?,?,?,?,?)`, "outcome-"+id, id, "execute", "manual", "worker", "test", "positive", "v1", index); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(t.Context(), `INSERT INTO experience_aggregates(context_item_id,policy_version) VALUES(?,?)`, id, "policy-1"); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(t.Context(), `INSERT INTO promotion_proposals(id,project_id,team_id,type,target_path,draft,draft_hash,policy_version,status,metrics_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, "promo-"+id, "project", "team", PromotionTypeSkill, "skills/test/SKILL.md", "draft", "hash", "policy-1", PromotionStatusProposed, "{}", index, index); err != nil {
				t.Fatal(err)
			}
		}
	}
	for index := range 1_000 {
		status := "retired"
		if index == 999 {
			status = "active"
		}
		if _, err = tx.ExecContext(t.Context(), `INSERT INTO memory_policy_versions(policy_version,snapshot_json,revision_hash,status,created_at,adopted_at) VALUES(?,?,?,?,?,?)`, fmt.Sprintf("policy-%04d", index), "{}", fmt.Sprintf("hash-%d", index), status, index, index); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return repo
}

func logCanonicalIndexMeasurements(t *testing.T, db *sql.DB, label string, queries []canonicalIndexQuery) {
	t.Helper()
	for _, query := range queries {
		plan := explainCanonicalQuery(t, db, query)
		samples := make([]time.Duration, 5)
		for index := range samples {
			started := time.Now()
			rows, err := db.QueryContext(t.Context(), query.sql, query.args...)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var ignored any
				if err = rows.Scan(&ignored); err != nil {
					t.Fatal(err)
				}
			}
			if err = rows.Close(); err != nil {
				t.Fatal(err)
			}
			samples[index] = time.Since(started)
		}
		t.Logf("%s query=%s median=%s plan=%q", label, query.name, medianDuration(samples), plan)
	}
}

func explainCanonicalQuery(t *testing.T, db *sql.DB, query canonicalIndexQuery) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.sql, query.args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	return strings.Join(details, "; ")
}

func measureCanonicalAppends(t *testing.T, repo *SQLiteRepository, label string) time.Duration {
	t.Helper()
	samples := make([]time.Duration, 5)
	for sample := range samples {
		started := time.Now()
		tx, err := repo.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for index := range 20 {
			id := fmt.Sprintf("write-%s-%d-%d", label, sample, index)
			content := "representative append " + id
			_, err = tx.ExecContext(t.Context(), `INSERT INTO context_items(id,kind,content,content_hash,project_id,authority,trust_level,priority,source_json,evidence_json,tags_json,metadata_json,created_at,updated_at,lifecycle,embedding_state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, ContextObservation, content, id, "write-project", AuthorityTool, TrustInternal, 0, "{}", "[]", "[]", "{}", index, index, LifecycleConfirmed, "pending")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(t.Context(), `INSERT INTO context_items_fts(id,content,kind,tags) VALUES(?,?,?,?)`, id, content, ContextObservation, ""); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(t.Context(), `INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES('append',?,'{}','{}',?)`, id, index); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		samples[sample] = time.Since(started)
	}
	return medianDuration(samples)
}

func canonicalDatabaseBytes(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	var pages, freePages, pageSize int64
	if err := db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "PRAGMA freelist_count").Scan(&freePages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	return (pages - freePages) * pageSize
}

func medianDuration(samples []time.Duration) time.Duration {
	for left := 1; left < len(samples); left++ {
		for right := left; right > 0 && samples[right] < samples[right-1]; right-- {
			samples[right], samples[right-1] = samples[right-1], samples[right]
		}
	}
	return samples[len(samples)/2]
}

func percentageDelta(before, after int64) float64 {
	if before == 0 {
		return 0
	}
	return float64(after-before) * 100 / float64(before)
}
