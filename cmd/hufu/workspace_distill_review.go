package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/taskdistill"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
	"github.com/spf13/cobra"
)

const operatorKnowledgeSelectionPolicy = "Explicit operator selection of existing parent knowledge. Preserve selected text and provenance exactly; omit unselected knowledge without authorizing source deletion."

func newWorkspaceDistillReviewCommand(deps workspaceCommandDeps) *cobra.Command {
	options := workspaceDistillOptions{maxCalls: taskdistill.DefaultMaxModelCalls}
	cmd := &cobra.Command{
		Use: "review <plan-id>", Short: "Save an explicitly selected knowledge subset as a new plan without model calls or application",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.review = args[0]
			return runWorkspaceDistill(cmd, deps, options)
		},
	}
	cmd.Flags().StringVar(&options.team, "team", "", "Destination team workspace (required)")
	cmd.Flags().IntSliceVar(&options.keepKnowledge, "keep-knowledge", nil, "Explicit zero-based parent knowledge indices to retain; comma-separated (required)")
	return cmd
}

func selectReviewedKnowledge(parent workspaceDistillationPlan, indices []int) (taskdistill.Refinement, taskdistill.Draft, []int, error) {
	selection := slices.Clone(indices)
	slices.Sort(selection)
	if len(selection) == 0 {
		return taskdistill.Refinement{}, taskdistill.Draft{}, nil, errors.New("--keep-knowledge requires at least one explicit zero-based index")
	}
	selected := map[string]bool{}
	for _, index := range selection {
		if index < 0 || index >= len(parent.Draft.Knowledge) || selected[fmt.Sprintf("fact_%d", index)] {
			return taskdistill.Refinement{}, taskdistill.Draft{}, nil, errors.New("knowledge selection contains an out-of-range or repeated index")
		}
		selected[fmt.Sprintf("fact_%d", index)] = true
	}
	batches, err := taskdistill.PrepareRefinement(parent.Draft, parent.Sources, operatorKnowledgeSelectionPolicy, taskdistill.MaxModelCalls)
	if err != nil {
		return taskdistill.Refinement{}, taskdistill.Draft{}, nil, err
	}
	evidence := taskdistill.Refinement{Batches: []taskdistill.Draft{}, Knowledge: []taskdistill.Knowledge{}}
	for _, batch := range batches {
		filtered := taskdistill.Draft{Decisions: []taskdistill.Decision{}, Knowledge: []taskdistill.Knowledge{}}
		for _, source := range batch {
			action, reason := "discard", "Operator omitted this parent knowledge item from the reviewed subset; preserve the original source."
			if selected[source.ID] {
				action, reason = "retain", "Operator explicitly selected this parent knowledge item verbatim."
			}
			filtered.Decisions = append(filtered.Decisions, taskdistill.Decision{SourceID: source.ID, Action: action, Reason: reason})
		}
		evidence.Batches = append(evidence.Batches, filtered)
	}
	for _, index := range selection {
		evidence.Knowledge = append(evidence.Knowledge, taskdistill.Knowledge{Text: parent.Draft.Knowledge[index].Text, SourceIDs: []string{fmt.Sprintf("fact_%d", index)}})
	}
	draft, err := taskdistill.RebuildRefinement(parent.Draft, parent.Sources, operatorKnowledgeSelectionPolicy, evidence)
	return evidence, draft, selection, err
}

func reviewWorkspaceDistillation(cmd *cobra.Command, manager *workspacepkg.WorkspaceManager, destination workspacepkg.Resolution, options workspaceDistillOptions, stateRoot string) error {
	parent, err := loadWorkspaceDistillationPlan(cmd.Context(), destination, options.review, 1)
	if err != nil {
		return err
	}
	indices := options.keepKnowledge
	evidence, draft, selection, err := selectReviewedKnowledge(parent, indices)
	if err != nil {
		return err
	}
	if err = validateRefinementSources(cmd, manager, destination, stateRoot, parent); err != nil {
		return err
	}
	plan := parent
	plan.ID = ""
	plan.Policy, plan.MaxModelCalls, plan.Draft = operatorKnowledgeSelectionPolicy, 0, draft
	plan.Refinement = &workspaceDistillationRefinement{ParentPlanID: parent.ID, Evidence: evidence, OperatorSelection: selection}
	plan.ID = taskdistill.Digest(plan)
	store, err := runtimestore.Open(cmd.Context(), destination.ControlRoot)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err = store.PutDocument(cmd.Context(), "task_distillation_plan:"+plan.ID, raw); err != nil {
		return err
	}
	return writeDistillationJSON(cmd, plan)
}
