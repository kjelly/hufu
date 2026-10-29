package team

import (
	"errors"
	"strings"
	"testing"
)

func dependencyViewContextFixture(t *testing.T) WorkerContextInput {
	t.Helper()
	result, source := dependencyViewRenderFixture()
	policy := &DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}
	view, err := buildDependencyResultView(policy, []TaskResult{result}, []DependencyResultViewSource{source})
	if err != nil {
		t.Fatal(err)
	}
	return WorkerContextInput{
		TaskDef: TaskDef{Agent: "worker", Goal: "consume", ContractID: "consume", ContractHash: strings.Repeat("d", 64), ContractRevision: selectedDependencyContractRevision, DependencyResultView: policy},
		Goal:    "consume", DependencyResults: []TaskResult{result}, DependencyView: view,
		DisableMemory: true, DisableCanonicalMemory: true,
		ModelContext: ModelContextSpec{ModelID: "test-selected", ContextWindow: 1_000_000, MaxOutputTokens: 256, SafetyMarginTokens: 64, Estimator: "estimated"},
	}
}

func TestDependencyResultViewContextDeliveryAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*WorkerContextInput)
		budget bool
	}{
		{"complete", func(*WorkerContextInput) {}, false},
		{"insufficient budget", func(input *WorkerContextInput) { input.ModelContext.ContextWindow = 350 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := dependencyViewContextFixture(t)
			tc.change(&input)
			compiled, err := CompileWorkerContext(t.Context(), input)
			if tc.budget {
				if _, ok := errors.AsType[*RequiredContextBudgetError](err); !ok {
					t.Fatalf("required selected context did not reject budget: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(compiled.Prompt, "unselected_details") || !strings.Contains(compiled.Prompt, "summary_sentinel") {
				t.Fatal("worker context did not retain exactly the selected presentation")
			}
			if err := validateRequiredEvidenceContext(input, compiled); err != nil {
				t.Fatal(err)
			}
			for i := range compiled.IncludedItems {
				if compiled.IncludedItems[i].ID == "dependency_results" {
					compiled.IncludedItems[i].Content = "changed"
				}
			}
			if err := validateRequiredEvidenceContext(input, compiled); err == nil {
				t.Fatal("changed selected context survived dispatch validation")
			}
		})
	}
}

func TestDependencyResultViewContextRejectsUnpreparedOrChangedInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*WorkerContextInput)
	}{
		{"unprepared", func(input *WorkerContextInput) { input.DependencyView = nil }},
		{"content", func(input *WorkerContextInput) { input.DependencyView.content = "changed" }},
		{"source", func(input *WorkerContextInput) { input.DependencyResults[0].Summary = "changed" }},
		{"policy", func(input *WorkerContextInput) { input.TaskDef.DependencyResultView.Fields = []string{"findings"} }},
		{"full override", func(input *WorkerContextInput) { input.TaskDef.DependencyResultView = nil }},
		{"required evidence", func(input *WorkerContextInput) { input.TaskDef.Execution.RequiresEvidence = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := dependencyViewContextFixture(t)
			tc.change(&input)
			if _, err := CompileWorkerContext(t.Context(), input); err == nil {
				t.Fatal("unprepared or changed view was compiled")
			}
		})
	}
}

func TestDependencyResultViewContextRejectsCompilerSourceRebinding(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*DependencyResultViewSource)
	}{
		{"run", func(source *DependencyResultViewSource) { source.RunID = "another-run" }},
		{"event", func(source *DependencyResultViewSource) { source.SourceEventID = "another-event" }},
		{"branch", func(source *DependencyResultViewSource) { source.BranchID = "another-branch" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			input := dependencyViewContextFixture(t)
			compiled, err := CompileWorkerContext(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			for i := range compiled.IncludedItems {
				item := &compiled.IncludedItems[i]
				if item.DependencyResultView != nil {
					item.DependencyResultView = item.DependencyResultView.Clone()
					change.apply(&item.DependencyResultView.Sources[0])
				}
			}
			if err := validateRequiredEvidenceContext(input, compiled); err == nil {
				t.Fatal("compiler rebound runtime-prepared source identity before dispatch")
			}
		})
	}
}
