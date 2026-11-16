package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/spf13/cobra"
)

func prepareWorkspaceDistillationBatches(ctx context.Context, workspace string, sources []taskdistill.Source, options workspaceDistillOptions) ([][]taskdistill.Source, error) {
	budget := options.maxCalls
	if options.probeBatch > 0 {
		budget = taskdistill.MaxModelCalls // Bound the inventory, not unselected model calls.
	}
	batches, err := taskdistill.PrepareBatches(sources, options.policy, budget)
	if err != nil && options.probeBatch == 0 && options.maxCalls >= 1 && options.maxCalls <= taskdistill.MaxModelCalls {
		// Keep fresh-work preflight before provider construction. When records
		// exist, scope admission is needed before any cache discount is safe.
		inventory, inventoryErr := taskdistill.PrepareBatches(sources, options.policy, taskdistill.MaxModelCalls)
		if inventoryErr == nil {
			cached, cacheErr := hasDistillationCheckpoints(ctx, workspace)
			if cacheErr != nil {
				return nil, cacheErr
			}
			if cached {
				batches, err = inventory, nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if options.probeBatch > len(batches) {
		return nil, fmt.Errorf("probe batch %d exceeds %d extraction batches", options.probeBatch, len(batches))
	}
	return batches, nil
}

type distillationProbeResult struct {
	Kind            string            `json:"kind"`
	Batch           int               `json:"batch"`
	TotalBatches    int               `json:"total_batches"`
	MaxModelCalls   int               `json:"max_model_calls"`
	AuditWorkspaces []string          `json:"audit_workspaces"`
	Draft           taskdistill.Draft `json:"draft"`
}

func runWorkspaceDistillationProbe(cmd *cobra.Command, generator maintenanceTextGenerator, plan workspaceDistillationPlan, options workspaceDistillOptions, total int) error {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Distillation probe: extraction batch %d/%d; maximum %d model calls. No applicable plan or source cleanup.\n", options.probeBatch, total, options.maxCalls)
	draft, err := taskdistill.ProbeBatch(cmd.Context(), generator, plan.Sources, options.policy, options.probeBatch, options.maxCalls)
	if err != nil {
		return fmt.Errorf("probe batch %d/%d: %w", options.probeBatch, total, err)
	}
	audits := []string{plan.DraftWorkspace}
	if cached, ok := generator.(*distillationCheckpointGenerator); ok {
		audits = slices.Clone(cached.audits)
	}
	return writeDistillationJSON(cmd, distillationProbeResult{Kind: "distillation_probe", Batch: options.probeBatch, TotalBatches: total, MaxModelCalls: options.maxCalls, AuditWorkspaces: audits, Draft: draft})
}
