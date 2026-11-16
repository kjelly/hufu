package main

import (
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/taskdistill"
)

// Extraction-only admission changes what can be proposed, never what may be
// deleted. Keep the complete original sources (including protection) in the
// hashed plan, and project only the model's cleanup classification inputs.
func distillationGenerationSources(plan workspaceDistillationPlan) []taskdistill.Source {
	if !plan.ExtractOnly {
		return plan.Sources
	}
	sources := slices.Clone(plan.Sources)
	for i := range sources {
		sources[i].Protected = false
	}
	return sources
}

func retainDistillationSources(draft *taskdistill.Draft) {
	for i, decision := range draft.Decisions {
		action := "retain"
		if decision.Action == "distill" || decision.Action == taskdistill.ActionExtractOnly {
			action = taskdistill.ActionExtractOnly
		}
		draft.Decisions[i] = taskdistill.Decision{SourceID: decision.SourceID, Action: action, Reason: "Extraction-only admission preserves the complete source. Original extraction judgments remain in the model call evidence."}
	}
}

func validateDistillationPlanDraft(plan workspaceDistillationPlan) error {
	if plan.ExtractOnly {
		for _, decision := range plan.Draft.Decisions {
			if !taskdistill.RetainsSource(decision.Action) {
				return fmt.Errorf("%w: extraction-only plan cannot authorize source deletion", taskdistill.ErrInvalid)
			}
		}
	}
	return taskdistill.Validate(plan.Draft, plan.Sources)
}
