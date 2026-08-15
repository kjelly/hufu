package team

import (
	"context"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestContextActivationEligibilityMatrix(t *testing.T) {
	dispatch := validTestContextRequest()
	dispatch.Phase = PhaseExecute
	dispatch.AgentRole = "coder"
	dispatch.Capabilities = nil
	retry := dispatch
	retry.Attempt = 2
	retry.Trigger = ContextTriggerRetry
	retry.Failure = &ContextFailure{ErrorClass: "ssh_timeout", ToolName: "ssh"}
	tests := []struct {
		name     string
		metadata map[string]string
		request  ContextRequest
		want     bool
		reason   ContextDecisionReason
	}{
		{"verify during execute", map[string]string{"activation.phases": "VERIFY"}, dispatch, false, ContextOmittedPhase},
		{"retry ssh timeout", map[string]string{"activation.triggers": "retry", "activation.error_classes": "ssh_timeout"}, retry, true, ContextIncludedRelevant},
		{"retry activation on dispatch", map[string]string{"activation.triggers": "retry", "activation.error_classes": "ssh_timeout"}, dispatch, false, ContextOmittedTrigger},
		{"reviewer role", map[string]string{"activation.roles": "reviewer"}, dispatch, false, ContextOmittedRole},
		{"missing capability", map[string]string{"activation.capabilities": "kubernetes"}, dispatch, false, ContextOmittedCapability},
		{"wrong environment", map[string]string{"activation.environment": "prod-a"}, dispatch, false, ContextOmittedEnvironment},
		{"generic", nil, dispatch, true, ContextIncludedRelevant},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := contextstore.ContextItem{ID: "ctx", Content: "memory", Metadata: tc.metadata, Lifecycle: contextstore.LifecycleConfirmed}
			got, reason, err := EvaluateContextEligibility(item, tc.request, "run-1", time.Now())
			if err != nil || got != tc.want || reason != tc.reason {
				t.Fatalf("eligibility = %v, %q, %v; want %v, %q", got, reason, err, tc.want, tc.reason)
			}
		})
	}
}

func TestContextActivationRejectsUnknownKeys(t *testing.T) {
	if _, err := ParseContextActivation(map[string]string{"activation.phase": "VERIFY"}); err == nil {
		t.Fatal("unknown activation key was accepted")
	}
}

func TestContextEligibilityRejectsForeignCandidateAndExpired(t *testing.T) {
	now := time.Now().UTC()
	request := validTestContextRequest()
	foreign := contextstore.ContextItem{ID: "candidate", Content: "failed run", Lifecycle: contextstore.LifecycleCandidate, Metadata: map[string]string{"run_id": "failed-run"}}
	if ok, reason, err := EvaluateContextEligibility(foreign, request, "current-run", now); err != nil || ok || reason != ContextOmittedLifecycle {
		t.Fatalf("foreign candidate = %v, %q, %v", ok, reason, err)
	}
	expiredAt := now.Add(-time.Second)
	expired := contextstore.ContextItem{ID: "expired", Content: "old", Lifecycle: contextstore.LifecycleConfirmed, ExpiresAt: &expiredAt}
	if ok, reason, err := EvaluateContextEligibility(expired, request, "current-run", now); err != nil || ok || reason != ContextOmittedExpired {
		t.Fatalf("expired = %v, %q, %v", ok, reason, err)
	}
}

func TestContextEligibilityRequiresExplicitVerifyActivation(t *testing.T) {
	request := validTestContextRequest()
	request.Phase = PhaseVerify
	generic := contextstore.ContextItem{ID: "generic", Lifecycle: contextstore.LifecycleConfirmed}
	eligible, reason, err := EvaluateContextEligibility(generic, request, request.RunID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if eligible || reason != ContextOmittedPhase {
		t.Fatalf("generic VERIFY eligibility = (%t, %s)", eligible, reason)
	}
	activated := generic
	activated.Metadata = map[string]string{"activation.phases": "VERIFY"}
	eligible, reason, err = EvaluateContextEligibility(activated, request, request.RunID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !eligible || reason != ContextIncludedRelevant {
		t.Fatalf("activated VERIFY eligibility = (%t, %s)", eligible, reason)
	}
}

func TestContextRouterAppliesActivationBeforeCandidateLimit(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	c.executionRunID = "run-1"
	c.memoryRankingPolicy = MemoryRuntimeRankingPolicy{CandidateTopK: 1, InjectTopK: 1, MinimumRelevance: 0}
	blocked := rankingItem("blocked-high-rank", 100)
	blocked.Content = "repair ssh repair ssh repair ssh"
	blocked.Metadata = map[string]string{"activation.phases": "VERIFY"}
	eligible := rankingItem("eligible-lower-rank", 1)
	eligible.Content = "repair ssh"
	if err := repo.Append(context.Background(), blocked, eligible); err != nil {
		t.Fatal(err)
	}
	request := validTestContextRequest()
	request.Phase = PhaseExecute
	request.Trigger = ContextTriggerTaskDispatch
	request.AssignRequestID()
	route, err := c.contextRouter().Route(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(route.Bundle.SharedPersistent) != 1 || route.Bundle.SharedPersistent[0].ID != eligible.ID {
		t.Fatalf("activation-ineligible item consumed candidate limit: %#v", route.Bundle.SharedPersistent)
	}
}

func TestRouterReadsTypedActivationProjection(t *testing.T) {
	_, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	item := rankingItem("typed-activation", 1)
	item.Metadata = map[string]string{"activation.phases": "VERIFY", "activation.triggers": "tool_failure"}
	if err := repo.Append(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Metadata = nil // prove the indexed projection, not caller metadata, owns routing input.
	projected, err := activationItemFromRepository(context.Background(), repo, loaded)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := ParseContextActivation(projected.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(activation.Phases) != 1 || activation.Phases[0] != "verify" || len(activation.Triggers) != 1 || activation.Triggers[0] != "tool_failure" {
		t.Fatalf("typed activation projection = %#v", activation)
	}
}
