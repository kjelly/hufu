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
