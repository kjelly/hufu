package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
	"github.com/spf13/cobra"
)

type workspaceDistillationRefinement struct {
	ParentPlanID      string                 `json:"parent_plan_id"`
	Evidence          taskdistill.Refinement `json:"evidence"`
	OperatorSelection []int                  `json:"operator_selection,omitempty"`
}

func newWorkspaceDistillRefineCommand(deps workspaceCommandDeps) *cobra.Command {
	options := workspaceDistillOptions{}
	cmd := &cobra.Command{
		Use: "refine <plan-id>", Short: "Filter and merge a saved plan into a new reviewable plan; preserve sources and checkpoints",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.refine = args[0]
			return runWorkspaceDistill(cmd, deps, options)
		},
	}
	cmd.Flags().StringVar(&options.team, "team", "", "Destination team workspace (required)")
	cmd.Flags().StringVar(&options.model, "model", "", "Refinement model; defaults to the parent plan model")
	cmd.Flags().StringVar(&options.policy, "policy", taskdistill.DefaultRefinementPolicy, "Guidance for filtering and merging parent knowledge")
	cmd.Flags().StringVar(&options.searchPath, "team-search-path", "", "Comma-separated team search paths")
	cmd.Flags().IntVar(&options.maxCalls, "max-model-calls", taskdistill.DefaultMaxModelCalls, "Shared refinement, repair and merge call budget (maximum 4096)")
	return cmd
}

func loadWorkspaceDistillationPlan(ctx context.Context, destination workspacepkg.Resolution, id string, depth int) (workspaceDistillationPlan, error) {
	if depth > 8 {
		return workspaceDistillationPlan{}, errors.New("distillation refinement ancestry exceeds 8 levels")
	}
	store, err := runtimestore.OpenRead(ctx, destination.ControlRoot)
	if err != nil {
		return workspaceDistillationPlan{}, err
	}
	raw, exists, readErr := store.GetDocument(ctx, "task_distillation_plan:"+id)
	if err = errors.Join(readErr, store.Close()); err != nil {
		return workspaceDistillationPlan{}, err
	}
	if !exists {
		return workspaceDistillationPlan{}, errors.New("distillation preview not found")
	}
	var plan workspaceDistillationPlan
	if err = json.Unmarshal(raw, &plan, json.RejectUnknownMembers(true)); err != nil {
		return workspaceDistillationPlan{}, err
	}
	savedID := plan.ID
	plan.ID = ""
	if savedID != id || taskdistill.Digest(plan) != id || plan.ProjectID != destination.ProjectID || plan.WorkspaceID != destination.WorkspaceID || plan.ContextScopeID != destination.ContextScopeID || plan.Team != destination.TeamName {
		return workspaceDistillationPlan{}, errors.New("distillation preview identity mismatch")
	}
	plan.ID = id
	if err = validateDistillationPlanDraft(plan); err != nil {
		return workspaceDistillationPlan{}, err
	}
	if plan.Refinement == nil {
		return plan, nil
	}
	parent, err := loadWorkspaceDistillationPlan(ctx, destination, plan.Refinement.ParentPlanID, depth+1)
	if err != nil {
		return workspaceDistillationPlan{}, fmt.Errorf("refinement parent: %w", err)
	}
	if plan.ExtractOnly != parent.ExtractOnly || taskdistill.Digest(plan.Sources) != taskdistill.Digest(parent.Sources) || taskdistill.Digest(plan.Tasks) != taskdistill.Digest(parent.Tasks) || taskdistill.Digest(plan.Trash) != taskdistill.Digest(parent.Trash) {
		return workspaceDistillationPlan{}, errors.New("refinement changed the parent source inventory")
	}
	if plan.MaxModelCalls == 0 || len(plan.Refinement.OperatorSelection) != 0 {
		if plan.Policy != operatorKnowledgeSelectionPolicy || plan.MaxModelCalls != 0 {
			return workspaceDistillationPlan{}, errors.New("operator review policy mismatch")
		}
		expected, _, selection, err := selectReviewedKnowledge(parent, plan.Refinement.OperatorSelection)
		if err != nil {
			return workspaceDistillationPlan{}, err
		}
		if !slices.Equal(selection, plan.Refinement.OperatorSelection) || taskdistill.Digest(expected) != taskdistill.Digest(plan.Refinement.Evidence) {
			return workspaceDistillationPlan{}, errors.New("operator review evidence does not match its explicit selection")
		}
	}
	rebuilt, err := taskdistill.RebuildRefinement(parent.Draft, parent.Sources, plan.Policy, plan.Refinement.Evidence)
	if err != nil {
		return workspaceDistillationPlan{}, err
	}
	if taskdistill.Digest(rebuilt) != taskdistill.Digest(plan.Draft) {
		return workspaceDistillationPlan{}, errors.New("refinement draft does not match its provenance")
	}
	return plan, nil
}

func validateRefinementSources(cmd *cobra.Command, manager *workspacepkg.WorkspaceManager, destination workspacepkg.Resolution, stateRoot string, parent workspaceDistillationPlan) error {
	completed, err := validateDistillationTrash(cmd.Context(), manager, destination, stateRoot, parent)
	if err != nil {
		return err
	}
	if len(completed) != 0 {
		return errors.New("cannot refine a plan whose source was already purged")
	}
	if parent.Tasks != nil {
		var ids []string
		for _, source := range parent.Tasks.Sources {
			ids = append(ids, source.ID)
		}
		current, err := team.SnapshotTasksForDistillation(cmd.Context(), destination.ControlRoot, ids)
		if err != nil || current.Fingerprint != parent.Tasks.Fingerprint {
			return errors.New("task state changed; create a new extraction preview")
		}
	}
	return nil
}

func refineWorkspaceDistillation(cmd *cobra.Command, manager *workspacepkg.WorkspaceManager, destination workspacepkg.Resolution, options workspaceDistillOptions, stateRoot string) error {
	parent, err := loadWorkspaceDistillationPlan(cmd.Context(), destination, options.refine, 1)
	if err != nil {
		return err
	}
	if err = validateRefinementSources(cmd, manager, destination, stateRoot, parent); err != nil {
		return err
	}
	batches, err := taskdistill.PrepareRefinement(parent.Draft, parent.Sources, options.policy, options.maxCalls)
	if err != nil {
		return err
	}
	registry := teamRegistryFromSearchPath(options.searchPath)
	if err = registry.Discover(); err != nil {
		return err
	}
	dir, err := registry.Resolve(options.team)
	if err != nil {
		return err
	}
	plan := parent
	plan.ID = ""
	plan.Policy, plan.MaxModelCalls = options.policy, options.maxCalls
	plan.Refinement = &workspaceDistillationRefinement{ParentPlanID: parent.ID}
	plan.DraftWorkspace, err = createDistillationDraftWorkspace(destination.ControlRoot)
	if err != nil {
		return err
	}
	model := options.model
	if model == "" {
		model = parent.Model
	}
	generationCtx := agent.WithProviderCallBudget(cmd.Context(), options.maxCalls)
	generator, model, release, err := taskDistillationGeneratorFactory(generationCtx, dir, plan.DraftWorkspace, model)
	if err != nil {
		return err
	}
	if release == nil {
		return errors.New("distillation generator did not provide cleanup")
	}
	defer release()
	plan.Model = model
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Distillation refinement: %d proposed facts; %d filter batch(es); maximum %d total model calls. Original source data is not sent again. No knowledge is applied and no sources are deleted.\n", len(parent.Draft.Knowledge), len(batches), options.maxCalls)
	progress := distillationProgressGenerator{generator, cmd, true}
	cached, closeCache, err := newDistillationCheckpointGenerator(cmd, destination.ControlRoot, plan, generator, progress)
	if err != nil {
		return err
	}
	defer closeCache()
	plan.Refinement.Evidence, plan.Draft, err = taskdistill.Refine(cmd.Context(), cached, parent.Draft, parent.Sources, options.policy, options.maxCalls)
	if err != nil {
		return err
	}
	plan.AuditWorkspaces = append(slices.Clone(parent.AuditWorkspaces), plan.DraftWorkspace)
	if parent.DraftWorkspace != "" {
		plan.AuditWorkspaces = append(plan.AuditWorkspaces, parent.DraftWorkspace)
	}
	if checkpointed, ok := cached.(*distillationCheckpointGenerator); ok {
		plan.AuditWorkspaces = append(plan.AuditWorkspaces, checkpointed.audits...)
	}
	slices.Sort(plan.AuditWorkspaces)
	plan.AuditWorkspaces = slices.Compact(plan.AuditWorkspaces)
	if err = validateRefinementSources(cmd, manager, destination, stateRoot, parent); err != nil {
		return err
	}
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

func newWorkspaceDistillShowCommand(deps workspaceCommandDeps) *cobra.Command {
	var teamName string
	var full bool
	cmd := &cobra.Command{
		Use: "show <plan-id>", Short: "Review a saved plan and its parent-fact provenance without model calls",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if teamName == "" {
				return errors.New("--team is required")
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			start, err := deps.getwd()
			if err != nil {
				return err
			}
			manager, err := workspacepkg.NewManager(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			destination, err := manager.Resolve(cmd.Context(), workspacepkg.ResolveRequest{StartDir: start, TeamName: teamName, Mode: workspacepkg.ResolveExisting})
			if err != nil {
				return err
			}
			plan, err := loadWorkspaceDistillationPlan(cmd.Context(), destination, args[0], 0)
			if err != nil {
				return err
			}
			if full {
				return writeDistillationJSON(cmd, plan)
			}
			type sourceSummary struct {
				ID        string `json:"id"`
				SHA256    string `json:"sha256"`
				Protected bool   `json:"protected"`
			}
			sources := []sourceSummary{}
			for _, source := range plan.Sources {
				sources = append(sources, sourceSummary{source.ID, taskdistill.Digest(source), source.Protected})
			}
			return writeDistillationJSON(cmd, struct {
				ID              string                           `json:"id"`
				Team            string                           `json:"team"`
				Policy          string                           `json:"policy"`
				Model           string                           `json:"model"`
				ExtractOnly     bool                             `json:"extract_only,omitzero"`
				Sources         []sourceSummary                  `json:"sources"`
				Draft           taskdistill.Draft                `json:"draft"`
				Refinement      *workspaceDistillationRefinement `json:"refinement,omitempty"`
				AuditWorkspaces []string                         `json:"audit_workspaces"`
			}{plan.ID, plan.Team, plan.Policy, plan.Model, plan.ExtractOnly, sources, plan.Draft, plan.Refinement, plan.AuditWorkspaces})
		},
	}
	cmd.Flags().StringVar(&teamName, "team", "", "Destination team workspace (required)")
	cmd.Flags().BoolVar(&full, "full", false, "Include the complete frozen source content and application inventory")
	return cmd
}
