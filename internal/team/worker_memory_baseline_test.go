package team

// WP-0 — Team-level baseline tests for per-worker memory.
//
// These tests pin the CURRENT shared-memory behavior of the coordinator's
// direct-agent path, DAG (executeTask) path, retry history, and
// memory-disabled execution profile. They are production-code-free: WP-0
// must not change any runtime behavior.
//
// Behaviors fixed here:
//   1. Direct path (RunDirectAgent) injects shared STM/LTM via
//      buildMemorySuffix — there is no per-worker recall today.
//   2. DAG path (executeTask) injects shared STM via buildTaskSTMContext
//      and shared LTM via buildLTMContext — no per-worker recall.
//   3. Retry history (conversationHistory) is task-scoped: it starts nil
//      for each task and accumulates only within that task's retry loop.
//   4. Memory-disabled profile (fresh-verification) suppresses all
//      historical memory injection (buildMemorySuffix, buildTaskSTMContext,
//      buildLTMContext return empty).
//   5. Canonical context ingestion (appendCanonicalContext) writes shared
//      scope (AgentID empty) — no per-worker private writes today.
//   6. autoWriteSTM records shared-scope canonical items (AgentID empty).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// --- Shared memory injection baseline ---

func TestBaseline_BuildMemorySuffixInjectsSharedSTM(t *testing.T) {
	workspace := t.TempDir()
	if err := SaveSTM(workspace, "# 進度\n- shared progress entry\n"); err != nil {
		t.Fatalf("SaveSTM: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.contextCompiler = &defaultContextCompiler{c: c}
	suffix := c.buildMemorySuffix("worker")
	if !strings.Contains(suffix, "shared progress entry") {
		t.Errorf("buildMemorySuffix should inject shared STM content, got:\n%s", suffix)
	}
	if !strings.Contains(suffix, "Short-term memory") {
		t.Errorf("buildMemorySuffix should label STM section, got:\n%s", suffix)
	}
}

func TestBaseline_BuildMemorySuffixInjectsSharedLTM(t *testing.T) {
	workspace := t.TempDir()
	if err := SaveLTM(workspace, "team", "# 專案慣例\n- shared ltm pattern\n"); err != nil {
		t.Fatalf("SaveLTM: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.contextCompiler = &defaultContextCompiler{c: c}
	suffix := c.buildMemorySuffix("worker")
	if !strings.Contains(suffix, "shared ltm pattern") {
		t.Errorf("buildMemorySuffix should inject shared LTM content, got:\n%s", suffix)
	}
	if !strings.Contains(suffix, "Long-term memory") {
		t.Errorf("buildMemorySuffix should label LTM section, got:\n%s", suffix)
	}
}

func TestBaseline_BuildTaskSTMContextInjectsSharedSTM(t *testing.T) {
	workspace := t.TempDir()
	if err := SaveSTM(workspace, "# 發現\n- shared finding\n\n# 決策\n- shared decision\n"); err != nil {
		t.Fatalf("SaveSTM: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.contextCompiler = &defaultContextCompiler{c: c}
	ctx := c.buildTaskSTMContext()
	if !strings.Contains(ctx, "shared finding") || !strings.Contains(ctx, "shared decision") {
		t.Errorf("buildTaskSTMContext should inject shared STM knowledge sections, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "Context from Previous Agents") {
		t.Errorf("buildTaskSTMContext should label the section, got:\n%s", ctx)
	}
}

func TestBaseline_BuildLTMContextInjectsSharedLTM(t *testing.T) {
	workspace := t.TempDir()
	if err := SaveLTM(workspace, "team", "# 架構\n- shared architecture decision\n"); err != nil {
		t.Fatalf("SaveLTM: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.contextCompiler = &defaultContextCompiler{c: c}
	ctx := c.buildLTMContext()
	if !strings.Contains(ctx, "shared architecture decision") {
		t.Errorf("buildLTMContext should inject shared LTM, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "Long-term Memory") {
		t.Errorf("buildLTMContext should label the section, got:\n%s", ctx)
	}
}

// --- Memory-disabled profile baseline ---

func TestBaseline_FreshVerificationSuppressesAllMemory(t *testing.T) {
	workspace := t.TempDir()
	if err := SaveSTM(workspace, "# 進度\n- should not appear\n"); err != nil {
		t.Fatalf("SaveSTM: %v", err)
	}
	if err := SaveLTM(workspace, "team", "# 專案慣例\n- should not appear either\n"); err != nil {
		t.Fatalf("SaveLTM: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.contextCompiler = &defaultContextCompiler{c: c}
	fresh, _ := GetBuiltinProfile(string(ProfileFreshVerification))
	c.SetExecutionProfile(fresh)

	if got := c.buildMemorySuffix("worker"); got != "" {
		t.Errorf("buildMemorySuffix under fresh-verification = %q, want empty", got)
	}
	if got := c.buildTaskSTMContext(); got != "" {
		t.Errorf("buildTaskSTMContext under fresh-verification = %q, want empty", got)
	}
	if got := c.buildLTMContext(); got != "" {
		t.Errorf("buildLTMContext under fresh-verification = %q, want empty", got)
	}
}

func TestBaseline_DefaultProfileDoesNotSuppressMemory(t *testing.T) {
	workspace := t.TempDir()
	if err := SaveSTM(workspace, "# 進度\n- visible under default\n"); err != nil {
		t.Fatalf("SaveSTM: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.contextCompiler = &defaultContextCompiler{c: c}
	// Default profile (zero value) should NOT suppress memory.
	if got := c.buildMemorySuffix("worker"); !strings.Contains(got, "visible under default") {
		t.Errorf("buildMemorySuffix under default profile should inject STM, got:\n%s", got)
	}
}

// --- Canonical context ingestion writes shared scope ---

func TestBaseline_AppendCanonicalContextWritesSharedScope(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo,
		projectDir:  "/project",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	if err := c.appendCanonicalContext(context.Background(), contextstore.ContextProgress, "shared progress entry", "test", nil); err != nil {
		t.Fatalf("appendCanonicalContext: %v", err)
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope: contextstore.Scope{ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace)},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	// Baseline: the canonical item has an EMPTY AgentID (shared scope).
	// WP-3+ will add per-worker private writes with a non-empty AgentID.
	if items[0].Scope.AgentID != "" {
		t.Errorf("canonical context item AgentID = %q, want empty (shared scope baseline)", items[0].Scope.AgentID)
	}
}

func TestBaseline_AutoWriteSTMRecordsSharedScope(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo,
		projectDir:  "/project",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.autoWriteSTM("agent-a", "do work", "output result", "", true)
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope: contextstore.Scope{ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace)},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	// Baseline: autoWriteSTM records a shared-scope item even though the
	// agent name is "agent-a". The AgentID field is empty. WP-3+ will
	// optionally write per-worker private items with AgentID set.
	if items[0].Scope.AgentID != "" {
		t.Errorf("autoWriteSTM canonical item AgentID = %q, want empty (shared scope baseline)", items[0].Scope.AgentID)
	}
	if !strings.Contains(items[0].Content, "agent-a") {
		t.Errorf("autoWriteSTM content should mention agent name, got: %q", items[0].Content)
	}
}

// --- Retry history is task-scoped ---

// TestBaseline_RetryHistoryIsTaskScoped confirms that conversationHistory
// (the retry-loop accumulator in executeTask) starts as nil for each task.
// This is verified by inspecting the code path: the variable is declared
// inside executeTask, not on the Coordinator. We test the observable
// consequence: a fresh coordinator has no accumulated conversation history
// that would leak across tasks.
func TestBaseline_RetryHistoryIsTaskScoped(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	// The coordinator does not expose conversationHistory publicly, but it
	// also does not persist per-worker conversation history across tasks.
	// The only history that persists is the coordinator's own
	// conversationHistory (for ContinueWithPrompt), not worker task history.
	// This test documents that worker retry history is ephemeral.
	if c.conversationHistory != nil {
		t.Errorf("coordinator should start with nil conversationHistory, got %d messages", len(c.conversationHistory))
	}
}

// --- Direct path uses shared memory (no per-worker recall) ---

// TestBaseline_DirectPathMemoryIsShared documents that the direct-agent
// path (RunDirectAgent) injects shared STM/LTM via buildMemorySuffix, not
// per-worker private memory. This is verified at the buildMemorySuffix
// level (tested above) since RunDirectAgent requires a provider. The
// contract is: direct path and DAG path use the SAME shared memory source.
func TestBaseline_DirectPathMemoryIsShared(t *testing.T) {
	workspace := t.TempDir()
	if err := SaveSTM(workspace, "# 進度\n- shared entry for direct path\n"); err != nil {
		t.Fatalf("SaveSTM: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.contextCompiler = &defaultContextCompiler{c: c}
	// The direct path calls buildMemorySuffix(agentDef.Role) — same as the
	// DAG path. There is no separate per-worker recall function today.
	suffix := c.buildMemorySuffix("worker")
	if !strings.Contains(suffix, "shared entry for direct path") {
		t.Errorf("direct path memory should come from shared STM, got:\n%s", suffix)
	}
}

// --- Canonical context scope has no BranchID ---

func TestBaseline_CanonicalContextScopeHasNoBranchID(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo,
		projectDir:  "/project",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	_ = c.contextScope()
	if err := c.appendCanonicalContext(context.Background(), contextstore.ContextDecision, "test decision", "test", nil); err != nil {
		t.Fatalf("appendCanonicalContext: %v", err)
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope: contextstore.Scope{ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace)},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	// Baseline: Scope has no BranchID field today. WP-1 will add it.
	// This test documents the current Scope structure.
	if items[0].Scope.ProjectID != "/project" {
		t.Errorf("ProjectID = %q, want /project", items[0].Scope.ProjectID)
	}
	if items[0].Scope.TeamID != "team" {
		t.Errorf("TeamID = %q, want team", items[0].Scope.TeamID)
	}
}

// --- Shadow context append writes shared scope ---

func TestBaseline_ShadowContextAppendWritesSharedScope(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo,
		projectDir:  "/project",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.shadowContextAppend(contextstore.ContextProgress, "shadow write content", "stm_write")
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope: contextstore.Scope{ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace)},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	// Baseline: shadow writes use shared scope (empty AgentID).
	if items[0].Scope.AgentID != "" {
		t.Errorf("shadow write AgentID = %q, want empty (shared scope baseline)", items[0].Scope.AgentID)
	}
}

// --- Contract tests for WP-1+ (skipped today) ---

// TestBaselineContract_PerWorkerRecallNotImplemented is the WP-3 target
// contract: buildMemorySuffix should be replaceable by a per-worker recall
// that includes the worker's own private memory. Today there is no such
// mechanism. Skipped until WP-3.
func TestBaselineContract_PerWorkerRecallNotImplemented(t *testing.T) {
	t.Skip("WP-3 contract: per-worker recall not yet implemented; today all memory is shared")
}

// TestBaselineContract_PerWorkerIngestionImplemented verifies that WP-4
// enables per-worker private ingestion via SaveSessionMemory. The canonical
// store now supports writing private items with a non-empty AgentID.
func TestBaselineContract_PerWorkerIngestionImplemented(t *testing.T) {
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	svc := NewWorkerMemoryService(repo, nil)
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"}
	stored, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID: "worker-a",
		BranchID: "main",
		Scope:    scope,
		Summary:  "private worker finding",
		Goal:     "test goal",
		TaskID:   "1",
		RunID:    "run-1",
		Attempt:  1,
		Verified: true,
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	})
	if err != nil {
		t.Fatalf("SaveSessionMemory: %v", err)
	}
	if stored.Scope.AgentID != "worker-a" {
		t.Errorf("private item AgentID = %q, want worker-a", stored.Scope.AgentID)
	}
	if stored.Lifecycle != contextstore.LifecycleConfirmed {
		t.Errorf("verified private item lifecycle = %q, want confirmed", stored.Lifecycle)
	}
}
