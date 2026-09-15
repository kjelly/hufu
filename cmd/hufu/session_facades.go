package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

type sessionFacadeOptions struct {
	team, run, branch, task, output string
	attempt                         int
}

type boundMutationTarget struct {
	bound       inspectpkg.BoundReadTarget
	item        *team.TodoItem
	eligibility *operatorpkg.RecoveryEligibility
}

func newSessionStatusCommand() *cobra.Command {
	options := new(sessionFacadeOptions)
	command := &cobra.Command{
		Use:               "status",
		Short:             "Show the canonical status of one exact session scope",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			format, err := resolveSessionOutput(command, options.output)
			if err != nil {
				return err
			}
			workspace := getSessionWorkspace()
			resolution, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: workspace, Mode: "exact", TeamName: options.team})
			if err != nil {
				return err
			}
			bound, err := inspectpkg.BindReadTarget(command.Context(), operatorpkg.BindingRequest{Workspace: resolution, TeamID: options.team, RunID: options.run, BranchID: options.branch})
			if err != nil {
				query := inspectpkg.InspectQuery{Workspace: workspace, RunID: options.run, BranchID: options.branch}
				return finishInspectOverview(command, format, query, nil, err)
			}
			query := inspectpkg.InspectQuery{Workspace: workspace, RunID: bound.Scope.RunID, BranchID: bound.Scope.BranchID}
			envelope, inspectErr := inspectpkg.InspectOverview(command.Context(), query)
			return finishInspectOverview(command, format, query, envelope, inspectErr)
		},
	}
	addSessionScopeFlags(command, options, false)
	command.Flags().StringVar(&options.output, "output", "", "Output format: text or json")
	registerStaticFlagCompletion(command, "output", []string{"text", "json"})
	return command
}

func newSessionResumeCommand() *cobra.Command {
	options := new(sessionFacadeOptions)
	command := &cobra.Command{
		Use:               "resume",
		Short:             "Resume the active compatible session in an exact workspace",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			target, err := bindActiveMutationTarget(command.Context(), getSessionWorkspace(), options, false)
			if err != nil {
				return err
			}
			if !sessionHasResumableWork(target.bound.Session) {
				return fmt.Errorf("session target is stale: the active run has no resumable work")
			}
			previousOpts, previousTeam := opts, resumeTeamName
			defer func() { opts, resumeTeamName = previousOpts, previousTeam }()
			opts.workspace = target.bound.Scope.WorkspaceExact
			opts.workspaceMode = "exact"
			resumeTeamName = target.bound.Scope.TeamName
			return runResumeCommand(command, nil)
		},
	}
	addSessionScopeFlags(command, options, false)
	return command
}

func newSessionRecoveryCommand(action team.TargetedRecoveryAction) *cobra.Command {
	options := new(sessionFacadeOptions)
	use := string(action)
	command := &cobra.Command{
		Use:               use,
		Short:             strings.ToUpper(use[:1]) + use[1:] + " one task in the active exact session scope",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			format, err := resolveSessionOutput(command, options.output)
			if err != nil {
				return err
			}
			target, err := bindActiveMutationTarget(command.Context(), getSessionWorkspace(), options, true)
			if err != nil {
				return err
			}
			switch action {
			case team.TargetedRecoveryRetry:
				if target.eligibility == nil || !target.eligibility.RetryEligible {
					return fmt.Errorf("retry denied for task %q: %s", options.task, recoveryReason(target.eligibility))
				}
			case team.TargetedRecoveryReconcile:
				if target.eligibility == nil || !target.eligibility.ReconcileEligible {
					return fmt.Errorf("reconcile denied for task %q: %s", options.task, recoveryReason(target.eligibility))
				}
			default:
				return fmt.Errorf("unsupported recovery action %q", action)
			}

			previousOpts := opts
			previousTask, previousTeam, previousJSON := targetedRecoveryTaskID, targetedRecoveryTeamName, targetedRecoveryJSON
			defer func() {
				opts = previousOpts
				targetedRecoveryTaskID, targetedRecoveryTeamName, targetedRecoveryJSON = previousTask, previousTeam, previousJSON
			}()
			opts.workspace = target.bound.Scope.WorkspaceExact
			opts.workspaceMode = "exact"
			targetedRecoveryTaskID = options.task
			targetedRecoveryTeamName = target.bound.Scope.TeamName
			targetedRecoveryJSON = format == "json"
			return runTargetedRecoveryCommand(action)
		},
	}
	addSessionScopeFlags(command, options, true)
	command.Flags().StringVar(&options.output, "output", "", "Output format: text or json")
	registerStaticFlagCompletion(command, "output", []string{"text", "json"})
	return command
}

func addSessionScopeFlags(command *cobra.Command, options *sessionFacadeOptions, task bool) {
	command.Flags().StringVar(&options.team, "team", "", "Exact persisted team name")
	command.Flags().StringVar(&options.run, "run", "", "Exact run ID")
	command.Flags().StringVar(&options.branch, "branch", "", "Exact branch ID, name, or label")
	if task {
		command.Flags().StringVar(&options.task, "task", "", "Exact runtime task ID")
		command.Flags().IntVar(&options.attempt, "attempt", 0, "Exact task attempt when multiple attempts exist")
		_ = command.MarkFlagRequired("task")
	}
}

func resolveSessionOutput(command *cobra.Command, output string) (string, error) {
	return resolveOutputAlias("output", output, flagChanged(command, "output"), "json", outputForJSONAlias(sessionJSON), flagChanged(command, "json"), "text", []string{"text", "json"})
}

func bindActiveMutationTarget(ctx context.Context, workspace string, options *sessionFacadeOptions, requireTask bool) (boundMutationTarget, error) {
	if strings.TrimSpace(options.team) == "" || strings.TrimSpace(options.run) == "" || strings.TrimSpace(options.branch) == "" {
		return boundMutationTarget{}, fmt.Errorf("--team, --run, and --branch are required for session mutation")
	}
	resolution, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: workspace, Mode: "exact", TeamName: options.team})
	if err != nil {
		return boundMutationTarget{}, fmt.Errorf("invalid exact workspace: %w", err)
	}
	bound, err := inspectpkg.BindReadTarget(ctx, operatorpkg.BindingRequest{
		Workspace: resolution, TeamID: options.team, RunID: options.run, BranchID: options.branch,
	})
	if err != nil {
		return boundMutationTarget{}, fmt.Errorf("session target binding failed: %w", err)
	}
	if bound.Lineage.BranchID != bound.Lineage.ActiveBranchID {
		return boundMutationTarget{}, fmt.Errorf("historical session mutation is unsupported: branch %q is not active", bound.Lineage.BranchID)
	}
	activeRun, err := inspectpkg.ActiveSessionRunID(bound.Session)
	if err != nil {
		return boundMutationTarget{}, fmt.Errorf("active session binding is invalid: %w", err)
	}
	if activeRun == "" || activeRun != bound.Scope.RunID {
		return boundMutationTarget{}, fmt.Errorf("historical session mutation is unsupported: run %q is not the active run", bound.Scope.RunID)
	}
	target := boundMutationTarget{bound: bound}
	if !requireTask {
		return target, nil
	}
	for _, item := range bound.Session.Tasks {
		if item != nil && item.ID == options.task {
			if target.item != nil {
				return boundMutationTarget{}, fmt.Errorf("task %q is ambiguous in the active session", options.task)
			}
			target.item = item
		}
	}
	if target.item == nil {
		return boundMutationTarget{}, fmt.Errorf("task %q is not present in the active session", options.task)
	}
	if target.item.Status == team.TaskDone || target.item.Status == team.TaskSkipped {
		return boundMutationTarget{}, fmt.Errorf("task %q target is stale: status is %s", options.task, target.item.Status)
	}
	interrupted := target.item.Status == team.TaskInProgress || target.item.Status == team.TaskPaused
	target.eligibility = inspectpkg.RecoveryEligibilityForTask(target.item, bound.Lineage.Events, bound.Scope.RunID, interrupted)
	if target.eligibility == nil {
		return boundMutationTarget{}, fmt.Errorf("task %q has no recovery eligibility", options.task)
	}
	if options.attempt == 0 && target.eligibility.Attempt > 1 {
		return boundMutationTarget{}, fmt.Errorf("task %q has multiple attempts; pass --attempt %d", options.task, target.eligibility.Attempt)
	}
	if options.attempt > 0 && options.attempt != target.eligibility.Attempt {
		return boundMutationTarget{}, fmt.Errorf("attempt %d is stale; active attempt is %d", options.attempt, target.eligibility.Attempt)
	}
	return target, nil
}

func sessionHasResumableWork(session *team.SessionData) bool {
	if session == nil {
		return false
	}
	if session.RecoveryRequired || session.PendingTerminalCommit != nil {
		return true
	}
	for _, item := range session.Tasks {
		if item == nil {
			continue
		}
		switch item.Status {
		case team.TaskPending, team.TaskPlanned, team.TaskInProgress, team.TaskPaused:
			return true
		}
	}
	return false
}

func recoveryReason(eligibility *operatorpkg.RecoveryEligibility) string {
	if eligibility == nil || strings.TrimSpace(eligibility.ReasonCode) == "" {
		return "recovery eligibility is unavailable"
	}
	return eligibility.ReasonCode
}
