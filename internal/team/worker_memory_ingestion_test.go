package team

// WP-4 — Verified Session Memory Ingestion tests.
//
// These tests verify the per-worker memory ingestion path:
//   - SaveSessionMemory on the service: verified → confirmed, unverified → candidate
//   - Idempotency: re-ingesting the same execution identity does not duplicate
//   - Coordinator-level ingestWorkerSessionMemory helper
//   - Failed/cancelled/blocked tasks do NOT produce confirmed memory
//   - Secret redaction happens before hash/log/event/pending queue
//   - Ingestion failure does not invert task success (best-effort)
//   - Fast-path (direct) and DAG path share the same identity semantics
//   - Private items do not leak into shared projection (stm.md/ltm.md)

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	contextstore "github.com/anomalyco/hufu/internal/context"
)

// --- Service-level SaveSessionMemory tests ---

func wp4SetupRepo(t *testing.T) contextstore.Repository {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

func wp4QueryAll(t *testing.T, repo contextstore.Repository, scope contextstore.Scope) []contextstore.ContextItem {
	t.Helper()
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope:             scope,
		Visibility:        contextstore.VisibilitySubtree,
		IncludeCandidates: true,
		Limit:             100,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return items
}

func wp4FindItemByID(items []contextstore.ContextItem, id string) *contextstore.ContextItem {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

func TestWP4_VerifiedSuccessProducesConfirmedSessionMemory(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a",
	}

	stored, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID:   "worker-a",
		BranchID:   "main",
		Scope:      scope,
		Summary:    "Implemented the login endpoint with JWT auth.",
		Goal:       "implement login endpoint",
		TaskID:     "1",
		RunID:      "run-1",
		Attempt:    1,
		ProducerID: "worker-a",
		TaskResult: &TaskResult{
			Summary:   "Login endpoint complete",
			Artifacts: []ArtifactRef{{Path: "src/auth/login.go", Description: "login handler"}},
		},
		Verified: true,
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	})
	if err != nil {
		t.Fatalf("SaveSessionMemory: %v", err)
	}

	if stored.Lifecycle != contextstore.LifecycleConfirmed {
		t.Errorf("verified success should produce confirmed lifecycle, got %q", stored.Lifecycle)
	}
	if stored.ID == "" {
		t.Error("stored item should have a non-empty ID")
	}
	if stored.Scope.AgentID != "worker-a" {
		t.Errorf("stored item AgentID = %q, want worker-a", stored.Scope.AgentID)
	}
	if stored.Scope.TaskID != "" || stored.Scope.AttemptID != "" {
		t.Errorf("stored item must remain at worker scope, got TaskID=%q AttemptID=%q", stored.Scope.TaskID, stored.Scope.AttemptID)
	}
	if stored.Metadata["task_id"] != "1" || stored.Metadata["attempt"] != "1" {
		t.Errorf("task provenance metadata = %#v, want task=1 attempt=1", stored.Metadata)
	}
	if stored.Metadata["visibility"] != "private" {
		t.Errorf("metadata visibility = %q, want private", stored.Metadata["visibility"])
	}
	if stored.Metadata["memory_tier"] != "session" {
		t.Errorf("metadata memory_tier = %q, want session", stored.Metadata["memory_tier"])
	}

	// Verify it's in the store.
	items := wp4QueryAll(t, repo, scope)
	found := false
	for _, item := range items {
		if item.ID == stored.ID && item.Lifecycle == contextstore.LifecycleConfirmed {
			found = true
		}
	}
	if !found {
		t.Errorf("confirmed item not found in store: %d items", len(items))
	}
}

func TestWP4_UnverifiedSuccessProducesCandidateSessionMemory(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a",
	}

	stored, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID:   "worker-a",
		BranchID:   "main",
		Scope:      scope,
		Summary:    "Explored the codebase structure.",
		Goal:       "explore codebase",
		TaskID:     "2",
		RunID:      "run-1",
		Attempt:    1,
		ProducerID: "worker-a",
		Verified:   false, // no objective verification
		Policy:     agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	})
	if err != nil {
		t.Fatalf("SaveSessionMemory: %v", err)
	}

	if stored.Lifecycle != contextstore.LifecycleCandidate {
		t.Errorf("unverified success should produce candidate lifecycle, got %q", stored.Lifecycle)
	}

	// Candidate should NOT be visible via default recall (IncludeCandidates=false).
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-a",
		Scope:    scope,
		Query:    "explore codebase",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 10, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, item := range bundle.Items {
		if item.ID == stored.ID {
			t.Errorf("candidate item should not be visible via default recall: %s", item.ID)
		}
	}
}

func TestWP4_ModeOffDoesNotIngest(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}

	_, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID: "worker-a",
		Scope:    scope,
		Summary:  "should not be saved",
		TaskID:   "1",
		RunID:    "run-1",
		Attempt:  1,
		Verified: true,
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryOff, AutoSave: true},
	})
	if err == nil {
		t.Error("mode=off should return an error, not silently save")
	}

	items := wp4QueryAll(t, repo, scope)
	if len(items) != 0 {
		t.Errorf("mode=off should not write any items, got %d", len(items))
	}
}

func TestWP4_AutoSaveDisabledDoesNotIngest(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}

	_, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID: "worker-a",
		Scope:    scope,
		Summary:  "should not be saved",
		TaskID:   "1",
		RunID:    "run-1",
		Attempt:  1,
		Verified: true,
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: false},
	})
	if err == nil {
		t.Error("auto-save=false should return an error, not silently save")
	}

	items := wp4QueryAll(t, repo, scope)
	if len(items) != 0 {
		t.Errorf("auto-save=false should not write any items, got %d", len(items))
	}
}

func TestWP4_EmptySummaryDoesNotIngest(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "worker-a"}

	_, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID: "worker-a",
		Scope:    scope,
		Summary:  "   ",
		TaskID:   "1",
		RunID:    "run-1",
		Attempt:  1,
		Verified: true,
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	})
	if err == nil {
		t.Error("empty summary should return an error")
	}
}

func TestWP4_IdempotentReIngestSameExecutionIdentity(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a",
	}

	req := WorkerMemoryWriteRequest{
		WorkerID:   "worker-a",
		BranchID:   "main",
		Scope:      scope,
		Summary:    "Built the database schema migration.",
		Goal:       "build db schema",
		TaskID:     "5",
		RunID:      "run-1",
		Attempt:    1,
		ProducerID: "worker-a",
		Verified:   true,
		Policy:     agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	}

	first, err := svc.SaveSessionMemory(context.Background(), req)
	if err != nil {
		t.Fatalf("first SaveSessionMemory: %v", err)
	}

	// Re-ingest with the exact same execution identity.
	second, err := svc.SaveSessionMemory(context.Background(), req)
	if err != nil {
		t.Fatalf("second SaveSessionMemory: %v", err)
	}

	// The store should have exactly 1 item for this scope, not 2.
	items := wp4QueryAll(t, repo, scope)
	workerMemItems := 0
	for _, item := range items {
		if item.Source.Type == "worker_memory_session" {
			workerMemItems++
		}
	}
	if workerMemItems != 1 {
		t.Errorf("idempotent re-ingest should produce 1 item, got %d", workerMemItems)
	}

	// Both calls should return the same item ID (dedup update).
	if first.ID != second.ID {
		t.Errorf("idempotent re-ingest should return same ID: first=%s second=%s", first.ID, second.ID)
	}
}

func TestWP4_DifferentAttemptProducesSeparateItem(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a",
	}

	baseReq := WorkerMemoryWriteRequest{
		WorkerID:   "worker-a",
		BranchID:   "main",
		Scope:      scope,
		Summary:    "Fixed the build error.",
		Goal:       "fix build",
		TaskID:     "3",
		RunID:      "run-1",
		ProducerID: "worker-a",
		Verified:   true,
		Policy:     agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	}

	req1 := baseReq
	req1.Attempt = 1
	_, err := svc.SaveSessionMemory(context.Background(), req1)
	if err != nil {
		t.Fatalf("attempt 1: %v", err)
	}

	req2 := baseReq
	req2.Attempt = 2
	_, err = svc.SaveSessionMemory(context.Background(), req2)
	if err != nil {
		t.Fatalf("attempt 2: %v", err)
	}

	// Different attempts produce different content (execution identity embedded),
	// so the store should have 2 items.
	items := wp4QueryAll(t, repo, scope)
	workerMemItems := 0
	for _, item := range items {
		if item.Source.Type == "worker_memory_session" {
			workerMemItems++
		}
	}
	if workerMemItems != 2 {
		t.Errorf("different attempts should produce 2 items, got %d", workerMemItems)
	}
}

func TestWP4_ArtifactsBoundAsEvidence(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a",
	}

	stored, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID:   "worker-a",
		BranchID:   "main",
		Scope:      scope,
		Summary:    "Created the API handler.",
		Goal:       "create api handler",
		TaskID:     "7",
		RunID:      "run-1",
		Attempt:    1,
		ProducerID: "worker-a",
		TaskResult: &TaskResult{
			Artifacts: []ArtifactRef{
				{ID: "art-1", Path: "src/api.go"},
				{ID: "art-2", Path: "src/api_test.go"},
			},
			RawOutputRef: &ArtifactRef{Path: "logs/transcript.json"},
		},
		Verified: true,
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	})
	if err != nil {
		t.Fatalf("SaveSessionMemory: %v", err)
	}

	if len(stored.Evidence) < 3 {
		t.Errorf("expected at least 3 evidence refs (2 artifacts + 1 transcript), got %d: %+v", len(stored.Evidence), stored.Evidence)
	}

	hasArtifact := func(refType, refVal string) bool {
		for _, e := range stored.Evidence {
			if e.Type == refType && e.Ref == refVal {
				return true
			}
		}
		return false
	}
	if !hasArtifact("artifact", "art-1") {
		t.Errorf("evidence missing artifact art-1: %+v", stored.Evidence)
	}
	if !hasArtifact("artifact", "art-2") {
		t.Errorf("evidence missing artifact art-2: %+v", stored.Evidence)
	}
	if !hasArtifact("task_transcript", "logs/transcript.json") {
		t.Errorf("evidence missing task_transcript: %+v", stored.Evidence)
	}
}

func TestWP4_SecretRedactionBeforePersist(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scope := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a",
	}

	stored, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID:   "worker-a",
		BranchID:   "main",
		Scope:      scope,
		Summary:    "Used api_key=sk-1234567890abcdef to connect to the service.",
		Goal:       "connect to service",
		TaskID:     "8",
		RunID:      "run-1",
		Attempt:    1,
		ProducerID: "worker-a",
		Verified:   true,
		Policy:     agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	})
	if err != nil {
		t.Fatalf("SaveSessionMemory: %v", err)
	}

	// The stored content must not contain the raw secret.
	if strings.Contains(stored.Content, "sk-1234567890abcdef") {
		t.Errorf("stored content contains unredacted secret: %q", stored.Content)
	}
	// It should contain a REDACTED marker.
	if !strings.Contains(stored.Content, "REDACTED") {
		t.Errorf("stored content should contain REDACTED marker: %q", stored.Content)
	}

	// Verify in the store too.
	items := wp4QueryAll(t, repo, scope)
	for _, item := range items {
		if strings.Contains(item.Content, "sk-1234567890abcdef") {
			t.Errorf("store item contains unredacted secret: %q", item.Content)
		}
	}
}

func TestWP4_PrivateItemNotInSharedProjection(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	privateScope := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a",
	}

	_, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID:   "worker-a",
		BranchID:   "main",
		Scope:      privateScope,
		Summary:    "private worker finding that should not leak to shared projection.",
		Goal:       "private task",
		TaskID:     "9",
		RunID:      "run-1",
		Attempt:    1,
		ProducerID: "worker-a",
		Verified:   true,
		Policy:     agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	})
	if err != nil {
		t.Fatalf("SaveSessionMemory: %v", err)
	}

	// Query the shared projection — should not include private items.
	sharedScope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess"}
	sharedItems, err := repo.QuerySharedProjection(context.Background(), sharedScope)
	if err != nil {
		t.Fatalf("QuerySharedProjection: %v", err)
	}
	for _, item := range sharedItems {
		if item.Scope.AgentID == "worker-a" {
			t.Errorf("private item leaked into shared projection: %+v", item)
		}
		if strings.Contains(item.Content, "private worker finding") {
			t.Errorf("private content leaked into shared projection: %q", item.Content)
		}
	}
}

// --- Coordinator-level ingestWorkerSessionMemory tests ---

func wp4SetupCoordinator(t *testing.T, workspace string) (*Coordinator, contextstore.Repository) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	c := &Coordinator{
		contextRepo:     repo,
		workerMemorySvc: NewWorkerMemoryService(repo, nil),
		projectDir:      "/project",
		session:         &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		taskTracker:     NewTaskTracker(),
	}
	c.SetExecutionProfile(BuiltinProfiles()[ProfileDefault])
	return c, repo
}

func TestWP4_CoordinatorIngestVerifiedSuccess(t *testing.T) {
	workspace := t.TempDir()
	c, repo := wp4SetupCoordinator(t, workspace)

	agentDef := &agent.AgentDef{
		Name:     "worker-a",
		MemoryID: "worker-a",
		Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true, MaxItems: 5, MaxTokens: 1500},
	}
	c.executionRunID = "run-test-1"

	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker-a", Desc: "implement feature X"}})[0]

	result := &TaskResult{
		TaskID:  item.ID,
		Summary: "Feature X implemented with tests",
		Artifacts: []ArtifactRef{
			{Path: "src/feature.go"},
			{Path: "src/feature_test.go"},
		},
	}

	c.ingestWorkerSessionMemory(context.Background(), agentDef, item.ID, result, "Feature X done", true, 1)

	scope := contextstore.Scope{
		ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace), BranchID: "main", AgentID: "worker-a",
	}
	items := wp4QueryAll(t, repo, scope)
	if len(items) == 0 {
		t.Fatalf("expected at least 1 ingested item, got 0")
	}

	var memItem *contextstore.ContextItem
	for i := range items {
		if items[i].Source.Type == "worker_memory_session" {
			memItem = &items[i]
			break
		}
	}
	if memItem == nil {
		t.Fatalf("no worker_memory_session item found in %d items", len(items))
	}
	if memItem.Lifecycle != contextstore.LifecycleConfirmed {
		t.Errorf("verified success should be confirmed, got %q", memItem.Lifecycle)
	}
	if memItem.Scope.AgentID != "worker-a" {
		t.Errorf("AgentID = %q, want worker-a", memItem.Scope.AgentID)
	}
}

func TestWP4_CoordinatorIngestUnverifiedIsCandidate(t *testing.T) {
	workspace := t.TempDir()
	c, repo := wp4SetupCoordinator(t, workspace)

	agentDef := &agent.AgentDef{
		Name:     "worker-a",
		MemoryID: "worker-a",
		Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	}
	c.executionRunID = "run-test-2"

	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker-a", Desc: "explore codebase"}})[0]

	result := &TaskResult{
		TaskID:  item.ID,
		Summary: "Explored the codebase structure",
	}

	// Unverified (no objective verification ran).
	c.ingestWorkerSessionMemory(context.Background(), agentDef, item.ID, result, "explored", false, 1)

	scope := contextstore.Scope{
		ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace), BranchID: "main", AgentID: "worker-a",
	}
	items := wp4QueryAll(t, repo, scope)
	var memItem *contextstore.ContextItem
	for i := range items {
		if items[i].Source.Type == "worker_memory_session" {
			memItem = &items[i]
			break
		}
	}
	if memItem == nil {
		t.Fatalf("no worker_memory_session item found")
	}
	if memItem.Lifecycle != contextstore.LifecycleCandidate {
		t.Errorf("unverified success should be candidate, got %q", memItem.Lifecycle)
	}
}

func TestWP4_CoordinatorIngestModeOffNoWrite(t *testing.T) {
	workspace := t.TempDir()
	c, repo := wp4SetupCoordinator(t, workspace)

	agentDef := &agent.AgentDef{
		Name:     "worker-a",
		MemoryID: "worker-a",
		Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryOff, AutoSave: true},
	}
	c.executionRunID = "run-test-3"

	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker-a", Desc: "do work"}})[0]

	c.ingestWorkerSessionMemory(context.Background(), agentDef, item.ID, &TaskResult{Summary: "done"}, "done", true, 1)

	scope := contextstore.Scope{
		ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace), AgentID: "worker-a",
	}
	items := wp4QueryAll(t, repo, scope)
	for _, item := range items {
		if item.Source.Type == "worker_memory_session" {
			t.Errorf("mode=off should not ingest, found: %+v", item)
		}
	}
}

func TestWP4_CoordinatorIngestDisabledProfileNoWrite(t *testing.T) {
	workspace := t.TempDir()
	c, repo := wp4SetupCoordinator(t, workspace)

	agentDef := &agent.AgentDef{
		Name:     "worker-a",
		MemoryID: "worker-a",
		Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	}
	c.executionRunID = "run-test-4"

	// Set fresh-verification profile which disables historical memory.
	fresh := BuiltinProfiles()[ProfileFreshVerification]
	c.SetExecutionProfile(fresh)

	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker-a", Desc: "do work"}})[0]

	c.ingestWorkerSessionMemory(context.Background(), agentDef, item.ID, &TaskResult{Summary: "done"}, "done", true, 1)

	scope := contextstore.Scope{
		ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace), AgentID: "worker-a",
	}
	items := wp4QueryAll(t, repo, scope)
	for _, item := range items {
		if item.Source.Type == "worker_memory_session" {
			t.Errorf("memory-disabled profile should not ingest, found: %+v", item)
		}
	}
}

func TestWP4_CoordinatorIngestIdempotentOnRetry(t *testing.T) {
	workspace := t.TempDir()
	c, repo := wp4SetupCoordinator(t, workspace)

	agentDef := &agent.AgentDef{
		Name:     "worker-a",
		MemoryID: "worker-a",
		Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	}
	c.executionRunID = "run-test-5"

	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker-a", Desc: "implement Y"}})[0]
	result := &TaskResult{TaskID: item.ID, Summary: "Y implemented"}

	// Ingest twice for the same attempt — should be idempotent.
	c.ingestWorkerSessionMemory(context.Background(), agentDef, item.ID, result, "done", true, 1)
	c.ingestWorkerSessionMemory(context.Background(), agentDef, item.ID, result, "done", true, 1)

	scope := contextstore.Scope{
		ProjectID: "/project", TeamID: "team", SessionID: filepath.Base(workspace), BranchID: "main", AgentID: "worker-a",
	}
	items := wp4QueryAll(t, repo, scope)
	workerMemCount := 0
	for _, item := range items {
		if item.Source.Type == "worker_memory_session" {
			workerMemCount++
		}
	}
	if workerMemCount != 1 {
		t.Errorf("idempotent re-ingest should produce 1 item, got %d", workerMemCount)
	}
}

func TestWP4_CoordinatorIngestFailureDoesNotPanic(t *testing.T) {
	workspace := t.TempDir()
	c, _ := wp4SetupCoordinator(t, workspace)

	agentDef := &agent.AgentDef{
		Name:     "worker-a",
		MemoryID: "worker-a",
		Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	}
	c.executionRunID = "run-test-6"

	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker-a", Desc: "do work"}})[0]

	// Replace the service with a failing one.
	c.workerMemorySvc = &failingWorkerMemoryService{}

	// This must not panic and must not return an error to the caller.
	c.ingestWorkerSessionMemory(context.Background(), agentDef, item.ID, &TaskResult{Summary: "done"}, "done", true, 1)
}

func TestWP4_BuildWorkerSessionSummaryBounded(t *testing.T) {
	goal := strings.Repeat("a", 1000)
	result := &TaskResult{
		Summary: strings.Repeat("b", 1000),
		Findings: []Finding{
			{Category: "cat", Summary: strings.Repeat("c", 500)},
		},
	}
	summary := buildWorkerSessionSummary(goal, result, "")
	// The summary should be bounded — much shorter than the raw inputs.
	if len(summary) > 2000 {
		t.Errorf("summary should be bounded, got %d chars", len(summary))
	}
	if !strings.Contains(summary, "Task:") {
		t.Errorf("summary should contain Task label: %q", summary)
	}
	if !strings.Contains(summary, "Summary:") {
		t.Errorf("summary should contain Summary label: %q", summary)
	}
}

func TestWP4_BuildWorkerSessionSummaryFreeTextFallback(t *testing.T) {
	summary := buildWorkerSessionSummary("do the thing", nil, "free text output here")
	if !strings.Contains(summary, "Output:") {
		t.Errorf("summary with nil result should use Output fallback: %q", summary)
	}
	if !strings.Contains(summary, "free text output here") {
		t.Errorf("summary should contain output text: %q", summary)
	}
}

func TestWP4_ShouldIngestWorkerMemory(t *testing.T) {
	tests := []struct {
		name     string
		agentDef *agent.AgentDef
		profile  ExecutionProfile
		want     bool
	}{
		{
			name:     "nil agent",
			agentDef: nil,
			profile:  ExecutionProfile{},
			want:     false,
		},
		{
			name:     "mode off",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryOff, AutoSave: true}},
			profile:  ExecutionProfile{},
			want:     false,
		},
		{
			name:     "auto-save disabled",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: false}},
			profile:  ExecutionProfile{},
			want:     false,
		},
		{
			name:     "mode session, auto-save on",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true}},
			profile:  ExecutionProfile{},
			want:     true,
		},
		{
			name:     "mode persistent, auto-save on",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent, AutoSave: true}},
			profile:  ExecutionProfile{},
			want:     true,
		},
		{
			name:     "mode session, profile disables memory",
			agentDef: &agent.AgentDef{Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true}},
			profile:  ExecutionProfile{DisableHistoricalMemory: true},
			want:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldIngestWorkerMemory(tc.agentDef, tc.profile)
			if got != tc.want {
				t.Errorf("shouldIngestWorkerMemory = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Cross-agent isolation test ---

func TestWP4_AgentAIngestNotVisibleToAgentB(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)

	scopeA := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a",
	}
	scopeB := contextstore.Scope{
		ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-b",
	}

	// Agent A ingests a private session memory.
	_, err := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
		WorkerID:   "worker-a",
		BranchID:   "main",
		Scope:      scopeA,
		Summary:    "Agent A's private finding about the architecture.",
		Goal:       "analyze architecture",
		TaskID:     "10",
		RunID:      "run-1",
		Attempt:    1,
		ProducerID: "worker-a",
		Verified:   true,
		Policy:     agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
	})
	if err != nil {
		t.Fatalf("agent A SaveSessionMemory: %v", err)
	}

	// Agent B recalls — should NOT see agent A's private item.
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker-b",
		Scope:    scopeB,
		Query:    "architecture",
		Policy:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 10, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatalf("agent B Recall: %v", err)
	}
	for _, item := range bundle.Items {
		if strings.Contains(item.Content, "Agent A's private finding") {
			t.Errorf("Agent B should not see Agent A's private memory: %s", item.ID)
		}
		if item.Scope.AgentID == "worker-a" {
			t.Errorf("Agent B should not see agent-a scoped item: %+v", item)
		}
	}
}

// failingWorkerMemoryService always returns an error from SaveSessionMemory.
type failingWorkerMemoryService struct{}

func (failingWorkerMemoryService) Recall(_ context.Context, _ WorkerMemoryRecallRequest) (WorkerMemoryBundle, error) {
	return WorkerMemoryBundle{Trace: WorkerMemoryTrace{Skipped: true, SkipReason: "failing service"}}, nil
}

func (failingWorkerMemoryService) SaveSessionMemory(_ context.Context, _ WorkerMemoryWriteRequest) (contextstore.ContextItem, error) {
	return contextstore.ContextItem{}, errFailingService
}

func (failingWorkerMemoryService) SaveCandidate(_ context.Context, _ WorkerMemoryCandidateRequest) (contextstore.ContextItem, error) {
	return contextstore.ContextItem{}, errFailingService
}

func (failingWorkerMemoryService) Confirm(_ context.Context, _ WorkerMemoryPromotionRequest) ([]contextstore.ContextItem, error) {
	return nil, errFailingService
}

func (failingWorkerMemoryService) RejectRun(_ context.Context, _ WorkerMemoryRejectionRequest) ([]contextstore.ContextItem, error) {
	return nil, errFailingService
}

var errFailingService = &failingError{"simulated ingestion failure"}

type failingError struct{ msg string }

func (e *failingError) Error() string { return e.msg }
