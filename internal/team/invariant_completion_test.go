package team

import (
	"fmt"
	"strings"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func invariantCompletionTodo(runID, taskID string, mode InvariantVerificationMode, status TaskStatus, assessments []InvariantAssessment) *TodoItem {
	manifest := ContextInjectionManifest{
		SchemaVersion: ContextManifestSchemaVersion, RequestID: "request-" + taskID, RequestHash: "hash",
		RunID: runID, TaskID: taskID, Attempt: 1, Agent: "reviewer", AgentRole: "worker",
		ModelExecutionID: "model-" + taskID, Phase: PhaseVerify, Trigger: ContextTriggerTaskDispatch,
		ModelCalled: true, Outcome: "model_call", CreatedAt: time.Unix(100, 0).UTC(),
	}
	findings := make([]Finding, 0)
	for i := range assessments {
		assessment := &assessments[i]
		manifest.Items = append(manifest.Items, ContextManifestItem{
			ID: assessment.ContextItemID, Kind: string(contextstore.ContextInvariant), Source: repositoryInvariantSource,
			Included: true, Reason: ContextIncludedRequired, ContentHash: assessment.InvariantContentHash,
			InvariantSeverity: assessment.Severity,
		})
		if assessment.Status == InvariantViolated {
			index := len(findings)
			assessment.FindingIndex = new(index)
			findings = append(findings, Finding{Summary: "violation", Severity: string(assessment.Severity)})
		}
	}
	manifest.Fingerprint = contextManifestFingerprint(manifest)
	return &TodoItem{
		ID: taskID, Agent: "reviewer", Status: status, InvariantVerification: mode,
		ContextManifests: []ContextInjectionManifest{manifest},
		TypedResult: &TaskResult{
			TaskID: taskID, Attempt: 1, Agent: "reviewer", Status: TaskResultStatusSuccess, Source: "submitted",
			Findings:              findings,
			InvariantVerification: &InvariantVerificationResult{ContextManifestFingerprint: manifest.Fingerprint, Assessments: assessments},
		},
	}
}

func completionAssessment(id string, severity InvariantSeverity, status InvariantAssessmentStatus) InvariantAssessment {
	return InvariantAssessment{
		InvariantID: id, ContextItemID: "invariant:team:" + id,
		InvariantContentHash: strings.Repeat("a", 64), Severity: severity, Status: status, Summary: "assessment " + id,
	}
}

func TestEvaluateSemanticRegressionModesAndSeverities(t *testing.T) {
	runID := "run-current"
	tests := []struct {
		name       string
		tasks      []*TodoItem
		configured bool
		clear      bool
		blocks     int
	}{
		{name: "legacy", clear: true},
		{name: "report error violation ignored", tasks: []*TodoItem{invariantCompletionTodo(runID, "report", InvariantVerificationReport, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantViolated)})}, clear: true},
		{name: "gate preserved", tasks: []*TodoItem{invariantCompletionTodo(runID, "gate", InvariantVerificationGate, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantPreserved)})}, configured: true, clear: true},
		{name: "warning violation", tasks: []*TodoItem{invariantCompletionTodo(runID, "gate", InvariantVerificationGate, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityWarning, InvariantViolated)})}, configured: true, clear: true},
		{name: "error violation", tasks: []*TodoItem{invariantCompletionTodo(runID, "gate", InvariantVerificationGate, TaskError, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantViolated)})}, configured: true, blocks: 1},
		{name: "error unknown", tasks: []*TodoItem{invariantCompletionTodo(runID, "gate", InvariantVerificationGate, TaskBlocked, []InvariantAssessment{{InvariantID: "safe", ContextItemID: "invariant:team:safe", InvariantContentHash: strings.Repeat("a", 64), Severity: InvariantSeverityError, Status: InvariantUnknown, Summary: "unknown", MissingEvidence: []string{"test output"}}})}, configured: true, blocks: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := EvaluateSemanticRegression(runID, tt.tasks)
			if decision.Configured != tt.configured || decision.Clear != tt.clear || decision.BlockingCount != tt.blocks {
				t.Fatalf("decision = %#v", decision)
			}
		})
	}
}

func TestValidateInvariantVerificationResultRejectsStructuralMismatch(t *testing.T) {
	base := invariantCompletionTodo("run-current", "gate", InvariantVerificationGate, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantPreserved)})
	tests := []struct {
		name string
		edit func(*TodoItem)
		code string
	}{
		{name: "typed result", edit: func(item *TodoItem) { item.TypedResult = nil }, code: "typed_result_missing"},
		{name: "envelope", edit: func(item *TodoItem) { item.TypedResult.InvariantVerification = nil }, code: "envelope_missing"},
		{name: "manifest", edit: func(item *TodoItem) { item.ContextManifests = nil }, code: "manifest_missing"},
		{name: "version", edit: func(item *TodoItem) { item.ContextManifests[0].SchemaVersion = 1 }, code: "manifest_version"},
		{name: "run", edit: func(item *TodoItem) {
			item.ContextManifests[0].RunID = "old"
			item.ContextManifests[0].Fingerprint = contextManifestFingerprint(item.ContextManifests[0])
			item.TypedResult.InvariantVerification.ContextManifestFingerprint = item.ContextManifests[0].Fingerprint
		}, code: "run_mismatch"},
		{name: "identity", edit: func(item *TodoItem) { item.TypedResult.Attempt = 2 }, code: "identity_mismatch"},
		{name: "agent identity", edit: func(item *TodoItem) { item.TypedResult.Agent = "other" }, code: "identity_mismatch"},
		{name: "set", edit: func(item *TodoItem) { item.TypedResult.InvariantVerification.Assessments = []InvariantAssessment{} }, code: "assessment_set_mismatch"},
		{name: "hash", edit: func(item *TodoItem) {
			item.TypedResult.InvariantVerification.Assessments[0].InvariantContentHash = strings.Repeat("b", 64)
		}, code: "attestation_invalid"},
		{name: "summary bound", edit: func(item *TodoItem) {
			item.TypedResult.InvariantVerification.Assessments[0].Summary = strings.Repeat("界", maxInvariantAssessmentSummaryRunes+1)
		}, code: "attestation_invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := cloneTodoItem(base)
			tt.edit(item)
			validation := ValidateInvariantVerificationResult(item, "run-current")
			if validation.Valid || validation.Code != tt.code || validation.Assessments != nil {
				t.Fatalf("validation = %#v, want code %q", validation, tt.code)
			}
		})
	}
}

func TestEvaluateSemanticRegressionRequiresDoneWithoutSemanticBlocker(t *testing.T) {
	item := invariantCompletionTodo("run-current", "gate", InvariantVerificationGate, TaskError, []InvariantAssessment{completionAssessment("safe", InvariantSeverityInfo, InvariantPreserved)})
	decision := EvaluateSemanticRegression("run-current", []*TodoItem{item})
	if decision.Clear || decision.BlockingCount != 1 || len(decision.Reasons) != 1 || !strings.Contains(decision.Reasons[0], "task_not_done") {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestEvaluateSemanticRegressionBoundsReasons(t *testing.T) {
	assessments := make([]InvariantAssessment, 55)
	for i := range assessments {
		assessments[i] = completionAssessment(fmt.Sprintf("inv-%02d", i), InvariantSeverityError, InvariantViolated)
	}
	item := invariantCompletionTodo("run-current", "gate", InvariantVerificationGate, TaskError, assessments)
	decision := EvaluateSemanticRegression("run-current", []*TodoItem{item})
	if decision.BlockingCount != 55 || len(decision.Reasons) != maxSemanticRegressionReasons || decision.Reasons[49] != "and 6 additional invariant blockers" {
		t.Fatalf("bounded decision = %#v", decision)
	}
}

func TestCurrentGateResultResolvesPriorAttempt(t *testing.T) {
	old := invariantCompletionTodo("old-run", "gate", InvariantVerificationGate, TaskError, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantViolated)})
	current := invariantCompletionTodo("run-current", "gate", InvariantVerificationGate, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantPreserved)})
	current.ContextManifests = append(old.ContextManifests, current.ContextManifests...)
	decision := EvaluateSemanticRegression("run-current", []*TodoItem{current})
	if !decision.Clear {
		t.Fatalf("current preserved result did not resolve prior violation: %#v", decision)
	}
}
