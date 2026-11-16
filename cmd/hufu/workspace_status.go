package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

type workspaceStatusData struct {
	Project   workspacepkg.Project  `json:"project"`
	Selection string                `json:"selection"`
	Complete  bool                  `json:"complete"`
	Latest    *inspectpkg.LatestRun `json:"latest"`
	Teams     []workspaceTeamStatus `json:"teams"`
}

type workspaceTeamStatus struct {
	Team      string                        `json:"team"`
	Workspace string                        `json:"workspace"`
	State     string                        `json:"state"`
	Latest    *inspectpkg.LatestRun         `json:"latest"`
	Error     *inspectpkg.OperatorErrorView `json:"error"`
}

func newWorkspaceStatusCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	command := &cobra.Command{
		Use:   "status [selector]",
		Short: "Find the project's latest run and each team's task status (read-only)",
		Long: "Read registered team workspaces for the current project or a project selector. " +
			"Each team's last run_started is selected across all branches; the project latest is ordered by start time, " +
			"then team name for ties. Saved state does not establish process liveness. " +
			"Partial reads set complete=false and leave latest=null; available team rows remain inspectable.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			registry, project, err := openAndResolveWorkspaceProject(command.Context(), deps, firstArgument(args))
			if err != nil {
				return err
			}
			defer func() { _ = registry.Close() }()
			workspaces, err := registry.ListWorkspaces(command.Context(), project.ID)
			if err != nil {
				return err
			}
			data, err := collectWorkspaceStatus(command.Context(), project, workspaces, readRegisteredWorkspaceLatestRun)
			if err != nil {
				return err
			}
			if output == "json" {
				return writeWorkspaceJSON(command.OutOrStdout(), data)
			}
			return writeWorkspaceStatus(command.OutOrStdout(), data)
		},
	}
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	addWorkspaceOutputFlag(command, &output)
	return command
}

func readRegisteredWorkspaceLatestRun(ctx context.Context, path string) (*inspectpkg.LatestRun, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: registered control root is unreadable", inspectpkg.ErrIntegrity)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: registered control root is not a directory", inspectpkg.ErrIntegrity)
	}
	return inspectpkg.InspectLatestRun(ctx, path)
}

func collectWorkspaceStatus(ctx context.Context, project workspacepkg.Project, workspaces []workspacepkg.Workspace,
	read func(context.Context, string) (*inspectpkg.LatestRun, error),
) (workspaceStatusData, error) {
	data := workspaceStatusData{Project: project, Selection: "latest_started_all_branches", Complete: true, Teams: []workspaceTeamStatus{}}
	workspaces = slices.SortedFunc(slices.Values(workspaces), func(a, b workspacepkg.Workspace) int { return cmp.Compare(a.TeamName, b.TeamName) })
	for _, workspace := range workspaces {
		if err := ctx.Err(); err != nil {
			return workspaceStatusData{}, err
		}
		row := workspaceTeamStatus{Team: workspace.TeamName, Workspace: workspace.ControlRoot, State: "no_runs"}
		if workspace.State != "active" {
			row.State = "unavailable"
			row.Error = &inspectpkg.OperatorErrorView{Kind: "workspace", Code: "workspace_not_active", Message: "workspace is not active"}
		} else {
			latest, err := read(ctx, workspace.ControlRoot)
			if ctx.Err() != nil {
				return workspaceStatusData{}, ctx.Err()
			}
			if err != nil {
				failure := inspectpkg.OverviewFailure(inspectpkg.InspectQuery{Workspace: workspace.ControlRoot}, err)
				row.State, row.Error = "unavailable", failure.Data.(inspectpkg.OverviewData).Error
			} else if latest != nil {
				row.State, row.Latest = "available", latest
				latest.Team = workspace.TeamName
				if data.Latest == nil || latest.StartedAt.After(data.Latest.StartedAt) {
					data.Latest = latest
				}
			}
		}
		if row.State == "unavailable" {
			data.Complete = false
		}
		data.Teams = append(data.Teams, row)
	}
	if !data.Complete {
		data.Latest = nil
	}
	return data, nil
}

func writeWorkspaceStatus(writer io.Writer, data workspaceStatusData) error {
	if _, err := fmt.Fprintf(writer, "project=%s complete=%t selection=%s\n", data.Project.ID, data.Complete, data.Selection); err != nil {
		return err
	}
	if data.Latest != nil {
		if _, err := fmt.Fprintf(writer, "latest: team=%s invocation=%s run=%s\n",
			operatorpkg.SafeDisplayText(data.Latest.Team, 1024), operatorpkg.SafeDisplayText(data.Latest.InvocationID, 1024), operatorpkg.SafeDisplayText(data.Latest.Run.RunID, 1024)); err != nil {
			return err
		}
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "TEAM\tSTATE\tDONE/TOTAL\tUNRESOLVED\tSTARTED\tINVOCATION\tRUN\tBRANCH\tWORKSPACE"); err != nil {
		return err
	}
	for _, row := range data.Teams {
		counts, unresolved, started, invocation, run, branch := "-", "-", "-", "-", "-", "-"
		state := row.State
		if row.Latest != nil {
			latest := row.Latest
			counts = fmt.Sprintf("%d/%d", latest.Run.TaskSummary.Done, latest.Run.TaskSummary.Total)
			unresolved = fmt.Sprint(latest.Run.TaskSummary.Unresolved)
			started, invocation, run, branch = latest.StartedAt.Format(time.RFC3339Nano), latest.InvocationID, latest.Run.RunID, latest.BranchID
			if latest.Run.Outcome != "" {
				state = latest.Run.Outcome
			}
		}
		if row.Error != nil {
			state += ":" + row.Error.Code
		}
		values := []string{row.Team, state, counts, unresolved, started, invocation, run, branch, row.Workspace}
		for i := range values {
			values[i] = operatorpkg.SafeDisplayText(values[i], 1024)
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8]); err != nil {
			return err
		}
	}
	return table.Flush()
}
