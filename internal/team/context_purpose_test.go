package team

import (
	"context"
	"testing"
)

func TestContextPurposeRegistryIsClosed(t *testing.T) {
	if _, err := contextPurposePolicy("not-a-purpose"); err == nil {
		t.Fatal("unknown purpose was accepted")
	}
	policy, err := contextPurposePolicy("guard_reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Trigger != ContextTriggerGuardReview || policy.FallbackAllowed {
		t.Fatalf("guard policy = %#v", policy)
	}
	compactor, err := contextPurposePolicy("compactor")
	if err != nil {
		t.Fatalf("sidecar compactor purpose rejected: %v", err)
	}
	if compactor.Trigger != ContextTriggerSidecarTask || !compactor.FallbackAllowed || compactor.FallbackOutcome != "uncompacted" {
		t.Fatalf("compactor policy = %#v", compactor)
	}
}

func TestAuxiliaryFallbackHonorsPurposePolicy(t *testing.T) {
	c := newDirectTerminationCoordinator(t, &contextManifestCountingAgent{})
	if err := c.recordAuxiliaryFallback(context.Background(), "guard_reviewer", ""); err == nil {
		t.Fatal("guard fallback was accepted instead of failing closed")
	}
}
