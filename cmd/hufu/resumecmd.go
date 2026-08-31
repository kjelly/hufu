package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

var (
	resumeTeamName string
	resumeSearch   string
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
  hufu resume --agent-team my-team`,
	Args: cobra.NoArgs,
	RunE: runResumeCommand,
}

func runResumeCommand(cmd *cobra.Command, _ []string) error {
	syncLogState()
	if err := applyProfile(cmd); err != nil {
		return err
	}
	if err := validateRunFlags(); err != nil {
		return err
	}
	configureOutputRendering()

	workspace, teamName, err := resolveCommandWorkspaceAndTeam(getWorkspace(), resumeTeamName, opts.workspace != "")
	if err != nil {
		return err
	}
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
	tc, err := loadTeamByNameAtWorkspace(context.Background(), teamName, workspace, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	if err != nil {
		return fmt.Errorf("failed to load team %q: %w", teamName, err)
	}
	if tc == nil || tc.coordinator == nil || tc.sessionData == nil {
		return fmt.Errorf("team %q has no resumable coordinator session", teamName)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	injector := newPromptInjector(nil)
	activeCoord := new(activeCoordinator)
	var loadedTeams map[string]*teamContext
	defer setupPromptSignals(injector)()
	defer setupInterruptHandler(injector, activeCoord, &loadedTeams, cancel)()

	loadedTeams = map[string]*teamContext{teamName: tc}
	segments := []team.PromptSegment{{
		Type:    team.SegmentSwitchTeam,
		Name:    teamName,
		Content: "Resume the existing session from its durable checkpoint.",
	}}
	return executeAndReport(ctx, cancel, "", "", segments, registry, loadedTeams, injector, activeCoord, nil, vars, RouteDecision{Route: RouteTeam, Team: teamName})
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
	f.StringVar(&resumeTeamName, "agent-team", "", "Agent team name (inferred from --workspace when omitted)")
	f.StringVar(&resumeSearch, "agent-team-search-path", "", "Comma-separated team search paths")
	f.StringVar(&opts.providerURL, "provider-url", "", "Provider API base URL")
	f.StringVar(&opts.providerAPIKey, "provider-api-key", "", "Provider API key")
	f.StringVar(&opts.modelOverride, "model", "", "Override default model for the resumed team")
	f.IntVar(&opts.contextWindowOverride, "context-window", 0, "Override model context window in tokens")
	f.Int64Var(&opts.timeoutOverride, "timeout", 0, "Override agent/coordinator timeout in seconds")
	f.StringArrayVar(&opts.varFlags, "var", nil, "Set template variable key=value (repeatable)")
	f.StringArrayVar(&opts.varFiles, "var-file", nil, "Read template variables from a file (repeatable)")
	f.StringArrayVar(&opts.forcedSkills, "skill", nil, "Force-load specific skills (repeatable)")
	f.BoolVar(&opts.verbose, "verbose", false, "Show full agent text output in real-time")
	f.BoolVar(&opts.quietMode, "quiet", false, "Suppress status output")
	f.StringVar(&opts.outputFormat, "output", "", "Final-result format: text or json")
	f.BoolVar(&opts.tuiMode, "tui", false, "Show a Bubble Tea TUI for real-time task tracking")
	f.BoolVar(&opts.think, "think", false, "Show coordinator decision reasoning")
	f.BoolVar(&opts.autoApprove, "auto-approve", false, "Automatically choose clearly safe ask_user options")
	f.BoolVar(&opts.noNet, "no-net", false, "Block all network access for agent subprocesses")
	f.BoolVar(&opts.forceMCP, "force-mcp", false, "Require MCP servers for execution")
	f.BoolVar(&opts.unattended, "unattended", false, "Run without human interaction")
	f.BoolVar(&opts.reportMode, "report", false, "Generate a full execution report")
	f.BoolVar(&opts.noSummary, "no-summary", false, "Suppress the execution summary")
	_ = resumeCmd.MarkFlagFilename("var-file")
}
