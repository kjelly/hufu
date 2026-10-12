package team

import (
	"context"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
)

func TestCoordinatorDiagnosticIdentityCannotChangeAuthorizationOrCostAttempt(t *testing.T) {
	c := &Coordinator{
		executionRunID: "run-1",
		sessionData: &SessionData{CoordinatorContextManifests: []ContextInjectionManifest{
			{RunID: "run-1", Agent: "coordinator", AgentRole: "coordinator", RequestID: "request-1", Fingerprint: "manifest-1", Attempt: 1, Phase: PhaseInit, Trigger: ContextTriggerCoordinatorStart, Purpose: "coordinator_start", ModelCalled: true},
		}},
	}
	ctx := context.WithValue(t.Context(), todoIDKey{}, CoordTodoID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, 3)
	ctx = withProviderCostContext(ctx, providerCostContext{Agent: "coordinator", Role: "coordinator", Purpose: cost.PurposeCoordinator})
	if c.withCoordinatorDiagnosticIdentity(ctx) != ctx {
		t.Fatal("disabled diagnostics changed the context")
	}
	ctx = agent.WithProviderTransportObserver(ctx, func(context.Context, agent.ProviderTransportObservation) {})
	ctx = c.withCoordinatorDiagnosticIdentity(ctx)
	if _, authorized := invocationMetadataFromContext(ctx); authorized {
		t.Fatal("diagnostics installed tool-authorizing metadata")
	}
	metadata, ok := InvocationMetadataSnapshot(ctx)
	if !ok || metadata.ParentRequestID != "request-1" || metadata.AgentName != "coordinator" {
		t.Fatalf("diagnostic identity = %+v, available %v", metadata, ok)
	}
	identity, err := c.providerCostIdentity(ctx, "provider-1")
	if err != nil || identity.OccurrenceAttempt != 3 || identity.Purpose != cost.PurposeCoordinator {
		t.Fatalf("cost identity = %+v, error %v", identity, err)
	}
}

func TestAuxiliaryDiagnosticIdentityUsesExactManifestWithoutAuthority(t *testing.T) {
	c := &Coordinator{executionRunID: "run-aux", session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "diagnostic"}}, sessionData: NewSession(), taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
	parent := InvocationMetadata{RunID: "run-aux", TaskID: "1", AgentName: "worker", AgentRole: "worker", Attempt: 2, ModelExecutionID: "parent-model", ParentRequestID: "parent-request", Phase: PhaseExecute, Trigger: ContextTriggerRetry}
	ctx := withTestAuxiliaryInvocationContext(withInvocationMetadata(t.Context(), parent))
	ctx = agent.WithProviderTransportObserver(ctx, func(context.Context, agent.ProviderTransportObservation) {})
	ctx, prompt, err := c.prepareAuxiliaryInvocation(ctx, "plan_reviewer", "review exact plan sentinel")
	if err != nil {
		t.Fatal(err)
	}
	if prompt == "" {
		t.Fatal("missing compiled prompt")
	}
	if authority, _ := invocationMetadataFromContext(ctx); !reflect.DeepEqual(authority, parent) {
		t.Fatalf("diagnostics changed parent authority: %+v", authority)
	}
	snapshot, ok := InvocationMetadataSnapshot(ctx)
	if !ok || snapshot.AgentName != "plan_reviewer" || snapshot.ParentRequestID == "parent-request" || snapshot.Purpose != "plan_reviewer" {
		t.Fatalf("auxiliary identity was inherited: %+v", snapshot)
	}
	var matched bool
	for _, manifest := range c.SessionData().CoordinatorContextManifests {
		if manifest.RequestID == snapshot.ParentRequestID && manifest.Fingerprint == snapshot.ParentManifestFingerprint {
			matched = true
		}
	}
	if !matched {
		t.Fatal("diagnostics did not bind the exact persisted manifest")
	}
	child := parent
	child.TaskID = "2"
	child.AgentName = "child"
	childCtx := withInvocationMetadata(ctx, child)
	childSnapshot, _ := InvocationMetadataSnapshot(childCtx)
	if childSnapshot.AgentName != "child" || childSnapshot.TaskID != "2" {
		t.Fatalf("nested child inherited auxiliary identity: %+v", childSnapshot)
	}
}
