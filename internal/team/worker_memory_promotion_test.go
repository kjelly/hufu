package team

import (
	"context"
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
		m.EvidenceResults = append(m.EvidenceResults, EvidenceResult{RequirementID: "task:" + task, Status: "passed"})
	}
	return m
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
