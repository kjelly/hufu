package improve

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestMemoryPolicyCandidateCannotAutoAdopt(t *testing.T) {
	baseline := DefaultMemoryPolicySnapshot("baseline")
	candidateInput := baseline
	candidateInput.Retrieval.TopK--
	candidate, err := CreateMemoryPolicyCandidate("candidate", baseline, candidateInput)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err = EvaluateMemoryPolicyCandidate(candidate, Metrics{TotalTasks: 1, Done: 1}, Metrics{TotalTasks: 1, Done: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteMemoryPolicySnapshot(t.TempDir(), candidate); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if _, err := WriteMemoryPolicySnapshot(workspace, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteMemoryPolicySnapshot(workspace, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveMemoryPolicyCandidate(workspace, candidate.ID, false); err == nil {
		t.Fatal("candidate adopted without explicit approval")
	}
}

func TestSideEffectTaskDisablesExploration(t *testing.T) {
	if ControlledMemoryExplorationAllowed(team.SideEffectExternalWrite, team.RecoveryRetry, []string{"view"}, true) {
		t.Fatal("external side effect allowed exploration")
	}
	if ControlledMemoryExplorationAllowed(team.SideEffectNone, team.RecoveryRetry, []string{"ssh"}, true) {
		t.Fatal("ssh allowed exploration")
	}
	if ControlledMemoryExplorationAllowed(team.SideEffectNone, team.RecoveryRetry, []string{"bash"}, true) {
		t.Fatal("general-purpose shell allowed read-only exploration")
	}
	if !ControlledMemoryExplorationAllowed(team.SideEffectNone, team.RecoveryRetry, []string{"view", "grep"}, true) {
		t.Fatal("read-only sandbox exploration was denied")
	}
}

func TestProductionRegressionKeepsPreviousPolicyAvailable(t *testing.T) {
	baseline := DefaultMemoryPolicySnapshot("baseline")
	candidateInput := baseline
	candidateInput.Retrieval.TopK--
	candidate, err := CreateMemoryPolicyCandidate("candidate", baseline, candidateInput)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err = EvaluateMemoryPolicyCandidate(candidate, Metrics{TotalTasks: 1, Done: 1}, Metrics{TotalTasks: 1, Error: 1, MemoryHarmfulUseRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Status != "rejected" || candidate.PreviousID != baseline.ID {
		t.Fatalf("candidate = %+v", candidate)
	}
	workspace := t.TempDir()
	if _, err := WriteMemoryPolicySnapshot(workspace, baseline); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMemoryPolicySnapshot(workspace, baseline.ID)
	if err != nil || loaded.RevisionHash != baseline.RevisionHash {
		t.Fatalf("previous policy unavailable: %+v, %v", loaded, err)
	}
}

func TestApprovedMemoryPolicyCanRollback(t *testing.T) {
	workspace := t.TempDir()
	baseline := DefaultMemoryPolicySnapshot("baseline")
	input := baseline
	input.Retrieval.TopK--
	candidate, err := CreateMemoryPolicyCandidate("candidate", baseline, input)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err = EvaluateMemoryPolicyCandidate(candidate, Metrics{TotalTasks: 1, Done: 1}, Metrics{TotalTasks: 1, Done: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []MemoryPolicySnapshot{baseline, candidate} {
		if _, err := WriteMemoryPolicySnapshot(workspace, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ApproveMemoryPolicyCandidate(workspace, candidate.ID, true); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := RollbackMemoryPolicy(workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.ID != baseline.ID {
		t.Fatalf("rollback policy = %s, want %s", rolledBack.ID, baseline.ID)
	}
}

func TestSkillOptimizationStopsAtReviewableProposal(t *testing.T) {
	proposal, err := ProposeSkillCandidate("safe-procedure", []string{"b", "a"}, []int64{2, 1})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != "proposal" || proposal.CandidateSnapshot == "" || !proposal.RequiresBenchmark || !proposal.RequiresReview || !proposal.RequiresPR || !proposal.RequiresMonitoring || !proposal.RollbackRequired {
		t.Fatalf("unsafe skill proposal = %+v", proposal)
	}
	if proposal.SourceContextIDs[0] != "a" || proposal.SourceRevisions[0] != 1 {
		t.Fatalf("proposal provenance is not deterministic: %+v", proposal)
	}
}

func TestMemoryL3BenchmarkHasFixedSafetyCases(t *testing.T) {
	fixture := MemoryL3BenchmarkFixture("team")
	want := []string{"positive_transfer", "irrelevant_high_utility", "stale_harmful"}
	if len(fixture.Cases) != len(want) {
		t.Fatalf("benchmark cases = %+v", fixture.Cases)
	}
	for i := range want {
		if fixture.Cases[i].Type != want[i] {
			t.Fatalf("case %d type = %q, want %q", i, fixture.Cases[i].Type, want[i])
		}
	}
}
