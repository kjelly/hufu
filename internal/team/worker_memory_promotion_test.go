package team

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func wp5Scope(worker string) contextstore.Scope {
	return contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session", BranchID: "main", AgentID: worker}
}

func wp5Candidate(t *testing.T, svc WorkerMemoryService, worker, task, run, content string) contextstore.ContextItem {
	t.Helper()
	item, err := svc.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: worker, Scope: wp5Scope(worker), Content: content, Category: "pattern", Tier: "persistent",
		RunID: run, TaskID: task, Source: "memory_save",
	})
	if err != nil {
		t.Fatalf("SaveCandidate: %v", err)
	}
	return item
}

func wp5AcceptedManifest(run string, tasks ...string) *EvidenceManifest {
	m := &EvidenceManifest{RunID: run, Status: "accepted", ManifestHash: "sealed-" + run}
	for _, task := range tasks {
		artifact := ArtifactRef{ID: "sha256-" + strings.Repeat("a", 64), RunID: run, TaskID: task, Attempt: 1}
		m.ArtifactRefs = append(m.ArtifactRefs, artifact)
		m.EvidenceResults = append(m.EvidenceResults, EvidenceResult{RequirementID: "task:" + task, Status: "passed", ArtifactRefs: []ArtifactRef{artifact}, Binding: &EvidenceBinding{
			RunID: run, TaskID: task, Attempt: 1, ModelExecutionID: "exec-" + task, ProducerID: "worker", TranscriptRef: artifact.ID, ArtifactIDs: []string{artifact.ID},
		}})
	}
	return m
}

func TestPrivateMemorySavePreservesExplicitZeroConfidence(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	zero := 0.0
	item, err := svc.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "agent-a", Scope: wp5Scope("agent-a"), Content: "low-trust private fact", Category: "pattern", Tier: "persistent",
		RunID: "run-a", TaskID: "task-a", Source: "memory_save", Confidence: &zero,
	})
	if err != nil {
		t.Fatalf("SaveCandidate: %v", err)
	}
	if item.Confidence != 0 {
		t.Fatalf("explicit private confidence 0 was rewritten to %v", item.Confidence)
	}
}

func TestWP5_CandidateIsInvisibleUntilAcceptedEvidence(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	item := wp5Candidate(t, svc, "agent-a", "task-a", "run-a", "private needle")

	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: wp5Scope("agent-a"), Visibility: contextstore.VisibilitySubtree, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range items {
		if got.ID == item.ID {
			t.Fatalf("candidate %q was visible to default runtime query", item.ID)
		}
	}
	if _, err := svc.Confirm(context.Background(), WorkerMemoryPromotionRequest{Scope: wp5Scope("agent-a"), WorkerID: "agent-a", Manifest: wp5AcceptedManifest("run-a", "task-a")}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	items, err = repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: wp5Scope("agent-a"), Visibility: contextstore.VisibilitySubtree, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, got := range items {
		found = found || got.ID == item.ID && got.Lifecycle == contextstore.LifecycleConfirmed
	}
	if !found {
		t.Fatal("accepted candidate was not made visible as confirmed memory")
	}
}

func TestWP5_WorkerCannotPromoteAnotherWorkersCandidate(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	b := wp5Candidate(t, svc, "agent-b", "task-b", "run-shared", "agent b only")
	confirmed, err := svc.Confirm(context.Background(), WorkerMemoryPromotionRequest{Scope: wp5Scope("agent-a"), WorkerID: "agent-a", Manifest: wp5AcceptedManifest("run-shared", "task-b")})
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmed) != 0 {
		t.Fatalf("agent-a promotion changed another worker's records: %#v", confirmed)
	}
	stored, err := repo.Get(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("agent-b candidate lifecycle = %q, want candidate", stored.Lifecycle)
	}
}

func TestWP5_IncompleteEvidenceFailsClosedAndRejectedRunsAreMarked(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	item := wp5Candidate(t, svc, "agent-a", "task-a", "run-failed", "do not trust yet")
	_, err := svc.Confirm(context.Background(), WorkerMemoryPromotionRequest{Scope: wp5Scope("agent-a"), WorkerID: "agent-a", Manifest: &EvidenceManifest{RunID: "run-failed", Status: "accepted", ManifestHash: "sealed"}})
	if err == nil {
		t.Fatal("confirmation without task evidence succeeded")
	}
	if _, err := svc.RejectRun(context.Background(), WorkerMemoryRejectionRequest{Scope: wp5Scope(""), RunID: "run-failed", Reason: "acceptance failed"}); err != nil {
		t.Fatalf("RejectRun: %v", err)
	}
	stored, err := repo.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("rejected run candidate lifecycle = %q, want rejected", stored.Lifecycle)
	}
}

func TestWP5_PromotionIsIdempotent(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	first := wp5Candidate(t, svc, "agent-a", "task-a", "run-idempotent", "stable convention")
	second := wp5Candidate(t, svc, "agent-a", "task-a", "run-idempotent", "stable convention")
	if first.ID != second.ID {
		t.Fatalf("candidate dedupe ID mismatch: %q != %q", first.ID, second.ID)
	}
	req := WorkerMemoryPromotionRequest{Scope: wp5Scope("agent-a"), WorkerID: "agent-a", Manifest: wp5AcceptedManifest("run-idempotent", "task-a")}
	if items, err := svc.Confirm(context.Background(), req); err != nil || len(items) != 1 {
		t.Fatalf("first Confirm = %#v, %v; want one promotion", items, err)
	}
	if items, err := svc.Confirm(context.Background(), req); err != nil || len(items) != 0 {
		t.Fatalf("second Confirm = %#v, %v; want idempotent no-op", items, err)
	}
}

func TestWP5_AcceptedSessionCandidateIsPromotedInPlace(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	item, err := svc.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "agent-a", Scope: wp5Scope("agent-a"), Content: "session-only detail", Category: "pattern", Tier: "session",
		RunID: "run-session", TaskID: "task-session", Source: "memory_save",
	})
	if err != nil {
		t.Fatalf("SaveCandidate: %v", err)
	}
	if item.Scope.SessionID != "session" || item.Scope.BranchID != "main" {
		t.Fatalf("session candidate scope = %#v, want trusted session and branch retained", item.Scope)
	}
	promoted, err := svc.Confirm(context.Background(), WorkerMemoryPromotionRequest{Scope: wp5Scope("agent-a"), WorkerID: "agent-a", Manifest: wp5AcceptedManifest("run-session", "task-session")})
	if err != nil || len(promoted) != 1 {
		t.Fatalf("Confirm = %#v, %v; want session candidate promotion", promoted, err)
	}
	stored, err := repo.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Lifecycle != contextstore.LifecycleConfirmed || stored.Scope.SessionID != "session" || stored.Scope.BranchID != "main" {
		t.Fatalf("promoted session record = %#v, want confirmed record with original private scope", stored)
	}
}

func TestWP5_RejectedRunRejectsLateCandidateWrite(t *testing.T) {
	repo := wp4SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	runID := "run-failed-late"

	// 1. Run fails and RejectRun is executed
	if _, err := svc.RejectRun(context.Background(), WorkerMemoryRejectionRequest{
		Scope:  wp5Scope(""),
		RunID:  runID,
		Reason: "task failed",
	}); err != nil {
		t.Fatalf("RejectRun: %v", err)
	}

	// 2. An asynchronous or delayed reflexion candidate write arrives after RejectRun
	saved, err := svc.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "agent-a",
		Scope:    wp5Scope("agent-a"),
		Content:  "late candidate lesson",
		Category: "lesson",
		Tier:     "persistent",
		RunID:    runID,
		TaskID:   "task-a",
		Source:   "reflexion",
	})
	if err != nil {
		t.Fatalf("SaveCandidate after RejectRun failed: %v", err)
	}
	if saved.Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("late candidate lifecycle = %q, want %q", saved.Lifecycle, contextstore.LifecycleRejected)
	}

	// 3. Query all candidates in repo for this run/worker
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope:             wp5Scope("agent-a"),
		Visibility:        contextstore.VisibilitySubtree,
		IncludeCandidates: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
			t.Fatalf("found unresolved candidate %q for rejected run %q", it.ID, runID)
		}
	}
}

func TestWP5_RejectedRunWithoutPriorCandidates_RejectsLateCandidateOnFreshServiceAndReopenedRepo(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, "context.sqlite")
	repo1, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", AgentID: "agent-a"}
	runID := "worker-run-failed-no-candidates"

	// 1. Reject a run that has 0 prior worker candidates using an ephemeral service
	svc1 := NewWorkerMemoryService(repo1, nil)
	if _, err := svc1.RejectRun(context.Background(), WorkerMemoryRejectionRequest{
		Scope:  scope,
		RunID:  runID,
		Reason: "worker failed before writing candidates",
	}); err != nil {
		t.Fatalf("RejectRun failed: %v", err)
	}

	// Close repo1 to simulate process exit/restart
	if err := repo1.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. Reopen SQLite repo in a fresh process/instance
	repo2, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo2.Close()

	// 3. Save a late worker candidate for the rejected run using a brand new WorkerMemoryService
	svc2 := NewWorkerMemoryService(repo2, nil)
	saved, err := svc2.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "agent-a",
		Scope:    scope,
		Content:  "late worker lesson",
		Category: "lesson",
		Tier:     "persistent",
		RunID:    runID,
		TaskID:   "task-1",
		Source:   "reflexion",
	})
	if err != nil {
		t.Fatalf("SaveCandidate failed: %v", err)
	}
	if saved.Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("saved lifecycle = %q, want %q", saved.Lifecycle, contextstore.LifecycleRejected)
	}

	// 4. Assert that no candidate for this run exists with LifecycleCandidate
	items, err := repo2.Query(context.Background(), contextstore.RepositoryQuery{
		Scope:             scope,
		Visibility:        contextstore.VisibilitySubtree,
		IncludeCandidates: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
			t.Fatalf("found undecided candidate %q for rejected run %q", it.ID, runID)
		}
	}
}

func TestWorkerMemory_RejectionIsScopeBound_DoesNotAffectOtherAgentOrTeam(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, "context.sqlite")
	repo1, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	scopeA := contextstore.Scope{ProjectID: "project-A", TeamID: "team-A", AgentID: "worker-A"}
	scopeB := contextstore.Scope{ProjectID: "project-A", TeamID: "team-A", AgentID: "worker-B"}
	runID := "worker-run-isolated"

	// 1. Reject runID under worker-A
	svc1 := NewWorkerMemoryService(repo1, nil)
	if _, err := svc1.RejectRun(context.Background(), WorkerMemoryRejectionRequest{
		Scope:  scopeA,
		RunID:  runID,
		Reason: "failed on worker A",
	}); err != nil {
		t.Fatalf("RejectRun on worker-A: %v", err)
	}

	// Reopen database
	if err := repo1.Close(); err != nil {
		t.Fatal(err)
	}
	repo2, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo2.Close()

	svc2 := NewWorkerMemoryService(repo2, nil)

	// 2. Propose a candidate under worker-B -> must remain candidate (not rejected)
	savedB, err := svc2.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "worker-B",
		Scope:    scopeB,
		Content:  "worker B lesson",
		Category: "lesson",
		Tier:     "persistent",
		RunID:    runID,
		TaskID:   "task-1",
		Source:   "reflexion",
	})
	if err != nil {
		t.Fatalf("SaveCandidate worker-B: %v", err)
	}
	if savedB.Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("worker-B candidate lifecycle = %q, want candidate (scope isolation violated)", savedB.Lifecycle)
	}

	// 3. Propose a candidate under worker-A -> must be rejected
	savedA, err := svc2.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "worker-A",
		Scope:    scopeA,
		Content:  "worker A lesson",
		Category: "lesson",
		Tier:     "persistent",
		RunID:    runID,
		TaskID:   "task-1",
		Source:   "reflexion",
	})
	if err != nil {
		t.Fatalf("SaveCandidate worker-A: %v", err)
	}
	if savedA.Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("worker-A candidate lifecycle = %q, want rejected", savedA.Lifecycle)
	}
}

func TestWorkerMemory_RejectRun_FailClosedOnAppendError(t *testing.T) {
	repo := wp4SetupRepo(t)
	failing := &failingAppendRepo{Repository: repo, failAppend: true}
	svc := NewWorkerMemoryService(failing, nil)

	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", AgentID: "worker-1"}
	runID := "worker-append-fail"

	// 1. RejectRun must fail closed when durable append fails
	_, err := svc.RejectRun(context.Background(), WorkerMemoryRejectionRequest{
		Scope:  scope,
		RunID:  runID,
		Reason: "test failure",
	})
	if err == nil {
		t.Fatal("expected RejectRun to return error when Append fails, got nil")
	}

	// 2. Clear failure: candidate write should not be prematurely blocked by un-persisted rejection
	failing.failAppend = false
	saved, err := svc.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "worker-1",
		Scope:    scope,
		Content:  "candidate after failed rejection",
		Category: "lesson",
		Tier:     "persistent",
		RunID:    runID,
		TaskID:   "task-1",
		Source:   "reflexion",
	})
	if err != nil {
		t.Fatalf("SaveCandidate: %v", err)
	}
	if saved.Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("candidate lifecycle = %q, want candidate when rejection failed to persist", saved.Lifecycle)
	}
}
