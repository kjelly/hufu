package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAuxiliaryRepairPreservesAdmittedInputsBeyondDiagnosticLimit(t *testing.T) {
	for _, purpose := range []string{"result_repair", "protocol_repair"} {
		t.Run(purpose, func(t *testing.T) {
			c, _ := newEvidenceRepairCoordinator(t, "repair-inputs")
			compiled, ref := compiledReviewContract(t, true)
			c.session.ResultContracts = map[string]*CompiledResultContract{ref.ID: compiled}
			var ids []string
			var sources []*TodoItem
			for index := range 14 {
				source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer"}})[0]
				source.Status = TaskDone
				source.TypedResult = &TaskResult{TaskID: source.ID, Agent: source.Agent, Status: TaskResultStatusSuccess, Summary: "accepted source"}
				source.ResultContract = &ref
				raw, _ := json.Marshal(map[string]any{"verdict": "approve", "findings": []any{map[string]any{"summary": strings.Repeat("evidence ", 500) + fmt.Sprintf("SOURCE-END-%d", index)}}})
				payload, err := validateStructuredResultPayload(compiled, ref, raw)
				if err != nil {
					t.Fatal(err)
				}
				source.TypedResult.StructuredPayload = payload
				ids = append(ids, source.ID)
				sources = append(sources, source)
			}
			constraints := strings.Repeat("receipt-verifiable constraint ", 1000) + "ATTESTATION-END"
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Goal: "Admitted task", ResultContract: &ref, Constraints: constraints, EvidenceFrom: ids, Execution: ExecutionContract{RequiresEvidence: true, MaxEvidenceSources: 64}}})[0]
			ctx := withTestAuxiliaryInvocationContext(context.WithValue(t.Context(), todoIDKey{}, item.ID))
			invocation, _ := providerBoundInvocationContextFromContext(ctx, "")
			invocation.AdmissionContext.ContextWindow = 200000
			invocation.ModelContext.ContextWindow = 200000
			ctx = withProviderBoundInvocationContext(ctx, invocation)
			prompt, err := c.prepareAuxiliaryPromptWithPersistence(ctx, purpose, "Repair the rejected submission", false)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{constraints, FormatDependencyResults(c.dependencyResultsForTask(item.ID)), "Admitted task", string(compiled.CanonicalSchema), "## Result Contract"} {
				if !strings.Contains(prompt, want) {
					t.Fatal("required repair input omitted or truncated")
				}
			}
			small := withTestAuxiliaryInvocationContext(context.WithValue(t.Context(), todoIDKey{}, item.ID))
			if _, err := c.prepareAuxiliaryPromptWithPersistence(small, purpose, "Repair", false); err == nil {
				t.Fatal("truncated required inputs to fit a small context window")
			}
			source := sources[0]
			originalHash := source.TypedResult.StructuredPayload.SHA256
			source.TypedResult.StructuredPayload.SHA256 = strings.Repeat("0", 64)
			if _, err := c.prepareAuxiliaryPromptWithPersistence(ctx, purpose, "Repair", false); err == nil {
				t.Fatal("accepted tampered source hash")
			}
			source.TypedResult.StructuredPayload.SHA256 = originalHash
			// Missing accepted results cannot be replaced with model guesses.
			unfinished := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer"}})[0]
			item.EvidenceFrom = append(item.EvidenceFrom, unfinished.ID)
			if _, err := c.prepareAuxiliaryPromptWithPersistence(ctx, purpose, "Repair", false); err == nil {
				t.Fatal("accepted unavailable dependency")
			}
		})
	}
}

func TestAuxiliaryReviewDoesNotInheritTaskDependencies(t *testing.T) {
	c, _ := newEvidenceRepairCoordinator(t, "isolated-review")
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Constraints: "PRIVATE-TASK-CONSTRAINT"}})[0]
	ctx := withTestAuxiliaryInvocationContext(context.WithValue(t.Context(), todoIDKey{}, item.ID))
	prompt, err := c.prepareAuxiliaryPromptWithPersistence(ctx, "guard_reviewer", "Review this candidate", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "PRIVATE-TASK-CONSTRAINT") {
		t.Fatal("auxiliary review inherited unrelated task input")
	}
}
