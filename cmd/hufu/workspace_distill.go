package main

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

type workspaceDistillOptions struct {
	team, model, searchPath, policy, apply string
	tasks, trash                           []string
	yes                                    bool
	maxCalls                               int
	probeBatch                             int
}

type distillationTrashSource struct {
	TrashID     string `json:"trash_id"`
	WorkspaceID string `json:"workspace_id"`
	Fingerprint string `json:"fingerprint"`
}

type workspaceDistillationPlan struct {
	ID              string                         `json:"id"`
	ProjectID       string                         `json:"project_id"`
	WorkspaceID     string                         `json:"workspace_id"`
	ContextScopeID  string                         `json:"context_scope_id"`
	DraftWorkspace  string                         `json:"draft_workspace"`
	AuditWorkspaces []string                       `json:"audit_workspaces,omitempty"`
	Team            string                         `json:"team"`
	Model           string                         `json:"model"`
	Policy          string                         `json:"policy"`
	Tasks           *team.TaskDistillationSnapshot `json:"tasks,omitempty"`
	Trash           []distillationTrashSource      `json:"trash,omitempty"`
	Sources         []taskdistill.Source           `json:"sources"`
	MaxModelCalls   int                            `json:"max_model_calls"`
	Draft           taskdistill.Draft              `json:"draft"`
}

var taskDistillationGeneratorFactory = func(ctx context.Context, dir, workspace, model string) (maintenanceTextGenerator, string, func(), error) {
	profile := sidecar.CompactorProfile
	// The extraction envelope includes source JSON, the schema and guidance.
	// UTF-8 rune counts cannot exceed this byte bound. Keep other maintenance
	// operations on their existing, smaller compactor profile.
	profile.MaxInputRunes = taskdistill.MaxPromptBytes
	profile.RequireCompleteOutput = true
	// Structured extraction needs the bounded completion budget for its JSON
	// answer. Do not inherit the compactor's separate reasoning request.
	profile.ReasoningEffort = "none"
	return newMaintenanceTextGenerator(ctx, maintenanceGeneratorOptions{TeamDir: dir, Workspace: workspace, ModelOverride: model, Purpose: "task_distillation", Profile: profile})
}

func newWorkspaceDistillCommand(deps workspaceCommandDeps) *cobra.Command {
	options := &workspaceDistillOptions{}
	command := &cobra.Command{
		Use: "distill", Short: "Preview knowledge extraction, then apply the reviewed plan before cleaning old tasks or trash",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runWorkspaceDistill(cmd, deps, *options) },
	}
	flags := command.Flags()
	flags.StringVar(&options.team, "team", "", "Active destination team workspace (required)")
	flags.StringSliceVar(&options.tasks, "task", nil, "Explicit task IDs in the active workspace; repeatable/comma-separated")
	flags.StringSliceVar(&options.trash, "trash", nil, "Retired workspace trash IDs from the same project and team; repeatable/comma-separated")
	flags.StringVar(&options.model, "model", "", "Model used to draft the preview")
	flags.StringVar(&options.searchPath, "team-search-path", "", "Comma-separated team search paths")
	flags.StringVar(&options.policy, "policy", "", "Operator guidance for usefulness, deduplication and retention")
	flags.IntVar(&options.maxCalls, "max-model-calls", taskdistill.DefaultMaxModelCalls, "Maximum extraction and merge model calls; preflight refuses larger inputs (maximum 4096)")
	flags.IntVar(&options.probeBatch, "probe-batch", 0, "Validate only this 1-based extraction batch within the call budget; no applicable plan or cleanup")
	flags.StringVar(&options.apply, "apply", "", "Apply this saved preview ID without another model call")
	flags.BoolVar(&options.yes, "yes", false, "Confirm saving reviewed knowledge and cleaning selected sources with --apply")
	command.AddCommand(newWorkspaceDistillListCommand(deps))
	return command
}

func runWorkspaceDistill(cmd *cobra.Command, deps workspaceCommandDeps, options workspaceDistillOptions) error {
	if err := validateWorkspaceDistillOptions(cmd, options); err != nil {
		return err
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
	resolution, err := manager.Resolve(cmd.Context(), workspacepkg.ResolveRequest{StartDir: start, TeamName: options.team, Mode: workspacepkg.ResolveExisting})
	if err != nil {
		return err
	}
	locks, err := workspacepkg.AcquireWorkspaceLocks(stateRoot, []string{resolution.WorkspaceID})
	if err != nil {
		return err
	}
	defer func() { _ = locks.Close() }()
	// Resolve again after locking so delete/rebind cannot substitute a target.
	current, err := manager.Resolve(cmd.Context(), workspacepkg.ResolveRequest{StartDir: start, TeamName: options.team, Mode: workspacepkg.ResolveExisting})
	if err != nil {
		return err
	}
	if current != resolution {
		return workspacepkg.ErrConflict
	}
	if options.apply != "" {
		return applyWorkspaceDistillation(cmd, manager, resolution, options.apply, stateRoot)
	}
	plan := workspaceDistillationPlan{ProjectID: resolution.ProjectID, WorkspaceID: resolution.WorkspaceID, ContextScopeID: resolution.ContextScopeID, Team: options.team, Policy: options.policy, MaxModelCalls: options.maxCalls}
	if len(options.tasks) > 0 {
		snapshot, err := team.SnapshotTasksForDistillation(cmd.Context(), resolution.ControlRoot, options.tasks)
		if err != nil {
			return err
		}
		plan.Tasks = &snapshot
		plan.Sources = append(plan.Sources, snapshot.Sources...)
	}
	for _, id := range options.trash {
		if slices.ContainsFunc(plan.Trash, func(source distillationTrashSource) bool { return source.TrashID == id }) {
			return errors.New("duplicate trash ID")
		}
		err = manager.VisitTrash(cmd.Context(), id, func(trash workspacepkg.TrashWorkspace) error {
			if trash.ProjectID != resolution.ProjectID || trash.TeamName != options.team || trash.WorkspaceID == resolution.WorkspaceID {
				return errors.New("trash source must belong to the destination project and team")
			}
			snapshot, err := team.SnapshotRetiredWorkspace(cmd.Context(), trash.TrashPath, contextstore.Scope{ProjectID: trash.ContextScopeID, TeamID: trash.TeamName})
			if err != nil {
				return err
			}
			plan.Trash = append(plan.Trash, distillationTrashSource{id, trash.WorkspaceID, snapshot.Fingerprint})
			plan.Sources = append(plan.Sources, taskdistill.Source{ID: id, Content: snapshot.Content, Protected: snapshot.Protected})
			return nil
		})
		if err != nil {
			return err
		}
	}
	// Validate the complete batch budget before constructing a provider.
	batches, err := prepareWorkspaceDistillationBatches(plan.Sources, options)
	if err != nil {
		return err
	}
	if len(batches) > 1 && options.probeBatch == 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Distillation preview: %d extraction batch(es); maximum %d total model calls including knowledge merging. No sources are deleted during preview.\n", len(batches), options.maxCalls)
	}
	registry := teamRegistryFromSearchPath(options.searchPath)
	if err = registry.Discover(); err != nil {
		return err
	}
	dir, err := registry.Resolve(options.team)
	if err != nil {
		return err
	}
	plan.DraftWorkspace, err = createDistillationDraftWorkspace(resolution.ControlRoot)
	if err != nil {
		return err
	}
	generationCtx := agent.WithProviderCallBudget(cmd.Context(), options.maxCalls)
	generator, model, release, err := taskDistillationGeneratorFactory(generationCtx, dir, plan.DraftWorkspace, options.model)
	if err != nil {
		return err
	}
	if release == nil {
		return errors.New("distillation generator did not provide cleanup")
	}
	defer release()
	plan.Model = model
	progress := distillationProgressGenerator{generator, cmd, len(batches) > 1}
	checkpointed, closeCheckpoints, err := newDistillationCheckpointGenerator(cmd, resolution.ControlRoot, plan, generator, progress)
	if err != nil {
		return err
	}
	defer closeCheckpoints()
	if options.probeBatch > 0 {
		return runWorkspaceDistillationProbe(cmd, checkpointed, plan, options, len(batches))
	}
	plan.Draft, err = taskdistill.GenerateBatched(cmd.Context(), checkpointed, plan.Sources, options.policy, options.maxCalls)
	if err != nil {
		return err
	}
	if cached, ok := checkpointed.(*distillationCheckpointGenerator); ok {
		plan.AuditWorkspaces = slices.Clone(cached.audits)
	}
	plan.ID = taskdistill.Digest(plan)
	store, err := runtimestore.Open(cmd.Context(), resolution.ControlRoot)
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

// Auxiliary model admission writes a session checkpoint. Give it an independent
// durable audit workspace so it cannot overwrite the tasks being maintained.
func createDistillationDraftWorkspace(destination string) (string, error) {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	for _, dir := range []string{"maintenance", "maintenance/task-distillation"} {
		if err = root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	relative := "maintenance/task-distillation/draft-" + rand.Text()
	if err = root.Mkdir(relative, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(destination, filepath.FromSlash(relative)), nil
}

func writeDistillationJSON(cmd *cobra.Command, value any) error {
	raw, err := json.Marshal(value, jsontext.Multiline(true))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	return err
}

func applyWorkspaceDistillation(cmd *cobra.Command, manager *workspacepkg.WorkspaceManager, destination workspacepkg.Resolution, id, stateRoot string) error {
	ctx := cmd.Context()
	store, err := runtimestore.OpenRead(ctx, destination.ControlRoot)
	if err != nil {
		return err
	}
	raw, exists, readErr := store.GetDocument(ctx, "task_distillation_plan:"+id)
	closeErr := store.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if !exists {
		return errors.New("distillation preview not found")
	}
	var plan workspaceDistillationPlan
	if err = json.Unmarshal(raw, &plan, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	savedID := plan.ID
	plan.ID = ""
	if savedID != id || taskdistill.Digest(plan) != id || plan.ProjectID != destination.ProjectID || plan.WorkspaceID != destination.WorkspaceID || plan.ContextScopeID != destination.ContextScopeID || plan.Team != destination.TeamName {
		return errors.New("distillation preview identity mismatch")
	}
	plan.ID = id
	if err = taskdistill.Validate(plan.Draft, plan.Sources); err != nil {
		return err
	}
	completedTrash, err := validateDistillationTrash(ctx, manager, destination, stateRoot, plan)
	if err != nil {
		return err
	}
	// Revalidate task state before writing knowledge or purging any source.
	if plan.Tasks != nil {
		ids := make([]string, 0, len(plan.Tasks.Sources))
		for _, source := range plan.Tasks.Sources {
			ids = append(ids, source.ID)
		}
		current, snapshotErr := team.SnapshotTasksForDistillation(ctx, destination.ControlRoot, ids)
		if snapshotErr != nil || current.Fingerprint != plan.Tasks.Fingerprint {
			check, openErr := runtimestore.OpenRead(ctx, destination.ControlRoot)
			if openErr != nil {
				return openErr
			}
			appliedSnapshot, applied, checkErr := check.GetDocument(ctx, "task_distillation_applied:"+id)
			_ = check.Close()
			if checkErr != nil {
				return checkErr
			}
			if !applied {
				return errors.New("task state changed; create a new preview")
			}
			if string(appliedSnapshot) != taskdistill.Digest(*plan.Tasks) {
				return errors.New("task distillation application receipt mismatch")
			}
		}
	}
	// No cleanup starts until the complete reviewed knowledge set is durable.
	repo, err := contextstore.OpenSQLite(filepath.Join(destination.ControlRoot, "context.sqlite"))
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	save := func() error {
		return taskdistill.SaveKnowledge(ctx, repo, contextstore.Scope{ProjectID: destination.ContextScopeID, TeamID: destination.TeamName}, id, plan.Sources, plan.Draft)
	}
	if err = save(); err != nil {
		return err
	}
	if plan.Tasks != nil {
		taskDraft := localTaskDistillationDraft(plan)
		if err = team.RemoveDistilledTasks(ctx, destination.ControlRoot, id, *plan.Tasks, taskDraft); err != nil {
			return err
		}
	}
	for _, source := range plan.Trash {
		if completedTrash[source.TrashID] {
			continue
		}
		decision := slices.IndexFunc(plan.Draft.Decisions, func(decision taskdistill.Decision) bool { return decision.SourceID == source.TrashID })
		if plan.Draft.Decisions[decision].Action == "retain" {
			continue
		}
		_, err = manager.Purge(ctx, workspacepkg.PurgeRequest{TrashID: source.TrashID, Preserve: func(ctx context.Context, trash workspacepkg.TrashWorkspace) error {
			if trash.ProjectID != destination.ProjectID || trash.TeamName != destination.TeamName || trash.WorkspaceID != source.WorkspaceID {
				return workspacepkg.ErrConflict
			}
			current, err := team.SnapshotRetiredWorkspace(ctx, trash.TrashPath, contextstore.Scope{ProjectID: trash.ContextScopeID, TeamID: trash.TeamName})
			if err != nil {
				return err
			}
			if current.Fingerprint != source.Fingerprint || current.Protected {
				return errors.New("trash source changed or requires recovery; create a new preview")
			}
			if err = save(); err != nil {
				return err
			}
			store, err := runtimestore.Open(ctx, destination.ControlRoot)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			return store.PutDocument(ctx, "task_distillation_preserved:"+id+":"+source.TrashID, []byte(source.Fingerprint))
		}})
		if err != nil {
			return fmt.Errorf("knowledge saved; cleanup of %s incomplete: %w", source.TrashID, err)
		}
	}
	return writeDistillationJSON(cmd, map[string]any{"plan_id": id, "status": "applied", "knowledge_saved": len(plan.Draft.Knowledge), "task_cleanup": "active projections only; immutable evidence and historical branches retained"})
}

// A source can disappear between purge commit and the CLI's reply. A durable
// preservation receipt plus absence from BOTH active and trash registries
// permits an idempotent retry; a restored/re-deleted source is never skipped.
func distillationTrashAlreadyPurged(ctx context.Context, stateRoot, destination, planID string, source distillationTrashSource) (bool, error) {
	store, err := runtimestore.OpenRead(ctx, destination)
	if err != nil {
		return false, err
	}
	raw, exists, err := store.GetDocument(ctx, "task_distillation_preserved:"+planID+":"+source.TrashID)
	_ = store.Close()
	if err != nil || !exists || string(raw) != source.Fingerprint {
		return false, err
	}
	registry, err := workspacepkg.OpenReadOnly(stateRoot)
	if err != nil {
		return false, err
	}
	defer func() { _ = registry.Close() }()
	_, err = registry.GetWorkspaceByID(ctx, source.WorkspaceID)
	if err == nil {
		return false, errors.New("distillation source was restored")
	}
	if !errors.Is(err, workspacepkg.ErrNotFound) {
		return false, err
	}
	trash, err := registry.ListTrashWorkspaces(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range trash {
		if item.WorkspaceID == source.WorkspaceID {
			return false, errors.New("distillation source is still registered in trash; run workspace doctor")
		}
	}
	return true, nil
}

func validateDistillationTrash(ctx context.Context, manager *workspacepkg.WorkspaceManager, destination workspacepkg.Resolution, stateRoot string, plan workspaceDistillationPlan) (map[string]bool, error) {
	id := plan.ID
	completedTrash := map[string]bool{}
	for _, source := range plan.Trash {
		err := manager.VisitTrash(ctx, source.TrashID, func(trash workspacepkg.TrashWorkspace) error {
			if trash.ProjectID != destination.ProjectID || trash.TeamName != destination.TeamName || trash.WorkspaceID != source.WorkspaceID {
				return workspacepkg.ErrConflict
			}
			current, err := team.SnapshotRetiredWorkspace(ctx, trash.TrashPath, contextstore.Scope{ProjectID: trash.ContextScopeID, TeamID: trash.TeamName})
			if err != nil {
				return err
			}
			if current.Fingerprint != source.Fingerprint {
				return errors.New("trash source changed; create a new preview")
			}
			return nil
		})
		if errors.Is(err, workspacepkg.ErrNotFound) {
			finished, checkErr := distillationTrashAlreadyPurged(ctx, stateRoot, destination.ControlRoot, id, source)
			if checkErr != nil {
				return nil, checkErr
			}
			if finished {
				completedTrash[source.TrashID] = true
				continue
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return completedTrash, nil
}

func localTaskDistillationDraft(plan workspaceDistillationPlan) taskdistill.Draft {
	taskDraft := taskdistill.Draft{}
	for _, decision := range plan.Draft.Decisions {
		if slices.ContainsFunc(plan.Tasks.Sources, func(source taskdistill.Source) bool { return source.ID == decision.SourceID }) {
			taskDraft.Decisions = append(taskDraft.Decisions, decision)
		}
	}
	// The global draft can merge task and trash sources. Restrict only
	// provenance lists for local validation, preserving text and judgment.
	for _, knowledge := range plan.Draft.Knowledge {
		copy := knowledge
		copy.SourceIDs = nil
		for _, sourceID := range knowledge.SourceIDs {
			if slices.ContainsFunc(plan.Tasks.Sources, func(source taskdistill.Source) bool { return source.ID == sourceID }) {
				copy.SourceIDs = append(copy.SourceIDs, sourceID)
			}
		}
		if len(copy.SourceIDs) > 0 {
			taskDraft.Knowledge = append(taskDraft.Knowledge, copy)
		}
	}
	return taskDraft
}

// Report progress without exposing source content or model credentials.
type distillationProgressGenerator struct {
	maintenanceTextGenerator
	command *cobra.Command
	enabled bool
}

func (g distillationProgressGenerator) GenerateText(ctx context.Context, prompt string) (string, error) {
	if g.enabled {
		_, _ = fmt.Fprintf(g.command.ErrOrStderr(), "Distillation model call (%d prompt bytes)…\n", len(prompt))
	}
	raw, err := g.maintenanceTextGenerator.GenerateText(ctx, prompt)
	if errors.Is(err, agent.ErrProviderCallBudget) {
		return "", fmt.Errorf("%w: %w", taskdistill.ErrCallBudget, agent.ErrProviderCallBudget)
	}
	if errors.Is(err, agent.ErrProviderTransportEOF) {
		return raw, taskdistill.NewTransportError(err)
	}
	if errors.Is(err, sidecar.ErrIncompleteOutput) {
		return "", fmt.Errorf("%w: %w", taskdistill.ErrIncompleteOutput, err)
	}
	return raw, err
}

func validateWorkspaceDistillOptions(cmd *cobra.Command, options workspaceDistillOptions) error {
	if flag := cmd.Flag("dry-run"); flag != nil && flag.Value.String() == "true" {
		return errors.New("workspace distill uses a model for preview; --dry-run does not permit drafting or applying")
	}
	if utils.RedactSecrets(options.policy) != options.policy {
		return errors.New("policy contains secret-like material")
	}
	if options.team == "" {
		return errors.New("--team is required")
	}
	if options.maxCalls < 1 || options.maxCalls > taskdistill.MaxModelCalls || options.probeBatch < 0 {
		return errors.New("invalid model call budget or probe batch")
	}
	if options.apply != "" {
		if !options.yes {
			return errors.New("--apply requires --yes")
		}
		if len(options.tasks)+len(options.trash) != 0 || options.policy != "" || options.model != "" || cmd.Flags().Changed("probe-batch") || cmd.Flags().Changed("max-model-calls") {
			return errors.New("--apply uses the saved preview; do not provide --task, --trash, --policy, --model or --max-model-calls")
		}
	} else if options.yes || len(options.tasks)+len(options.trash) == 0 {
		return errors.New("preview requires --task or --trash; --yes is only valid with --apply")
	}
	return nil
}
