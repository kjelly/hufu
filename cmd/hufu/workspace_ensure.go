package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func newWorkspaceEnsureCommand(deps workspaceCommandDeps) *cobra.Command {
	var teamName, output string
	command := &cobra.Command{
		Use:   "ensure",
		Short: "Create a missing managed team workspace without running agents; preserve an existing one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
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
			resolution, err := manager.Resolve(cmd.Context(), workspacepkg.ResolveRequest{StartDir: start, TeamName: teamName, Mode: workspacepkg.ResolveEnsure})
			if err != nil {
				return err
			}
			if output == "json" {
				return writeWorkspaceJSON(cmd.OutOrStdout(), map[string]string{
					"project_id": resolution.ProjectID, "workspace_id": resolution.WorkspaceID,
					"team_name": resolution.TeamName, "control_root": resolution.ControlRoot,
					"context_scope_id": resolution.ContextScopeID,
				})
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), resolution.ControlRoot)
			return err
		},
	}
	command.Flags().StringVar(&teamName, "team", "", "Team workspace to ensure (required)")
	addWorkspaceOutputFlag(command, &output)
	return command
}
