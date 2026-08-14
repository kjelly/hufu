package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func sharedMemoryTestRepo(t *testing.T) contextstore.Repository {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestSharedMemoryCandidateLifecycleIsCanonical(t *testing.T) {
	ctx := context.Background()
	repo := sharedMemoryTestRepo(t)
	svc := NewSharedMemoryService(repo)
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session-1"}

	item, err := svc.Propose(ctx, SharedMemoryProposal{
		Scope: scope, Content: "use the verified adapter", Section: ltmSectionPatterns,
		Source: "ltm_update", RunID: "run-1", TaskID: "task-1",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if item.Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("lifecycle = %q, want candidate", item.Lifecycle)
	}
	if item.Scope.SessionID != "" || item.Scope.AgentID != "" || item.Scope.BranchID != "" {
		t.Fatalf("persistent shared candidate scope = %#v, want project/team only", item.Scope)
	}

	// Normal runtime retrieval sees only confirmed knowledge.
	visible, err := repo.Query(ctx, contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact})
	if err != nil {
		t.Fatalf("Query runtime visible: %v", err)
	}
	if len(visible) != 0 {
		t.Fatalf("candidate leaked into runtime query: %#v", visible)
	}

	confirmed, err := svc.ConfirmRun(ctx, SharedMemoryPromotion{Scope: scope, Manifest: &EvidenceManifest{RunID: "run-1", Status: "accepted", ManifestHash: "manifest-1"}})
	if err != nil {
		t.Fatalf("ConfirmRun: %v", err)
	}
	if len(confirmed) != 1 || confirmed[0].Lifecycle != contextstore.LifecycleConfirmed {
		t.Fatalf("confirmed = %#v", confirmed)
	}
	if confirmed[0].Metadata["manifest_hash"] != "manifest-1" {
		t.Fatalf("manifest binding = %#v", confirmed[0].Metadata)
	}
	foundManifest := false
	for _, evidence := range confirmed[0].Evidence {
		if evidence.Type == "evidence_manifest" && evidence.Ref == "manifest-1" {
			foundManifest = true
		}
	}
	if !foundManifest {
		t.Fatalf("manifest evidence missing: %#v", confirmed[0].Evidence)
	}
}

func TestSharedMemoryRejectedRunRemainsHidden(t *testing.T) {
	ctx := context.Background()
	repo := sharedMemoryTestRepo(t)
	svc := NewSharedMemoryService(repo)
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session-1"}
	if _, err := svc.Propose(ctx, SharedMemoryProposal{Scope: scope, Content: "failed approach", Section: ltmSectionIssues, Source: "reflexion", RunID: "run-rejected"}); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	rejected, err := svc.RejectRun(ctx, SharedMemoryRejection{Scope: scope, RunID: "run-rejected", Reason: "acceptance failed"})
	if err != nil {
		t.Fatalf("RejectRun: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("rejected = %#v", rejected)
	}
	if confirmed, err := svc.ConfirmRun(ctx, SharedMemoryPromotion{Scope: scope, Manifest: &EvidenceManifest{RunID: "run-rejected", Status: "accepted", ManifestHash: "manifest"}}); err != nil || len(confirmed) != 0 {
		t.Fatalf("rejected candidate was promoted: items=%#v err=%v", confirmed, err)
	}
}

func TestSharedMemorySupersessionIsAtomicOnConfirmation(t *testing.T) {
	ctx := context.Background()
	repo := sharedMemoryTestRepo(t)
	svc := NewSharedMemoryService(repo)
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session-1"}
	old, err := svc.Propose(ctx, SharedMemoryProposal{Scope: scope, Content: "use adapter v1", Section: ltmSectionPatterns, Source: "memory_save", RunID: "run-old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ConfirmRun(ctx, SharedMemoryPromotion{Scope: scope, Manifest: &EvidenceManifest{RunID: "run-old", Status: "accepted", ManifestHash: "manifest-old"}}); err != nil {
		t.Fatal(err)
	}
	revision, err := svc.Propose(ctx, SharedMemoryProposal{Scope: scope, Content: "use adapter v2", Section: ltmSectionPatterns, Source: "memory_save", RunID: "run-new", Supersedes: []string{old.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := repo.Get(ctx, old.ID); err != nil || stored.SupersededBy != "" {
		t.Fatalf("candidate proposal changed old truth: %#v, %v", stored, err)
	}
	if _, err = svc.ConfirmRun(ctx, SharedMemoryPromotion{Scope: scope, Manifest: &EvidenceManifest{RunID: "run-new", Status: "accepted", ManifestHash: "manifest-new"}}); err != nil {
		t.Fatal(err)
	}
	storedOld, err := repo.Get(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedNew, err := repo.Get(ctx, revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedOld.SupersededBy != revision.ID || storedNew.Lifecycle != contextstore.LifecycleConfirmed {
		t.Fatalf("supersession state old=%#v new=%#v", storedOld, storedNew)
	}
	visible, err := repo.Query(ctx, contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact})
	if err != nil || len(visible) != 1 || visible[0].ID != revision.ID {
		t.Fatalf("visible current records = %#v, %v", visible, err)
	}
}

func TestSharedMemorySupersessionRefusesForeignTarget(t *testing.T) {
	ctx := context.Background()
	repo := sharedMemoryTestRepo(t)
	svc := NewSharedMemoryService(repo)
	other := contextstore.ContextItem{ID: "other-team", Kind: contextstore.ContextPattern, Content: "other team's pattern", Scope: contextstore.Scope{ProjectID: "project", TeamID: "other"}, Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal, Metadata: map[string]string{"visibility": "shared", "memory_lifetime": "persistent"}}
	if err := repo.Append(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Propose(ctx, SharedMemoryProposal{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Content: "cannot replace foreign", Section: ltmSectionPatterns, RunID: "run", Supersedes: []string{other.ID}}); err == nil {
		t.Fatal("foreign shared record was accepted as a supersession target")
	}
}

func TestSharedMemoryProposalPreservesExplicitZeroConfidence(t *testing.T) {
	ctx := context.Background()
	repo := sharedMemoryTestRepo(t)
	svc := NewSharedMemoryService(repo)
	zero := 0.0
	item, err := svc.Propose(ctx, SharedMemoryProposal{
		Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Content: "low-trust fact",
		Section: ltmSectionPatterns, Source: "memory_save", RunID: "run-1", Confidence: &zero,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if item.Confidence != 0 {
		t.Fatalf("explicit confidence 0 was rewritten to %v", item.Confidence)
	}
	// Omitted confidence defaults to 0.8.
	omitted, err := svc.Propose(ctx, SharedMemoryProposal{
		Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Content: "default trust fact",
		Section: ltmSectionPatterns, Source: "memory_save", RunID: "run-2",
	})
	if err != nil {
		t.Fatalf("Propose omitted: %v", err)
	}
	if omitted.Confidence != 0.8 {
		t.Fatalf("omitted confidence = %v, want default 0.8", omitted.Confidence)
	}
	// Out-of-range values are rejected.
	bad := 1.5
	if _, err := svc.Propose(ctx, SharedMemoryProposal{
		Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Content: "bad trust fact",
		Section: ltmSectionPatterns, Source: "memory_save", RunID: "run-3", Confidence: &bad,
	}); err == nil {
		t.Fatal("out-of-range confidence was accepted")
	}
}

func TestCoordinatorSharedKnowledgeDoesNotWriteLegacyJSONL(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo, projectDir: "project", executionRunID: "run-1",
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.persistKnowledgeCandidate("canonical team lesson", ltmSectionPatterns, "ltm_update")
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact, IncludeCandidates: true,
	})
	if err != nil || len(items) != 1 || items[0].Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("canonical candidate = %#v, err=%v", items, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, logsDir, "reflexion_candidates.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("legacy candidate JSONL was written: %v", err)
	}
}

func TestSharedMemory_RejectedRunWithoutPriorCandidates_RejectsLateProposalOnFreshServiceAndReopenedRepo(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, "context.sqlite")
	repo1, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	runID := "run-failed-no-candidates"

	// 1. Reject a run that has 0 prior shared candidates using an ephemeral/first service
	svc1 := NewSharedMemoryService(repo1)
	if _, err := svc1.RejectRun(context.Background(), SharedMemoryRejection{
		Scope:  scope,
		RunID:  runID,
		Reason: "task failed with no preliminary candidates",
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

	// 3. Propose a late shared candidate for the rejected run using a brand new SharedMemoryService
	svc2 := NewSharedMemoryService(repo2)
	proposed, err := svc2.Propose(context.Background(), SharedMemoryProposal{
		Scope:    scope,
		Content:  "late shared candidate lesson",
		Section:  ltmSectionPatterns,
		Category: "pattern",
		Source:   "memory_save",
		RunID:    runID,
	})
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if proposed.Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("proposed lifecycle = %q, want %q", proposed.Lifecycle, contextstore.LifecycleRejected)
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

func TestSharedMemory_RejectionIsScopeBound_DoesNotAffectOtherProjectOrTeam(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, "context.sqlite")
	repo1, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	scopeA := contextstore.Scope{ProjectID: "project-A", TeamID: "team-A"}
	scopeB := contextstore.Scope{ProjectID: "project-B", TeamID: "team-B"}
	runID := "shared-run-isolated"

	// 1. Reject runID under scopeA with zero preliminary candidates
	svc1 := NewSharedMemoryService(repo1)
	if _, err := svc1.RejectRun(context.Background(), SharedMemoryRejection{
		Scope:  scopeA,
		RunID:  runID,
		Reason: "failed on team A",
	}); err != nil {
		t.Fatalf("RejectRun on scopeA: %v", err)
	}

	// Reopen database to verify durability across restart
	if err := repo1.Close(); err != nil {
		t.Fatal(err)
	}
	repo2, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo2.Close()

	svc2 := NewSharedMemoryService(repo2)

	// 2. Propose a candidate with same runID for scopeB -> must remain candidate (not rejected)
	proposedB, err := svc2.Propose(context.Background(), SharedMemoryProposal{
		Scope:    scopeB,
		Content:  "team B pattern",
		Section:  ltmSectionPatterns,
		Category: "pattern",
		Source:   "memory_save",
		RunID:    runID,
	})
	if err != nil {
		t.Fatalf("Propose on scopeB failed: %v", err)
	}
	if proposedB.Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("scopeB candidate lifecycle = %q, want candidate (scope isolation violated)", proposedB.Lifecycle)
	}

	// 3. Propose a candidate with same runID for scopeA -> must be rejected
	proposedA, err := svc2.Propose(context.Background(), SharedMemoryProposal{
		Scope:    scopeA,
		Content:  "team A pattern",
		Section:  ltmSectionPatterns,
		Category: "pattern",
		Source:   "memory_save",
		RunID:    runID,
	})
	if err != nil {
		t.Fatalf("Propose on scopeA failed: %v", err)
	}
	if proposedA.Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("scopeA candidate lifecycle = %q, want rejected", proposedA.Lifecycle)
	}
}

type failingAppendRepo struct {
	contextstore.Repository
	failAppend bool
}

func (f *failingAppendRepo) Append(ctx context.Context, items ...contextstore.ContextItem) error {
	if f.failAppend {
		return errors.New("simulated append failure")
	}
	return f.Repository.Append(ctx, items...)
}

func TestSharedMemory_RejectRun_FailClosedOnAppendError(t *testing.T) {
	repo := sharedMemoryTestRepo(t)
	failing := &failingAppendRepo{Repository: repo, failAppend: true}
	svc := NewSharedMemoryService(failing)

	scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	runID := "run-append-fail"

	// 1. RejectRun must fail closed when durable append fails
	_, err := svc.RejectRun(context.Background(), SharedMemoryRejection{
		Scope:  scope,
		RunID:  runID,
		Reason: "test failure",
	})
	if err == nil {
		t.Fatal("expected RejectRun to return error when Append fails, got nil")
	}

	// 2. Clear failure: proposing should not be prematurely blocked by un-persisted rejection
	failing.failAppend = false
	proposed, err := svc.Propose(context.Background(), SharedMemoryProposal{
		Scope:    scope,
		Content:  "candidate after failed rejection",
		Section:  ltmSectionPatterns,
		Category: "pattern",
		Source:   "memory_save",
		RunID:    runID,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposed.Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("candidate lifecycle = %q, want candidate when rejection failed to persist", proposed.Lifecycle)
	}
}
