package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func newWorkspaceConvertStorageCommand(deps workspaceCommandDeps) *cobra.Command {
	var teamName, output string
	var dryRun, yes bool
	command := &cobra.Command{
		Use:   "convert-storage [selector]",
		Short: "Convert a file-based team workspace's runtime state into runtime.sqlite",
		Long: `Convert a team workspace written by an older Hufu, which kept its event log
and runtime state in files, so current Hufu can use it with its history.

The event log is imported with full hash-chain validation; the session
checkpoint, session tree, compaction state, conversation history, terminal
sessions, reliability report, task journal, and decision index are imported
as they are. Everything is verified in a staging database before it replaces
anything, so a failed conversion leaves the workspace unchanged. The original
files are moved, not deleted, into legacy-runtime-files/ in the workspace.

--dry-run imports and verifies without changing the workspace.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			if !dryRun {
				if err := confirmWorkspaceMutation("convert-storage", yes, deps.confirmMutation); err != nil {
					return err
				}
			}
			registry, project, err := openAndResolveWorkspaceProject(command.Context(), deps, firstArgument(args))
			if err != nil {
				return err
			}
			workspace, err := registry.GetWorkspace(command.Context(), project.ID, teamName)
			closeErr := registry.Close()
			if err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
			if workspace.State != "active" {
				return fmt.Errorf("%w: workspace %s/%s is %s", workspacepkg.ErrConflict, project.ID, workspace.TeamName, workspace.State)
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			// A run holds this lock for its whole duration.
			locks, err := workspacepkg.AcquireWorkspaceLocks(stateRoot, []string{workspace.ID})
			if err != nil {
				return fmt.Errorf("convert-storage: %w", err)
			}
			defer func() { _ = locks.Close() }()
			result, err := team.ConvertFileBasedWorkspace(command.Context(), workspace.ControlRoot, dryRun)
			if err != nil {
				return err
			}
			if output == "json" {
				return writeWorkspaceJSON(command.OutOrStdout(), result)
			}
			return renderWorkspaceConversion(command, result)
		},
	}
	command.Flags().StringVar(&teamName, "team", "default", "Team workspace name")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Import and verify without changing the workspace")
	command.Flags().BoolVar(&yes, "yes", false, "Confirm converting the workspace")
	addWorkspaceOutputFlag(command, &output)
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	return command
}

func renderWorkspaceConversion(command *cobra.Command, result team.WorkspaceConversion) error {
	verb, moved := "Converted", "moved to"
	if result.DryRun {
		verb, moved = "Would convert", "would move to"
	}
	if result.Resumed {
		_, err := fmt.Fprintf(command.OutOrStdout(), "%s: runtime.sqlite was already in place; moved %d original file(s) to %s\n",
			result.Workspace, len(result.MovedFiles), result.BackupDir)
		return err
	}
	_, err := fmt.Fprintf(command.OutOrStdout(), "%s %s\n  events: %d\n  documents: %d\n  session branches: %d\n  task journal rows: %d\n  decision index rows: %d\n  originals (%d file(s)) %s %s\n",
		verb, result.Workspace, result.Events, len(result.Documents), result.SessionBranches, result.TaskJournalRows,
		result.DecisionIndexRows, len(result.MovedFiles), moved, result.BackupDir)
	return err
}
