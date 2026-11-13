package main

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

var (
	resumeTeamName              string
	resumeSearch                string
	resumeBranch                string
	resumeRun                   string
	resumeAcceptanceTimeoutSet  bool
	resumeRetryAcceptance       bool
	resumeAcceptanceTimeout     time.Duration
	resumeRepairResult          string
	resumeRepairAction          string
	resumeRebuildActionPayloads bool
	resumeRetryWork             string
	resumeRepairModel           string
	resumeUpgradeContracts      bool
	resumeUpgradeInputPolicy    bool
	resumeMetadataOnly          bool
	resumeRecoveryBranch        string
	resumeRecoveryPlan          string
)

var resumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume a session from its durable checkpoint",
	Long: `Resume one team session from its existing workspace checkpoint.

The command restores session state, re-drives only interrupted tasks according
to their recovery policy, and then returns control to the coordinator. It does
not create a new session or replay completed tasks automatically.

New sessions save secret-free launch settings in runtime.sqlite. Resume restores
them before applying explicit CLI/profile overrides and checking policy drift.
Older sessions without these settings still require the original configuration.

--run checks membership in the selected branch lineage; it does not rewind to
that run. To start from a saved event, use session fork <event-id>.
Required workspace versioning switches the managed files and checkpoint together.
Off/observe mode restores session metadata only, without rewinding project files.

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
	resumeAcceptanceTimeoutSet = cmd.Flags().Changed("acceptance-timeout")
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
	promoted, registeredTeam, err := bindManagedResumeWorkspace(commandContext(cmd), binding, cmp.Or(resumeTeamName, opts.agentTeamName), opts.dryRun)
	if err != nil {
		return err
	}
	binding = promoted
	if registeredTeam != "" {
		teamName = registeredTeam
	}
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
	source, err := team.ReadResumeSource(workspace, resumeBranch, resumeRun)
	if err != nil {
		return err
	}
	restoreResumeLaunchDefaults(cmd, source)
	// Saved unattended defaults must disable interactive flags just as they
	// would when supplied on the command line.
	if err := validateRunFlags(); err != nil {
		return err
	}
	if err := validateResumeProfile(teamName, registry, vars); err != nil {
		return err
	}
	if resumeBranchRecoveryRequested() {
		return runContractRecoveryCommand(cmd, source, teamName, binding, registry, vars)
	}
	if err := validateResumeRepairTarget(source, resumeRepairResult); err != nil {
		return team.ResumeSourceError(workspace, err)
	}
	if opts.dryRun {
		tc, err := loadTeamByNameWithWorkspaceBinding(withResumeLaunchPolicy(context.Background(), source), teamName, binding, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, tc.Close()) }()
		if resumeRepairResult == "" {
			return runResumeSourcePreflight(cmd, tc.session, tc.coordinator, source)
		}
		if err := bindResumePreflightVersionContext(cmd.Context(), tc.session, false); err != nil {
			return err
		}
		assessment, err := tc.coordinator.AssessResumeResultCollection(cmd.Context(), source, resumeRepairResult)
		if err != nil {
			return err
		}
		return printResultRecoveryAssessment(cmd, assessment)
	}
	activationCtx := withResumeActivation(withResumeLaunchPolicy(commandContext(cmd), source), source)
	tc, err := loadTeamByNameWithWorkspaceBinding(activationCtx, teamName, binding, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
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
	if resumeRetryAcceptance {
		ctx = team.WithFinalAcceptanceRetry(ctx, resumeRun, resumeAcceptanceTimeout)
	}
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
	_, err = team.ResolveExecutionProfile(opts.executionProfile, session.Config.ExecutionProfile)
	if err != nil {
		return fmt.Errorf("failed to resolve resume profile: %w", err)
	}
	// Canonical source selection and explicit resume distinguish same-session
	// restoration from a fresh user request. Frozen policy admission still
	// rejects profile or execution-authority changes.
	return nil
}

func init() {
	f := resumeCmd.Flags()
	registerResumeLaunchFlags(f)
	f.StringVar(&resumeBranch, "branch", "", "Exact existing branch ID, name, or label to resume (defaults to active)")
	f.StringVar(&resumeRun, "run", "", "Require the source run to belong to the selected branch")
	f.BoolVar(&resumeUpgradeContracts, "upgrade-contracts", false, "Preview or create a recovery child for result schemas or rejected submission-only transport; ordinary resume checks remain mandatory")
	f.BoolVar(&resumeUpgradeInputPolicy, "upgrade-input-policy", false, "Explicitly include resolver/provider policy changes in --upgrade-contracts; preserves verified frozen inputs and requires read-only contracts")
	f.BoolVar(&resumeMetadataOnly, "metadata-only", false, "Explicit lineage-only recovery for unversioned read-only work or verified result-only upgrades; project files remain shared")
	f.StringVar(&resumeRecoveryBranch, "recovery-branch", "", "New child branch name for an explicitly previewed recovery")
	f.StringVar(&resumeRecoveryPlan, "plan-hash", "", "Exact SHA-256 from the recovery preview")
	f.BoolVar(&resumeRetryAcceptance, "retry-acceptance", false, "Retry unavailable final request acceptance against the saved answer; never redispatch workers or collectors")
	f.DurationVar(&resumeAcceptanceTimeout, "acceptance-timeout", 5*time.Minute, "Per-review call timeout for --retry-acceptance; preserves frozen work and execution policy")
	f.StringVar(&resumeRepairResult, "repair-result", "", "Authorize bounded result-only recovery; may combine with --upgrade-contracts; never replay worker tools")
	f.StringVar(&resumeRepairAction, "repair-action", "", "Preview or fork a changed read-only Go action source repair; retains verified work and frozen inputs")
	f.BoolVar(&resumeRebuildActionPayloads, "rebuild-action-payloads", false, "Explicitly reconstruct redacted public numeric budgets with --repair-action, verified against original frozen payload hashes")
	f.StringVar(&resumeRetryWork, "retry-work", "", "Preview or fork a controlled retry of one completed read-only work attempt whose required decisions never started")
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
	f.BoolVar(&opts.dryRun, "dry-run", false, "Read-only resume, result repair or contract recovery preflight; no branch activation, tools or model calls")
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

func resumeBranchRecoveryRequested() bool {
	return resumeUpgradeContracts || resumeRetryWork != "" || resumeRepairAction != ""
}

func validateResumeRepairFlags() error {
	if err := validateAcceptanceRetryFlags(); err != nil {
		return err
	}

	if err := validateResumeActionRepairFlags(); err != nil {
		return err
	}

	if resumeRetryWork != "" && (strings.TrimSpace(resumeRetryWork) != resumeRetryWork || resumeUpgradeContracts || resumeUpgradeInputPolicy) {
		return fmt.Errorf("--retry-work requires an exact task ID and cannot combine with contract or input policy upgrades")
	}
	if resumeBranchRecoveryRequested() {
		if resumeRepairResult != "" && (!resumeUpgradeContracts || resumeUpgradeInputPolicy || strings.TrimSpace(resumeRepairResult) != resumeRepairResult) {
			return fmt.Errorf("combined result repair requires --upgrade-contracts, an exact task ID and unchanged input policy")
		}
		if resumeRepairModel != "" || opts.reportMode || opts.tuiMode || len(opts.inputFlags) > 0 || len(opts.inputFiles) > 0 {
			return fmt.Errorf("recovery branching cannot combine with model overrides, report, tui or input assignments")
		}
		if opts.dryRun {
			if resumeRecoveryBranch != "" || resumeRecoveryPlan != "" {
				return fmt.Errorf("preview cannot apply a recovery plan")
			}
			return nil
		}
		if strings.TrimSpace(resumeRecoveryBranch) == "" || len(resumeRecoveryPlan) != 64 {
			return fmt.Errorf("preview with --upgrade-contracts, --retry-work or --repair-action and --dry-run, then supply --recovery-branch and --plan-hash")
		}
		return nil
	}
	if resumeRecoveryBranch != "" || resumeRecoveryPlan != "" || resumeMetadataOnly || resumeUpgradeInputPolicy {
		return fmt.Errorf("branch recovery options require --upgrade-contracts, --retry-work or --repair-action; --upgrade-input-policy requires --upgrade-contracts")
	}

	if resumeRepairModel != "" && resumeRepairResult == "" {
		return fmt.Errorf("--repair-model requires --repair-result")
	}
	if opts.dryRun && (resumeRepairModel != "" || opts.reportMode || opts.tuiMode || len(opts.inputFlags) > 0 || len(opts.inputFiles) > 0) {
		return fmt.Errorf("resume --dry-run cannot use --repair-model, --report, --tui, or input assignments")
	}
	return nil
}

func validateAcceptanceRetryFlags() error {
	if resumeRetryAcceptance {
		if strings.TrimSpace(resumeRun) == "" || strings.TrimSpace(resumeRun) != resumeRun || resumeAcceptanceTimeout <= 0 {
			return fmt.Errorf("--retry-acceptance requires an exact --run and positive --acceptance-timeout")
		}
		if resumeRepairResult != "" || resumeRepairAction != "" || resumeRebuildActionPayloads || resumeRepairModel != "" || resumeRetryWork != "" || resumeUpgradeContracts || resumeUpgradeInputPolicy || resumeMetadataOnly || resumeRecoveryBranch != "" || resumeRecoveryPlan != "" || len(opts.imagePaths) > 0 || len(opts.inputFlags) > 0 || len(opts.inputFiles) > 0 {
			return fmt.Errorf("--retry-acceptance cannot combine with task repair, branch recovery, media or new inputs")
		}
	} else if resumeAcceptanceTimeoutSet {
		return fmt.Errorf("--acceptance-timeout requires --retry-acceptance")
	}
	return nil
}

func validateResumeActionRepairFlags() error {
	if resumeRebuildActionPayloads && resumeRepairAction == "" {
		return fmt.Errorf("--rebuild-action-payloads requires --repair-action")
	}
	if resumeRepairAction != "" && (strings.TrimSpace(resumeRepairAction) != resumeRepairAction || resumeUpgradeContracts || resumeUpgradeInputPolicy || resumeRetryWork != "" || resumeRepairResult != "") {
		return fmt.Errorf("--repair-action requires one exact task ID and cannot combine with other repair modes")
	}
	return nil
}
