package main

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

var (
	resumeTeamName     string
	resumeSearch       string
	resumeBranch       string
	resumeRun          string
	resumeRepairResult string
	resumeRepairModel  string
)

var resumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume a session from its durable checkpoint",
	Long: `Resume one team session from its existing workspace checkpoint.

The command restores session state, re-drives only interrupted tasks according
to their recovery policy, and then returns control to the coordinator. It does
not create a new session or replay completed tasks automatically.

Examples:
  hufu resume --workspace ./workspace/my-team
  hufu resume --team my-team`,
	Args: cobra.NoArgs,
	RunE: runResumeCommand,
}

func runResumeCommand(cmd *cobra.Command, _ []string) (runErr error) {
	syncLogState()
	if err := applyProfile(cmd); err != nil {
		return err
	}
	opts.requestAcceptanceSet = cmd.Flags().Changed("request-acceptance")
	if err := validateRunFlags(); err != nil {
		return err
	}
	if err := validateResumeRepairFlags(); err != nil {
		return err
	}
	if err := captureRuntimeRoots(); err != nil {
		return err
	}
	configureOutputRendering()

	binding, teamName, err := resolveExistingExecutionWorkspace(context.Background(), resumeTeamName, opts.workspace != "")
	if err != nil {
		return err
	}
	defer func() {
		if binding != nil && binding.Lease != nil {
			runErr = errors.Join(runErr, binding.Lease.Close())
		}
	}()
	workspace := binding.Resolution.ControlRoot
	if !team.HasSession(workspace) {
		return fmt.Errorf("no session found in %s; resume requires an existing session checkpoint", workspace)
	}

	searchPaths := resolveSearchPaths()
	if strings.TrimSpace(resumeSearch) != "" {
		searchPaths = strings.Split(resumeSearch, ",")
	}
	registry := team.NewTeamRegistry(searchPaths)
	if err := registry.Discover(); err != nil {
		return fmt.Errorf("failed to discover teams: %w", err)
	}
	if !registry.HasTeam(teamName) {
		return fmt.Errorf("team %q not found; pass --agent-team-search-path or run hufu list", teamName)
	}

	vars, err := buildVars()
	if err != nil {
		return err
	}
	if err := validateResumeProfile(teamName, registry, vars); err != nil {
		return err
	}
	source, err := team.ReadResumeSource(workspace, resumeBranch, resumeRun)
	if err != nil {
		return err
	}
	if resumeRepairResult != "" {
		found := false
		for _, item := range source.Session.Tasks {
			if item != nil && item.ID == resumeRepairResult && item.Status == team.TaskBlocked && item.FailureEvent != nil && item.FailureEvent.FailureClass == team.FailureProtocol {
				found = true
			}
		}
		if !found {
			return team.ResumeSourceError(workspace, fmt.Errorf("branch %q has no blocked protocol task %q", source.BranchID, resumeRepairResult))
		}
	}
	if opts.dryRun {
		tc, err := loadTeamByNameWithWorkspaceBinding(context.Background(), teamName, binding, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), vars, opts.forcedSkills, false, false)
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, tc.Close()) }()
		assessment, err := tc.coordinator.AssessResumeResultCollection(cmd.Context(), source, resumeRepairResult)
		if err != nil {
			return err
		}
		return printResultRecoveryAssessment(cmd, assessment)
	}
	if err := team.ActivateResumeSource(context.Background(), workspace, source); err != nil {
		return team.ResumeSourceError(workspace, err)
	}
	tc, err := loadTeamByNameWithWorkspaceBinding(context.Background(), teamName, binding, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	if err != nil {
		return team.ResumeSourceError(workspace, fmt.Errorf("failed to load team %q: %w", teamName, err))
	}
	if tc == nil || tc.coordinator == nil || tc.sessionData == nil {
		_ = tc.Close()
		return fmt.Errorf("team %q has no resumable coordinator session", teamName)
	}
	defer func() {
		runErr = errors.Join(runErr, tc.Close())
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = team.WithSessionResume(ctx)
	if resumeRepairResult != "" {
		ctx = team.WithResultOnlyRepair(ctx, resumeRepairResult)
		ctx = team.WithResultRepairModel(ctx, resumeRepairModel)
	}
	injector := newPromptInjector(nil)
	activeCoord := new(activeCoordinator)
	var loadedTeams map[string]*teamContext
	defer setupPromptSignals(injector)()
	defer setupInterruptHandler(injector, activeCoord, &loadedTeams, cancel)()

	loadedTeams = map[string]*teamContext{teamName: tc}
	if err := configureRunInputAssignments(loadedTeams); err != nil {
		return err
	}
	segments := []team.PromptSegment{{
		Type:    team.SegmentSwitchTeam,
		Name:    teamName,
		Content: resumeInstruction(tc.sessionData),
	}}
	return executeAndReport(ctx, cancel, "", "", segments, registry, loadedTeams, injector, activeCoord, nil, vars, RouteDecision{Route: RouteTeam, Team: teamName})
}

func resumeInstruction(session *team.SessionData) string {
	base := team.ResumeInstruction
	if session == nil {
		return base
	}
	var taskIDs []string
	for _, item := range session.Tasks {
		if item != nil && item.FailureEvent != nil && item.FailureEvent.RetryDisposition == team.ReplanRequired {
			taskIDs = append(taskIDs, item.ID)
		}
	}
	if len(taskIDs) == 0 {
		return base
	}
	return fmt.Sprintf("%s Tasks %s require a materially changed plan. Preserve their failure evidence and do not replay the original attempts.", base, strings.Join(taskIDs, ", "))
}

func validateResumeProfile(teamName string, registry *team.TeamRegistry, vars map[string]string) error {
	teamDir, err := registry.Resolve(teamName)
	if err != nil {
		return err
	}
	session, err := team.LoadTeam(teamDir, vars, nil, team.DefaultProviderRegistry)
	if err != nil {
		return fmt.Errorf("failed to inspect team %q: %w", teamName, err)
	}
	profile, err := team.ResolveExecutionProfile(opts.executionProfile, session.Config.ExecutionProfile)
	if err != nil {
		return fmt.Errorf("failed to resolve resume profile: %w", err)
	}
	if profile.DisableHistoricalTaskReuse || profile.DisableJournalRestore {
		return fmt.Errorf("resume cannot use execution profile %q because it disables historical session restore", profile.Name)
	}
	return nil
}

func init() {
	f := resumeCmd.Flags()
	f.StringVar(&resumeBranch, "branch", "", "Exact existing branch ID, name, or label to resume (defaults to active)")
	f.StringVar(&resumeRun, "run", "", "Require the source run to belong to the selected branch")
	f.StringVar(&resumeRepairResult, "repair-result", "", "Authorize two result-only repair turns for one blocked protocol task; never replay worker tools")
	f.StringVar(&resumeRepairModel, "repair-model", "", "Auxiliary result submission model (must already be frozen in the source policy); preserves the original worker target")
	registerTeamSelectionFlags(f, &resumeTeamName, "Agent team name (inferred from --workspace when omitted)")
	f.StringVar(&resumeSearch, "agent-team-search-path", "", "Comma-separated team search paths")
	f.StringVar(&opts.providerURL, "provider-url", "", "Provider API base URL")
	f.StringVar(&opts.providerAPIKey, "provider-api-key", "", "Provider API key")
	f.StringVar(&opts.modelOverride, "model", "", "Override worker execution target for the resumed team")
	f.StringSliceVar(&opts.workerModelOverrides, "worker-model", nil, "Override the execution target of specific worker(s) as agent=target; repeatable (must match the original run)")
	f.StringVarP(&opts.coordinatorModelOverride, "coordinator-model", "c", "", "Override coordinator LLM target for the resumed team")
	f.IntVar(&opts.contextWindowOverride, "context-window", 0, "Override model context window in tokens")
	f.Int64Var(&opts.timeoutOverride, "timeout", 0, "Override agent/coordinator timeout in seconds")
	f.DurationVar(&opts.gracefulWrapUpTimeout, "graceful-wrap-up-timeout", 0, "Maximum time to let active work finish after the first Ctrl+C")
	f.StringArrayVar(&opts.varFlags, "var", nil, "Set template variable key=value (repeatable)")
	f.StringArrayVar(&opts.varFiles, "var-file", nil, "Read template variables from a file (repeatable)")
	f.StringArrayVar(&opts.inputFlags, "input", nil, "Set a typed run input (name=value; repeatable)")
	f.StringArrayVar(&opts.inputFiles, "input-file", nil, "Read typed run inputs from JSON or YAML (repeatable)")
	f.StringArrayVar(&opts.forcedSkills, "skill", nil, "Force-load specific skills (repeatable)")
	f.BoolVar(&opts.dryRun, "dry-run", false, "Read-only result repair assessment; no branch activation, tools or model calls")
	f.BoolVar(&opts.verbose, "verbose", false, "Show full agent text output in real-time")
	f.BoolVar(&opts.quietMode, "quiet", false, "Suppress status output")
	f.StringVar(&opts.outputFormat, "output", "", "Final-result format: text or json")
	f.BoolVar(&opts.tuiMode, "tui", false, "Show a Bubble Tea TUI for real-time task tracking")
	f.BoolVar(&opts.think, "think", false, "Show coordinator decision reasoning")
	f.BoolVar(&opts.autoApprove, "auto-approve", false, "Automatically choose clearly safe ask_user options")
	f.BoolVar(&opts.noNet, "no-net", false, "Block all network access for agent subprocesses")
	f.BoolVar(&opts.forceMCP, "force-mcp", false, "Require MCP servers for execution")
	f.BoolVar(&opts.requestAcceptance, "request-acceptance", false, "Preserve and enforce frozen request requirements")
	f.BoolVar(&opts.unattended, "unattended", false, "Run without human interaction")
	f.BoolVar(&opts.reportMode, "report", false, "Export the saved run report to workspace/report.md")
	f.BoolVar(&opts.noSummary, "no-summary", false, "Suppress the execution summary")
	_ = resumeCmd.MarkFlagFilename("var-file")
}

func printResultRecoveryAssessment(cmd *cobra.Command, a *team.ResultCollectionRecoveryAssessment) error {
	if opts.outputFormat == "json" {
		body, err := json.Marshal(a, json.Deterministic(true))
		if err != nil {
			return err
		}
		if a.Checkpoint == nil {
			var fields map[string]jsontext.Value
			if err := json.Unmarshal(body, &fields); err != nil {
				return err
			}
			delete(fields, "turns_used")
			delete(fields, "remaining_turns")
			delete(fields, "additional_turns")
			body, err = json.Marshal(fields, json.Deterministic(true))
			if err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(body))
		return err
	}
	count := "unknown"
	if a.AcceptedTasks != nil {
		count = fmt.Sprint(*a.AcceptedTasks)
	}
	continuation := "After a successful repair, execution continues the coordinator and any undispatched downstream work."
	if !a.ContinuesWorkflow {
		continuation = "Other unresolved tasks remain; inspect them before ordinary resume continues the workflow."
	}
	if a.Disposition == team.RecoveryBlocked {
		continuation = "Recovery is blocked; no grant or execution is authorized."
	}
	turns := "unknown (no verified collection checkpoint)"
	if a.Checkpoint != nil {
		turns = fmt.Sprintf("used: %d; remaining: %d; additional maximum: %d", a.TurnsUsed, a.RemainingTurns, a.AdditionalTurns)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Task: %s; branch: %s; source run: %s\nRecovery: %s\nAccepted tasks retained: %s\nCollector turns %s\n%s\n", cmp.Or(a.TaskID, "unknown"), cmp.Or(a.BranchID, "unknown"), cmp.Or(a.SourceRunID, "unknown"), a.Disposition, count, turns, continuation)
	if err != nil {
		return err
	}
	for _, check := range a.Checks {
		if !check.Passed {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", check.Code, check.Diagnostic); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateResumeRepairFlags() error {
	if resumeRepairModel != "" && resumeRepairResult == "" {
		return fmt.Errorf("--repair-model requires --repair-result")
	}
	if opts.dryRun && (resumeRepairResult == "" || resumeRepairModel != "" || opts.reportMode || opts.tuiMode || len(opts.inputFlags) > 0 || len(opts.inputFiles) > 0) {
		return fmt.Errorf("resume --dry-run requires --repair-result and cannot use --repair-model, --report, --tui, or input assignments")
	}
	return nil
}
