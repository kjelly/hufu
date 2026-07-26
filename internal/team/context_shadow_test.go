package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	contextstore "github.com/anomalyco/hufu/internal/context"
)

// failingContextRepo lets tests force a shadow Append to fail (simulating a
// transient store outage) without needing a real SQLite failure.
type failingContextRepo struct {
	*contextstore.SQLiteRepository
	fail bool
}

func (f *failingContextRepo) Append(ctx context.Context, items ...contextstore.ContextItem) error {
	if f.fail {
		return errors.New("simulated context store outage")
	}
	return f.SQLiteRepository.Append(ctx, items...)
}

func TestShadowContextAppendDoesNotNeedLegacyPromptPath(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo,
		projectDir:  "/project",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.shadowContextAppend(contextstore.ContextProgress, "completed migration", "stm_write")
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "/project", TeamID: "team"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Content != "completed migration" || items[0].Source.Ref != "stm_write" {
		t.Fatalf("unexpected shadow item: %#v", items)
	}
	projection, err := os.ReadFile(filepath.Join(workspace, "context-stm.md"))
	if err != nil || !strings.Contains(string(projection), "completed migration") {
		t.Fatalf("canonical projection missing committed item: %v %q", err, projection)
	}
}

func TestCanonicalMemoryIngestionGeneratesLegacySTMProjection(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{contextRepo: repo, projectDir: "/project", session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}}
	if err := c.appendCanonicalContext(context.Background(), contextstore.ContextProgress, "completed canonical migration", "stm_write", nil); err != nil {
		t.Fatal(err)
	}
	stm := LoadSTM(workspace)
	if !strings.Contains(stm, "# \u9032\u5ea6") || !strings.Contains(stm, "- completed canonical migration") {
		t.Fatalf("legacy STM must be generated from canonical context: %q", stm)
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "/project", TeamID: "team"}})
	if err != nil || len(items) != 1 {
		t.Fatalf("canonical STM item = %#v, err=%v", items, err)
	}
}

func TestShadowContextAppendAndLegacyAppendsPreserveAllSTMAndLTMEntries(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo,
		projectDir:  "/project",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	for _, finding := range []string{"finding number A", "finding number B", "finding number C"} {
		c.shadowContextAppend(contextstore.ContextProgress, finding, "stm_write")
		if err := c.updateSTM(func(existing string) string {
			return appendSTMEntry(existing, "- "+finding, stmSectionFindings)
		}); err != nil {
			t.Fatal(err)
		}

		c.shadowContextAppend(contextstore.ContextPattern, finding, "ltm_update")
		existing := LoadLTM(workspace, "team")
		if err := SaveLTM(workspace, "team", appendLTMEntry(existing, "- "+finding, ltmSectionPatterns)); err != nil {
			t.Fatal(err)
		}
	}
	for _, memory := range []struct {
		name    string
		content string
	}{
		{"STM", LoadSTM(workspace)},
		{"LTM", LoadLTM(workspace, "team")},
	} {
		for _, finding := range []string{"finding number A", "finding number B", "finding number C"} {
			if !strings.Contains(memory.content, finding) {
				t.Fatalf("%s lost %q after projection rebuilds: %q", memory.name, finding, memory.content)
			}
		}
		sections := ParseSTMSections(memory.content)
		if len(sections) != 1 || len(sections[0].Entries) != 3 {
			t.Fatalf("%s legacy section parse = %#v, want one section with three entries", memory.name, sections)
		}
	}
}

func TestShadowContextAppendFailureIsRepairable(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	failing := &failingContextRepo{SQLiteRepository: repo, fail: true}
	c := &Coordinator{
		contextRepo: failing,
		projectDir:  "/project",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}

	// The failure must not be fatal to the legacy write path, and it must
	// leave enough information behind (redacted) to replay the item later.
	c.shadowContextAppend(contextstore.ContextProgress, "token=eyJhbGciOiJIUzI1NiJ9 saved during stm_write", "stm_write")

	pendingPath := c.contextPendingPath()
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("expected a pending record after a failed shadow write: %v", err)
	}
	if strings.Contains(string(raw), "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatalf("pending record must not retain the secret in cleartext: %s", raw)
	}
	if strings.TrimSpace(string(raw)) == "" {
		t.Fatal("expected a non-empty pending record")
	}

	// Repairing while the store is still down must not lose the record.
	recovered, remaining, err := c.RepairContextShadowWrites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 0 || remaining != 1 {
		t.Fatalf("recovered=%d remaining=%d while store is down, want 0/1", recovered, remaining)
	}

	// Once the store recovers, repair should drain the queue idempotently.
	failing.fail = false
	recovered, remaining, err = c.RepairContextShadowWrites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 || remaining != 0 {
		t.Fatalf("recovered=%d remaining=%d after recovery, want 1/0", recovered, remaining)
	}

	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "/project", TeamID: "team"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || strings.Contains(items[0].Content, "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatalf("unexpected repaired item: %#v", items)
	}

	// A second repair pass on an already-drained queue must be a no-op, not
	// an error, so operators can run it on a schedule safely.
	recovered, remaining, err = c.RepairContextShadowWrites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 0 || remaining != 0 {
		t.Fatalf("recovered=%d remaining=%d on drained queue, want 0/0", recovered, remaining)
	}
}

func TestAutoExtractLTMCanonicalFirstAndDeduplicates(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := SaveSTM(workspace, stmSectionDecisions+"\n\n- Use canonical SQLite projection\n"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{contextRepo: repo, projectDir: "/project", session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}}
	c.AutoExtractLTM(context.Background())
	scope := contextstore.Scope{ProjectID: "/project", TeamID: "team"}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Content, "canonical SQLite") {
		t.Fatalf("canonical AutoExtractLTM item = %#v", items)
	}
	c.AutoExtractLTM(context.Background())
	items, err = repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("duplicate AutoExtractLTM canonical item: %#v", items)
	}
}

// secretLeakingContextRepo simulates a repository/driver error that echoes
// the rejected value back verbatim, as a real SQLite constraint or
// validation error might.
type secretLeakingContextRepo struct {
	*contextstore.SQLiteRepository
}

func (secretLeakingContextRepo) Append(context.Context, ...contextstore.ContextItem) error {
	return errors.New(`store rejected value containing password=hunter2hunter2`)
}

func TestShadowContextAppendRedactsFailureCauseEverywhere(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	es, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()

	c := &Coordinator{
		contextRepo: secretLeakingContextRepo{repo},
		projectDir:  "/project",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		eventStore:  es,
	}
	c.shadowContextAppend(contextstore.ContextProgress, "unrelated content", "stm_write")

	pendingRaw, err := os.ReadFile(c.contextPendingPath())
	if err != nil {
		t.Fatalf("expected a pending record: %v", err)
	}
	if strings.Contains(string(pendingRaw), "hunter2hunter2") {
		t.Fatalf("pending record leaked the append error's embedded secret: %s", pendingRaw)
	}

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type != "context_shadow_write_error" {
			continue
		}
		found = true
		if strings.Contains(string(ev.Payload), "hunter2hunter2") {
			t.Fatalf("context_shadow_write_error event leaked the append error's embedded secret: %s", ev.Payload)
		}
	}
	if !found {
		t.Fatal("expected a context_shadow_write_error event to be recorded")
	}
}
