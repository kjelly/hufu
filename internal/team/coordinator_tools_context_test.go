package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestContextQueryToolUsesBoundInvocationModelContext(t *testing.T) {
	c, _ := rankingTestCoordinator(t, agent.MemoryLearningOff)
	compileErr := errors.New("stop after recording bound model context")
	compiler := &mockContextCompiler{compileWorkerErr: compileErr}
	c.SetContextCompiler(compiler)

	_, err := (&contextQueryTool{coordinator: c}).Run(withTestAuxiliaryInvocationContext(t.Context()), fantasy.ToolCall{
		ID:    "context-call-1",
		Input: `{"query":"bounded lookup"}`,
	})
	if err != nil {
		t.Fatalf("context query returned transport error: %v", err)
	}
	if compiler.workerModelContext != testAuxiliaryInvocationModelContext() {
		t.Fatalf("context query compiler model context = %#v, want %#v", compiler.workerModelContext, testAuxiliaryInvocationModelContext())
	}
}

func TestCompileRoutedContextForToolFailsClosedWithoutBoundInvocationContext(t *testing.T) {
	c := newDirectTerminationCoordinator(t, &contextManifestCountingAgent{})
	compiler := &mockContextCompiler{}
	c.SetContextCompiler(compiler)

	_, err := compileRoutedContextForTool(t.Context(), c, ContextRequest{}, ContextRoute{})
	if err == nil || !strings.Contains(err.Error(), "provider-bound context unavailable") {
		t.Fatalf("unbound context-tool compilation error = %v", err)
	}
	if compiler.workerModelContext != (ModelContextSpec{}) {
		t.Fatalf("unbound context-tool compilation invoked compiler with model context %#v", compiler.workerModelContext)
	}
}

func TestContextToolRequestInheritsRetryInvocationMetadata(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	c.executionRunID = "run-current"
	item := rankingItem("retry-only", 30)
	item.Metadata = map[string]string{"activation.triggers": "retry"}
	if err := repo.Append(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	metadata := InvocationMetadata{
		RunID: "run-current", TaskID: "task-1", AgentName: "worker", AgentRole: "worker",
		ModelExecutionID: "model-execution-1", Attempt: 2, Phase: PhaseExecute, Trigger: ContextTriggerRetry,
		ParentRequestID: "ctx-parent", ParentManifestFingerprint: "manifest-parent",
	}
	ctx := withInvocationMetadata(context.Background(), metadata)
	request := c.contextToolRequest(ctx, "look up retry guidance", "", nil)
	if request.Trigger != ContextTriggerRetry || request.Purpose != "task_retry" || request.Phase != PhaseExecute || request.Attempt != 2 || request.ModelExecutionID != "model-execution-1" {
		t.Fatalf("inherited request = %#v", request)
	}
	if request.ParentTrigger != ContextTriggerRetry || request.ParentRequestID != "ctx-parent" || request.ParentManifestFingerprint != "manifest-parent" {
		t.Fatalf("missing parent identity: %#v", request)
	}
	if _, reason, err := c.GetAuthorizedContextItem(ctx, request, item.ID); err != nil || reason != ContextIncludedRelevant {
		t.Fatalf("retry item authorization = %q, %v", reason, err)
	}

	dispatch := metadata
	dispatch.Attempt = 1
	dispatch.Trigger = ContextTriggerTaskDispatch
	dispatch.ParentRequestID = "ctx-dispatch"
	dispatch.ParentManifestFingerprint = "manifest-dispatch"
	dispatchRequest := c.contextToolRequest(withInvocationMetadata(context.Background(), dispatch), "look up dispatch guidance", "", nil)
	if dispatchRequest.Purpose != "task_execution" {
		t.Fatalf("dispatch purpose = %q", dispatchRequest.Purpose)
	}
	if _, reason, err := c.GetAuthorizedContextItem(context.Background(), dispatchRequest, item.ID); err == nil || reason != ContextOmittedTrigger {
		t.Fatalf("dispatch accessed retry-only item: reason=%q err=%v", reason, err)
	}
}

func TestGetAuthorizedContextItemUsesTypedActivationProjection(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	c.executionRunID = "run-current"
	item := rankingItem("verify-only", 30)
	item.Metadata = map[string]string{"activation.phases": "VERIFY"}
	if err := repo.Append(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	request := validTestContextRequest()
	request.RunID = "run-current"
	request.Phase = PhaseExecute
	request.AssignRequestID()
	if _, reason, err := c.GetAuthorizedContextItem(context.Background(), request, item.ID); err == nil || reason != ContextOmittedPhase {
		t.Fatalf("execute accessed typed VERIFY-only item: reason=%q err=%v", reason, err)
	}
	request.Phase = PhaseVerify
	request.AssignRequestID()
	got, reason, err := c.GetAuthorizedContextItem(context.Background(), request, item.ID)
	if err != nil || reason != ContextIncludedRelevant || got.ID != item.ID {
		t.Fatalf("verify authorization = %#v, %q, %v", got, reason, err)
	}
}

func TestContextToolRequestWithoutInvocationUsesContextToolPurpose(t *testing.T) {
	c, _ := rankingTestCoordinator(t, agent.MemoryLearningOff)
	request := c.contextToolRequest(context.Background(), "bounded lookup", "", nil)
	if request.Trigger != ContextTriggerAuxiliary || request.Purpose != "context_tool" {
		t.Fatalf("fallback context-tool request = %#v", request)
	}
}

func TestGetAuthorizedContextItemRejectsForeignCandidate(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	c.executionRunID = "run-current"
	item := contextstore.ContextItem{ID: "foreign-candidate", Kind: contextstore.ContextObservation, Content: "candidate from another run", Scope: c.contextScope(), Lifecycle: contextstore.LifecycleCandidate, Metadata: map[string]string{"run_id": "other-run"}}
	if err := repo.Append(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	request := validTestContextRequest()
	request.RunID = "run-current"
	request.AssignRequestID()
	if _, reason, err := c.GetAuthorizedContextItem(context.Background(), request, item.ID); err == nil || reason != ContextOmittedLifecycle {
		t.Fatalf("foreign candidate authorization = %q, %v", reason, err)
	}
}
