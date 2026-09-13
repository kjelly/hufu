package improve

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestHandoffStoreCreateGetAndIdempotentCreate(t *testing.T) {
	workspace := t.TempDir()
	handoff, err := NewImprovementHandoff(
		HandoffSkill,
		HandoffScope{ProjectID: "project", TeamID: "team"},
		ArtifactRef{Kind: "promotion_proposal", ID: "proposal", Revision: "draft-revision"},
		[]SourceBinding{
			{Ref: ArtifactRef{Kind: "context_item", ID: "source-b", Revision: "hash-b"}, ContentHash: "hash-b", ProjectID: "project", TeamID: "team"},
			{Ref: ArtifactRef{Kind: "context_item", ID: "source-a", Revision: "hash-a"}, ContentHash: "hash-a", ProjectID: "project", TeamID: "team"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHandoffStore(workspace)
	var audits []team.RunEvent
	store.audit = func(_ context.Context, event team.RunEvent) error {
		audits = append(audits, event)
		return nil
	}
	created, err := store.Create(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != handoff.ID || len(audits) != 1 || audits[0].Type != "handoff_created" {
		t.Fatalf("unexpected create result: %#v audits=%#v", created, audits)
	}
	if got, err := store.Get(handoff.ID); err != nil || got.ID != handoff.ID {
		t.Fatalf("get handoff: %#v %v", got, err)
	}
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if len(audits) != 2 || audits[0].IdempotencyKey != audits[1].IdempotencyKey {
		t.Fatalf("idempotent create audit keys = %#v", audits)
	}
	recreated := handoff
	recreated.CreatedAt = handoff.CreatedAt.Add(time.Second)
	recreated.UpdatedAt = recreated.CreatedAt
	if got, err := store.Create(t.Context(), recreated); err != nil || got.CreatedAt != handoff.CreatedAt {
		t.Fatalf("fresh-process idempotent create = %#v, %v", got, err)
	}

	data, err := marshalHandoff(handoff)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-2] = '2'
	if err := team.AtomicWriteFile(store.path(handoff.ID), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(handoff.ID); err == nil {
		t.Fatal("tampered handoff was accepted")
	}
}

func TestHandoffStoreSerializesConcurrentCASAcrossInstances(t *testing.T) {
	workspace := t.TempDir()
	handoff, err := NewImprovementHandoff(HandoffMemoryPolicy, HandoffScope{ProjectID: "project", TeamID: "team"}, ArtifactRef{Kind: "memory_policy_proposal", ID: "proposal", Revision: "revision"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, second := NewHandoffStore(workspace), NewHandoffStore(workspace)
	first.audit = func(context.Context, team.RunEvent) error { return nil }
	second.audit = first.audit
	if _, err := first.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for store, candidateID := range map[*HandoffStore]string{first: "candidate-a", second: "candidate-b"} {
		wg.Go(func() {
			<-start
			next := handoff
			next.Status = HandoffCandidateReady
			next.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: candidateID, Revision: "revision"}
			_, transitionErr := store.Transition(t.Context(), handoff.ID, handoff.Revision, next)
			results <- transitionErr
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var successes int
	for result := range results {
		if result == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent transitions = %d, want 1", successes)
	}
}

func TestHandoffStoreSerializesConcurrentCASAcrossProcesses(t *testing.T) {
	workspace := t.TempDir()
	handoff, err := NewImprovementHandoff(HandoffMemoryPolicy, HandoffScope{ProjectID: "project", TeamID: "team"}, ArtifactRef{Kind: "memory_policy_proposal", ID: "proposal", Revision: "revision"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHandoffStore(workspace)
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	barrier := filepath.Join(workspace, "start-cas")
	commands := make([]*exec.Cmd, 2)
	for i, candidate := range []string{"candidate-a", "candidate-b"} {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHandoffStoreProcessHelper$")
		command.Env = append(os.Environ(),
			"HUFU_HANDOFF_PROCESS_HELPER=1",
			"HUFU_HANDOFF_WORKSPACE="+workspace,
			"HUFU_HANDOFF_ID="+handoff.ID,
			"HUFU_HANDOFF_CANDIDATE="+candidate,
			"HUFU_HANDOFF_BARRIER="+barrier,
		)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands[i] = command
	}
	if err := os.WriteFile(barrier, []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	var successes int
	for _, command := range commands {
		if err := command.Wait(); err == nil {
			successes++
		} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 3 {
			t.Fatalf("handoff CAS helper: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful cross-process transitions = %d, want 1", successes)
	}
}

func TestHandoffStoreProcessHelper(t *testing.T) {
	if os.Getenv("HUFU_HANDOFF_PROCESS_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv("HUFU_HANDOFF_BARRIER")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(4)
		}
		time.Sleep(5 * time.Millisecond)
	}
	store := NewHandoffStore(os.Getenv("HUFU_HANDOFF_WORKSPACE"))
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	handoff, err := store.Get(os.Getenv("HUFU_HANDOFF_ID"))
	if err != nil {
		os.Exit(4)
	}
	next := handoff
	next.Status = HandoffCandidateReady
	next.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: os.Getenv("HUFU_HANDOFF_CANDIDATE"), Revision: "revision"}
	if _, err := store.Transition(t.Context(), handoff.ID, 1, next); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestHandoffStoreRejectsBoundRefMutationAndInvalidEligibility(t *testing.T) {
	store := NewHandoffStore(t.TempDir())
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	handoff, err := NewImprovementHandoff(HandoffMemoryPolicy, HandoffScope{ProjectID: "project", TeamID: "team"}, ArtifactRef{Kind: "memory_policy_proposal", ID: "proposal", Revision: "revision"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	next := handoff
	next.Status = HandoffCandidateReady
	next.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate", Revision: "candidate-revision"}
	next, err = store.Transition(t.Context(), handoff.ID, handoff.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	mutated := next
	mutated.Status = HandoffBenchmarkBound
	mutated.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "other", Revision: "other-revision"}
	mutated.Benchmark = &BenchmarkBinding{Ref: ArtifactRef{Kind: "benchmark_fixture", ID: "benchmark", Revision: "benchmark-revision"}, Name: "benchmark", Category: "test", Cases: 1}
	if _, err := store.Transition(t.Context(), handoff.ID, next.Revision, mutated); err == nil {
		t.Fatal("bound candidate mutation was accepted")
	}
	bound := next
	bound.Status = HandoffBenchmarkBound
	bound.Benchmark = mutated.Benchmark
	bound, err = store.Transition(t.Context(), handoff.ID, next.Revision, bound)
	if err != nil {
		t.Fatal(err)
	}
	reportRef := &ArtifactRef{Kind: "experiment_report", ID: "experiment", Revision: "experiment-revision"}
	evaluated := bound
	evaluated.Status = HandoffEvaluated
	evaluated.Experiment = reportRef
	evaluated.Evaluation = EvaluationState{Decision: "retry", Status: "inconclusive", Report: reportRef}
	evaluated, err = store.Transition(t.Context(), handoff.ID, bound.Revision, evaluated)
	if err != nil {
		t.Fatal(err)
	}
	eligible := evaluated
	eligible.Status = HandoffEligibleForReview
	if _, err := store.Transition(t.Context(), handoff.ID, evaluated.Revision, eligible); err == nil {
		t.Fatal("inconclusive evaluation became eligible for review")
	}
}

func TestHandoffStoreRetriesAuditWithoutRepeatingTransition(t *testing.T) {
	store := NewHandoffStore(t.TempDir())
	handoff, err := NewImprovementHandoff(HandoffMemoryPolicy, HandoffScope{ProjectID: "project", TeamID: "team"}, ArtifactRef{Kind: "memory_policy_proposal", ID: "proposal", Revision: "revision"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	var failedEvent team.RunEvent
	store.audit = func(_ context.Context, event team.RunEvent) error {
		failedEvent = event
		return errors.New("audit unavailable")
	}
	next := handoff
	next.Status = HandoffCandidateReady
	next.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate", Revision: "candidate-revision"}
	if _, err := store.Transition(t.Context(), handoff.ID, handoff.Revision, next); err == nil {
		t.Fatal("transition audit failure was ignored")
	}
	durable, err := store.Get(handoff.ID)
	if err != nil || durable.Status != HandoffCandidateReady {
		t.Fatalf("durable transition = %#v, %v", durable, err)
	}
	var retried team.RunEvent
	store.audit = func(_ context.Context, event team.RunEvent) error {
		retried = event
		return nil
	}
	if err := store.RetryAudit(t.Context(), durable.ID); err != nil {
		t.Fatal(err)
	}
	if retried.IdempotencyKey != failedEvent.IdempotencyKey {
		t.Fatalf("retry key = %q, want %q", retried.IdempotencyKey, failedEvent.IdempotencyKey)
	}
}

func TestHandoffStoreTransitionCASAndImmutableBindings(t *testing.T) {
	handoff, err := NewImprovementHandoff(
		HandoffMemoryPolicy,
		HandoffScope{ProjectID: "project", TeamID: "team"},
		ArtifactRef{Kind: "memory_policy_proposal", ID: "proposal", Revision: "proposal-revision"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHandoffStore(t.TempDir())
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	next := handoff
	next.Status = HandoffCandidateReady
	next.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate", Revision: "candidate-revision"}
	updated, err := store.Transition(t.Context(), handoff.ID, handoff.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision = %d, want 2", updated.Revision)
	}
	if _, err := store.Transition(t.Context(), handoff.ID, handoff.Revision, next); err == nil {
		t.Fatal("stale CAS transition was accepted")
	}
	mutated := updated
	mutated.Scope.TeamID = "other-team"
	if _, err := store.Transition(t.Context(), handoff.ID, updated.Revision, mutated); err == nil {
		t.Fatal("immutable scope mutation was accepted")
	}
}

func TestHandoffStoreRetainsRecordWhenAuditFails(t *testing.T) {
	handoff, err := NewImprovementHandoff(
		HandoffConsolidation,
		HandoffScope{ProjectID: "project", TeamID: "team"},
		ArtifactRef{Kind: "consolidation_proposal", ID: "proposal", Revision: "proposal-revision"},
		[]SourceBinding{{Ref: ArtifactRef{Kind: "context_item", ID: "source", Revision: "hash"}, ContentHash: "hash", ProjectID: "project", TeamID: "team"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHandoffStore(t.TempDir())
	store.audit = func(context.Context, team.RunEvent) error { return errors.New("audit unavailable") }
	if _, err := store.Create(t.Context(), handoff); err == nil {
		t.Fatal("audit failure was ignored")
	}
	if _, err := store.Get(handoff.ID); err != nil {
		t.Fatalf("handoff was not retained after audit failure: %v", err)
	}
}

func TestMemoryPolicyOptimizationProposalIsDurableAndContentFree(t *testing.T) {
	workspace := t.TempDir()
	base := DefaultMemoryPolicySnapshot("base-policy")
	if _, err := WriteMemoryPolicySnapshot(workspace, base); err != nil {
		t.Fatal(err)
	}
	optimizer, err := ProposeMemoryPolicyOptimization("candidate-policy", base, Metrics{MemoryHarmfulUseRate: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := NewMemoryPolicyOptimizationProposal(workspace, "memory-proposal", optimizer, Metrics{MemoryHarmfulUseRate: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMemoryPolicyOptimizationProposal(workspace, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Candidate.Kind != "memory_policy_snapshot" || loaded.Candidate.ID != optimizer.Candidate.ID {
		t.Fatalf("unexpected candidate ref: %#v", loaded.Candidate)
	}
	data, err := os.ReadFile(filepath.Join(ImprovementRoot(workspace), "memory-policies", "proposals", proposal.ID, "proposal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"retrieval"`)) || bytes.Contains(data, []byte(`"learning"`)) {
		t.Fatal("durable proposal inlined policy content")
	}
}

func TestApprovedMemoryPolicyHandoffUsesCanonicalActivationPath(t *testing.T) {
	workspace := t.TempDir()
	base := DefaultMemoryPolicySnapshot("base-policy")
	if _, err := WriteMemoryPolicySnapshot(workspace, base); err != nil {
		t.Fatal(err)
	}
	optimizer, err := ProposeMemoryPolicyOptimization("candidate-policy", base, Metrics{MemoryHarmfulUseRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := NewMemoryPolicyOptimizationProposal(workspace, "policy-proposal", optimizer, Metrics{MemoryHarmfulUseRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	fixture := BenchmarkFixture{Name: "policy-benchmark", Team: "team", Category: "memory", Cases: []BenchmarkCase{{ID: "case", Type: "happy", Prompt: "Use memory."}}}
	if _, _, err := CreateBenchmark(workspace, fixture); err != nil {
		t.Fatal(err)
	}
	baselineSnapshot := TeamSnapshot{Version: snapshotVersion, ID: "baseline-team", Kind: baselineSnapshotKind, Team: "team", DefinitionRevision: "baseline-team-revision", ContentRevision: "baseline-team-content"}
	candidateSnapshot := TeamSnapshot{Version: snapshotVersion, ID: "candidate-team", Kind: candidateSnapshotKind, Team: "team", DefinitionRevision: "candidate-team-revision", ContentRevision: "candidate-team-content", BaselineID: baselineSnapshot.ID}
	report, err := EvaluateExperiment("policy-experiment", fixture,
		ExperimentInput{Snapshot: baselineSnapshot, Report: &Report{Team: "team", RunIDs: []string{"baseline-run"}, TeamRevisions: []string{baselineSnapshot.DefinitionRevision}, MemoryPolicyVersions: []string{proposal.BasePolicy.ID}, Metrics: Metrics{TotalTasks: 1, Done: 1}}, MemoryPolicy: &proposal.BasePolicy, AcceptancePassed: true},
		ExperimentInput{Snapshot: candidateSnapshot, Report: &Report{Team: "team", RunIDs: []string{"candidate-run"}, TeamRevisions: []string{candidateSnapshot.DefinitionRevision}, MemoryPolicyVersions: []string{proposal.Candidate.ID}, Metrics: Metrics{TotalTasks: 1, Done: 1}}, MemoryPolicy: &proposal.Candidate, AcceptancePassed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteExperimentReport(workspace, report); err != nil {
		t.Fatal(err)
	}
	handoff, err := NewImprovementHandoff(HandoffMemoryPolicy, HandoffScope{ProjectID: "project", TeamID: "team"}, ArtifactRef{Kind: "memory_policy_proposal", ID: proposal.ID, Revision: proposal.RevisionHash}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHandoffStore(workspace)
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	next := handoff
	next.Status = HandoffCandidateReady
	next.Candidate = &proposal.Candidate
	next, err = store.Transition(t.Context(), handoff.ID, handoff.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	next.Status = HandoffBenchmarkBound
	next.Benchmark = &BenchmarkBinding{Ref: ArtifactRef{Kind: "benchmark_fixture", ID: fixture.Name, Revision: report.Benchmark.Revision}, Name: fixture.Name, Category: fixture.Category, Cases: len(fixture.Cases)}
	next, err = store.Transition(t.Context(), handoff.ID, next.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	reportRef := &ArtifactRef{Kind: "experiment_report", ID: report.ID, Revision: ExperimentReportRevision(report)}
	next.Status = HandoffEvaluated
	next.Experiment = reportRef
	next.Evaluation = EvaluationState{Decision: report.Decision, Status: report.Status, Report: reportRef}
	next, err = store.Transition(t.Context(), handoff.ID, next.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	next.Status = HandoffEligibleForReview
	next, err = store.Transition(t.Context(), handoff.ID, next.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	next.Status = HandoffApproved
	if _, err := store.Transition(t.Context(), handoff.ID, next.Revision, next); err != nil {
		t.Fatal(err)
	}
	active, err := ApproveMemoryPolicyCandidate(workspace, optimizer.Candidate.ID, true)
	if err != nil {
		t.Fatalf("activate approved handoff candidate: %v", err)
	}
	if active.Status != "active" || active.RevisionHash != proposal.Candidate.Revision {
		t.Fatalf("active policy = %#v", active)
	}
}
