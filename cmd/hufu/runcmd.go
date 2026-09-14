package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type canonicalRunOptions struct {
	team, agentTeam, workspace, workspaceRoot, searchPath string
	providerURL, providerAPIKey                           string
	model, coordinatorModel, output, eventFormat          string
	temperature, maxTokens, topP, topK, reasoningEffort   string
	sidecarModel, guardModel, judgeModel, planReviewer    string
	quiet, verbose, dryRun, defaultTeam, noNet, forceMCP  bool
	unattended, plan, autoSkills, report, steps, tui      bool
	rbash, direnv, noJournal, autoApprove, think          bool
	vars, varFiles, inputs, inputFiles, skills, allowPath []string
	timeout, verifyTimeout, maxDuration, maxTotalTokens   int64
	maxRounds, maxConcurrent, maxSteps, contextWindow     int
}

// newRunCommand is an instance-scoped canonical facade. Its flags never bind
// package globals; the legacy runner sees a temporary option snapshot only
// for the duration of RunE.
func newRunCommand() *cobra.Command {
	options := new(canonicalRunOptions)
	command := &cobra.Command{
		Use:   "run [--] <task>",
		Short: "Run one team with explicit workspace semantics",
		Long: `Run one team through the canonical execution facade.

--workspace is the exact team workspace. --workspace-root is a parent under
which the team name is joined exactly once. Omit both to use
<project>/workspace/<team>.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			teamName, err := resolveCanonicalTeamAlias(command, options)
			if err != nil {
				return err
			}
			mode, path, err := resolveCanonicalWorkspaceFlags(command, options)
			if err != nil {
				return err
			}
			if options.defaultTeam && teamName != "" {
				return fmt.Errorf("--default cannot be combined with --team or --agent-team")
			}

			previous := opts
			defer func() { opts = previous }()
			opts.agentTeamName = teamName
			opts.agentTeamSearchPath = options.searchPath
			opts.workspace = path
			opts.workspaceMode = mode
			opts.canonicalRun = true
			opts.defaultTeam = options.defaultTeam
			opts.providerURL = options.providerURL
			opts.providerAPIKey = options.providerAPIKey
			opts.modelOverride = options.model
			opts.coordinatorModelOverride = options.coordinatorModel
			opts.contextWindowOverride = options.contextWindow
			opts.temperatureOverride = options.temperature
			opts.maxTokensOverride = options.maxTokens
			opts.topPOverride = options.topP
			opts.topKOverride = options.topK
			opts.reasoningEffortOverride = options.reasoningEffort
			opts.sidecarModelOverride = options.sidecarModel
			opts.guardModelOverride = options.guardModel
			opts.judgeModelOverride = options.judgeModel
			opts.planReviewerModelOverride = options.planReviewer
			opts.outputFormat = options.output
			opts.eventFormat = options.eventFormat
			opts.quietMode = options.quiet
			opts.verbose = options.verbose
			opts.dryRun = options.dryRun
			opts.noNet = options.noNet
			opts.forceMCP = options.forceMCP
			opts.rbashMode = options.rbash
			opts.direnv = options.direnv
			opts.noJournal = options.noJournal
			opts.autoApprove = options.autoApprove
			opts.unattended = options.unattended
			opts.planMode = options.plan
			opts.autoSkills = options.autoSkills
			opts.reportMode = options.report
			opts.stepsMode = options.steps
			opts.tuiMode = options.tui
			opts.think = options.think
			opts.timeoutOverride = options.timeout
			opts.verifyTimeoutOverride = options.verifyTimeout
			opts.maxDuration = options.maxDuration
			opts.maxTotalTokens = options.maxTotalTokens
			opts.maxRoundsOverride = options.maxRounds
			opts.maxConcurrentOverride = options.maxConcurrent
			opts.maxStepsOverride = options.maxSteps
			opts.allowPaths = append([]string(nil), options.allowPath...)
			opts.varFlags = append([]string(nil), options.vars...)
			opts.varFiles = append([]string(nil), options.varFiles...)
			opts.inputFlags = append([]string(nil), options.inputs...)
			opts.inputFiles = append([]string(nil), options.inputFiles...)
			opts.forcedSkills = append([]string(nil), options.skills...)
			return runTeam(command, args)
		},
	}

	flags := command.Flags()
	flags.StringVar(&options.team, "team", "", "Team name")
	flags.StringVar(&options.agentTeam, "agent-team", "", "Legacy alias for --team")
	flags.StringVarP(&options.workspace, "workspace", "w", "", "Exact team workspace")
	flags.StringVar(&options.workspaceRoot, "workspace-root", "", "Workspace root; team name is joined once")
	flags.StringVar(&options.searchPath, "agent-team-search-path", "", "Comma-separated team search paths")
	flags.BoolVar(&options.defaultTeam, "default", false, "Use the built-in default team")
	flags.StringVar(&options.providerURL, "provider-url", "", "Provider API base URL")
	flags.StringVar(&options.providerAPIKey, "provider-api-key", "", "Provider API key")
	flags.StringVarP(&options.model, "model", "m", "", "Override the worker execution target")
	flags.StringVar(&options.coordinatorModel, "coordinator-model", "", "Override the coordinator LLM target")
	flags.IntVar(&options.contextWindow, "context-window", 0, "Override model context window in tokens")
	flags.StringVar(&options.temperature, "temperature", "", "Override sampling temperature")
	flags.StringVar(&options.maxTokens, "max-tokens", "", "Override max output tokens")
	flags.StringVar(&options.topP, "top-p", "", "Override top-p")
	flags.StringVar(&options.topK, "top-k", "", "Override top-k")
	flags.StringVar(&options.reasoningEffort, "reasoning-effort", "", "Override reasoning effort")
	flags.StringVar(&options.sidecarModel, "sidecar-model", "", "Override sidecar LLM target")
	flags.StringVar(&options.guardModel, "guard-model", "", "Override guard LLM target")
	flags.StringVar(&options.judgeModel, "judge-model", "", "Override judge LLM target")
	flags.StringVar(&options.planReviewer, "plan-reviewer-model", "", "Override plan reviewer LLM target")
	flags.StringVar(&options.output, "output", "", "Final-result format: text or json")
	flags.StringVar(&options.eventFormat, "event-format", "text", "Status event format: text or jsonl")
	flags.BoolVarP(&options.quiet, "quiet", "q", false, "Suppress status output")
	flags.BoolVarP(&options.verbose, "verbose", "v", false, "Show full agent output")
	flags.BoolVar(&options.dryRun, "dry-run", false, "Preview without executing agents")
	flags.BoolVar(&options.noNet, "no-net", false, "Block network access for agent subprocesses")
	flags.BoolVar(&options.forceMCP, "force-mcp", false, "Require MCP servers for execution")
	flags.BoolVar(&options.rbash, "rbash", false, "Use restricted bash for the bash tool")
	flags.BoolVar(&options.direnv, "direnv", false, "Load the project environment for bash")
	flags.BoolVar(&options.noJournal, "no-journal", false, "Disable the durable task journal")
	flags.StringSliceVar(&options.allowPath, "allow-path", nil, "Additional allowed filesystem path")
	flags.BoolVar(&options.autoApprove, "auto-approve", false, "Choose clearly safe ask_user options automatically")
	flags.BoolVar(&options.unattended, "unattended", false, "Run without human interaction")
	flags.Int64Var(&options.maxDuration, "max-duration", 0, "Maximum total wall-clock seconds")
	flags.Int64Var(&options.maxTotalTokens, "max-total-tokens", 0, "Maximum cumulative LLM tokens")
	flags.BoolVar(&options.plan, "plan", false, "Require plan-first execution")
	flags.BoolVar(&options.autoSkills, "auto-skills", false, "Enable automatic skill matching")
	flags.BoolVar(&options.report, "report", false, "Generate an execution report")
	flags.BoolVarP(&options.steps, "steps", "s", false, "Confirm each task batch")
	flags.BoolVar(&options.tui, "tui", false, "Show the real-time TUI")
	flags.BoolVar(&options.think, "think", false, "Show coordinator decision reasoning")
	flags.Int64Var(&options.timeout, "timeout", 0, "Override agent/coordinator timeout in seconds")
	flags.Int64Var(&options.verifyTimeout, "verify-timeout", 0, "Override verification timeout in seconds")
	flags.IntVar(&options.maxRounds, "max-rounds", 0, "Override coordinator round limit")
	flags.IntVar(&options.maxConcurrent, "max-concurrent", 0, "Override parallel worker limit")
	flags.IntVar(&options.maxSteps, "max-steps", 0, "Override per-agent step budget")
	flags.StringArrayVar(&options.vars, "var", nil, "Set template variable key=value (repeatable)")
	flags.StringArrayVar(&options.varFiles, "var-file", nil, "Read template variables from a file (repeatable)")
	flags.StringArrayVar(&options.inputs, "input", nil, "Set typed run input name=value (repeatable)")
	flags.StringArrayVar(&options.inputFiles, "input-file", nil, "Read typed run inputs from a file (repeatable)")
	flags.StringArrayVar(&options.skills, "skill", nil, "Force-load a skill (repeatable)")
	registerStaticFlagCompletion(command, "output", []string{"text", "json"})
	registerStaticFlagCompletion(command, "event-format", []string{"text", "jsonl"})
	return command
}

func resolveCanonicalTeamAlias(command *cobra.Command, options *canonicalRunOptions) (string, error) {
	teamName := strings.ToLower(strings.TrimSpace(options.team))
	legacyName := strings.ToLower(strings.TrimSpace(options.agentTeam))
	if command.Flags().Changed("team") && command.Flags().Changed("agent-team") && teamName != legacyName {
		return "", fmt.Errorf("--team and --agent-team select different teams")
	}
	if teamName == "" {
		teamName = legacyName
	}
	return teamName, nil
}

func resolveCanonicalWorkspaceFlags(command *cobra.Command, options *canonicalRunOptions) (string, string, error) {
	exact := command.Flags().Changed("workspace")
	root := command.Flags().Changed("workspace-root")
	if exact && root {
		return "", "", fmt.Errorf("--workspace and --workspace-root are mutually exclusive")
	}
	if exact {
		return "exact", options.workspace, nil
	}
	if root {
		return "root", options.workspaceRoot, nil
	}
	return "default", "", nil
}
