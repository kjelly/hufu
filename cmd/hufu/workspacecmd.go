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
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

const workspaceOutputSchemaVersion = 1
const (
	workspaceTrashRetentionText = "720h"
	workspaceTrashRetention     = 720 * time.Hour
)

type workspaceCommandDeps struct {
	stateRoot       func() (string, error)
	getwd           func() (string, error)
	confirmMutation func(action string) error
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
	return workspaceCommandDeps{
		stateRoot:       workspacepkg.DefaultStateRoot,
		getwd:           os.Getwd,
		confirmMutation: promptWorkspaceMutation,
	}
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
		newWorkspaceMigrateCommand(deps),
		newWorkspaceDeleteCommand(deps),
		newWorkspaceRestoreCommand(deps),
		newWorkspacePurgeCommand(deps),
		newWorkspaceGCCommand(deps),
		newWorkspaceDoctorCommand(deps),
		newWorkspaceShellInitCommand(),
		newWorkspaceVersionCommand(),
	)
	return command
}

func newWorkspaceGCCommand(deps workspaceCommandDeps) *cobra.Command {
	var output, olderThan string
	var dryRun, apply, yes bool
	command := &cobra.Command{
		Use:   "gc",
		Short: "Preview or purge expired managed workspace trash",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			if dryRun && apply {
				return errors.New("--dry-run and --apply are mutually exclusive")
			}
			if apply && !yes {
				return errors.New("workspace gc --apply requires explicit --yes confirmation")
			}
			if !apply && yes {
				return errors.New("--yes is only valid with --apply")
			}
			retention, err := time.ParseDuration(olderThan)
			if err != nil || retention <= 0 {
				return fmt.Errorf("invalid --trash-older-than %q: expected a positive duration", olderThan)
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			manager, err := workspacepkg.NewManager(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			result, gcErr := manager.GC(command.Context(), workspacepkg.GCRequest{Apply: apply, TrashOlderThan: retention})
			if gcErr != nil && result.Outcome == "" {
				return &workspaceOutcomeError{outcome: "failed", cause: gcErr}
			}
			if err = writeWorkspaceGC(command.OutOrStdout(), output, result); err != nil {
				return errors.Join(gcErr, err)
			}
			if gcErr != nil || result.Outcome != "complete" {
				return &workspaceOutcomeError{outcome: result.Outcome, cause: gcErr}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Preview eligible trash without changing state (default)")
	command.Flags().BoolVar(&apply, "apply", false, "Permanently purge eligible trash")
	command.Flags().BoolVar(&yes, "yes", false, "Confirm permanent purge when --apply is set")
	command.Flags().StringVar(&olderThan, "trash-older-than", workspaceTrashRetentionText, "Only select trash older than this duration")
	addWorkspaceOutputFlag(command, &output)
	command.ValidArgsFunction = cobra.NoFileCompletions
	return command
}

func newWorkspaceShellInitCommand() *cobra.Command {
	command := &cobra.Command{
		Use:               "shell-init <bash|zsh|fish|powershell>",
		Short:             "Print static shell helpers for managed workspace navigation",
		Args:              cobra.ExactArgs(1),
		ValidArgs:         []string{"bash", "zsh", "fish", "powershell"},
		ValidArgsFunction: cobra.FixedCompletions([]string{"bash", "zsh", "fish", "powershell"}, cobra.ShellCompDirectiveNoFileComp),
		RunE: func(command *cobra.Command, args []string) error {
			source, ok := workspaceShellInitSource(args[0])
			if !ok {
				return fmt.Errorf("unsupported shell %q: expected bash, zsh, fish, or powershell", args[0])
			}
			_, err := io.WriteString(command.OutOrStdout(), source)
			return err
		},
	}
	return command
}

func workspaceShellInitSource(shell string) (string, bool) {
	switch strings.ToLower(shell) {
	case "bash", "zsh":
		return `hcd() {
    local p
    p="$(command hufu workspace path "$@")" || return
    cd -- "$p"
}

hproj() {
    local p
    p="$(command hufu workspace subject-path "$@")" || return
    cd -- "$p"
}
`, true
	case "fish":
		return `function hcd
    set -l p (command hufu workspace path $argv); or return
    cd -- "$p"
end

function hproj
    set -l p (command hufu workspace subject-path $argv); or return
    cd -- "$p"
end
`, true
	case "powershell":
		return `function hcd {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]] $HufuArgs)
    $p = & hufu workspace path @HufuArgs
    if ($LASTEXITCODE -ne 0) { return }
    Set-Location -LiteralPath $p
}

function hproj {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]] $HufuArgs)
    $p = & hufu workspace subject-path @HufuArgs
    if ($LASTEXITCODE -ne 0) { return }
    Set-Location -LiteralPath $p
}
`, true
	default:
		return "", false
	}
}

func newWorkspaceDeleteCommand(deps workspaceCommandDeps) *cobra.Command {
	var teamName, output string
	var allTeams, yes bool
	command := &cobra.Command{
		Use:   "delete [selector]",
		Short: "Move managed team workspaces to recoverable trash",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			if allTeams && command.Flags().Changed("team") {
				return errors.New("--team and --all-teams are mutually exclusive")
			}
			if err := confirmWorkspaceMutation("delete", yes, deps.confirmMutation); err != nil {
				return err
			}
			start, err := deps.getwd()
			if err != nil {
				return fmt.Errorf("read current directory: %w", err)
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			manager, err := workspacepkg.NewManager(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			result, lifecycleErr := manager.Delete(command.Context(), workspacepkg.DeleteRequest{
				StartDir: start, Selector: firstArgument(args), TeamName: teamName,
				AllTeams: allTeams, Retention: workspaceTrashRetention,
			})
			return finishWorkspaceLifecycle(command, output, "delete", result, lifecycleErr)
		},
	}
	command.Flags().StringVar(&teamName, "team", "default", "Team workspace name")
	command.Flags().BoolVar(&allTeams, "all-teams", false, "Delete every active team workspace")
	command.Flags().BoolVar(&yes, "yes", false, "Confirm moving the selected workspace(s) to trash")
	addWorkspaceOutputFlag(command, &output)
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	return command
}

func newWorkspaceRestoreCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	var yes bool
	command := &cobra.Command{
		Use:   "restore <trash-id>",
		Short: "Restore one trashed managed workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			if err := confirmWorkspaceMutation("restore", yes, deps.confirmMutation); err != nil {
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
			result, lifecycleErr := manager.Restore(command.Context(), workspacepkg.RestoreRequest{TrashID: args[0]})
			return finishWorkspaceLifecycle(command, output, "restore", result, lifecycleErr)
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "Confirm restoring the selected workspace")
	addWorkspaceOutputFlag(command, &output)
	command.ValidArgsFunction = completeWorkspaceTrashIDs(deps)
	return command
}

func newWorkspacePurgeCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	var yes bool
	command := &cobra.Command{
		Use:   "purge <trash-id>",
		Short: "Permanently remove one trashed managed workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			if !yes {
				return errors.New("workspace purge requires explicit --yes confirmation")
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			manager, err := workspacepkg.NewManager(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			result, lifecycleErr := manager.Purge(command.Context(), workspacepkg.PurgeRequest{TrashID: args[0]})
			return finishWorkspaceLifecycle(command, output, "purge", result, lifecycleErr)
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "Confirm permanent deletion")
	addWorkspaceOutputFlag(command, &output)
	command.ValidArgsFunction = completeWorkspaceTrashIDs(deps)
	return command
}

func confirmWorkspaceMutation(action string, yes bool, confirm func(string) error) error {
	if yes {
		return nil
	}
	if confirm == nil {
		return fmt.Errorf("workspace %s requires --yes when confirmation input is unavailable", action)
	}
	return confirm(action)
}

func promptWorkspaceMutation(action string) error {
	if !term.IsTerminal(os.Stdin.Fd()) {
		return fmt.Errorf("workspace %s requires --yes when stdin is not a TTY", action)
	}
	prompt := promptui.Prompt{Label: fmt.Sprintf("Confirm workspace %s", action), IsConfirm: true}
	if _, err := prompt.Run(); err != nil {
		return fmt.Errorf("workspace %s cancelled: %w", action, err)
	}
	return nil
}

func finishWorkspaceLifecycle(command *cobra.Command, output, action string, result workspacepkg.LifecycleResult, lifecycleErr error) error {
	if len(result.Items) == 0 && lifecycleErr != nil {
		return lifecycleErr
	}
	if err := writeWorkspaceLifecycle(command.OutOrStdout(), output, action, result); err != nil {
		return errors.Join(lifecycleErr, err)
	}
	if lifecycleErr != nil || result.Outcome != "complete" {
		return &workspaceOutcomeError{outcome: result.Outcome, cause: lifecycleErr}
	}
	return nil
}

type workspaceOutcomeError struct {
	outcome string
	cause   error
}

func (e *workspaceOutcomeError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return "workspace operation finished with outcome " + e.outcome
}

func (e *workspaceOutcomeError) Unwrap() error { return e.cause }

func newWorkspaceMigrateCommand(deps workspaceCommandDeps) *cobra.Command {
	var teamName, legacyRoot, output string
	var allTeams bool
	command := &cobra.Command{
		Use:   "migrate [selector]",
		Short: "Import legacy workspaces without modifying their sources",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			if allTeams && command.Flags().Changed("team") {
				return errors.New("--team and --all-teams are mutually exclusive")
			}
			start, err := deps.getwd()
			if err != nil {
				return fmt.Errorf("read current directory: %w", err)
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			manager, err := workspacepkg.NewManager(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			result, migrateErr := manager.Migrate(command.Context(), workspacepkg.MigrateRequest{
				StartDir: start, Selector: firstArgument(args), TeamName: teamName,
				AllTeams: allTeams, LegacyRoot: legacyRoot,
			})
			if migrateErr != nil && len(result.Failed) == 0 && len(result.Completed) == 0 {
				return migrateErr
			}
			if err = writeWorkspaceMigration(command.OutOrStdout(), output, result); err != nil {
				return errors.Join(migrateErr, err)
			}
			if migrateErr != nil || result.Outcome != "complete" {
				return &workspaceOutcomeError{outcome: result.Outcome, cause: migrateErr}
			}
			return nil
		},
	}
	command.Flags().StringVar(&teamName, "team", "", "Legacy team to migrate (default: default)")
	command.Flags().BoolVar(&allTeams, "all-teams", false, "Migrate every legacy team in sorted order")
	command.Flags().StringVar(&legacyRoot, "legacy-root", "", "Explicit legacy workspace parent directory")
	addWorkspaceOutputFlag(command, &output)
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
	return command
}

func newWorkspaceDoctorCommand(deps workspaceCommandDeps) *cobra.Command {
	var output string
	var repair bool
	command := &cobra.Command{
		Use:   "doctor [selector]",
		Short: "Inspect workspace registry and managed storage consistency",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateWorkspaceOutput(output); err != nil {
				return err
			}
			start, err := deps.getwd()
			if err != nil {
				return fmt.Errorf("read current directory: %w", err)
			}
			stateRoot, err := deps.stateRoot()
			if err != nil {
				return err
			}
			manager, err := workspacepkg.NewManager(stateRoot, deps.registryOptions...)
			if err != nil {
				return err
			}
			result, doctorErr := manager.Doctor(command.Context(), workspacepkg.DoctorRequest{StartDir: start, Selector: firstArgument(args), Repair: repair})
			if doctorErr != nil && result.Outcome == "" {
				return &workspaceOutcomeError{outcome: "failed", cause: doctorErr}
			}
			if err = writeWorkspaceDoctor(command.OutOrStdout(), output, result); err != nil {
				return errors.Join(doctorErr, err)
			}
			if doctorErr != nil || result.Outcome != "complete" {
				return &workspaceOutcomeError{outcome: result.Outcome, cause: doctorErr}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&repair, "repair", false, "Apply only deterministic, marker-verified repairs")
	addWorkspaceOutputFlag(command, &output)
	command.ValidArgsFunction = completeWorkspaceProjectSelectors(deps)
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

func completeWorkspaceTrashIDs(deps workspaceCommandDeps) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(command *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		stateRoot, err := deps.stateRoot()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		registry, err := workspacepkg.OpenReadOnly(stateRoot)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		defer func() { _ = registry.Close() }()
		trash, err := registry.ListTrashWorkspaces(command.Context())
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		values := make([]string, 0, len(trash))
		for _, item := range trash {
			if item.State == "trashed" && strings.HasPrefix(item.TrashID, prefix) {
				values = append(values, item.TrashID)
			}
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
	return writeWorkspaceJSONWarnings(writer, data, []string{})
}

func writeWorkspaceJSONWarnings(writer io.Writer, data any, warnings []string) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(workspaceOutputEnvelope{SchemaVersion: workspaceOutputSchemaVersion, Data: data, Warnings: warnings})
}

func writeWorkspaceMigration(writer io.Writer, output string, result workspacepkg.MigrationResult) error {
	warnings := make([]string, 0)
	for _, item := range result.Completed {
		warnings = append(warnings, item.Warnings...)
	}
	sort.Strings(warnings)
	if output == "json" {
		return writeWorkspaceJSONWarnings(writer, result, warnings)
	}
	if _, err := fmt.Fprintf(writer, "outcome=%s\n", result.Outcome); err != nil {
		return err
	}
	for _, item := range result.Completed {
		if _, err := fmt.Fprintf(writer, "completed\t%s\t%s\t%s\n", item.TeamName, item.Workspace.ID, item.Workspace.ControlRoot); err != nil {
			return err
		}
	}
	for _, team := range result.Failed {
		if _, err := fmt.Fprintf(writer, "failed\t%s\n", team); err != nil {
			return err
		}
	}
	for _, team := range result.Pending {
		if _, err := fmt.Fprintf(writer, "pending\t%s\n", team); err != nil {
			return err
		}
	}
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(writer, "warning=%s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

func writeWorkspaceDoctor(writer io.Writer, output string, result workspacepkg.DoctorResult) error {
	if output == "json" {
		return writeWorkspaceJSON(writer, result)
	}
	if _, err := fmt.Fprintf(writer, "outcome=%s\n", result.Outcome); err != nil {
		return err
	}
	for _, issue := range result.Issues {
		status := "found"
		if issue.Repaired {
			status = "repaired"
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", issue.Code, status, displayEmpty(issue.ProjectID), displayEmpty(issue.WorkspaceID), displayEmpty(issue.Path)); err != nil {
			return err
		}
	}
	return nil
}

func writeWorkspaceLifecycle(writer io.Writer, output, action string, result workspacepkg.LifecycleResult) error {
	if output == "json" {
		return writeWorkspaceJSON(writer, result)
	}
	if _, err := fmt.Fprintf(writer, "%s outcome=%s\n", action, result.Outcome); err != nil {
		return err
	}
	for _, item := range result.Items {
		if _, err := fmt.Fprintf(writer, "control_root=%s\noperation_id=%s\nproject_id=%s\nteam_name=%s\ntrash_id=%s\ntrash_path=%s\nworkspace_id=%s\n", item.ControlRoot, item.OperationID, item.ProjectID, item.TeamName, displayEmpty(item.TrashID), displayEmpty(item.TrashPath), item.WorkspaceID); err != nil {
			return err
		}
	}
	return nil
}

func writeWorkspaceGC(writer io.Writer, output string, result workspacepkg.GCResult) error {
	if output == "json" {
		return writeWorkspaceJSON(writer, result)
	}
	mode := "dry-run"
	if result.Apply {
		mode = "apply"
	}
	if _, err := fmt.Fprintf(writer, "gc outcome=%s\nmode=%s\n", result.Outcome, mode); err != nil {
		return err
	}
	for _, item := range result.Candidates {
		if _, err := fmt.Fprintf(writer, "candidate\t%s\t%s\t%s\t%s\n", item.TrashID, item.ProjectID, item.TeamName, item.TrashPath); err != nil {
			return err
		}
	}
	for _, item := range result.Purged {
		if _, err := fmt.Fprintf(writer, "purged\t%s\t%s\t%s\t%s\n", item.TrashID, item.ProjectID, item.TeamName, item.TrashPath); err != nil {
			return err
		}
	}
	return nil
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
