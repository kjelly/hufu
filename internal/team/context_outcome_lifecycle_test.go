package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestObjectiveVerificationCreatesContextLifecycleObservations(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	item := &TodoItem{
		ID:           "task-1",
		VerifyResult: &VerificationResult{ExitCode: 1},
		ContextManifests: []ContextInjectionManifest{{
			RequestID: "request-1", RunID: "run-1", TaskID: "task-1", Attempt: 1, AgentRole: "worker", Phase: PhaseExecute,
			Trigger: ContextTriggerTaskDispatch, Fingerprint: "manifest-1",
			Items: []ContextManifestItem{{ID: "a", Source: "shared_persistent", Included: true}},
		}},
	}
	c.recordGeneralContextOutcome(item, "task_failed")
	c.recordGeneralContextOutcome(item, "task_failed")
	for _, outcome := range []string{"verification_assessed", "failure_attributed", "failure"} {
		count, err := repo.ContextOutcomeCount(context.Background(), "a", string(PhaseExecute), string(ContextTriggerTaskDispatch), "worker", "", outcome)
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s observations = %d, want 1", outcome, count)
		}
	}
}

func TestAcceptanceObservationOnlyIncludesInjectedItems(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	c.sessionData = NewSession()
	c.sessionData.CoordinatorContextManifests = []ContextInjectionManifest{{
		RequestID: "request-1", RunID: "run-1", Attempt: 1, AgentRole: "coordinator", Phase: PhaseInit, Trigger: ContextTriggerCoordinatorStart,
		Fingerprint: "manifest-1", Items: []ContextManifestItem{{ID: "a", Source: "shared_persistent", Included: true}, {ID: "b", Source: "shared_persistent", Included: false}},
	}}
	if err := c.recordContextAcceptanceObservations(&AcceptanceResult{State: AcceptancePassed}); err != nil {
		t.Fatal(err)
	}
	count, err := repo.ContextOutcomeCount(context.Background(), "a", string(PhaseInit), string(ContextTriggerCoordinatorStart), "coordinator", "", "acceptance_assessed")
	if err != nil || count != 1 {
		t.Fatalf("included acceptance observations = %d, %v", count, err)
	}
	count, err = repo.ContextOutcomeCount(context.Background(), "b", string(PhaseInit), string(ContextTriggerCoordinatorStart), "coordinator", "", "acceptance_assessed")
	if err != nil || count != 0 {
		t.Fatalf("omitted acceptance observations = %d, %v", count, err)
	}
}

func TestJudgeAndSkepticSignalsOnlyUseIncludedContext(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	c.sessionData = NewSession()
	c.sessionData.CoordinatorContextManifests = []ContextInjectionManifest{
		{RequestID: "judge-request", RunID: "run-1", TaskID: "task-1", Attempt: 1, AgentRole: "auxiliary", Phase: PhaseVerify, Trigger: ContextTriggerJudge, Purpose: "judge", ModelCalled: true, Fingerprint: "judge-manifest", Items: []ContextManifestItem{{ID: "included", Source: "shared_persistent", Included: true}, {ID: "omitted", Source: "shared_persistent", Included: false}}},
		{RequestID: "skeptic-request", RunID: "run-1", TaskID: "task-1", Attempt: 1, AgentRole: "auxiliary", Phase: PhaseVerify, Trigger: ContextTriggerSkeptic, Purpose: "skeptic", ModelCalled: true, Fingerprint: "skeptic-manifest", Items: []ContextManifestItem{{ID: "included", Source: "shared_persistent", Included: true}, {ID: "omitted", Source: "shared_persistent", Included: false}}},
	}
	if err := c.recordAuxiliaryContextSignal("task-1", "judge", "judge_signal", "selected_candidate"); err != nil {
		t.Fatal(err)
	}
	if err := c.recordAuxiliaryContextSignal("task-1", "skeptic", "skeptic_signal", "confirmed"); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ id, trigger, outcome string }{{"included", string(ContextTriggerJudge), "judge_signal"}, {"included", string(ContextTriggerSkeptic), "skeptic_signal"}, {"omitted", string(ContextTriggerJudge), "judge_signal"}, {"omitted", string(ContextTriggerSkeptic), "skeptic_signal"}} {
		count, err := repo.ContextOutcomeCount(context.Background(), check.id, string(PhaseVerify), check.trigger, "auxiliary", "", check.outcome)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if check.id == "omitted" {
			want = 0
		}
		if count != want {
			t.Fatalf("%s/%s %s observations = %d, want %d", check.id, check.trigger, check.outcome, count, want)
		}
	}
}
