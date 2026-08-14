package main

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

// newRootCommand builds the root `hufu` command: subcommands, flags bound to
// the package-level opts, and shell completions. Keeping construction out of
// main lets tests build a fresh root command without process-level effects.
func newRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "hufu [prompt]",
		Short: "Run an agent team to accomplish a task",
		Long: `hufu discovers and runs agent teams of LLM agents that collaborate on tasks.

Quick start:
  hufu doctor                                # preflight: check provider + teams
  hufu init my-team --model ollama/qwen3:8b  # scaffold a team
  hufu @my-team "explain this codebase"      # run a team
  hufu --default --model ollama/qwen3:8b "hello"  # use built-in team (no config)
  hufu chat --agent-team my-team             # interactive REPL
  hufu list                                  # show all teams

Specify the team with --agent-team <name> or by writing @<team-name> in the prompt.
Within a team, target a specific agent with @<agent-name> <task>.

Set the model with --model <name> (highest priority), in team.yaml, or in hufu.yaml.`,
		Args:    cobra.MaximumNArgs(1),
		RunE:    runTeam,
		Version: version,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			configureOutputRendering()
		},
	}

	// Add skill management commands
	rootCmd.AddCommand(skillCmd)
	rootCmd.AddCommand(replCmd)
	rootCmd.AddCommand(historyCmd)
	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(improveCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(teamCmd)
	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(sessionCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(contextCmd)
	rootCmd.AddCommand(terminalCmd)
	rootCmd.AddCommand(debugCmd)
	rootCmd.AddCommand(examplesCmd, helpFlagsCmd)

	// Add custom completion commands
	completionCmd.AddCommand(completionBashCmd, completionZshCmd, completionFishCmd, completionPowerShellCmd, completionNushellCmd)
	rootCmd.AddCommand(completionCmd)
	rootCmd.AddCommand(completionHelperCmd)

	rootCmd.Flags().StringVar(&opts.providerURL, "provider-url", "", "Ollama API base URL (default: from hufu.yaml or http://localhost:11434/v1)")
	rootCmd.Flags().StringVar(&opts.providerAPIKey, "provider-api-key", "", "Provider API key (default: from HUFU_PROVIDER_API_KEY env or team.yaml)")
	rootCmd.Flags().BoolVarP(&opts.verbose, "verbose", "v", false, "Show full agent text output in real-time")
	rootCmd.PersistentFlags().StringVarP(&opts.workspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	rootCmd.Flags().BoolVarP(&opts.newSession, "new", "n", false, "Archive old session and start fresh")
	rootCmd.Flags().BoolVarP(&opts.tempWorkspace, "temp", "t", false, "Use a temporary directory for workspace")
	rootCmd.Flags().StringVar(&opts.agentTeamName, "agent-team", "", "Agent team name to load")
	rootCmd.Flags().StringVar(&opts.agentTeamSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams (default: .agent-teams/,~/.agent-teams/)")
	rootCmd.Flags().BoolVar(&opts.memoryEnabled, "memory", false, "Enable long-term memory (RAG with vector search)")
	rootCmd.Flags().StringVar(&opts.memoryModel, "memory-model", "", "Embedding model for memory (default: ollama/nomic-embed-text:latest, overrides hufu.yaml)")
	rootCmd.Flags().BoolVar(&opts.archiveMemory, "archive-memory", false, "Archive session summary to memory and exit")
	rootCmd.Flags().BoolVar(&opts.showHistory, "show-history", false, "Show previous session history on resume")
	rootCmd.Flags().BoolVarP(&opts.stepsMode, "steps", "s", false, "Pause for user confirmation before executing each batch of worker tasks")
	rootCmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Preview skill matching and task delegation without executing agents")
	rootCmd.Flags().BoolVar(&opts.tuiMode, "tui", false, "Show a Bubble Tea TUI for real-time task tracking")
	rootCmd.Flags().BoolVar(&opts.enablePTYTerminal, "enable-pty-terminal", false, "Eagerly initialize experimental PTY handoff (normally starts automatically on terminal pty:true)")
	rootCmd.Flags().BoolVar(&opts.rbashMode, "rbash", false, "Use restricted bash (rbash) for the bash tool")
	rootCmd.Flags().BoolVar(&opts.noNet, "no-net", false, "Block all network access for agent subprocesses")
	rootCmd.Flags().BoolVar(&opts.noJournal, "no-journal", false, "Disable the persistent task-result journal (workspace/logs/task_journal.jsonl)")
	rootCmd.Flags().BoolVar(&opts.forceMCP, "force-mcp", false, "Force MCP mode: disable built-in execution/network tools, require MCP servers")
	rootCmd.Flags().BoolVar(&opts.direnv, "direnv", false, "Load .envrc/.env environment for bash tool; parses .env (key=value) and/or uses direnv for full shell support")
	rootCmd.Flags().BoolVar(&opts.think, "think", false, "Show coordinator decision reasoning (skills, agents, tasks, system prompt)")
	rootCmd.Flags().StringArrayVar(&opts.varFlags, "var", nil, "Set template variable (key=value). Can be specified multiple times; later values override earlier ones")
	rootCmd.Flags().StringArrayVar(&opts.varFiles, "var-file", nil, "Read template variables from a file (.yaml/.yml or KEY=VALUE format). Can be specified multiple times; later files override earlier ones")
	rootCmd.Flags().StringArrayVar(&opts.forcedSkills, "skill", nil, "Force-load specific skills (repeatable, e.g. --skill code-review --skill tdd)")
	rootCmd.Flags().BoolVar(&opts.planMode, "plan", false, "Force plan-first mode: agents must submit plans before executing")
	rootCmd.Flags().BoolVar(&opts.autoSkills, "auto-skills", false, "Enable automatic skill detection via sidecar/LLM matching")
	rootCmd.Flags().StringVar(&opts.fixQuestion, "fix", "", "Analyze previous execution data and suggest improvements for the given question")
	rootCmd.Flags().BoolVar(&opts.reportMode, "report", false, "Generate a full execution report as a markdown file")
	rootCmd.Flags().BoolVar(&opts.defaultTeam, "default", false, "Use the built-in default team (coordinator + Helper); no .agent-teams/ directory required")
	rootCmd.Flags().StringVar(&opts.helperTools, "helper-tools", "", "Comma-separated extra tools to enable for the default Helper worker when --default is set (e.g. 'bash' or 'bash,sudo,ssh'). Whitespace is trimmed.")
	rootCmd.Flags().StringSliceVar(&opts.allowPaths, "allow-path", nil, "Additional filesystem paths to allow for the active team; can be repeated.")
	rootCmd.Flags().BoolVar(&opts.autoApprove, "auto-approve", false, "Automatically choose clearly safe ask_user options; dangerous or ambiguous choices still prompt the user")
	rootCmd.Flags().StringVar(&opts.modelOverride, "model", "", "Override default model for the active team (e.g. ollama/qwen3:8b)")
	rootCmd.Flags().StringVar(&opts.temperatureOverride, "temperature", "", "Override sampling temperature (e.g. 0.2)")
	rootCmd.Flags().StringVar(&opts.maxTokensOverride, "max-tokens", "", "Override max output tokens (e.g. 4096)")
	rootCmd.Flags().StringVar(&opts.topPOverride, "top-p", "", "Override top-p value (e.g. 0.9)")
	rootCmd.Flags().StringVar(&opts.topKOverride, "top-k", "", "Override top-k value (e.g. 40)")
	rootCmd.Flags().StringVar(&opts.reasoningEffortOverride, "reasoning-effort", "", "Override reasoning effort: high, medium, low, or none")
	rootCmd.Flags().StringVar(&opts.sidecarModelOverride, "sidecar-model", "", "Override sidecar model used for skill matching (e.g. ollama/qwen3:1b); falls back to --model when not set")
	rootCmd.Flags().StringVar(&opts.guardModelOverride, "guard-model", "", "Override guard model used for output review (e.g. ollama/qwen3:8b); falls back to --model when not set")
	rootCmd.Flags().StringVar(&opts.judgeModelOverride, "judge-model", "", "Override judge model used to pick the best multi-model result (e.g. ollama/qwen3:8b); falls back to the sidecar model when not set")
	rootCmd.Flags().StringVar(&opts.planReviewerModelOverride, "plan-reviewer-model", "", "Override plan reviewer model used for plan review (e.g. ollama/qwen3:8b); falls back to --model when not set")
	rootCmd.Flags().Int64Var(&opts.timeoutOverride, "timeout", 0, "Override agent/coordinator timeout in seconds (e.g. 1800 for 30 min). 0 = use team/agent default.")
	rootCmd.Flags().Int64Var(&opts.verifyTimeoutOverride, "verify-timeout", 0, "Override deliverable verification timeout in seconds (e.g. 120). 0 = use team default.")
	rootCmd.Flags().IntVar(&opts.maxRoundsOverride, "max-rounds", 0, "Override team.yaml max-rounds (coordinator round limit). 0 = use team default.")
	rootCmd.Flags().IntVar(&opts.maxConcurrentOverride, "max-concurrent", 0, "Override team.yaml max-concurrent (parallel worker dispatch). 0 = use team default.")
	rootCmd.Flags().IntVar(&opts.maxStepsOverride, "max-steps", 0, "Override team.yaml max-steps (per-agent step budget). 0 = use team/agent default.")
	rootCmd.Flags().BoolVar(&opts.unattended, "unattended", false, "Run with no human present: ask_user returns safe defaults instead of blocking, --steps/--tui are disabled, and only allowlisted tools may run")
	rootCmd.Flags().Int64Var(&opts.maxDuration, "max-duration", 0, "Budget: max total wall-clock seconds before forcing wrap-up (0 = unlimited). Recommended for unattended runs.")
	rootCmd.Flags().Int64Var(&opts.maxTotalTokens, "max-total-tokens", 0, "Budget: max cumulative LLM tokens before forcing wrap-up (0 = unlimited). Recommended for unattended runs.")
	rootCmd.Flags().BoolVar(&opts.autoTeam, "auto-team", false, "Auto-select the team best suited to the prompt (sidecar LLM match, keyword fallback) instead of prompting")
	rootCmd.Flags().StringVar(&opts.routeMode, "route", "auto", "Execution route selection mode: auto, fast, or team")
	rootCmd.Flags().BoolVar(&opts.projectContext, "project-context", false, "Inject Git Status and Project Directory Structure into prompt context")
	rootCmd.PersistentFlags().StringVar(&opts.profileName, "profile", "", "Apply a named flag bundle from hufu.yaml `profiles:` (CLI flags still override)")
	rootCmd.PersistentFlags().StringVar(&opts.executionProfile, "execution-profile", "", "Set execution profile: default, unattended, strict-verification, fresh-verification")
	rootCmd.PersistentFlags().StringVar(&opts.goalMode, "goal-mode", "", "Goal mode: outcome (default for strict/unattended) or exploratory")
	rootCmd.Flags().BoolVarP(&opts.quietMode, "quiet", "q", false, "Suppress status output; print only the final result to stdout")
	rootCmd.Flags().StringVar(&opts.outputFormat, "output", "", "Output format for the final result: text (default) or json")
	rootCmd.Flags().StringVar(&opts.displayMode, "display-mode", "auto", "Status display mode: auto, terminal, or plain")
	rootCmd.PersistentFlags().BoolVar(&opts.noColorMode, "no-color", false, "Disable ANSI color output (also honors NO_COLOR)")
	rootCmd.Flags().BoolVar(&opts.noSummary, "no-summary", false, "Suppress the execution summary written to stderr")
	rootCmd.Flags().BoolVar(&opts.noSpinner, "no-spinner", false, "Disable the TUI waiting spinner (also honors NO_SPINNER)")
	rootCmd.Flags().BoolVar(&opts.tuiCompact, "tui-compact", false, "Force the compact three-column TUI layout")
	rootCmd.Flags().StringVar(&opts.eventFormat, "event-format", "text", "Status event format: text or jsonl")

	rootCmd.Flags().StringVar(&opts.templateName, "template", "", "Load prompt template by name from .hufu-templates/ or ~/.config/hufu/templates/")

	// init scaffolding flags (consumed by initcmd.go).
	if initCmd.Flags().Lookup("template") == nil {
		initCmd.Flags().StringVar(&opts.initTemplateName, "template", "default", "Scaffold template: default, dev, research, ops, or minimal")
		initCmd.Flags().StringVar(&opts.modelOverride, "model", "", "Pin a model in the scaffolded team.yaml (e.g. ollama/qwen3:8b)")
	}

	if improveCmd.PersistentFlags().Lookup("workspace") == nil {
		improveCmd.PersistentFlags().StringVarP(&improveWorkspace, "workspace", "w", "", "Workspace to analyze (default: <cwd>/workspace)")
		improveCmd.PersistentFlags().StringVar(&improveTeam, "team", "", "Target team (default: newest execution run)")
		improveCmd.PersistentFlags().StringVar(&improveSearchPath, "agent-team-search-path", "", "Comma-separated team search paths")
		improveCmd.Flags().StringVarP(&improveOutput, "output", "o", "", "Markdown report path (default: workspace/reports/improve-<team>-<timestamp>.md)")
		improveCmd.Flags().StringVar(&improveFormat, "format", "markdown", "Report format: markdown or json (json writes to stdout)")
		improveCmd.Flags().IntVar(&improveRuns, "runs", 1, "Number of most recent runs for the selected team to analyze")
	}

	_ = rootCmd.RegisterFlagCompletionFunc("agent-team", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		registry := team.NewTeamRegistry(resolveSearchPaths())
		if err := registry.Discover(); err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		var matches []string
		for _, name := range registry.ListTeams() {
			if strings.HasPrefix(strings.ToLower(name), strings.ToLower(toComplete)) {
				matches = append(matches, name)
			}
		}
		return matches, cobra.ShellCompDirectiveNoFileComp
	})
	registerStaticFlagCompletion(rootCmd, "output", []string{"text", "json"})
	registerStaticFlagCompletion(rootCmd, "display-mode", []string{"auto", "terminal", "plain"})
	registerStaticFlagCompletion(rootCmd, "event-format", []string{"text", "jsonl"})
	registerStaticFlagCompletion(contextQueryCmd, "tier", []string{"session", "persistent"})
	registerStaticFlagCompletion(contextQueryCmd, "lifecycle", []string{"candidate", "confirmed", "rejected"})
	registerStaticFlagCompletion(contextListCmd, "tier", []string{"session", "persistent"})
	registerStaticFlagCompletion(contextListCmd, "lifecycle", []string{"candidate", "confirmed", "rejected"})
	registerStaticFlagCompletion(contextPromotionAnalyzeCmd, "type", []string{"skill", "team-policy", "agent-policy"})
	_ = rootCmd.RegisterFlagCompletionFunc("profile", func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		var names []string
		for name := range config.LoadConfig().Profiles {
			if strings.HasPrefix(name, prefix) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		return names, cobra.ShellCompDirectiveNoFileComp
	})
	_ = initCmd.RegisterFlagCompletionFunc("template", func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		var names []string
		for name := range scaffoldTemplates {
			if strings.HasPrefix(name, prefix) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		return names, cobra.ShellCompDirectiveNoFileComp
	})

	rootCmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		matches := completeAtNames(toComplete)
		return matches, cobra.ShellCompDirectiveNoFileComp
	}

	return rootCmd
}

func registerStaticFlagCompletion(cmd *cobra.Command, name string, values []string) {
	_ = cmd.RegisterFlagCompletionFunc(name, func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		matches := make([]string, 0, len(values))
		for _, value := range values {
			if strings.HasPrefix(value, prefix) {
				matches = append(matches, value)
			}
		}
		return matches, cobra.ShellCompDirectiveNoFileComp
	})
}
