package team

import (
	"context"
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
