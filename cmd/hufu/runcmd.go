package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
)

type canonicalRunOptions struct {
	team, agentTeam, workspace, workspaceRoot, searchPath string
	intent, primaryDecisionProfile, rigor, resumeDecision string
	providerURL, providerAPIKey                           string
	model, coordinatorModel, output, eventFormat          string
	route                                                 string
	displayMode, theme, displayPreset                     string
	temperature, maxTokens, topP, topK, reasoningEffort   string
	sidecarModel, guardModel, judgeModel, planReviewer    string
	quiet, verbose, dryRun, defaultTeam, noNet, forceMCP  bool
	autoTeam, newSession, tempWorkspace                   bool
	noSpinner, noSummary, tuiCompact                      bool
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
	command, _ := newCanonicalRunCommandWithOptions("run", "execute")
	return command
}

func newRunCommandWithOptions() (*cobra.Command, *canonicalRunOptions) {
	return newCanonicalRunCommandWithOptions("run", "execute")
}

func newDecideCommand() *cobra.Command {
	command, _ := newCanonicalRunCommandWithOptions("decide", "decision")
	return command
}

func newCanonicalRunCommandWithOptions(name, defaultIntent string) (*cobra.Command, *canonicalRunOptions) {
	options := new(canonicalRunOptions)
	command := &cobra.Command{
		Use:   name + " [--] <task>",
		Short: "Run one team with explicit workspace and intent semantics",
		Long: `Run one team through the canonical execution facade.

--workspace is the exact team workspace. --workspace-root is a parent under
which the team name is joined exactly once. Omit both to use
<project>/workspace/<team>.`,
		Args: func(command *cobra.Command, args []string) error {
			if strings.TrimSpace(options.resumeDecision) != "" {
				return cobra.NoArgs(command, args)
			}
			return cobra.ExactArgs(1)(command, args)
		},
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) (runErr error) {
			previous := opts
			defer func() {
				opts = previous
				syncLogState()
			}()

			resolved, err := resolveCanonicalRunOptionsForIntent(command, options, defaultIntent)
			delegated := false
			// A valid profile may select JSONL before a later alias/scope check
			// fails, so establish command-error framing from the resolved local
			// flag even when no runtime snapshot can be returned.
			if options.eventFormat == "jsonl" {
				command.Root().SilenceErrors = true
				defer func() {
					if !delegated {
						emitJSONLCommandError(runErr)
					}
				}()
			}
			if err != nil {
				return err
			}
			opts = resolved
			delegated = true
			return runTeam(command, args)
		},
	}

	flags := command.Flags()
	flags.StringVar(&options.intent, "intent", defaultIntent, "Run intent: execute or decision")
	flags.StringVar(&options.team, "team", "", "Team name")
	flags.StringVar(&options.agentTeam, "agent-team", "", "Legacy alias for --team")
	flags.StringVarP(&options.workspace, "workspace", "w", "", "Exact team workspace")
	flags.StringVar(&options.workspaceRoot, "workspace-root", "", "Workspace root; team name is joined once")
	flags.StringVar(&options.searchPath, "agent-team-search-path", "", "Comma-separated team search paths")
	flags.BoolVar(&options.defaultTeam, "default", false, "Use the built-in default team")
	flags.BoolVar(&options.autoTeam, "auto-team", false, "Auto-select one team from the task domain")
	flags.BoolVarP(&options.newSession, "new", "n", false, "Archive the old session and start fresh")
	flags.BoolVarP(&options.tempWorkspace, "temp", "t", false, "Use a temporary workspace")
	flags.StringVar(&options.primaryDecisionProfile, "primary-decision-profile", "", "Exact primary decision profile or team-local primary profile")
	flags.StringVar(&options.rigor, "rigor", "", "Primary decision rigor: light, standard, or high")
	flags.StringVar(&options.resumeDecision, "resume-decision", "", "Resume an existing logical decision run")
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
	flags.StringVar(&options.route, "route", "auto", "Execution route selection mode: auto, fast, or team")
	flags.StringVar(&options.displayMode, "display-mode", "auto", "Status display mode: auto, terminal, or plain")
	flags.StringVar(&options.theme, "theme", "", "Display theme: auto, light, dark, or mono")
	flags.StringVar(&options.displayPreset, "display-preset", "", "Display preset: default or epaper")
	flags.BoolVar(&options.noSpinner, "no-spinner", false, "Disable the TUI waiting spinner (also honors NO_SPINNER)")
	flags.BoolVar(&options.noSummary, "no-summary", false, "Suppress the execution summary written to stderr")
	flags.BoolVar(&options.tuiCompact, "tui-compact", false, "Force the compact three-column TUI layout")
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
	registerStaticFlagCompletion(command, "route", []string{"auto", "fast", "team"})
	registerStaticFlagCompletion(command, "display-mode", []string{"auto", "terminal", "plain"})
	registerStaticFlagCompletion(command, "theme", []string{"auto", "light", "dark", "mono"})
	registerStaticFlagCompletion(command, "display-preset", []string{"default", "epaper"})
	registerStaticFlagCompletion(command, "intent", []string{"execute", "decision"})
	registerStaticFlagCompletion(command, "rigor", []string{"light", "standard", "high"})
	return command, options
}

// resolveCanonicalRunOptions applies the profile to the command-local flags
// before producing the single runtime option snapshot consumed by runTeam.
// The caller owns restoring opts because root-only profile flags may bind to it.
func resolveCanonicalRunOptions(command *cobra.Command, options *canonicalRunOptions) (runOptions, error) {
	return resolveCanonicalRunOptionsForIntent(command, options, "execute")
}

func resolveCanonicalRunOptionsForIntent(command *cobra.Command, options *canonicalRunOptions, commandIntent string) (runOptions, error) {
	if err := applyProfile(command); err != nil {
		return runOptions{}, err
	}
	teamName, err := resolveCanonicalTeamAlias(command, options)
	if err != nil {
		return runOptions{}, err
	}
	mode, path, err := resolveCanonicalWorkspaceFlags(command, options)
	if err != nil {
		return runOptions{}, err
	}
	if options.defaultTeam && teamName != "" {
		return runOptions{}, fmt.Errorf("--default cannot be combined with --team or --agent-team")
	}
	if options.autoTeam && (options.defaultTeam || teamName != "") {
		return runOptions{}, fmt.Errorf("--auto-team cannot be combined with an explicit team or --default")
	}
	intent := strings.ToLower(strings.TrimSpace(options.intent))
	if intent != "execute" && intent != "decision" {
		return runOptions{}, fmt.Errorf("--intent must be execute or decision")
	}
	if commandIntent == "decision" && intent != "decision" {
		return runOptions{}, fmt.Errorf("hufu decide requires --intent decision")
	}
	primaryProfile, err := resolvePrimaryDecisionFlags(command, options, intent)
	if err != nil {
		return runOptions{}, err
	}

	resolved := opts
	resolved.agentTeamName = teamName
	resolved.agentTeamSearchPath = options.searchPath
	resolved.workspace = path
	resolved.workspaceMode = mode
	resolved.canonicalRun = true
	resolved.intent = intent
	resolved.primaryDecisionProfile = primaryProfile
	resolved.decisionRigor = strings.TrimSpace(options.rigor)
	resolved.resumeDecision = strings.TrimSpace(options.resumeDecision)
	resolved.defaultTeam = options.defaultTeam
	resolved.autoTeam = options.autoTeam
	resolved.newSession = options.newSession
	resolved.tempWorkspace = options.tempWorkspace
	resolved.providerURL = options.providerURL
	resolved.providerAPIKey = options.providerAPIKey
	resolved.modelOverride = options.model
	resolved.coordinatorModelOverride = options.coordinatorModel
	resolved.contextWindowOverride = options.contextWindow
	resolved.temperatureOverride = options.temperature
	resolved.maxTokensOverride = options.maxTokens
	resolved.topPOverride = options.topP
	resolved.topKOverride = options.topK
	resolved.reasoningEffortOverride = options.reasoningEffort
	resolved.sidecarModelOverride = options.sidecarModel
	resolved.guardModelOverride = options.guardModel
	resolved.judgeModelOverride = options.judgeModel
	resolved.planReviewerModelOverride = options.planReviewer
	resolved.outputFormat = options.output
	resolved.eventFormat = options.eventFormat
	resolved.routeMode = options.route
	resolved.displayMode = options.displayMode
	resolved.themeMode = options.theme
	resolved.displayPreset = options.displayPreset
	resolved.noSpinner = options.noSpinner
	resolved.noSummary = options.noSummary
	resolved.tuiCompact = options.tuiCompact
	resolved.quietMode = options.quiet
	resolved.verbose = options.verbose
	resolved.dryRun = options.dryRun
	resolved.noNet = options.noNet
	resolved.forceMCP = options.forceMCP
	resolved.rbashMode = options.rbash
	resolved.direnv = options.direnv
	resolved.noJournal = options.noJournal
	resolved.autoApprove = options.autoApprove
	resolved.unattended = options.unattended
	resolved.planMode = options.plan
	resolved.autoSkills = options.autoSkills
	resolved.reportMode = options.report
	resolved.stepsMode = options.steps
	resolved.tuiMode = options.tui
	resolved.think = options.think
	resolved.timeoutOverride = options.timeout
	resolved.verifyTimeoutOverride = options.verifyTimeout
	resolved.maxDuration = options.maxDuration
	resolved.maxTotalTokens = options.maxTotalTokens
	resolved.maxRoundsOverride = options.maxRounds
	resolved.maxConcurrentOverride = options.maxConcurrent
	resolved.maxStepsOverride = options.maxSteps
	resolved.allowPaths = slices.Clone(options.allowPath)
	resolved.varFlags = slices.Clone(options.vars)
	resolved.varFiles = slices.Clone(options.varFiles)
	resolved.inputFlags = slices.Clone(options.inputs)
	resolved.inputFiles = slices.Clone(options.inputFiles)
	resolved.forcedSkills = slices.Clone(options.skills)
	return resolved, nil
}

func resolvePrimaryDecisionFlags(command *cobra.Command, options *canonicalRunOptions, intent string) (string, error) {
	profileChanged := command.Flags().Changed("primary-decision-profile")
	rigorChanged := command.Flags().Changed("rigor")
	resume := strings.TrimSpace(options.resumeDecision)
	if intent == "execute" {
		if profileChanged || rigorChanged || resume != "" {
			return "", fmt.Errorf("primary decision flags require --intent decision")
		}
		return "", nil
	}
	if options.noJournal {
		return "", fmt.Errorf("--no-journal is not supported for decision intent")
	}
	if profileChanged && rigorChanged {
		return "", fmt.Errorf("--primary-decision-profile and --rigor are mutually exclusive")
	}
	if resume != "" {
		if !strings.HasPrefix(resume, "ldr_") || len(resume) != 36 {
			return "", fmt.Errorf("--resume-decision requires a logical run id")
		}
		for _, flag := range []string{"primary-decision-profile", "rigor", "model", "coordinator-model", "sidecar-model", "guard-model", "judge-model", "plan-reviewer-model", "temperature", "max-tokens", "top-p", "top-k", "reasoning-effort", "context-window", "decision-profile", "input", "input-file", "var", "var-file", "new", "temp", "max-duration", "max-total-tokens", "auto-team", "dry-run", "route", "skill", "auto-skills", "plan", "report", "allow-path", "helper-tools"} {
			if command.Flags().Changed(flag) || command.InheritedFlags().Changed(flag) {
				return "", fmt.Errorf("--resume-decision cannot be combined with --%s", flag)
			}
		}
		return "", nil
	}
	if rigorChanged {
		switch strings.TrimSpace(options.rigor) {
		case "light":
			return agent.DecisionProfileBuiltinLightV2, nil
		case "standard":
			return agent.DecisionProfileBuiltinStandardV2, nil
		case "high":
			return agent.DecisionProfileBuiltinHighStakesV2, nil
		default:
			return "", fmt.Errorf("--rigor must be light, standard, or high")
		}
	}
	profile := strings.TrimSpace(options.primaryDecisionProfile)
	if profile == "" {
		return "", nil
	}
	if profile == agent.DecisionProfileOff {
		return "", fmt.Errorf("primary decision profile cannot be off for decision intent")
	}
	if strings.HasPrefix(profile, "builtin/") {
		if _, err := agent.ResolveBuiltInDecisionProfileBundle(profile); err != nil {
			return "", fmt.Errorf("unknown primary decision profile %q: %w", profile, err)
		}
	}
	return profile, nil
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
