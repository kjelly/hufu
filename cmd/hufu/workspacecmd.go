package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

const workspaceOutputSchemaVersion = 1

type workspaceCommandDeps struct {
	stateRoot       func() (string, error)
	getwd           func() (string, error)
	registryOptions []workspacepkg.RegistryOption
}

type workspaceOutputEnvelope struct {
	SchemaVersion int      `json:"schema_version"`
	Data          any      `json:"data"`
	Warnings      []string `json:"warnings"`
}

type workspaceProjectData struct {
	Project workspacepkg.Project `json:"project"`
}

type workspaceShowData struct {
	Project    workspacepkg.Project     `json:"project"`
	Workspaces []workspacepkg.Workspace `json:"workspaces"`
}

type workspaceListData struct {
	Projects             []workspacepkg.Project        `json:"projects"`
	Trash                []workspacepkg.TrashWorkspace `json:"trash"`
	IncompleteOperations []workspacepkg.Operation      `json:"incomplete_operations"`
}

type workspaceProjectListData struct {
	Projects []workspacepkg.Project `json:"projects"`
}

func defaultWorkspaceCommandDeps() workspaceCommandDeps {
	return workspaceCommandDeps{stateRoot: workspacepkg.DefaultStateRoot, getwd: os.Getwd}
}

func newWorkspaceCommand(deps workspaceCommandDeps) *cobra.Command {
	command := &cobra.Command{
		Use:   "workspace",
		Short: "Manage registered projects and durable team workspaces",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(
		newWorkspaceRegisterCommand(deps),
		newWorkspaceListCommand(deps),
		newWorkspaceShowCommand(deps),
		newWorkspacePathCommand(deps),
		newWorkspaceSubjectPathCommand(deps),
		newWorkspaceAliasCommand(deps),
		newWorkspaceRebindCommand(deps),
	)
	return command
}

func newWorkspaceRebindCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	command := &cobra.Command{
		Use:   "rebind <selector> <new-root>",
		Short: "Rebind a registered project to a new subject root",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			manager, err := workspacepkg.NewManager(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			result, err := manager.Rebind(command.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return writeWorkspaceShow(command.OutOrStdout(), output, workspaceShowData{Project: result.Project, Workspaces: nonNilWorkspaces(result.Workspaces)})
		},
	}
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	addWorkspaceOutputFlag(command, &output)
	return command
}

func newWorkspaceRegisterCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	command := &cobra.Command{
		Use:   "register [path]",
		Short: "Register a project subject root",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			path, err := workspaceStartPath(deps, args)
			if err != nil {
				return err
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			registry, err := workspacepkg.OpenReadWrite(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			project, operationErr := registry.RegisterProject(command.Context(), path)
			if closeErr := registry.Close(); operationErr != nil || closeErr != nil {
				return errors.Join(operationErr, closeErr)
			}
			return writeWorkspaceProject(command.OutOrStdout(), output, project)
		},
	}
	addWorkspaceOutputFlag(command, &output)
	return command
}

func newWorkspaceListCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	var includeAll bool
	command := &cobra.Command{
		Use:               "list",
		Short:             "List registered projects",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			data, err := loadWorkspaceList(command.Context(), deps, includeAll)
			if err != nil {
				return err
			}
			return writeWorkspaceList(command.OutOrStdout(), output, data, includeAll)
		},
	}
	command.Flags().BoolVar(&includeAll, "all", false, "Include trash and incomplete operations")
	addWorkspaceOutputFlag(command, &output)
	return command
}

func newWorkspaceShowCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	command := &cobra.Command{
		Use:   "show [selector]",
		Short: "Show one registered project and its team workspaces",
		Args:  cobra.MaximumNArgs(1),
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
			return writeWorkspaceShow(command.OutOrStdout(), output, workspaceShowData{Project: project, Workspaces: nonNilWorkspaces(workspaces)})
		},
	}
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	addWorkspaceOutputFlag(command, &output)
	return command
}

func newWorkspacePathCommand(deps workspaceCommandDeps) *cobra.Command {
	var teamName string
	command := &cobra.Command{
		Use:   "path [selector]",
		Short: "Print the active managed control root",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			registry, project, err := openAndResolveWorkspaceProject(command.Context(), deps, firstArgument(args))
			if err != nil {
				return err
			}
			defer func() { _ = registry.Close() }()
			workspace, err := registry.GetWorkspace(command.Context(), project.ID, teamName)
			if err != nil {
				return err
			}
			if workspace.State != "active" {
				return fmt.Errorf("%w: workspace %s/%s is %s", workspacepkg.ErrConflict, project.ID, workspace.TeamName, workspace.State)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), workspace.ControlRoot)
			return err
		},
	}
	command.Flags().StringVar(&teamName, "team", "default", "Team workspace name")
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	return command
}

func newWorkspaceSubjectPathCommand(deps workspaceCommandDeps) *cobra.Command {
	command := &cobra.Command{
		Use:   "subject-path [selector]",
		Short: "Print the registered project subject root",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			registry, project, err := openAndResolveWorkspaceProject(command.Context(), deps, firstArgument(args))
			if err != nil {
				return err
			}
			defer func() { _ = registry.Close() }()
			_, err = fmt.Fprintln(command.OutOrStdout(), project.SubjectRoot)
			return err
		},
	}
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	return command
}

func newWorkspaceAliasCommand(deps workspaceCommandDeps) *cobra.Command {
	command := &cobra.Command{Use: "alias", Short: "Set or clear a project alias", Args: cobra.NoArgs}
	command.AddCommand(newWorkspaceAliasSetCommand(deps), newWorkspaceAliasClearCommand(deps))
	return command
}

func newWorkspaceAliasSetCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	command := &cobra.Command{
		Use:   "set <selector> <alias>",
		Short: "Set a project alias",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			registry, project, err := openWritableAndResolveWorkspaceProject(command.Context(), deps, args[0])
			if err != nil {
				return err
			}
			if err = registry.SetAlias(command.Context(), project.ID, args[1]); err == nil {
				project, err = registry.ResolveProjectByRoot(command.Context(), project.SubjectRoot)
			}
			if closeErr := registry.Close(); err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return writeWorkspaceProject(command.OutOrStdout(), output, project)
		},
	}
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	addWorkspaceOutputFlag(command, &output)
	return command
}

func newWorkspaceAliasClearCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	command := &cobra.Command{
		Use:   "clear <selector>",
		Short: "Clear a project alias",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			registry, project, err := openWritableAndResolveWorkspaceProject(command.Context(), deps, args[0])
			if err != nil {
				return err
			}
			if err = registry.ClearAlias(command.Context(), project.ID); err == nil {
				project, err = registry.ResolveProjectByRoot(command.Context(), project.SubjectRoot)
			}
			if closeErr := registry.Close(); err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return writeWorkspaceProject(command.OutOrStdout(), output, project)
		},
	}
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	addWorkspaceOutputFlag(command, &output)
	return command
}

func openAndResolveWorkspaceProject(ctx context.Context, deps workspaceCommandDeps, selector string) (*workspacepkg.SQLiteRegistry, workspacepkg.Project, error) {
	stateRoot, err := deps.stateRoot()
	if err != nil {
		return nil, workspacepkg.Project{}, err
	}
	registry, err := workspacepkg.OpenReadOnly(stateRoot)
	if err != nil {
		return nil, workspacepkg.Project{}, err
	}
	project, err := resolveWorkspaceProject(ctx, registry, deps, selector)
	if err != nil {
		_ = registry.Close()
		return nil, workspacepkg.Project{}, err
	}
	return registry, project, nil
}

func openWritableAndResolveWorkspaceProject(ctx context.Context, deps workspaceCommandDeps, selector string) (*workspacepkg.SQLiteRegistry, workspacepkg.Project, error) {
	stateRoot, err := deps.stateRoot()
	if err != nil {
		return nil, workspacepkg.Project{}, err
	}
	registry, err := workspacepkg.OpenReadWrite(stateRoot, deps.registryOptions...)
	if err != nil {
		return nil, workspacepkg.Project{}, err
	}
	project, err := resolveWorkspaceProject(ctx, registry, deps, selector)
	if err != nil {
		_ = registry.Close()
		return nil, workspacepkg.Project{}, err
	}
	return registry, project, nil
}

func resolveWorkspaceProject(ctx context.Context, registry *workspacepkg.SQLiteRegistry, deps workspaceCommandDeps, selector string) (workspacepkg.Project, error) {
	if selector != "" {
		return registry.ResolveProject(ctx, selector)
	}
	start, err := deps.getwd()
	if err != nil {
		return workspacepkg.Project{}, fmt.Errorf("read current directory: %w", err)
	}
	subjectRoot, err := workspacepkg.DiscoverSubjectRoot(start)
	if err != nil {
		return workspacepkg.Project{}, err
	}
	return registry.ResolveProjectByRoot(ctx, subjectRoot)
}

func loadWorkspaceList(ctx context.Context, deps workspaceCommandDeps, includeAll bool) (workspaceListData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stateRoot, err := deps.stateRoot()
	if err != nil {
		return workspaceListData{}, err
	}
	registry, err := workspacepkg.OpenReadOnly(stateRoot)
	if errors.Is(err, workspacepkg.ErrNotFound) {
		return emptyWorkspaceList(), nil
	}
	if err != nil {
		return workspaceListData{}, err
	}
	defer func() { _ = registry.Close() }()
	projects, err := registry.ListProjects(ctx, workspacepkg.ListOptions{})
	if err != nil {
		return workspaceListData{}, err
	}
	data := workspaceListData{Projects: nonNilProjects(projects), Trash: []workspacepkg.TrashWorkspace{}, IncompleteOperations: []workspacepkg.Operation{}}
	if includeAll {
		if data.Trash, err = registry.ListTrashWorkspaces(ctx); err != nil {
			return workspaceListData{}, err
		}
		if data.IncompleteOperations, err = registry.ListIncompleteOperations(ctx); err != nil {
			return workspaceListData{}, err
		}
		if data.Trash == nil {
			data.Trash = []workspacepkg.TrashWorkspace{}
		}
		if data.IncompleteOperations == nil {
			data.IncompleteOperations = []workspacepkg.Operation{}
		}
	}
	return data, nil
}

func completeWorkspaceProjectSelectors(deps workspaceCommandDeps) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(command *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		data, err := loadWorkspaceList(command.Context(), deps, false)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		unique := make(map[string]struct{})
		for _, project := range data.Projects {
			for _, candidate := range []string{project.ID, project.Alias, project.Slug} {
				if candidate != "" && strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(prefix)) {
					unique[candidate] = struct{}{}
				}
			}
		}
		values := make([]string, 0, len(unique))
		for value := range unique {
			values = append(values, value)
		}
		sort.Strings(values)
		if len(values) > completionResultLimit {
			values = values[:completionResultLimit]
		}
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

func writeWorkspaceProject(writer io.Writer, output string, project workspacepkg.Project) error {
	if output == "json" {
		return writeWorkspaceJSON(writer, workspaceProjectData{Project: project})
	}
	_, err := fmt.Fprintf(writer, "alias=%s\nproject_id=%s\nslug=%s\nstate_dir=%s\nsubject_root=%s\n", displayEmpty(project.Alias), project.ID, project.Slug, project.StateDir, project.SubjectRoot)
	return err
}

func writeWorkspaceShow(writer io.Writer, output string, data workspaceShowData) error {
	if output == "json" {
		return writeWorkspaceJSON(writer, data)
	}
	if err := writeWorkspaceProject(writer, "text", data.Project); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "workspaces:"); err != nil {
		return err
	}
	for _, workspace := range data.Workspaces {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", workspace.TeamName, workspace.State, workspace.ID, workspace.ControlRoot); err != nil {
			return err
		}
	}
	return nil
}

func writeWorkspaceList(writer io.Writer, output string, data workspaceListData, includeAll bool) error {
	if output == "json" {
		if !includeAll {
			return writeWorkspaceJSON(writer, workspaceProjectListData{Projects: data.Projects})
		}
		return writeWorkspaceJSON(writer, data)
	}
	if _, err := fmt.Fprintln(writer, "PROJECT_ID\tALIAS\tSLUG\tSUBJECT_ROOT"); err != nil {
		return err
	}
	for _, project := range data.Projects {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", project.ID, displayEmpty(project.Alias), project.Slug, project.SubjectRoot); err != nil {
			return err
		}
	}
	if !includeAll {
		return nil
	}
	if _, err := fmt.Fprintln(writer, "TRASH_ID\tPROJECT_ID\tTEAM\tSTATE\tTRASH_PATH"); err != nil {
		return err
	}
	for _, item := range data.Trash {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", item.TrashID, item.ProjectID, item.TeamName, item.State, item.TrashPath); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(writer, "OPERATION_ID\tKIND\tSTATE\tPROJECT_ID\tWORKSPACE_ID"); err != nil {
		return err
	}
	for _, operation := range data.IncompleteOperations {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", operation.ID, operation.Kind, operation.State, operation.ProjectID, operation.WorkspaceID); err != nil {
			return err
		}
	}
	return nil
}

func writeWorkspaceJSON(writer io.Writer, data any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(workspaceOutputEnvelope{SchemaVersion: workspaceOutputSchemaVersion, Data: data, Warnings: []string{}})
}

func workspaceStartPath(deps workspaceCommandDeps, args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	path, err := deps.getwd()
	if err != nil {
		return "", fmt.Errorf("read current directory: %w", err)
	}
	return path, nil
}

func addWorkspaceOutputFlag(command *cobra.Command, target *string) {
	command.Flags().StringVar(target, "output", "text", "Output format: text or json")
	registerStaticFlagCompletion(command, "output", []string{"text", "json"})
}

func validateWorkspaceOutput(output string) error {
	if output != "text" && output != "json" {
		return fmt.Errorf("invalid --output %q: expected text or json", output)
	}
	return nil
}

func firstArgument(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func displayEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func emptyWorkspaceList() workspaceListData {
	return workspaceListData{Projects: []workspacepkg.Project{}, Trash: []workspacepkg.TrashWorkspace{}, IncompleteOperations: []workspacepkg.Operation{}}
}

func nonNilProjects(projects []workspacepkg.Project) []workspacepkg.Project {
	if projects == nil {
		return []workspacepkg.Project{}
	}
	return projects
}

func nonNilWorkspaces(workspaces []workspacepkg.Workspace) []workspacepkg.Workspace {
	if workspaces == nil {
		return []workspacepkg.Workspace{}
	}
	return workspaces
}
