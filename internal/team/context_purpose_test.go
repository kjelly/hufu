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
}

func TestAuxiliaryFallbackHonorsPurposePolicy(t *testing.T) {
	c := newDirectTerminationCoordinator(t, &contextManifestCountingAgent{})
	if err := c.recordAuxiliaryFallback(context.Background(), "guard_reviewer", ""); err == nil {
		t.Fatal("guard fallback was accepted instead of failing closed")
	}
}
