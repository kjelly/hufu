package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	ergoreadline "github.com/ergochat/readline"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/memory"
	"github.com/anomalyco/hufu/internal/notify"
	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
	tuipkg "github.com/anomalyco/hufu/internal/tui"
)

var (
	providerURL               string
	providerAPIKey            string
	verbose                   bool
	workspace                 string
	newSession                bool
	tempWorkspace             bool
	agentTeamName             string
	agentTeamSearchPath       string
	memoryEnabled             bool
	memoryModel               string
	archiveMemory             bool
	showHistory               bool
	stepsMode                 bool
	dryRun                    bool
	tuiMode                   bool
	rbashMode                 bool
	noNet                     bool
	noJournal                 bool
	forceMCP                  bool
	think                     bool
	direnv                    bool
	varFlags                  []string
	varFiles                  []string
	forcedSkills              []string
	planMode                  bool
	autoSkills                bool
	fixQuestion               string
	reportMode                bool
	defaultTeam               bool
	helperTools               string
	allowPaths                []string
	modelOverride             string
	temperatureOverride       string
	maxTokensOverride         string
	topPOverride              string
	topKOverride              string
	sidecarModelOverride      string
	guardModelOverride        string
	judgeModelOverride        string
	planReviewerModelOverride string
	timeoutOverride           int64
	verifyTimeoutOverride     int64
	maxRoundsOverride         int
	maxConcurrentOverride     int
	maxStepsOverride          int
	unattended                bool
	autoApprove               bool
	maxDuration               int64
	maxTotalTokens            int64
	autoTeam                  bool
	templateName              string
	initTemplateName          string
	profileName               string
	quietMode                 bool
	outputFormat              string
	displayMode               string
	noColorMode               bool
	noSummary                 bool
	projectContext            bool
	isChatTUI                 bool
	globalPromptReader        atomic.Pointer[readline.PromptReader]
)

type errInterrupted struct{}

func (errInterrupted) Error() string { return "interrupted" }

var version = "dev"

func main() {
	// Pin the interactivity decision before any prompt widget can take over
	// stdin; a live per-call probe would flip tool permissions mid-session.
	tools.CaptureInteractiveEnvironment()
	exitCode := 0
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

	// Add custom completion commands
	completionCmd.AddCommand(completionBashCmd, completionZshCmd, completionFishCmd, completionPowerShellCmd, completionNushellCmd)
	rootCmd.AddCommand(completionCmd)
	rootCmd.AddCommand(completionHelperCmd)

	rootCmd.Flags().StringVar(&providerURL, "provider-url", "", "Ollama API base URL (default: from hufu.yaml or http://localhost:11434/v1)")
	rootCmd.Flags().StringVar(&providerAPIKey, "provider-api-key", "", "Provider API key (default: from HUFU_PROVIDER_API_KEY env or team.yaml)")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show full agent text output in real-time")
	rootCmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	rootCmd.Flags().BoolVarP(&newSession, "new", "n", false, "Archive old session and start fresh")
	rootCmd.Flags().BoolVarP(&tempWorkspace, "temp", "t", false, "Use a temporary directory for workspace")
	rootCmd.Flags().StringVar(&agentTeamName, "agent-team", "", "Agent team name to load")
	rootCmd.Flags().StringVar(&agentTeamSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams (default: .agent-teams/,~/.agent-teams/)")
	rootCmd.Flags().BoolVar(&memoryEnabled, "memory", false, "Enable long-term memory (RAG with vector search)")
	rootCmd.Flags().StringVar(&memoryModel, "memory-model", "", "Embedding model for memory (default: ollama/nomic-embed-text:latest, overrides hufu.yaml)")
	rootCmd.Flags().BoolVar(&archiveMemory, "archive-memory", false, "Archive session summary to memory and exit")
	rootCmd.Flags().BoolVar(&showHistory, "show-history", false, "Show previous session history on resume")
	rootCmd.Flags().BoolVarP(&stepsMode, "steps", "s", false, "Pause for user confirmation before executing each batch of worker tasks")
	rootCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview skill matching and task delegation without executing agents")
	rootCmd.Flags().BoolVar(&tuiMode, "tui", false, "Show a Bubble Tea TUI for real-time task tracking")
	rootCmd.Flags().BoolVar(&rbashMode, "rbash", false, "Use restricted bash (rbash) for the bash tool")
	rootCmd.Flags().BoolVar(&noNet, "no-net", false, "Block all network access for agent subprocesses")
	rootCmd.Flags().BoolVar(&noJournal, "no-journal", false, "Disable the persistent task-result journal (workspace/logs/task_journal.jsonl)")
	rootCmd.Flags().BoolVar(&forceMCP, "force-mcp", false, "Force MCP mode: disable built-in execution/network tools, require MCP servers")
	rootCmd.Flags().BoolVar(&direnv, "direnv", false, "Load .envrc/.env environment for bash tool; parses .env (key=value) and/or uses direnv for full shell support")
	rootCmd.Flags().BoolVar(&think, "think", false, "Show coordinator decision reasoning (skills, agents, tasks, system prompt)")
	rootCmd.Flags().StringArrayVar(&varFlags, "var", nil, "Set template variable (key=value). Can be specified multiple times; later values override earlier ones")
	rootCmd.Flags().StringArrayVar(&varFiles, "var-file", nil, "Read template variables from a file (.yaml/.yml or KEY=VALUE format). Can be specified multiple times; later files override earlier ones")
	rootCmd.Flags().StringArrayVar(&forcedSkills, "skill", nil, "Force-load specific skills (repeatable, e.g. --skill code-review --skill tdd)")
	rootCmd.Flags().BoolVar(&planMode, "plan", false, "Force plan-first mode: agents must submit plans before executing")
	rootCmd.Flags().BoolVar(&autoSkills, "auto-skills", false, "Enable automatic skill detection via sidecar/LLM matching")
	rootCmd.Flags().StringVar(&fixQuestion, "fix", "", "Analyze previous execution data and suggest improvements for the given question")
	rootCmd.Flags().BoolVar(&reportMode, "report", false, "Generate a full execution report as a markdown file")
	rootCmd.Flags().BoolVar(&defaultTeam, "default", false, "Use the built-in default team (coordinator + Helper); no .agent-teams/ directory required")
	rootCmd.Flags().StringVar(&helperTools, "helper-tools", "", "Comma-separated extra tools to enable for the default Helper worker when --default is set (e.g. 'bash' or 'bash,sudo,ssh'). Whitespace is trimmed.")
	rootCmd.Flags().StringSliceVar(&allowPaths, "allow-path", nil, "Additional filesystem paths to allow for the active team; can be repeated.")
	rootCmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "Automatically choose clearly safe ask_user options; dangerous or ambiguous choices still prompt the user")
	rootCmd.Flags().StringVar(&modelOverride, "model", "", "Override default model for the active team (e.g. ollama/qwen3:8b)")
	rootCmd.Flags().StringVar(&temperatureOverride, "temperature", "", "Override sampling temperature (e.g. 0.2)")
	rootCmd.Flags().StringVar(&maxTokensOverride, "max-tokens", "", "Override max output tokens (e.g. 4096)")
	rootCmd.Flags().StringVar(&topPOverride, "top-p", "", "Override top-p value (e.g. 0.9)")
	rootCmd.Flags().StringVar(&topKOverride, "top-k", "", "Override top-k value (e.g. 40)")
	rootCmd.Flags().StringVar(&sidecarModelOverride, "sidecar-model", "", "Override sidecar model used for skill matching (e.g. ollama/qwen3:1b); falls back to --model when not set")
	rootCmd.Flags().StringVar(&guardModelOverride, "guard-model", "", "Override guard model used for output review (e.g. ollama/qwen3:8b); falls back to --model when not set")
	rootCmd.Flags().StringVar(&judgeModelOverride, "judge-model", "", "Override judge model used to pick the best multi-model result (e.g. ollama/qwen3:8b); falls back to the sidecar model when not set")
	rootCmd.Flags().StringVar(&planReviewerModelOverride, "plan-reviewer-model", "", "Override plan reviewer model used for plan review (e.g. ollama/qwen3:8b); falls back to --model when not set")
	rootCmd.Flags().Int64Var(&timeoutOverride, "timeout", 0, "Override agent/coordinator timeout in seconds (e.g. 1800 for 30 min). 0 = use team/agent default.")
	rootCmd.Flags().Int64Var(&verifyTimeoutOverride, "verify-timeout", 0, "Override deliverable verification timeout in seconds (e.g. 120). 0 = use team default.")
	rootCmd.Flags().IntVar(&maxRoundsOverride, "max-rounds", 0, "Override team.yaml max-rounds (coordinator round limit). 0 = use team default.")
	rootCmd.Flags().IntVar(&maxConcurrentOverride, "max-concurrent", 0, "Override team.yaml max-concurrent (parallel worker dispatch). 0 = use team default.")
	rootCmd.Flags().IntVar(&maxStepsOverride, "max-steps", 0, "Override team.yaml max-steps (per-agent step budget). 0 = use team/agent default.")
	rootCmd.Flags().BoolVar(&unattended, "unattended", false, "Run with no human present: ask_user returns safe defaults instead of blocking, --steps/--tui are disabled, and only allowlisted tools may run")
	rootCmd.Flags().Int64Var(&maxDuration, "max-duration", 0, "Budget: max total wall-clock seconds before forcing wrap-up (0 = unlimited). Recommended for unattended runs.")
	rootCmd.Flags().Int64Var(&maxTotalTokens, "max-total-tokens", 0, "Budget: max cumulative LLM tokens before forcing wrap-up (0 = unlimited). Recommended for unattended runs.")
	rootCmd.Flags().BoolVar(&autoTeam, "auto-team", false, "Auto-select the team best suited to the prompt (sidecar LLM match, keyword fallback) instead of prompting")
	rootCmd.Flags().BoolVar(&projectContext, "project-context", false, "Inject Git Status and Project Directory Structure into prompt context")
	rootCmd.PersistentFlags().StringVar(&profileName, "profile", "", "Apply a named flag bundle from hufu.yaml `profiles:` (CLI flags still override)")
	rootCmd.Flags().BoolVarP(&quietMode, "quiet", "q", false, "Suppress status output; print only the final result to stdout")
	rootCmd.Flags().StringVar(&outputFormat, "output", "", "Output format for the final result: text (default) or json")
	rootCmd.Flags().StringVar(&displayMode, "display-mode", "auto", "Status display mode: auto, terminal, or plain")
	rootCmd.PersistentFlags().BoolVar(&noColorMode, "no-color", false, "Disable ANSI color output (also honors NO_COLOR)")
	rootCmd.Flags().BoolVar(&noSummary, "no-summary", false, "Suppress the execution summary written to stderr")

	rootCmd.Flags().StringVar(&templateName, "template", "", "Load prompt template by name from .hufu-templates/ or ~/.config/hufu/templates/")

	// init scaffolding flags (consumed by initcmd.go).
	initCmd.Flags().StringVar(&initTemplateName, "template", "default", "Scaffold template: default, dev, research, ops, or minimal")
	initCmd.Flags().StringVar(&modelOverride, "model", "", "Pin a model in the scaffolded team.yaml (e.g. ollama/qwen3:8b)")

	// Root flags are intentionally not inherited by Cobra subcommands, so the
	// diagnostic command declares the small, relevant subset explicitly.
	improveCmd.PersistentFlags().StringVarP(&improveWorkspace, "workspace", "w", "", "Workspace to analyze (default: <cwd>/workspace)")
	improveCmd.PersistentFlags().StringVar(&improveTeam, "team", "", "Target team (default: newest execution run)")
	improveCmd.PersistentFlags().StringVar(&improveSearchPath, "agent-team-search-path", "", "Comma-separated team search paths")
	improveCmd.Flags().StringVarP(&improveOutput, "output", "o", "", "Markdown report path (default: workspace/reports/improve-<team>-<timestamp>.md)")
	improveCmd.Flags().StringVar(&improveFormat, "format", "markdown", "Report format: markdown or json (json writes to stdout)")
	improveCmd.Flags().IntVar(&improveRuns, "runs", 1, "Number of most recent runs for the selected team to analyze")

	_ = rootCmd.RegisterFlagCompletionFunc("agent-team", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var searchPaths []string
		if agentTeamSearchPath != "" {
			searchPaths = strings.Split(agentTeamSearchPath, ",")
		} else {
			searchPaths = team.DefaultSearchPaths()
		}
		registry := team.NewTeamRegistry(searchPaths)
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

	rootCmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		matches := completeAtNames(toComplete)
		return matches, cobra.ShellCompDirectiveNoFileComp
	}

	if err := rootCmd.Execute(); err != nil {
		var interrupted errInterrupted
		if errors.Is(err, interrupted) {
			exitCode = 130
		} else {
			exitCode = 1
		}
	}
	if pr := globalPromptReader.Load(); pr != nil {
		_ = pr.Close()
	}
	os.Exit(exitCode)
}

type teamContext struct {
	teamName    string
	session     *team.TeamSession
	coordinator *team.Coordinator
	sessionData *team.SessionData
	notifier    *notify.Notifier
}

// jsonRunOutput is the machine-readable shape emitted by --output json.
type jsonRunOutput struct {
	Result string         `json:"result"`
	Teams  []jsonRunTeam  `json:"teams"`
	Skills []jsonRunSkill `json:"skills,omitempty"`
}

type jsonRunTeam struct {
	Name   string        `json:"name"`
	Tokens int64         `json:"tokens"`
	Tasks  []jsonRunTask `json:"tasks,omitempty"`
}

type jsonRunTask struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Desc   string `json:"desc"`
	Status string `json:"status"`
}

type jsonRunSkill struct {
	Name   string   `json:"name"`
	Count  int      `json:"count"`
	Agents []string `json:"agents"`
}

// printResultJSON writes the run result, per-team task/token data and skill
// usage to stdout as a single JSON object, for scripting and piping.
func printResultJSON(result string, loadedTeams map[string]*teamContext, skills []team.SkillUsageEntry) error {
	out := jsonRunOutput{Result: result}

	names := make([]string, 0, len(loadedTeams))
	for name := range loadedTeams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tc := loadedTeams[name]
		if tc == nil || tc.coordinator == nil {
			continue
		}
		jt := jsonRunTeam{Name: name, Tokens: tc.coordinator.TokensUsed()}
		for _, it := range tc.coordinator.TaskTracker().TodoList().Items() {
			jt.Tasks = append(jt.Tasks, jsonRunTask{
				ID:     it.ID,
				Agent:  it.Agent,
				Desc:   it.Desc,
				Status: string(it.Status),
			})
		}
		out.Teams = append(out.Teams, jt)
	}
	for _, s := range skills {
		out.Skills = append(out.Skills, jsonRunSkill{Name: s.Name, Count: s.Count, Agents: s.Agents})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// applyProfile applies a named flag bundle from hufu.yaml `profiles:` to the
// current command's flags. Precedence is: explicit CLI flag > profile > default,
// achieved by skipping any flag the user already set (flag.Changed). Flag values
// are stored as strings the flag itself parses, so type handling is automatic.
// A flag named in the profile that this command does not define is reported as
// an error rather than silently ignored, so typos surface early.
func applyProfile(cmd *cobra.Command) error {
	if profileName == "" {
		return nil
	}
	cfg := config.LoadConfig()
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		var names []string
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return fmt.Errorf("profile %q not found: no profiles defined in hufu.yaml", profileName)
		}
		return fmt.Errorf("profile %q not found. Available: %s", profileName, strings.Join(names, ", "))
	}

	// Resolve a flag by name against this command, then its root (for flags
	// bound on the root command while a subcommand is executing).
	lookup := func(name string) *pflag.FlagSet {
		if cmd.Flags().Lookup(name) != nil {
			return cmd.Flags()
		}
		if root := cmd.Root(); root != nil && root.Flags().Lookup(name) != nil {
			return root.Flags()
		}
		return nil
	}

	// Apply in sorted key order for deterministic behavior.
	keys := make([]string, 0, len(profile))
	for k := range profile {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		fs := lookup(name)
		if fs == nil {
			return fmt.Errorf("profile %q sets unknown flag %q", profileName, name)
		}
		if fs.Changed(name) {
			continue // explicit CLI flag wins
		}
		if err := fs.Set(name, profile[name]); err != nil {
			return fmt.Errorf("profile %q: invalid value for --%s: %w", profileName, name, err)
		}
	}
	fmt.Fprintf(os.Stderr, "%s Applied profile %s\n", dimStyle.Render("·"), boldStyle.Render(profileName))
	return nil
}
func runTeam(cmd *cobra.Command, args []string) error {
	// Push current quiet/JSON/TUI state into internal/log so internal/* packages
	// that log through it stay in sync with the CLI.
	syncLogState()

	// Apply a named profile first so its values feed the checks below; explicit
	// CLI flags still win (applyProfile respects flag.Changed).
	if err := applyProfile(cmd); err != nil {
		return err
	}
	if err := validateRunFlags(); err != nil {
		return err
	}
	configureOutputRendering()

	pr, err := readline.NewPromptReader(defaultHistoryPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s Readline initialization failed, falling back to basic input: %v\n", errStyle.Render("⚠"), err)
		pr = nil
	}
	globalPromptReader.Store(pr)
	if tempWorkspace {
		tmpDir, err := os.MkdirTemp("", "hufu-*")
		if err != nil {
			return fmt.Errorf("failed to create temp directory: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmpDir) }()
		workspace = filepath.Join(tmpDir, "workspace")
		fmt.Fprintf(os.Stderr, "%s Temp workspace: %s\n", stepStyle.Render("⟳"), workspace)
	}

	vars, err := team.ResolveVars(varFiles, varFlags)
	if err != nil {
		return fmt.Errorf("failed to resolve template variables: %w", err)
	}
	cfgVars := config.LoadConfig().GetVars()
	if len(cfgVars) > 0 || len(vars) > 0 {
		merged := team.MergeVars(cfgVars, vars)
		vars = merged
	}

	// Capture the first positional argument as the initial prompt.
	prompt := ""
	if len(args) > 0 {
		prompt = args[0]
	}

	if archiveMemory && prompt == "" && templateName == "" {
		var searchPaths []string
		if agentTeamSearchPath != "" {
			searchPaths = strings.Split(agentTeamSearchPath, ",")
		} else {
			searchPaths = team.DefaultSearchPaths()
		}
		registry := team.NewTeamRegistry(searchPaths)
		if err := registry.Discover(); err != nil {
			return fmt.Errorf("failed to discover teams: %w", err)
		}
		return runArchiveMemory(context.Background(), registry, vars)
	}

	// resolveInitialPrompt handles stdin/template/file/project/interactive
	// sources and returns a fully-resolved prompt.
	prompt, err = resolveInitialPrompt(prompt, pr, vars)
	if err != nil {
		return err
	}
	originalPrompt := prompt

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	injector := newPromptInjector(pr)
	activeCoord := new(atomic.Pointer[team.Coordinator])
	defer setupPromptSignals(injector)()

	// loadedTeams is assigned below; the SIGINT handler reads it via
	// the pointer to print session paths during the watchdog step.
	// Declared as a top-level var so the closure captures the
	// pointer, not the value.
	var loadedTeams map[string]*teamContext
	defer setupInterruptHandler(injector, activeCoord, &loadedTeams, cancel)()

	var searchPaths []string
	if agentTeamSearchPath != "" {
		searchPaths = strings.Split(agentTeamSearchPath, ",")
	} else {
		searchPaths = team.DefaultSearchPaths()
	}

	registry := team.NewTeamRegistry(searchPaths)
	if !defaultTeam {
		if err := registry.Discover(); err != nil {
			return fmt.Errorf("failed to discover teams: %w", err)
		}

		if registry.TeamCount() == 0 {
			return offerFirstTimeWizard(searchPaths)
		}

		fmt.Fprintf(os.Stderr, "%s Available teams: %s\n", boldStyle.Render("Teams:"), strings.Join(registry.ListTeams(), ", "))
	}

	if archiveMemory && prompt == "" && !newSession {
		return runArchiveMemory(context.Background(), registry, vars)
	}

	if fixQuestion != "" {
		fixPC := newPathConsent()
		return runFixMode(ctx, prompt, fixQuestion, registry, providerURL, providerAPIKey, fixPC, vars, forcedSkills, planMode, autoSkills)
	}

	initialTeam := strings.ToLower(agentTeamName)
	if defaultTeam {
		initialTeam = "default"
	}

	// --auto-team: when the user did not name a team (no --agent-team, no
	// @team in the prompt, not --default), automatically pick the team best
	// suited to the prompt instead of showing the interactive picker.
	if autoTeam && initialTeam == "" && !team.HasAtName(prompt) && registry.TeamCount() > 0 {
		picked, method := autoSelectTeam(ctx, prompt, registry)
		if picked != "" {
			initialTeam = strings.ToLower(picked)
			fmt.Fprintf(os.Stderr, "%s Auto-selected team %s (%s)\n", boldStyle.Render("→"), teamStyle.Render(picked), method)
		} else {
			fmt.Fprintf(os.Stderr, "%s --auto-team could not confidently pick a team; falling back to selection.\n", dimStyle.Render("·"))
		}
	}

	initialSegments, err := team.ParsePromptWithLazyAgents(prompt, registry, initialTeam)
	if err != nil {
		teamNotFound := false
		if initialTeam == "" && !team.HasAtName(prompt) {
			teamNotFound = true
		} else {
			for _, prefix := range []string{"no team found", "no team specified"} {
				if strings.HasPrefix(err.Error(), prefix) {
					teamNotFound = true
					break
				}
			}
		}
		if teamNotFound {
			var chosen string
			if pr != nil {
				c, err := askUserForTeam(registry.ListTeams(), pr)
				if err != nil {
					return err
				}
				chosen = c
			} else {
				chosen = askUserForTeamFallback(registry.ListTeams())
			}
			if chosen == "" {
				fmt.Fprintf(os.Stderr, "%s No team selected. Pass --agent-team <name>, use @<team> in the prompt, or run 'hufu init <name>' to create one.\n", errStyle.Render("✗"))
				return fmt.Errorf("no team selected")
			}
			initialTeam = chosen
			initialSegments, err = team.ParsePromptWithLazyAgents(prompt, registry, initialTeam)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	pathConsent := newPathConsent()

	loadedTeams, vars, err = loadTeamsForSegments(ctx, initialSegments, registry, pathConsent, pr, vars)
	if err != nil {
		return err
	}

	segments, err := expandSegmentsWithAgents(initialSegments, loadedTeams, registry)
	if err != nil {
		return err
	}

	if dryRun {
		return executeDryRun(ctx, segments, prompt, loadedTeams)
	}

	return executeAndReport(ctx, cancel, prompt, originalPrompt, segments, registry, loadedTeams, injector, activeCoord, pathConsent, vars)
}

// applyUnattendedAndBudget configures the coordinator's unattended mode,
// run budgets, and acceptance check from the CLI flags and team config.
// CLI flags take precedence; team.yaml values are the fallback.
func applyUnattendedAndBudget(coordinator *team.Coordinator, session *team.TeamSession) {
	coordinator.SetUnattended(unattended || session.Config.Unattended)
	coordinator.SetAutoApprove(autoApprove || session.Config.AutoApprove)
	coordinator.SetNoJournal(noJournal)

	budgetSeconds := maxDuration
	if budgetSeconds == 0 {
		budgetSeconds = session.Config.MaxWallClock
	}
	budgetTokens := maxTotalTokens
	if budgetTokens == 0 {
		budgetTokens = session.Config.MaxTotalTokens
	}
	coordinator.SetBudget(budgetSeconds, budgetTokens)
	coordinator.SetAcceptance(session.Config.Acceptance)
	coordinator.SetRollback(session.Config.Rollback)
}

// loadTeamCommon is the shared post-load setup for both named and default
// teams. It handles workspace creation, session lifecycle, model/provider
// resolution, coordinator construction, and notification setup.
// The session must already be loaded (via team.LoadTeam or team.LoadDefaultTeam)
// and have its Workspace set.
func loadTeamCommon(ctx context.Context, teamName string, session *team.TeamSession, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, registry *team.TeamRegistry, forcedSkills []string, planMode bool, autoSkillsMode bool, buildMCP bool) (*teamContext, error) {
	// Apply CLI model overrides as the highest-priority model config layer.
	applyCLIModelOverrides(&session.Config, currentModelOverrides())
	applyCLITimeoutOverrides(session, currentTimeoutOverrides())
	applyCLIVerifyTimeoutOverrides(session, currentVerifyTimeoutOverrides())
	applyCLITuningOverrides(session, currentTuningOverrides())
	propagateTeamGenerationToAgents(session)

	resolvedProviderURL := config.ResolveProviderURL(defaultProviderURL, session.Config.ProviderURL, "")
	resolvedProviderAPIKey := config.ResolveProviderAPIKey(defaultProviderAPIKey, session.Config.ProviderAPIKey)

	if err := team.EnsureWorkspaceDirs(session.Workspace); err != nil {
		stderrLog("%s Failed to ensure workspace dirs: %v\n", errStyle.Render("⚠"), err)
	}
	if err := team.InitLTM(session.Workspace, session.Config.Name); err != nil {
		stderrLog("%s Failed to init ltm.md: %v\n", errStyle.Render("⚠"), err)
	}
	if err := team.InitSTM(session.Workspace); err != nil {
		stderrLog("%s Failed to init stm.md: %v\n", errStyle.Render("⚠"), err)
	}
	if newSession {
		team.ExtractLTMFromHistory(session.Workspace, session.Config.Name)
	}

	sessionData, oldSessionEntries, err := prepareSessionLifecycle(session)
	if err != nil {
		return nil, err
	}

	displayTeamHeader(session)

	cfg := config.LoadConfig()
	var mcpManager *mcp.MCPToolManager
	if buildMCP {
		mcpManager = buildMCPManager(ctx, session, cfg)
	}
	memStore := buildMemoryStore(resolvedProviderURL)

	resolvedModelList := cfg.ResolveModelList(session.Config.ModelList)
	resolvedSidecarModel := cfg.ResolveSidecarModel(session.Config.SidecarModel)
	resolvedGuardModel := cfg.ResolveGuardModel(session.Config.GuardModel, session.Config.SidecarModel)
	resolvedJudgeModel := cfg.ResolveJudgeModel(session.Config.JudgeModel, session.Config.SidecarModel)
	resolvedPlanReviewerModel := cfg.ResolvePlanReviewerModel(session.Config.PlanReviewerModel, session.Config.Generation.Model)
	resolvedMaxConcurrent := cfg.ResolveMaxConcurrent(session.Config.MaxConcurrent)
	if resolvedMaxConcurrent <= 0 {
		resolvedMaxConcurrent = 8
	}
	if err := resolveAndCheckModel(session, cfg); err != nil {
		return nil, err
	}

	allowedPaths := buildAllowedPaths(session, registry, cfg)

	hookRegistry := registerHooks(cfg)
	resolvedRestrictedPath := resolveRestrictedPath(session, cfg)
	resolvedNoNet := noNet || cfg.NoNet || session.Config.NoNet
	resolvedForceMCP := forceMCP || cfg.ForceMCP || session.Config.ForceMCP

	// Each team receives its own consent store. The store persists explicit
	// "always" decisions in the team directory and prevents one team's policy
	// from granting access to another team in a multi-team prompt.
	teamPathConsent := pathConsent
	if pathConsent != nil {
		teamPathConsent, err = tools.NewTeamPathConsent(session.Dir)
		if err != nil {
			return nil, fmt.Errorf("failed to load path consent policy: %w", err)
		}
	}

	roleModels := team.RoleModels{Sidecar: resolvedSidecarModel, Guard: resolvedGuardModel, Judge: resolvedJudgeModel, PlanReviewer: resolvedPlanReviewerModel}
	coordinator, err := team.NewCoordinator(session, resolvedProviderURL, resolvedProviderAPIKey, mcpManager, memStore, resolvedModelList, roleModels, resolvedMaxConcurrent, verbose, think, direnv, allowedPaths, teamPathConsent, hookRegistry, rbashMode, resolvedRestrictedPath, resolvedNoNet, resolvedForceMCP, forcedSkills, planMode, autoSkillsMode)
	if err != nil {
		return nil, fmt.Errorf("failed to create coordinator: %w", err)
	}
	coordinator.SetSessionData(sessionData)
	applyUnattendedAndBudget(coordinator, session)
	archiveToMemory(ctx, memStore, coordinator, session, oldSessionEntries)
	displayResolvedConfig(session, resolvedModelList, resolvedSidecarModel, resolvedGuardModel, resolvedJudgeModel, resolvedPlanReviewerModel, resolvedMaxConcurrent)
	notifierInst := buildNotifier(cfg, session)

	return &teamContext{
		teamName:    teamName,
		session:     session,
		coordinator: coordinator,
		sessionData: sessionData,
		notifier:    notifierInst,
	}, nil
}

func loadTeamByName(ctx context.Context, teamName string, registry *team.TeamRegistry, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, vars map[string]string, forcedSkills []string, planMode bool, autoSkillsMode bool) (*teamContext, error) {
	teamDir, err := registry.Resolve(teamName)
	if err != nil {
		return nil, err
	}

	session, err := team.LoadTeam(teamDir, vars, forcedSkills)
	if err != nil {
		return nil, err
	}

	if err := resolveTeamWorkspace(teamName, session); err != nil {
		return nil, err
	}

	return loadTeamCommon(ctx, teamName, session, defaultProviderURL, defaultProviderAPIKey, pathConsent, registry, forcedSkills, planMode, autoSkillsMode, true)
}

// loadDefaultTeam builds an in-memory default team (coordinator + Helper)
// without consulting the .agent-teams/ registry.
func loadDefaultTeam(ctx context.Context, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, vars map[string]string, forcedSkills []string, planMode bool, autoSkillsMode bool) (*teamContext, error) {
	teamName := "default"

	dummySession := &team.TeamSession{}
	if err := resolveTeamWorkspace(teamName, dummySession); err != nil {
		return nil, err
	}
	teamWorkspace := dummySession.Workspace

	session, err := team.LoadDefaultTeam(teamWorkspace, forcedSkills, helperTools)
	if err != nil {
		return nil, err
	}
	session.Workspace = teamWorkspace
	session.Config.WorkspaceDir = teamWorkspace

	return loadTeamCommon(ctx, teamName, session, defaultProviderURL, defaultProviderAPIKey, pathConsent, nil, forcedSkills, planMode, autoSkillsMode, false)
}

func buildAllowedPaths(session *team.TeamSession, registry *team.TeamRegistry, cfg *config.Config) []string {
	seen := make(map[string]bool)
	var paths []string

	if projectDir := currentWorkingDir(); projectDir != "" && !seen[projectDir] {
		seen[projectDir] = true
		paths = append(paths, projectDir)
	}

	if session.Dir != "" && !seen[session.Dir] {
		seen[session.Dir] = true
		paths = append(paths, session.Dir)
	}

	if session.Workspace != "" && !seen[session.Workspace] {
		seen[session.Workspace] = true
		paths = append(paths, session.Workspace)
	}

	if registry != nil {
		for _, searchPath := range registry.SearchPaths() {
			if !seen[searchPath] {
				seen[searchPath] = true
				paths = append(paths, searchPath)
			}
		}

		for _, teamDir := range registry.TeamDirs() {
			if !seen[teamDir] {
				seen[teamDir] = true
				paths = append(paths, teamDir)
			}
		}
	}

	skillDirs := []string{
		filepath.Join(session.Dir, "skills"),
		filepath.Join(session.Dir, ".agents", "skills"),
		filepath.Join(os.Getenv("HOME"), ".agents", "skills"),
	}
	if registry != nil {
		for _, teamDir := range registry.TeamDirs() {
			skillDirs = append(skillDirs, filepath.Join(teamDir, "skills"))
		}
	}
	migrateLegacyDrafts(skillDirs)
	for _, dir := range skillDirs {
		abs, err := filepath.Abs(dir)
		if err == nil && !seen[abs] {
			seen[abs] = true
			paths = append(paths, abs)
		}
	}

	for _, p := range normalizeAllowedPaths(cfg.AllowedPaths) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	for _, p := range normalizeAllowedPaths(session.Config.AllowedPaths) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	for _, p := range normalizeAllowedPaths(allowPaths) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	return paths
}

func normalizeAllowedPaths(paths []string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, p := range paths {
		expanded := os.ExpandEnv(strings.TrimSpace(p))
		if expanded == "" {
			continue
		}
		if strings.HasPrefix(expanded, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				expanded = filepath.Join(home, expanded[1:])
			}
		}
		abs, err := filepath.Abs(expanded)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		result = append(result, abs)
	}
	return result
}

func currentWorkingDir() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return ""
	}
	if eval, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = eval
	}
	return filepath.Clean(cwd)
}

// migrateLegacyDrafts moves any skills/draft-* to skills/drafts/draft-*
// to support the new draft directory layout. Idempotent.
func migrateLegacyDrafts(skillDirs []string) {
	for _, dir := range skillDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "draft-") {
				continue
			}
			src := filepath.Join(dir, entry.Name())
			dst := filepath.Join(dir, "drafts", entry.Name())
			// Skip if dst already exists
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := os.MkdirAll(filepath.Join(dir, "drafts"), 0o755); err != nil {
				log.Printf("[WARN] failed to create drafts/ in %s: %v", dir, err)
				continue
			}
			if err := os.Rename(src, dst); err != nil {
				log.Printf("[WARN] failed to migrate %s -> %s: %v", src, dst, err)
				continue
			}
			log.Printf("[INFO] migrated legacy draft: %s -> %s", src, dst)
		}
	}
}

func newPathConsent() *tools.PathConsent {
	return tools.NewPathConsent()
}

func makeStepConfirmFn() func(context.Context, []team.TaskDef) (bool, error) {
	return func(ctx context.Context, tasks []team.TaskDef) (bool, error) {
		tools.StdinMu.Lock()
		defer tools.StdinMu.Unlock()

		tools.SetAskUserActive(true)
		defer tools.SetAskUserActive(false)

		fmt.Fprintf(os.Stderr, "\n%s %d worker task(s) ready to execute:\n",
			boldStyle.Render("─── STEPS ───"), len(tasks))
		for i, t := range tasks {
			fmt.Fprintf(os.Stderr, "  %s %s: %s\n",
				dimStyle.Render(fmt.Sprintf("%d.", i+1)),
				agentStyle.Render(strings.ToLower(t.Agent)),
				t.Goal)
		}
		fmt.Fprintf(os.Stderr, "\n")

		pr := globalPromptReader.Load()
		var answer string
		if pr != nil {
			line, err := pr.ReadLine(boldStyle.Render("Execute? [Y/n]: "))
			if err != nil {
				if errors.Is(err, io.EOF) {
					return false, nil
				}
				return false, err
			}
			answer = strings.TrimSpace(line)
		} else {
			prompt := promptui.Prompt{
				Label: "Execute? [Y/n]",
			}
			var err error
			answer, err = prompt.Run()
			if err != nil {
				if err == promptui.ErrInterrupt || err == io.EOF {
					return false, nil
				}
				return false, err
			}
		}

		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "" || answer == "y" || answer == "yes", nil
	}
}

func runWithInjection(ctx context.Context, tc *teamContext, initialResult string, injector *promptInjector) (string, error) {
	result := initialResult
	turn := 0
	for {
		var prompt string
		var ok bool
		var wrapUp bool

		if isChatTUI {
			select {
			case <-ctx.Done():
				return result, nil
			case <-injector.wrapUpCh:
				wrapUp = true
			case prompt, ok = <-injector.ch:
				if !ok {
					return result, nil
				}
			}
		} else {
			select {
			case <-injector.wrapUpCh:
				wrapUp = true
			case prompt, ok = <-injector.ch:
				if !ok {
					return result, nil
				}
			default:
				return result, nil
			}
		}

		if wrapUp {
			stderrLog("\n%s Wrapping up — coordinator will summarize and finish.\n\n", boldStyle.Render("⏹"))
			contResult, err := tc.coordinator.ContinueWithPrompt(ctx, "")
			if err != nil {
				if ctx.Err() == context.Canceled {
					return result, nil
				}
				return result, err
			}
			result += "\n\n---\n\n" + contResult
			return result, nil
		}

		stderrLog("\n%s Injecting additional prompt...\n\n", boldStyle.Render("↩"))

		var contResult string
		var err error
		if isChatTUI && turn == 0 {
			contResult, err = tc.coordinator.Run(ctx, prompt)
		} else {
			contResult, err = tc.coordinator.ContinueWithPrompt(ctx, prompt)
		}
		if err != nil {
			return result, err
		}
		result += "\n\n---\n\n" + contResult
		turn++
	}
}

// validateRunFlags checks for invalid flag combinations and adjusts
// flags that conflict with --unattended. Returns an error if the
// command cannot run, otherwise nil (with side effects on stepsMode
// and tuiMode as needed).
func validateRunFlags() error {
	switch outputFormat {
	case "", "text", "json":
	default:
		return fmt.Errorf("invalid --output %q: use 'text' or 'json'", outputFormat)
	}
	switch displayMode {
	case "auto", "terminal", "plain":
	default:
		return fmt.Errorf("invalid --display-mode %q: use 'auto', 'terminal', or 'plain'", displayMode)
	}
	// JSON output implies quiet: stdout must carry only the JSON document.
	if outputFormat == "json" {
		quietMode = true
	}
	// Unattended mode is inherently non-interactive: silently disable the
	// human-in-the-loop features rather than erroring, so the same command
	// line works whether or not a human is present.
	if unattended {
		if stepsMode {
			fmt.Fprintln(os.Stderr, "note: --unattended disables --steps (no human to confirm)")
			stepsMode = false
		}
		if tuiMode {
			fmt.Fprintln(os.Stderr, "note: --unattended disables --tui (no human to watch)")
			tuiMode = false
		}
	}
	if stepsMode && tuiMode {
		return fmt.Errorf("cannot use --steps (step confirmation) with --tui (TUI mode); remove one flag")
	}
	if defaultTeam && agentTeamName != "" {
		return fmt.Errorf("cannot use --default with --agent-team; pick one")
	}
	return nil
}

// resolveInitialPrompt determines the prompt to use by combining CLI
// args, stdin, the --template flag, and (if still empty) an
// interactive prompt. The returned prompt is fully resolved including
// any template expansion and file/project context injection.
func resolveInitialPrompt(initialPrompt string, pr *readline.PromptReader, vars map[string]string) (string, error) {
	prompt := initialPrompt
	if prompt == "" && templateName == "" {
		prompt = readStdin()
	}
	if templateName != "" {
		templated, err := loadPromptTemplate(templateName, vars)
		if err != nil {
			return "", err
		}
		if prompt != "" {
			prompt = templated + "\n\n" + prompt
		} else {
			prompt = templated
		}
	}
	if prompt == "" {
		var err error
		if pr != nil {
			prompt, err = askUserForPrompt(pr)
		} else {
			prompt, err = askUserForPromptFallback()
		}
		if err != nil {
			return "", err
		}
	}
	prompt, _ = injectFileContexts(prompt)
	cfg := config.LoadConfig()
	if projectContext || cfg.ProjectContext {
		prompt = injectProjectContext(prompt)
	}
	return prompt, nil
}

// setupInterruptHandler installs the SIGINT / Ctrl+C handler that
// drives the wrap-up / force-quit two-stage shutdown. Returns a
// cleanup function that tears down the signal handler. The signal
// goroutine calls cancelFn on the second Ctrl+C to stop in-flight work.
func setupInterruptHandler(injector *promptInjector, activeCoord *activeCoordinator, loadedTeamsPointer *map[string]*teamContext, cancelFn context.CancelFunc) func() {
	sigIntCh := make(chan os.Signal, 1)
	signal.Notify(sigIntCh, os.Interrupt)
	sigIntDone := make(chan struct{})

	var watchdog *time.Timer
	stopWatchdog := func() {
		if watchdog != nil {
			watchdog.Stop()
			watchdog = nil
		}
	}

	go func() {
		defer close(sigIntDone)
		first := true
		for range sigIntCh {
			if first {
				tools.RequestInteractiveAbort()
				if p := activeTUIProgram.Load(); p != nil {
					p.Send(tuipkg.WrapUpMsg{})
				} else {
					logCancelSource("sigint", "wrap-up requested")
					fmt.Fprintf(os.Stderr, "\n%s Wrapping up... (press Ctrl+C again to force quit)\n", boldStyle.Render("⏹"))
					if c := activeCoord.Load(); c != nil {
						c.SetWrapUp()
					}
					injector.injectWrapUp()
				}
				first = false
			} else {
				if activeTUIProgram.Load() == nil {
					currentStatus := "unknown"
					if c := activeCoord.Load(); c != nil {
						currentStatus = c.GetCurrentStatus()
					}
					logCancelSource("sigint", "force quit requested")
					fmt.Fprintf(os.Stderr, "\n%s Force quit requested\n", errStyle.Render("✗"))
					fmt.Fprintf(os.Stderr, "  Current: %s\n", currentStatus)
					fmt.Fprintf(os.Stderr, "  Cancelling in-flight operations (up to 8s grace period)...\n")
					fmt.Fprintf(os.Stderr, "  Press Ctrl+\\\\ (SIGQUIT) to dump stack if still stuck\n")
				}
				stopWatchdog()
				watchdog = time.AfterFunc(8*time.Second, func() {
					fmt.Fprintf(os.Stderr, "\n%s Operations did not cancel within 8s. Forcing exit.\n",
						errStyle.Render("⚠"))
					for _, tc := range *loadedTeamsPointer {
						if tc == nil {
							continue
						}
						if tc.session != nil && tc.session.Workspace != "" {
							fmt.Fprintf(os.Stderr, "  Session: %s\n", tc.session.Workspace)
							if tc.sessionData != nil {
								_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
								_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
							}
						}
					}
					os.Exit(130)
				})
				cancelFn()
			}
		}
	}()

	return func() {
		stopWatchdog()
		signal.Stop(sigIntCh)
		close(sigIntCh)
		<-sigIntDone
	}
}

func logCancelSource(source, detail string) {
	if detail == "" {
		stderrLog("\n%s [cancel] source=%s\n", boldStyle.Render("⏹"), source)
		return
	}
	stderrLog("\n%s [cancel] source=%s detail=%s\n", boldStyle.Render("⏹"), source, detail)
}

// promptForMissingTemplateVars detects required template variables
// (e.g. {{PROJECT_NAME}}) in the team's config files and prompts the
// user to fill them in when --unattended is not set. The updated vars
// map is returned (or unchanged if there are no missing vars).
func promptForMissingTemplateVars(ctx context.Context, segName string, registry *team.TeamRegistry, pr *readline.PromptReader, vars map[string]string) (map[string]string, error) {
	if unattended {
		return vars, nil
	}
	teamDir, err := registry.Resolve(segName)
	if err != nil {
		return vars, nil // unknown team — loadTeamByName will surface the error
	}
	missingVars, err := team.FindMissingVars(teamDir, vars)
	if err != nil || len(missingVars) == 0 {
		return vars, nil
	}
	fmt.Fprintf(os.Stderr, "\n%s Some template variables are required for team %s:\n", boldStyle.Render("📋"), teamStyle.Render(segName))
	for _, mv := range missingVars {
		var val string
		if pr != nil {
			promptStr := fmt.Sprintf("  Enter value for %s: ", teamStyle.Render(mv))
			val, err = pr.ReadLine(boldStyle.Render(promptStr))
		} else {
			prompt := promptui.Prompt{
				Label: fmt.Sprintf("Enter value for %s", mv),
			}
			val, err = prompt.Run()
		}
		if err != nil {
			return vars, fmt.Errorf("failed to read variable input: %w", err)
		}
		if vars == nil {
			vars = make(map[string]string)
		}
		vars[mv] = val
	}
	return vars, nil
}

// offerFirstTimeWizard is shown when no teams are discovered in the
// search paths. In an interactive TTY it offers the user three escape
// hatches: try --default, scaffold a team with `hufu init`, or exit so
// they can read the docs. In a non-TTY environment it falls back to a
// plain error message.
func offerFirstTimeWizard(searchPaths []string) error {
	if !tools.IsInteractiveEnvironment() {
		return fmt.Errorf("no agent teams found in search paths: %s\n  Use --default for the built-in team, or scaffold one with `hufu init <team>`", strings.Join(searchPaths, ", "))
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Welcome to hufu ───"))
	fmt.Fprintf(os.Stderr, "No agent teams found in:\n")
	for _, p := range searchPaths {
		fmt.Fprintf(os.Stderr, "  %s %s\n", dimStyle.Render("•"), p)
	}
	fmt.Fprintln(os.Stderr)

	options := []string{
		"Use --default (try hufu with no team files - built-in coordinator + Helper)",
		"Scaffold a team (create a new team interactively)",
		"Cancel (exit and read the docs)",
	}

	prompt := promptui.Select{
		Label: "No agent teams found. Pick an option",
		Items: options,
		Size:  3,
	}

	index, _, err := prompt.Run()
	if err != nil {
		return fmt.Errorf("no team configured; user cancelled first-time wizard")
	}

	switch index {
	case 1:
		fmt.Fprintf(os.Stderr, "%s Run `hufu init <team-name>` to scaffold a new team, then re-run hufu.\n", doneStyle.Render("→"))
		return fmt.Errorf("no team configured; scaffold one with `hufu init <team-name>`")
	case 2:
		fmt.Fprintf(os.Stderr, "%s See README.md Quick Start or run `hufu doctor` to verify your setup.\n", dimStyle.Render("○"))
		return fmt.Errorf("no team configured; user cancelled first-time wizard")
	default: // index == 0
		fmt.Fprintf(os.Stderr, "%s Re-run with --default (and --model <name> if you want a specific LLM):\n", doneStyle.Render("→"))
		fmt.Fprintf(os.Stderr, "    hufu --default --model ollama/qwen3:8b \"your task here\"\n")
		return fmt.Errorf("no team configured; run with --default")
	}
}

func handleSegmentError(ctx context.Context, tc *teamContext, results []string, err error, kind string, args ...any) (string, error) {
	if ctx.Err() == context.Canceled {
		if tc != nil {
			agentName, taskDesc, todoID, detail := tc.coordinator.GetLastFailureContext()
			if detail == "" {
				source := team.FailureSourceContextCanceled
				if tools.IsInteractiveAbortRequested() {
					source = team.FailureSourceSigint
				}
				detail = tc.coordinator.FailureDetail(err, source)
				if agentName == "" {
					agentName = "coordinator"
				}
				if taskDesc == "" {
					taskDesc = fmt.Sprintf(kind, args...)
				}
				tc.coordinator.PersistFailure(agentName, taskDesc, todoID, detail)
			}
			_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
		}
		stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
		return "", errInterrupted{}
	}
	if tc != nil {
		agentName, taskDesc, todoID, detail := tc.coordinator.GetLastFailureContext()
		if detail == "" {
			detail = tc.coordinator.FailureDetail(err, team.SegmentFailureSource(kind))
			if agentName == "" {
				agentName = "coordinator"
			}
			if taskDesc == "" {
				taskDesc = fmt.Sprintf(kind, args...)
			}
			tc.coordinator.PersistFailure(agentName, taskDesc, todoID, detail)
		}
		_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
		_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
	}
	return strings.Join(results, "\n\n"), fmt.Errorf(kind+": %w", append(args, err)...)
}

func executeSegments(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry, defaultProviderURL string, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string) (string, error) {
	var results []string
	currentTeamName := ""
	var prevResult string

	for i, seg := range segments {
		content := seg.Content
		if seg.IsPiped && prevResult != "" {
			content = content + "\n\n" + prevResult
		} else if strings.Contains(content, "{{PREV_RESULT}}") {
			content = strings.ReplaceAll(content, "{{PREV_RESULT}}", prevResult)
		}

		switch seg.Type {
		case team.SegmentSwitchTeam:
			teamName := seg.Name
			tc, ok := loadedTeams[teamName]
			if !ok {
				loaded, err := loadTeamByName(ctx, teamName, registry, providerURL, providerAPIKey, pathConsent, vars, forcedSkills, planMode, autoSkills)
				if err != nil {
					return strings.Join(results, "\n\n"), fmt.Errorf("failed to load team %q: %w", teamName, err)
				}
				if stepsMode {
					loaded.coordinator.SetStepConfirmFn(makeStepConfirmFn())
				}
				tc = loaded
				loadedTeams[teamName] = tc
			}

			if currentTeamName != "" && currentTeamName != teamName {
				prevTC := loadedTeams[currentTeamName]
				if prevTC != nil {
					_ = team.SaveSessionMD(prevTC.session.Workspace, team.GenerateSessionMD(prevTC.sessionData, prevTC.session.Config.Name))
				}
				stderrLog("\n%s Switching team: %s → %s\n\n", boldStyle.Render("⇒"), teamStyle.Render(currentTeamName), teamStyle.Render(teamName))
			}

			currentTeamName = teamName

			if isChatTUI {
				disp := newCoordDisplay(tc)
				activeCoord.Store(tc.coordinator)
				result, err := runWithInjection(ctx, tc, "", injector)
				activeCoord.Store(nil)
				disp.stopTimer()
				disp.finalizeTasks()
				if err != nil {
					return handleSegmentError(ctx, tc, results, err, "chat session failed")
				}
				results = append(results, result)
				continue
			}

			if content == "" {
				continue
			}

			disp := newCoordDisplay(tc)

			stderrLog("\n%s Starting team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(teamName))

			activeCoord.Store(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, content)
			activeCoord.Store(nil)
			disp.stopTimer()

			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q failed", teamName)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q continuation failed", teamName)
			}

			disp.finalizeTasks()
			stderrLog("\n%s Team %s coordination complete.\n", doneStyle.Render("✓"), teamStyle.Render(teamName))
			results = append(results, fmt.Sprintf("## Team: %s\n%s", teamName, result))
			prevResult = result

		case team.SegmentInvokeAgent:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("@%s — no active team. Specify a team with --agent-team or @team-name first", seg.Name)
			}

			tc := loadedTeams[currentTeamName]
			if tc == nil {
				return strings.Join(results, "\n\n"), fmt.Errorf("@%s — team %q not loaded", seg.Name, currentTeamName)
			}

			disp2 := newCoordDisplay(tc)

			stderrLog("\n%s Direct invocation: @%s (team: %s)\n\n", boldStyle.Render("→"), agentStyle.Render(seg.Name), teamStyle.Render(currentTeamName))

			activeCoord.Store(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			directResult, err := tc.coordinator.RunDirectAgent(ctx, seg.Name, content)
			activeCoord.Store(nil)
			disp2.stopTimer()

			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "direct agent @%s failed", seg.Name)
			}

			if directResult.Error != nil {
				stderrLog("\n%s %s failed: %s\n", errStyle.Render("✗"), agentStyle.Render(seg.Name), errStyle.Render(directResult.Error.Error()))
				results = append(results, fmt.Sprintf("## Agent: @%s\n**ERROR**: %s", seg.Name, directResult.Error))
				continue
			}

			stderrLog("\n%s %s completed, synthesizing...\n\n", doneStyle.Render("✓"), agentStyle.Render(seg.Name))

			orchDef := tc.coordinator.GetOrchestratorDef()
			if orchDef == nil {
				disp2.finalizeTasks()
				results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, directResult.Output))
				prevResult = directResult.Output
			} else {
				synthesisPrompt := fmt.Sprintf("A user directly asked @%s to do the following task:\n\n%s\n\nHere is what %s produced:\n\n---\n%s\n---\n\nPlease synthesize this into a final, well-organized answer for the user.",
					seg.Name, content, seg.Name, directResult.Output)
				activeCoord.Store(tc.coordinator)
				if injector.IsWrapUpRequested() {
					tc.coordinator.SetWrapUp()
				}
				synthResult, err := tc.coordinator.Run(ctx, synthesisPrompt)
				activeCoord.Store(nil)
				if err != nil {
					return handleSegmentError(ctx, tc, results, err, "synthesis for @%s failed", seg.Name)
				}

				synthResult, err = runWithInjection(ctx, tc, synthResult, injector)
				if err != nil {
					return handleSegmentError(ctx, tc, results, err, "synthesis continuation for @%s failed", seg.Name)
				}

				disp2.finalizeTasks()
				results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, synthResult))
				prevResult = synthResult
			}

		case team.SegmentText:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("text segment with no active team — specify a team with --agent-team or @team-name first")
			}

			tc := loadedTeams[currentTeamName]

			disp3 := newCoordDisplay(tc)

			stderrLog("\n%s Team %s processing...\n\n", boldStyle.Render("→"), teamStyle.Render(currentTeamName))

			activeCoord.Store(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, content)
			activeCoord.Store(nil)
			disp3.stopTimer()

			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q failed", currentTeamName)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q continuation failed", currentTeamName)
			}

			disp3.finalizeTasks()
			results = append(results, fmt.Sprintf("## Team: %s\n%s", currentTeamName, result))
			prevResult = result
		}

		if i == len(segments)-1 {
			if tc, ok := loadedTeams[currentTeamName]; ok {
				_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
			}
		}
	}

	if len(results) == 0 {
		return "", nil
	}
	if len(results) == 1 {
		return results[0], nil
	}
	return strings.Join(results, "\n\n---\n\n"), nil
}

func agentNamesFromSession(session *team.TeamSession) []*agent.AgentDef {
	seen := make(map[string]bool)
	var agents []*agent.AgentDef
	for _, def := range session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		agents = append(agents, def)
	}
	return agents
}

func sortedAgents(agents map[string]*agent.AgentDef) []*agent.AgentDef {
	seen := make(map[string]bool)
	var result []*agent.AgentDef
	for _, def := range agents {
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		result = append(result, def)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func defaultHistoryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".hufu")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return filepath.Join(dir, "prompt_history")
}

func archiveCurrentSessionToMemory(ctx context.Context, tc *teamContext) {
	if tc == nil || tc.sessionData == nil || len(tc.sessionData.Entries) == 0 {
		fmt.Fprintf(os.Stderr, "%s No session data to archive.\n", dimStyle.Render("○"))
		return
	}

	resolvedURL := config.ResolveProviderURL(providerURL, "", "")
	ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedURL)
	embedModel := config.ResolveEmbeddingModel(memoryModel)
	projectDir, _ := os.Getwd()

	memStore, err := memory.NewMemoryStore(projectDir, ollamaAPIURL, embedModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s Memory unavailable for archive: %v\n", errStyle.Render("⚠"), err)
		return
	}
	defer func() { _ = memStore.Close() }()

	var entries []memory.SessionSummaryEntry
	for _, e := range tc.sessionData.Entries {
		entries = append(entries, memory.SessionSummaryEntry{
			Role:      e.Role,
			Content:   e.Content,
			Timestamp: e.Timestamp,
		})
	}

	var summarizeFn memory.SummarizeFunc
	if s := tc.coordinator.Sidecar(); s != nil {
		summarizeFn = s.Summarize
	}
	if err := memory.ArchiveSessionSummary(ctx, memStore, entries, tc.session.Config.Name, summarizeFn); err != nil {
		fmt.Fprintf(os.Stderr, "%s Failed to archive session to memory: %v\n", errStyle.Render("⚠"), err)
		return
	}
	fmt.Fprintf(os.Stderr, "%s Session archived to memory.\n", doneStyle.Render("✓"))
}

func runArchiveMemory(ctx context.Context, registry *team.TeamRegistry, vars map[string]string) error {
	teams := registry.ListTeams()
	if len(teams) == 0 {
		return fmt.Errorf("no teams available")
	}

	var teamNames []string
	if agentTeamName != "" {
		teamNames = []string{strings.ToLower(agentTeamName)}
	} else {
		teamNames = teams
	}

	archived := 0
	for _, name := range teamNames {
		tc, err := loadTeamByName(ctx, name, registry, providerURL, providerAPIKey, newPathConsent(), vars, nil, false, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s Failed to load team %q: %v\n", errStyle.Render("⚠"), name, err)
			continue
		}
		if tc.sessionData != nil && len(tc.sessionData.Entries) > 0 {
			archiveCurrentSessionToMemory(ctx, tc)
			archived++
		} else {
			fmt.Fprintf(os.Stderr, "%s No session data for team %q\n", dimStyle.Render("○"), name)
		}
	}

	if archived == 0 {
		fmt.Fprintf(os.Stderr, "%s No session data found to archive.\n", dimStyle.Render("○"))
	}
	return nil
}

func askUserForPromptFallback() (string, error) {
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	prompt := promptui.Prompt{
		Label: "Prompt",
	}
	result, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return "", errInterrupted{}
		}
		return "", err
	}
	promptVal := strings.TrimSpace(result)
	if promptVal == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		return "", fmt.Errorf("no prompt provided")
	}
	return promptVal, nil
}

func askUserForTeamFallback(teams []string) string {
	res, _ := askUserForTeamWithPromptUI(teams)
	return res
}

func readStdin() string {
	stat, err := os.Stdin.Stat()
	if err != nil || (stat.Mode()&os.ModeCharDevice) != 0 {
		return ""
	}
	data, _ := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(data))
}

func askUserForPrompt(pr *readline.PromptReader) (string, error) {
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	prompt, err := pr.ReadLine(boldStyle.Render("> "))
	if err != nil {
		if err == ergoreadline.ErrInterrupt || err == io.EOF {
			fmt.Fprintf(os.Stderr, "\n")
			return "", errInterrupted{}
		}
		fmt.Fprintf(os.Stderr, "%s Input error: %v\n", errStyle.Render("✗"), err)
		return "", fmt.Errorf("input error: %w", err)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		return "", fmt.Errorf("no prompt provided")
	}
	return prompt, nil
}

func askUserForTeam(teams []string, pr *readline.PromptReader) (string, error) {
	return askUserForTeamWithPromptUI(teams)
}

func askUserForTeamWithPromptUI(teams []string) (string, error) {
	if len(teams) == 0 {
		return "", nil
	}
	sort.Strings(teams)

	stat, err := os.Stdin.Stat()
	isTTY := err == nil && (stat.Mode()&os.ModeCharDevice) != 0
	if unattended || !isTTY {
		return teams[0], nil
	}

	searcher := func(input string, index int) bool {
		team := teams[index]
		name := strings.ReplaceAll(strings.ToLower(team), " ", "")
		input = strings.ReplaceAll(strings.ToLower(input), " ", "")
		return strings.Contains(name, input)
	}

	prompt := promptui.Select{
		Label:    "Select Team",
		Items:    teams,
		Size:     10,
		Searcher: searcher,
	}

	_, result, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return "", errInterrupted{}
		}
		return "", err
	}
	return result, nil
}

func completeAtNames(toComplete string) []string {
	var searchPaths []string
	if agentTeamSearchPath != "" {
		searchPaths = strings.Split(agentTeamSearchPath, ",")
	} else {
		searchPaths = team.DefaultSearchPaths()
	}
	registry := team.NewTeamRegistry(searchPaths)
	if err := registry.Discover(); err != nil {
		return nil
	}

	var results []string
	prefix := strings.ToLower(toComplete)
	if !strings.HasPrefix(prefix, "@") {
		// Suggest all teams with @ prefix
		for _, name := range registry.ListTeams() {
			results = append(results, "@"+name)
		}
		sort.Strings(results)
		return results
	}

	subToComplete := prefix[1:]

	// Find matching teams
	for _, name := range registry.ListTeams() {
		if strings.HasPrefix(strings.ToLower(name), subToComplete) {
			results = append(results, "@"+name)
		}
	}

	// Scan all team directories for agent names too!
	for _, dir := range registry.TeamDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			agentName := strings.ToLower(strings.TrimSuffix(entry.Name(), ".md"))
			if strings.HasPrefix(agentName, subToComplete) {
				results = append(results, "@"+agentName)
			}
		}
	}

	unique := make(map[string]bool)
	for _, r := range results {
		unique[r] = true
	}
	var sorted []string
	for k := range unique {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	return sorted
}

func injectFileContexts(prompt string) (string, string) {
	re := regexp.MustCompile(`\B#([^\s#]+)`)
	matches := re.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return prompt, ""
	}

	var additions strings.Builder
	replacedPrompt := prompt
	injected := make(map[string]bool)
	var missing []string

	for _, match := range matches {
		rawPath := match[1]
		cleanPath := strings.TrimRight(rawPath, ".,;:!?\")'")
		if injected[cleanPath] {
			continue
		}
		injected[cleanPath] = true

		fi, err := os.Stat(cleanPath)
		if err != nil || fi.IsDir() {
			missing = append(missing, cleanPath)
			continue
		}

		if fi.Size() > 100*1024 {
			stderrLog("%s Warning: File %s is too large (>100KB), skipping context injection.\n", errStyle.Render("⚠"), cleanPath)
			continue
		}

		data, err := os.ReadFile(cleanPath)
		if err != nil {
			continue
		}

		isBinary := false
		for i := 0; i < len(data) && i < 1024; i++ {
			if data[i] == 0 {
				isBinary = true
				break
			}
		}
		if isBinary {
			stderrLog("%s Warning: File %s appears to be binary, skipping context injection.\n", errStyle.Render("⚠"), cleanPath)
			continue
		}

		ext := strings.TrimPrefix(filepath.Ext(cleanPath), ".")
		if ext == "" {
			ext = "text"
		}

		fmt.Fprintf(&additions, "\n\n---\nFile: %s\n```%s\n%s\n```", cleanPath, ext, string(data))
		injected[cleanPath] = true

		replacedPrompt = strings.ReplaceAll(replacedPrompt, "#"+rawPath, rawPath)
	}

	if len(missing) > 0 {
		stderrLog("%s %d file reference(s) not found and were skipped: %s\n", errStyle.Render("⚠"), len(missing), strings.Join(missing, ", "))
	}
	if additions.Len() > 0 {
		stderrLog("%s Injected content of %d file(s) into context.\n", doneStyle.Render("✓"), len(injected))
		return replacedPrompt + additions.String(), replacedPrompt
	}

	return prompt, ""
}

func loadPromptTemplate(name string, existingVars map[string]string) (string, error) {
	var searchDirs []string
	cwd, err := os.Getwd()
	if err == nil {
		searchDirs = append(searchDirs, filepath.Join(cwd, ".hufu-templates"))
	}
	home, err := os.UserHomeDir()
	if err == nil {
		searchDirs = append(searchDirs, filepath.Join(home, ".config", "hufu", "templates"))
	}

	var filePath string
	var found bool
	for _, dir := range searchDirs {
		for _, ext := range []string{".md", ".yaml", ".yml", ""} {
			p := filepath.Join(dir, name+ext)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				filePath = p
				found = true
				break
			}
		}
		if found {
			break
		}
	}

	if !found {
		return "", fmt.Errorf("prompt template %q not found in search directories", name)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read template file: %w", err)
	}

	content := string(data)
	body := content
	var vars []string

	if strings.HasPrefix(content, "---\n") {
		parts := strings.SplitN(content, "---\n", 3)
		if len(parts) >= 3 {
			var metadata struct {
				Description string   `yaml:"description"`
				Vars        []string `yaml:"vars"`
			}
			if yaml.Unmarshal([]byte(parts[1]), &metadata) == nil {
				vars = metadata.Vars
				body = parts[2]
				if metadata.Description != "" {
					stderrLog("%s Template: %s (%s)\n", boldStyle.Render("📋"), name, metadata.Description)
				}
			}
		}
	}

	if len(vars) > 0 {
		for _, v := range vars {
			v = strings.TrimSpace(v)
			if _, ok := existingVars[v]; ok {
				continue
			}

			if unattended {
				return "", fmt.Errorf("template variable %q is required but not provided in unattended mode", v)
			}

			var val string
			var inputErr error
			promptStr := fmt.Sprintf("  Enter value for template variable %s: ", teamStyle.Render(v))

			pr := globalPromptReader.Load()
			if pr != nil {
				val, inputErr = pr.ReadLine(boldStyle.Render(promptStr))
			} else {
				prompt := promptui.Prompt{
					Label: fmt.Sprintf("Enter value for template variable %s", v),
				}
				val, inputErr = prompt.Run()
			}

			if inputErr != nil {
				return "", fmt.Errorf("failed to read variable input: %w", inputErr)
			}
			existingVars[v] = val
		}
	}

	templated := body
	for k, v := range existingVars {
		templated = strings.ReplaceAll(templated, "{{"+k+"}}", v)
		templated = strings.ReplaceAll(templated, "{{ "+k+" }}", v)
	}

	return strings.TrimSpace(templated), nil
}

func injectProjectContext(prompt string) string {
	isGit := false
	if _, err := os.Stat(".git"); err == nil {
		isGit = true
	}

	var sb strings.Builder
	sb.WriteString(prompt)

	if isGit {
		cmd := exec.Command("git", "status", "--short")
		if output, err := cmd.Output(); err == nil && len(output) > 0 {
			sb.WriteString("\n\n---\n## Git Status\n```text\n")
			sb.WriteString(string(output))
			sb.WriteString("```")
		}
	}

	var tree []string
	cwd, err := os.Getwd()
	if err == nil {
		tree = generateDirectoryTree(cwd, "", 0, 2)
	}

	if len(tree) > 0 {
		sb.WriteString("\n\n## Project Directory Structure\n```text\n")
		sb.WriteString(strings.Join(tree, "\n"))
		sb.WriteString("\n```")
	}

	return sb.String()
}

func generateDirectoryTree(dir string, prefix string, depth int, maxDepth int) []string {
	if depth >= maxDepth {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var lines []string
	skipDirs := map[string]bool{
		".git":         true,
		"node_modules": true,
		"workspace":    true,
		".agent-teams": true,
		"tmp":          true,
		"temp":         true,
		"vendor":       true,
		"dist":         true,
		"build":        true,
	}

	var filtered []os.DirEntry
	for _, e := range entries {
		if skipDirs[e.Name()] {
			continue
		}
		filtered = append(filtered, e)
	}

	for i, e := range filtered {
		isLast := i == len(filtered)-1
		connector := "├── "
		nextPrefix := prefix + "│   "
		if isLast {
			connector = "└── "
			nextPrefix = prefix + "    "
		}

		lines = append(lines, prefix+connector+e.Name())
		if e.IsDir() {
			subLines := generateDirectoryTree(filepath.Join(dir, e.Name()), nextPrefix, depth+1, maxDepth)
			lines = append(lines, subLines...)
		}
	}

	return lines
}

func savePromptToHistory(ctx context.Context, prompt string, defaultProviderURL string) {
	resolvedProviderURL := config.ResolveProviderURL(defaultProviderURL, "", "")
	ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedProviderURL)
	embedModel := config.ResolveEmbeddingModel("")

	store, err := memory.NewGlobalMemoryStore(ollamaAPIURL, embedModel)
	if err != nil {
		return
	}

	id := fmt.Sprintf("hist_%d", time.Now().UnixNano())
	metadata := map[string]string{
		"type":      "prompt_history",
		"timestamp": time.Now().Format(time.RFC3339),
	}
	saveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = store.Save(saveCtx, id, prompt, metadata)
}

var historyCmd = &cobra.Command{
	Use:   "history [query]",
	Short: "Search semantic history of prompts",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		resolvedProviderURL := config.ResolveProviderURL(providerURL, "", "")
		ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedProviderURL)
		embedModel := config.ResolveEmbeddingModel(memoryModel)

		store, err := memory.NewGlobalMemoryStore(ollamaAPIURL, embedModel)
		if err != nil {
			return fmt.Errorf("failed to open global memory store: %w", err)
		}

		query := ""
		if len(args) > 0 {
			query = args[0]
		}

		if query == "" {
			fmt.Println("Please provide a query to search prompt history. Example: hufu history \"write python script\"")
			return nil
		}

		results, err := store.Query(ctx, query, 5, map[string]string{"type": "prompt_history"})
		if err != nil {
			return fmt.Errorf("semantic search failed: %w", err)
		}

		if len(results) == 0 {
			fmt.Println("No matching prompt history found.")
			return nil
		}

		fmt.Printf("\n%s Semantic History Search Results (query: %q):\n\n", boldStyle.Render("🗂️"), query)
		for i, res := range results {
			ts := res.Metadata["timestamp"]
			fmt.Printf("  %d. %s %s\n", i+1, doneStyle.Render("→"), boldStyle.Render(res.Content))
			if ts != "" {
				fmt.Printf("     %s\n", dimStyle.Render("Time: "+ts))
			}
			fmt.Println()
		}
		return nil
	},
}
