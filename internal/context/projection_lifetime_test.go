package context

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRebuildProjectionSeparatesSessionAndPersistentKnowledge(t *testing.T) {
	workspace := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	sessionScope := Scope{ProjectID: "project", TeamID: "team", SessionID: "session-1"}
	persistentScope := Scope{ProjectID: "project", TeamID: "team"}
	items := []ContextItem{
		{Kind: ContextDecision, Content: "session-only decision", Scope: sessionScope, Lifecycle: LifecycleConfirmed},
		{Kind: ContextArchitecture, Content: "persistent architecture", Scope: persistentScope, Lifecycle: LifecycleConfirmed},
		{Kind: ContextPattern, Content: "unconfirmed persistent candidate", Scope: persistentScope, Lifecycle: LifecycleCandidate},
		{Kind: ContextSummary, Content: "private worker memory", Scope: Scope{ProjectID: "project", TeamID: "team", SessionID: "session-1", AgentID: "worker"}, Lifecycle: LifecycleConfirmed},
	}
	if err := repo.Append(ctx, items...); err != nil {
		t.Fatal(err)
	}
	if err := repo.RebuildProjection(ctx, sessionScope); err != nil {
		t.Fatalf("RebuildProjection: %v", err)
	}
	stm, err := os.ReadFile(filepath.Join(workspace, "context-stm.md"))
	if err != nil {
		t.Fatal(err)
	}
	ltm, err := os.ReadFile(filepath.Join(workspace, "context-ltm.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stm), "session-only decision") || strings.Contains(string(stm), "persistent architecture") || strings.Contains(string(stm), "private worker memory") {
		t.Fatalf("STM projection mixed lifetimes:\n%s", stm)
	}
	if !strings.Contains(string(ltm), "persistent architecture") || strings.Contains(string(ltm), "session-only decision") || strings.Contains(string(ltm), "unconfirmed persistent candidate") || strings.Contains(string(ltm), "private worker memory") {
		t.Fatalf("LTM projection mixed lifecycle/scope:\n%s", ltm)
	}
}
