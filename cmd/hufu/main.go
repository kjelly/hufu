package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	ergoreadline "github.com/ergochat/readline"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/hooks"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/memory"
	"github.com/anomalyco/hufu/internal/notify"
	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
	tuipkg "github.com/anomalyco/hufu/internal/tui"
)

var (
	providerURL          string
	providerAPIKey       string
	verbose              bool
	workspace            string
	newSession           bool
	tempWorkspace        bool
	agentTeamName        string
	agentTeamSearchPath  string
	memoryEnabled        bool
	memoryModel          string
	archiveMemory        bool
	showHistory          bool
	stepsMode            bool
	dryRun               bool
	tuiMode              bool
	rbashMode            bool
	noNet                bool
	forceMCP             bool
	think                bool
	direnv               bool
	varFlags             []string
	varFiles             []string
	forcedSkills         []string
	planMode             bool
	autoSkills           bool
	fixQuestion          string
	reportMode           bool
	defaultTeam          bool
	helperTools          string
	modelOverride        string
	temperatureOverride  string
	maxTokensOverride    string
	topPOverride         string
	topKOverride         string
	sidecarModelOverride string
	guardModelOverride   string
	timeoutOverride      int64
	unattended           bool
	maxDuration          int64
	maxTotalTokens       int64
	autoTeam             bool
	templateName         string
	profileName          string
	quietMode            bool
	outputFormat         string
	globalPromptReader   atomic.Pointer[readline.PromptReader]
)

type errInterrupted struct{}

func (errInterrupted) Error() string { return "interrupted" }

var version = "dev"

func main() {
	exitCode := 0
	rootCmd := &cobra.Command{
		Use:     "hufu [prompt]",
		Short:   "Run an agent team to accomplish a task",
		Long:    "hufu discovers and runs agent teams by name. Use --agent-team or @team-name in the prompt to select a team.",
		Args:    cobra.MaximumNArgs(1),
		RunE:    runTeam,
		Version: version,
	}

	// Add skill management commands
	rootCmd.AddCommand(skillCmd)
	rootCmd.AddCommand(replCmd)
	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(initCmd)

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
	rootCmd.Flags().StringVar(&memoryModel, "memory-model", "", "Embedding model for memory (default: qwen3-embedding:4b, overrides hufu.yaml)")
	rootCmd.Flags().BoolVar(&archiveMemory, "archive-memory", false, "Archive session summary to memory and exit")
	rootCmd.Flags().BoolVar(&showHistory, "show-history", false, "Show previous session history on resume")
	rootCmd.Flags().BoolVarP(&stepsMode, "steps", "s", false, "Pause for user confirmation before executing each batch of worker tasks")
	rootCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview skill matching and task delegation without executing agents")
	rootCmd.Flags().BoolVar(&tuiMode, "tui", false, "Show a Bubble Tea TUI for real-time task tracking")
	rootCmd.Flags().BoolVar(&rbashMode, "rbash", false, "Use restricted bash (rbash) for the bash tool")
	rootCmd.Flags().BoolVar(&noNet, "no-net", false, "Block all network access for agent subprocesses")
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
	rootCmd.Flags().StringVar(&modelOverride, "model", "", "Override default model for the active team (e.g. ollama/qwen3:8b)")
	rootCmd.Flags().StringVar(&temperatureOverride, "temperature", "", "Override sampling temperature (e.g. 0.2)")
	rootCmd.Flags().StringVar(&maxTokensOverride, "max-tokens", "", "Override max output tokens (e.g. 4096)")
	rootCmd.Flags().StringVar(&topPOverride, "top-p", "", "Override top-p value (e.g. 0.9)")
	rootCmd.Flags().StringVar(&topKOverride, "top-k", "", "Override top-k value (e.g. 40)")
	rootCmd.Flags().StringVar(&sidecarModelOverride, "sidecar-model", "", "Override sidecar model used for skill matching (e.g. ollama/qwen3:1b); falls back to --model when not set")
	rootCmd.Flags().StringVar(&guardModelOverride, "guard-model", "", "Override guard model used for output review (e.g. ollama/qwen3:8b); falls back to --model when not set")
	rootCmd.Flags().Int64Var(&timeoutOverride, "timeout", 0, "Override agent/coordinator timeout in seconds (e.g. 1800 for 30 min). 0 = use team/agent default.")
	rootCmd.Flags().BoolVar(&unattended, "unattended", false, "Run with no human present: ask_user returns safe defaults instead of blocking, --steps/--tui are disabled, and only allowlisted tools may run")
	rootCmd.Flags().Int64Var(&maxDuration, "max-duration", 0, "Budget: max total wall-clock seconds before forcing wrap-up (0 = unlimited). Recommended for unattended runs.")
	rootCmd.Flags().Int64Var(&maxTotalTokens, "max-total-tokens", 0, "Budget: max cumulative LLM tokens before forcing wrap-up (0 = unlimited). Recommended for unattended runs.")
	rootCmd.Flags().BoolVar(&autoTeam, "auto-team", false, "Auto-select the team best suited to the prompt (sidecar LLM match, keyword fallback) instead of prompting")
	rootCmd.PersistentFlags().StringVar(&profileName, "profile", "", "Apply a named flag bundle from hufu.yaml `profiles:` (CLI flags still override)")
	rootCmd.Flags().BoolVarP(&quietMode, "quiet", "q", false, "Suppress status output; print only the final result to stdout")
	rootCmd.Flags().StringVar(&outputFormat, "output", "", "Output format for the final result: text (default) or json")

	// init scaffolding flags (consumed by initcmd.go).
	initCmd.Flags().StringVar(&templateName, "template", "default", "Scaffold template for `hufu init`: default")
	initCmd.Flags().StringVar(&modelOverride, "model", "", "Pin a model in the scaffolded team.yaml (e.g. ollama/qwen3:8b)")

	rootCmd.RegisterFlagCompletionFunc("agent-team", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
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
	// Apply a named profile first so its values feed the checks below; explicit
	// CLI flags still win (applyProfile respects flag.Changed).
	if err := applyProfile(cmd); err != nil {
		return err
	}

	switch outputFormat {
	case "", "text", "json":
	default:
		return fmt.Errorf("invalid --output %q: use 'text' or 'json'", outputFormat)
	}
	// JSON output implies quiet: stdout must carry only the JSON document.
	if outputFormat == "json" {
		quietMode = true
	}

	// Unattended mode is inherently non-interactive: silently disable the
	// human-in-the-loop features rather than erroring, so the same command line
	// works whether or not a human is present.
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

	// Refuse --steps + --tui combination: step confirmation requires terminal
	// access that conflicts with the Bubble Tea altscreen.
	if stepsMode && tuiMode {
		return fmt.Errorf("cannot use --steps (step confirmation) with --tui (TUI mode); remove one flag")
	}

	// --default and --agent-team are mutually exclusive: --default provides its
	// own in-memory team, so an explicit --agent-team would conflict.
	if defaultTeam && agentTeamName != "" {
		return fmt.Errorf("cannot use --default with --agent-team; pick one")
	}

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

	prompt := ""
	if len(args) > 0 {
		prompt = args[0]
	}
	if prompt == "" {
		prompt = readStdin()
	}

	if archiveMemory && prompt == "" {
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

	if prompt == "" {
		if pr != nil {
			p, err := askUserForPrompt(pr)
			if err != nil {
				return err
			}
			prompt = p
		} else {
			p, err := askUserForPromptFallback()
			if err != nil {
				return err
			}
			prompt = p
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	injector := newPromptInjector(pr)
	activeCoord := &activeCoordinator{}
	defer setupPromptSignals(injector)()

	// loadedTeams is assigned below; the SIGINT handler reads it via
	// the pointer to print session paths during the watchdog step.
	// Declared as a top-level var so the closure captures the
	// pointer, not the value.
	var loadedTeams map[string]*teamContext

	sigIntCh := make(chan os.Signal, 1)
	signal.Notify(sigIntCh, os.Interrupt)
	sigIntDone := make(chan struct{})

	// Watchdog: 8 seconds after the second Ctrl+C, if the program has
	// not exited, attempt to save session data and force-exit. This
	// guarantees users see SOMETHING even if a goroutine is stuck on
	// a network call that doesn't honor context cancellation.
	var watchdog *time.Timer
	stopWatchdog := func() {
		if watchdog != nil {
			watchdog.Stop()
			watchdog = nil
		}
	}
	defer stopWatchdog()

	go func() {
		defer close(sigIntDone)
		first := true
		for range sigIntCh {
			if first {
				if p := activeTUIProgram.Load(); p != nil {
					// TUI path: send WrapUpMsg and let the TUI handle SetWrapUp/injectWrapUp
					// to avoid double-calling (the TUI's wrapUpCh goroutine calls them).
					p.Send(tuipkg.WrapUpMsg{})
				} else {
					// Non-TUI path: need to call SetWrapUp and injectWrapUp here.
					fmt.Fprintf(os.Stderr, "\n%s Wrapping up... (press Ctrl+C again to force quit)\n", boldStyle.Render("⏹"))
					if c := activeCoord.Get(); c != nil {
						c.SetWrapUp()
					}
					injector.injectWrapUp()
				}
				first = false
			} else {
				if activeTUIProgram.Load() == nil {
					// Print current state so user knows where the program is stuck.
					currentStatus := "unknown"
					if c := activeCoord.Get(); c != nil {
						currentStatus = c.GetCurrentStatus()
					}
					fmt.Fprintf(os.Stderr, "\n%s Force quit requested\n", errStyle.Render("✗"))
					fmt.Fprintf(os.Stderr, "  Current: %s\n", currentStatus)
					fmt.Fprintf(os.Stderr, "  Cancelling in-flight operations (up to 8s grace period)...\n")
					fmt.Fprintf(os.Stderr, "  Press Ctrl+\\\\ (SIGQUIT) to dump stack if still stuck\n")
				}

				// Start watchdog: if main hasn't returned within 8s, force-exit.
				// Snapshot loaded teams under no lock; best-effort session save.
				stopWatchdog() // ensure no double-watchdog
				watchdog = time.AfterFunc(8*time.Second, func() {
					fmt.Fprintf(os.Stderr, "\n%s Operations did not cancel within 8s. Forcing exit.\n",
						errStyle.Render("⚠"))
					for _, tc := range loadedTeams {
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

				cancel()
			}
		}
	}()

	defer func() {
		signal.Stop(sigIntCh)
		close(sigIntCh)
		<-sigIntDone
	}()

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
			return fmt.Errorf("no agent teams found in search paths: %s\n  Use --default for the built-in team, or scaffold one with `hufu init <team>`", strings.Join(searchPaths, ", "))
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
				fmt.Fprintf(os.Stderr, "%s No team selected.\n", errStyle.Render("✗"))
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

	loadedTeams = map[string]*teamContext{}
	for _, seg := range initialSegments {
		if seg.Type == team.SegmentSwitchTeam {
			if _, ok := loadedTeams[seg.Name]; !ok {
				var tc *teamContext
				var err error
				if defaultTeam && seg.Name == "default" {
					tc, err = loadDefaultTeam(ctx, providerURL, providerAPIKey, pathConsent, vars, forcedSkills, planMode, autoSkills)
				} else {
					teamDir, resolveErr := registry.Resolve(seg.Name)
					if resolveErr == nil && !unattended {
						missingVars, scanErr := team.FindMissingVars(teamDir, vars)
						if scanErr == nil && len(missingVars) > 0 {
							fmt.Fprintf(os.Stderr, "\n%s Some template variables are required for team %s:\n", boldStyle.Render("📋"), teamStyle.Render(seg.Name))
							for _, mv := range missingVars {
								var val string
								var inputErr error
								promptStr := fmt.Sprintf("  Enter value for %s: ", teamStyle.Render(mv))
								if pr != nil {
									val, inputErr = pr.ReadLine(boldStyle.Render(promptStr))
								} else {
									fmt.Fprint(os.Stderr, boldStyle.Render(promptStr))
									var line string
									_, inputErr = fmt.Scanln(&line)
									val = strings.TrimSpace(line)
								}
								if inputErr != nil {
									return fmt.Errorf("failed to read variable input: %w", inputErr)
								}
								if vars == nil {
									vars = make(map[string]string)
								}
								vars[mv] = val
							}
						}
					}
					tc, err = loadTeamByName(ctx, seg.Name, registry, providerURL, providerAPIKey, pathConsent, vars, forcedSkills, planMode, autoSkills)
				}
				if err != nil {
					return fmt.Errorf("failed to load team %q: %w", seg.Name, err)
				}
				if stepsMode {
					tc.coordinator.SetStepConfirmFn(makeStepConfirmFn())
				}
				loadedTeams[seg.Name] = tc
			}
		}
	}

	var segments []team.PromptSegment
	for _, seg := range initialSegments {
		if seg.Type == team.SegmentSwitchTeam {
			tc := loadedTeams[seg.Name]
			if tc != nil && seg.Content != "" {
				subSegs, err := team.SplitSegmentByAgents(seg, registry, agentNamesFromSession(tc.session))
				if err != nil {
					return err
				}
				segments = append(segments, subSegs...)
			} else {
				segments = append(segments, seg)
			}
		} else {
			segments = append(segments, seg)
		}
	}

	if dryRun {
		var dryRunTeamName string
		for _, seg := range segments {
			if seg.Type == team.SegmentSwitchTeam {
				dryRunTeamName = seg.Name
				break
			}
		}
		if dryRunTeamName == "" {
			dryRunTeamName = strings.ToLower(agentTeamName)
		}
		if dryRunTeamName == "" {
			return fmt.Errorf("--dry-run requires a team (use --agent-team or @team-name in the prompt)")
		}
		tc, ok := loadedTeams[dryRunTeamName]
		if !ok {
			return fmt.Errorf("failed to load team %q for dry-run", dryRunTeamName)
		}
		dryRunPrompt := ""
		for _, seg := range segments {
			if seg.Type == team.SegmentSwitchTeam && seg.Name == dryRunTeamName {
				dryRunPrompt = seg.Content
			} else if seg.Type == team.SegmentText && dryRunPrompt == "" && dryRunTeamName != "" {
				dryRunPrompt = seg.Content
			}
		}
		if dryRunPrompt == "" {
			dryRunPrompt = prompt
		}
		if dryRunPrompt == "" {
			return fmt.Errorf("--dry-run requires a prompt")
		}

		fmt.Fprintf(os.Stderr, "\n%s Running dry-run for team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(dryRunTeamName))

		dryDisp := newCoordDisplay(tc)
		result, err := tc.coordinator.DryRun(ctx, dryRunPrompt)
		dryDisp.stopTimer()

		if err != nil {
			return fmt.Errorf("dry-run failed: %w", err)
		}
		renderDryRun(result)
		if reportMode {
			generateReport(loadedTeams, "(dry-run — no tasks executed)")
		}
		return nil
	}

	var result string
	var runErr error
	if tuiMode {
		var teamInfo tuipkg.TeamInfo
		teamInfo.AvailableTeams = registry.ListTeams()
		for _, tc := range loadedTeams {
			if tc != nil && tc.session != nil {
				teamInfo.TeamName = tc.session.Config.Name
				teamInfo.Workspace = tc.session.Workspace
				teamInfo.TeamDir = tc.session.Dir
				teamInfo.DefaultModel = tc.session.Config.Generation.Model
				for _, ag := range sortedAgents(tc.session.Agents) {
					model := ag.Generation.Model
					if model == "" {
						model = tc.session.Config.Generation.Model
					}
					teamInfo.Agents = append(teamInfo.Agents, tuipkg.AgentInfoEntry{
						Name:  ag.Name,
						Role:  ag.Role,
						Model: model,
					})
				}
				for _, s := range tc.session.Skills {
					teamInfo.Skills = append(teamInfo.Skills, s.Name)
				}
				if sc := tc.session.Config.SidecarModel; sc != "" {
					teamInfo.SidecarModel = sc
				}
				if gm := tc.session.Config.GuardModel; gm != "" {
					teamInfo.GuardModel = gm
				}
				teamInfo.MemoryEnabled = memoryEnabled && !tempWorkspace
				if teamInfo.MemoryEnabled {
					teamInfo.MemoryModel = config.ResolveEmbeddingModel(memoryModel)
				}
				teamInfo.SSHSessions = 0
				break
			}
		}
		result, runErr = runWithTUI(ctx, cancel, prompt, segments, registry, loadedTeams, injector, activeCoord, pathConsent, vars, teamInfo)
	} else {
		result, runErr = executeSegments(ctx, segments, registry, providerURL, loadedTeams, injector, activeCoord, pathConsent, vars)
	}
	if runErr != nil {
		return runErr
	}

	if reportMode {
		generateReport(loadedTeams, result)
	}

	var allSkillUsage []team.SkillUsageEntry
	seenSkill := map[string]int{}
	for teamName, tc := range loadedTeams {
		for _, entry := range tc.coordinator.SkillUsage() {
			key := strings.ToLower(entry.Name)
			if idx, ok := seenSkill[key]; ok {
				allSkillUsage[idx].Count += entry.Count
				for _, a := range entry.Agents {
					prefixed := teamName + "/" + a
					if !slices.Contains(allSkillUsage[idx].Agents, prefixed) {
						allSkillUsage[idx].Agents = append(allSkillUsage[idx].Agents, prefixed)
					}
				}
			} else {
				seenSkill[key] = len(allSkillUsage)
				prefixed := make([]string, len(entry.Agents))
				for i, a := range entry.Agents {
					prefixed[i] = teamName + "/" + a
				}
				allSkillUsage = append(allSkillUsage, team.SkillUsageEntry{
					Name:   entry.Name,
					Count:  entry.Count,
					Agents: prefixed,
				})
			}
		}
	}
	if outputFormat == "json" {
		if err := printResultJSON(result, loadedTeams, allSkillUsage); err != nil {
			return err
		}
	} else {
		fmt.Println(result)
		if !quietMode {
			renderSkillSummary(allSkillUsage)
		}
	}

	if archiveMemory && !newSession {
		for _, tc := range loadedTeams {
			archiveCurrentSessionToMemory(ctx, tc)
		}
	}

	if tempWorkspace {
		absWS, _ := filepath.Abs(workspace)
		fmt.Fprintf(os.Stderr, "\n%s\n  Path: %s\n  Reuse: hufu -w %s [prompt]\n",
			boldStyle.Render("─── Temporary Workspace ───"),
			absWS, absWS)
	}

	return nil
}

// applyUnattendedAndBudget configures the coordinator's unattended mode,
// run budgets, and acceptance check from the CLI flags and team config.
// CLI flags take precedence; team.yaml values are the fallback.
func applyUnattendedAndBudget(coordinator *team.Coordinator, session *team.TeamSession) {
	coordinator.SetUnattended(unattended || session.Config.Unattended)

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

func loadTeamByName(ctx context.Context, teamName string, registry *team.TeamRegistry, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, vars map[string]string, forcedSkills []string, planMode bool, autoSkillsMode bool) (*teamContext, error) {
	teamDir, err := registry.Resolve(teamName)
	if err != nil {
		return nil, err
	}

	session, err := team.LoadTeam(teamDir, vars, forcedSkills)
	if err != nil {
		return nil, err
	}

	// Apply CLI model overrides as the highest-priority model config layer,
	// then propagate the resolved Generation to all agents so per-agent
	// dispatch uses the same model the user asked for on the command line.
	applyCLIModelOverrides(&session.Config, currentModelOverrides())
	applyCLITimeoutOverrides(session, currentTimeoutOverrides())
	propagateTeamGenerationToAgents(session)

	resolvedProviderURL := config.ResolveProviderURL(defaultProviderURL, session.Config.ProviderURL, "")
	resolvedProviderAPIKey := config.ResolveProviderAPIKey(defaultProviderAPIKey, session.Config.ProviderAPIKey)

	if workspace != "" {
		absWorkspace, err := filepath.Abs(workspace)
		if err != nil {
			return nil, fmt.Errorf("invalid workspace path: %w", err)
		}
		teamWorkspace := filepath.Join(absWorkspace, teamName)
		if err := os.MkdirAll(teamWorkspace, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create workspace: %w", err)
		}
		session.Workspace = teamWorkspace
		session.Config.WorkspaceDir = teamWorkspace
	}

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
		// Extract knowledge from all accumulated history/*-stm.md snapshots
		// into ltm.md, then delete the history files.
		team.ExtractLTMFromHistory(session.Workspace, session.Config.Name)
	}

	var sessionData *team.SessionData
	var oldSessionEntries []memory.SessionSummaryEntry
	if newSession {
		oldSession := team.LoadSession(session.Workspace)
		if oldSession != nil {
			for _, e := range oldSession.Entries {
				oldSessionEntries = append(oldSessionEntries, memory.SessionSummaryEntry{
					Role:      e.Role,
					Content:   e.Content,
					Timestamp: e.Timestamp,
				})
			}
		}

		existingMD := team.LoadSessionMD(session.Workspace)
		if existingMD != "" {
			stderrLog("%s Archiving previous session...\n", stepStyle.Render("⟳"))
			archivedPath, err := team.ArchiveSessionMD(session.Workspace)
			if err != nil {
				stderrLog("%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
			} else {
				stderrLog("%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
			}
		} else if team.HasSession(session.Workspace) {
			oldSession := team.LoadSession(session.Workspace)
			if oldSession != nil && len(oldSession.Entries) > 0 {
				stderrLog("%s Archiving previous session...\n", stepStyle.Render("⟳"))
				md := team.GenerateSessionMD(oldSession, session.Config.Name)
				if err := team.SaveSessionMD(session.Workspace, md); err != nil {
					stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
				}
				archivedPath, err := team.ArchiveSessionMD(session.Workspace)
				if err != nil {
					stderrLog("%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
				} else {
					stderrLog("%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
				}
				// ArchiveSessionMD already removed session.json; no further cleanup needed.
			} else {
				if err := os.Remove(filepath.Join(session.Workspace, "session.json")); err != nil && !os.IsNotExist(err) {
					stderrLog("%s Failed to remove session file: %v\n", errStyle.Render("⚠"), err)
				}
			}
			if err := team.DeleteConversationHistory(session.Workspace); err != nil {
				stderrLog("%s Failed to delete conversation history: %v\n", errStyle.Render("⚠"), err)
			}
		}
		if err := team.CleanRunDirs(session.Workspace); err != nil {
			stderrLog("%s Failed to clean workspace: %v\n", errStyle.Render("⚠"), err)
		}
		if err := team.EnsureWorkspaceDirs(session.Workspace); err != nil {
			stderrLog("%s Failed to ensure workspace dirs: %v\n", errStyle.Render("⚠"), err)
		}
		sessionData = team.NewSession()
		if err := team.SaveSession(session.Workspace, sessionData); err != nil {
			stderrLog("%s Failed to save session: %v\n", errStyle.Render("⚠"), err)
		}
		if err := team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name)); err != nil {
			stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
		}
		stderrLog("%s Started new session\n", boldStyle.Render("→"))
	} else {
		sessionData = team.LoadSession(session.Workspace)
		existingMD := team.LoadSessionMD(session.Workspace)
		if existingMD != "" {
			stderrLog("%s Resuming session\n", boldStyle.Render("→"))
		} else if sessionData != nil && len(sessionData.Entries) > 0 {
			md := team.GenerateSessionMD(sessionData, session.Config.Name)
			if err := team.SaveSessionMD(session.Workspace, md); err != nil {
				stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
			}
			existingMD = md
			stderrLog("%s Resuming session (%d exchanges, since %s)\n",
				boldStyle.Render("→"),
				len(sessionData.Entries),
				sessionData.CreatedAt,
			)
		} else {
			if sessionData == nil {
				sessionData = team.NewSession()
			}
			if err := team.SaveSession(session.Workspace, sessionData); err != nil {
				stderrLog("%s Failed to save session: %v\n", errStyle.Render("⚠"), err)
			}
			if err := team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name)); err != nil {
				stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
			}
			stderrLog("%s Starting new session\n", boldStyle.Render("→"))
		}

		if showHistory && existingMD != "" {
			lines := strings.SplitN(existingMD, "\n", 30)
			preview := strings.Join(lines, "\n")
			stderrLog("\n%s\n%s\n\n",
				boldStyle.Render("─── Previous Session ───"),
				dimStyle.Render(preview),
			)
		}
	}

	stderrLog("%s %s\n", boldStyle.Render("Team:"), session.Config.Name)
	stderrLog("%s ", boldStyle.Render("Agents:"))
	var agentDisplayNames []string
	for _, def := range sortedAgents(session.Agents) {
		roleLabel := def.Role
		agentDisplayNames = append(agentDisplayNames, fmt.Sprintf("%s (%s)", agentStyle.Render(def.Name), dimStyle.Render(roleLabel)))
	}
	stderrLog("%s\n", strings.Join(agentDisplayNames, ", "))

	var mcpManager *mcp.MCPToolManager
	cfg := config.LoadConfig()

	// Check if any agent has mcp-tools defined
	hasMCPTools := false
	for _, def := range session.Agents {
		if len(def.MCPTools) > 0 {
			hasMCPTools = true
			break
		}
	}

	// Create manager if there are external MCP servers or agent mcp-tools
	if len(session.MCPServers) > 0 || hasMCPTools {
		globalShell := cfg.Shell
		if globalShell == "" {
			globalShell = "bash"
		}
		teamShell := session.Config.Shell
		if teamShell == "" {
			teamShell = globalShell
		}
		mcpManager = mcp.NewMCPToolManager(globalShell, teamShell)

		if len(session.MCPServers) > 0 {
			stderrLog("%s Loading MCP servers...\n", stepStyle.Render("⟳"))
			if err := mcpManager.LoadTools(ctx, session.MCPServers); err != nil {
				stderrLog("%s MCP loading failed: %v\n", errStyle.Render("⚠"), err)
			} else {
				stderrLog("%s MCP tools: %d loaded\n", doneStyle.Render("✓"), len(mcpManager.GetTools()))
			}
		}
	}

	var memStore *memory.MemoryStore
	if memoryEnabled && !tempWorkspace {
		ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedProviderURL)
		embedModel := config.ResolveEmbeddingModel(memoryModel)
		projectDir, _ := os.Getwd()
		var err error
		memStore, err = memory.NewMemoryStore(projectDir, ollamaAPIURL, embedModel)
		if err != nil {
			stderrLog("%s Memory store directory creation failed: %v\n", errStyle.Render("⚠"), err)
		}
		stderrLog("%s Memory: enabled (model: %s)\n", doneStyle.Render("✓"), embedModel)
	} else if !memoryEnabled {
		stderrLog("%s Memory: disabled\n", dimStyle.Render("○"))
	}

	resolvedModelList := cfg.ResolveModelList(session.Config.ModelList)
	resolvedSidecarModel := cfg.ResolveSidecarModel(session.Config.SidecarModel)
	resolvedGuardModel := cfg.ResolveGuardModel(session.Config.GuardModel, session.Config.SidecarModel)
	resolvedMaxConcurrent := cfg.ResolveMaxConcurrent(session.Config.MaxConcurrent)
	if resolvedMaxConcurrent <= 0 {
		resolvedMaxConcurrent = 8
	}
	session.Config.Generation.Model = cfg.ResolveModel(session.Config.Generation.Model)

	allowedPaths := buildAllowedPaths(session, registry, cfg)

	hookRegistry := hooks.NewHookRegistry()
	if configHooks := cfg.GetHooks(); len(configHooks) > 0 {
		if err := hooks.RegisterShellHooks(hookRegistry, configHooks); err != nil {
			stderrLog("%s Invalid hooks config: %v\n", errStyle.Render("⚠"), err)
		} else {
			for k := range configHooks {
				stderrLog("%s Hook: %s\n", dimStyle.Render("◆"), k)
			}
		}
	}

	resolvedRestrictedPath := cfg.RestrictedPath
	if session.Config.RestrictedPath != "" {
		resolvedRestrictedPath = session.Config.RestrictedPath
	}
	if rbashMode && resolvedRestrictedPath == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			rbashBin := filepath.Join(home, ".rbash-bin")
			if fi, err := os.Stat(rbashBin); err == nil && fi.IsDir() {
				resolvedRestrictedPath = rbashBin
			}
		}
	}

	resolvedNoNet := noNet || cfg.NoNet || session.Config.NoNet
	resolvedForceMCP := forceMCP || cfg.ForceMCP || session.Config.ForceMCP

	coordinator, err := team.NewCoordinator(session, resolvedProviderURL, resolvedProviderAPIKey, mcpManager, memStore, resolvedModelList, resolvedSidecarModel, resolvedGuardModel, resolvedMaxConcurrent, verbose, think, direnv, allowedPaths, pathConsent, hookRegistry, rbashMode, resolvedRestrictedPath, resolvedNoNet, resolvedForceMCP, forcedSkills, planMode, autoSkillsMode)
	if err != nil {
		return nil, fmt.Errorf("failed to create coordinator: %w", err)
	}
	coordinator.SetSessionData(sessionData)
	applyUnattendedAndBudget(coordinator, session)

	if memStore != nil && len(oldSessionEntries) > 0 {
		var summarizeFn memory.SummarizeFunc
		if s := coordinator.Sidecar(); s != nil {
			summarizeFn = s.Summarize
		}
		if err := memory.ArchiveSessionSummary(ctx, memStore, oldSessionEntries, session.Config.Name, summarizeFn); err != nil {
			stderrLog("%s Failed to archive session to memory: %v\n", errStyle.Render("⚠"), err)
		}
	}

	if len(session.Skills) > 0 {
		var skillNames []string
		for _, s := range session.Skills {
			skillNames = append(skillNames, s.Name)
		}
		stderrLog("%s %s\n", boldStyle.Render("Skills:"), strings.Join(skillNames, ", "))
	}

	if len(resolvedModelList) > 0 {
		var modelIDs []string
		for _, m := range resolvedModelList {
			modelIDs = append(modelIDs, m.ID)
		}
		stderrLog("%s %s\n", boldStyle.Render("Models:"), strings.Join(modelIDs, ", "))
	}

	if resolvedSidecarModel != "" {
		stderrLog("%s %s\n", boldStyle.Render("Sidecar:"), resolvedSidecarModel)
	}
	if resolvedGuardModel != "" {
		stderrLog("%s %s\n", boldStyle.Render("Guard:"), resolvedGuardModel)
	}
	if resolvedMaxConcurrent != 8 {
		stderrLog("%s %d\n", boldStyle.Render("Max concurrent:"), resolvedMaxConcurrent)
	}

	resolvedNotify := cfg.ResolveNotify(session.Config.Notify)
	var notifierInst *notify.Notifier
	if resolvedNotify.Enabled() {
		notifierInst = notify.NewNotifier(resolvedNotify, os.Stderr)
		if resolvedNotify.OSC {
			stderrLog("%s %s\n", dimStyle.Render("◆"), "Notify: OSC enabled")
		}
		if resolvedNotify.Command != "" {
			stderrLog("%s %s %s\n", dimStyle.Render("◆"), "Notify:", resolvedNotify.Command)
		}
		// Push a notification when an agent needs human input but none is
		// available (unattended) so an operator can follow up out-of-band.
		tools.SetOnNeedsHuman(func(question string) {
			notifierInst.Notify("needs_human", "", question, "")
		})
	}

	return &teamContext{
		session:     session,
		coordinator: coordinator,
		sessionData: sessionData,
		notifier:    notifierInst,
	}, nil
}

// loadDefaultTeam builds an in-memory default team (coordinator + Helper)
// without consulting the .agent-teams/ registry. It mirrors loadTeamByName's
// post-load setup (workspace, session lifecycle, coordinator) so the rest
// of the CLI works identically.
func loadDefaultTeam(ctx context.Context, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, vars map[string]string, forcedSkills []string, planMode bool, autoSkillsMode bool) (*teamContext, error) {
	teamName := "default"

	// Build a base workspace path the same way loadTeamByName does.
	// The default team reuses the parent workspace without a subdirectory
	// because there is no on-disk team folder to back it.
	var baseWorkspace string
	if workspace != "" {
		abs, err := filepath.Abs(workspace)
		if err != nil {
			return nil, fmt.Errorf("invalid workspace path: %w", err)
		}
		baseWorkspace = abs
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}
		baseWorkspace = filepath.Join(cwd, "workspace")
	}
	teamWorkspace := filepath.Join(baseWorkspace, teamName)
	if err := os.MkdirAll(teamWorkspace, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create workspace: %w", err)
	}

	session, err := team.LoadDefaultTeam(teamWorkspace, forcedSkills, helperTools)
	if err != nil {
		return nil, err
	}
	session.Workspace = teamWorkspace
	session.Config.WorkspaceDir = teamWorkspace

	// Apply CLI model overrides as the highest-priority model config layer,
	// then propagate to all agents (Helper, coordinator).
	applyCLIModelOverrides(&session.Config, currentModelOverrides())
	applyCLITimeoutOverrides(session, currentTimeoutOverrides())
	propagateTeamGenerationToAgents(session)

	resolvedProviderURL := config.ResolveProviderURL(defaultProviderURL, session.Config.ProviderURL, "")
	resolvedProviderAPIKey := config.ResolveProviderAPIKey(defaultProviderURL, session.Config.ProviderAPIKey)

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

	var sessionData *team.SessionData
	var oldSessionEntries []memory.SessionSummaryEntry
	if newSession {
		oldSession := team.LoadSession(session.Workspace)
		if oldSession != nil {
			for _, e := range oldSession.Entries {
				oldSessionEntries = append(oldSessionEntries, memory.SessionSummaryEntry{
					Role:      e.Role,
					Content:   e.Content,
					Timestamp: e.Timestamp,
				})
			}
		}

		existingMD := team.LoadSessionMD(session.Workspace)
		if existingMD != "" {
			stderrLog("%s Archiving previous session...\n", stepStyle.Render("⟳"))
			archivedPath, err := team.ArchiveSessionMD(session.Workspace)
			if err != nil {
				stderrLog("%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
			} else {
				stderrLog("%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
			}
		} else if team.HasSession(session.Workspace) {
			oldSession := team.LoadSession(session.Workspace)
			if oldSession != nil && len(oldSession.Entries) > 0 {
				stderrLog("%s Archiving previous session...\n", stepStyle.Render("⟳"))
				md := team.GenerateSessionMD(oldSession, session.Config.Name)
				if err := team.SaveSessionMD(session.Workspace, md); err != nil {
					stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
				}
				archivedPath, err := team.ArchiveSessionMD(session.Workspace)
				if err != nil {
					stderrLog("%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
				} else {
					stderrLog("%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
				}
			} else {
				if err := os.Remove(filepath.Join(session.Workspace, "session.json")); err != nil && !os.IsNotExist(err) {
					stderrLog("%s Failed to remove session file: %v\n", errStyle.Render("⚠"), err)
				}
			}
			if err := team.DeleteConversationHistory(session.Workspace); err != nil {
				stderrLog("%s Failed to delete conversation history: %v\n", errStyle.Render("⚠"), err)
			}
		}
		if err := team.CleanRunDirs(session.Workspace); err != nil {
			stderrLog("%s Failed to clean workspace: %v\n", errStyle.Render("⚠"), err)
		}
		if err := team.EnsureWorkspaceDirs(session.Workspace); err != nil {
			stderrLog("%s Failed to ensure workspace dirs: %v\n", errStyle.Render("⚠"), err)
		}
		sessionData = team.NewSession()
		if err := team.SaveSession(session.Workspace, sessionData); err != nil {
			stderrLog("%s Failed to save session: %v\n", errStyle.Render("⚠"), err)
		}
		if err := team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name)); err != nil {
			stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
		}
		stderrLog("%s Started new session\n", boldStyle.Render("→"))
	} else {
		sessionData = team.LoadSession(session.Workspace)
		existingMD := team.LoadSessionMD(session.Workspace)
		if existingMD != "" {
			stderrLog("%s Resuming session\n", boldStyle.Render("→"))
		} else if sessionData != nil && len(sessionData.Entries) > 0 {
			md := team.GenerateSessionMD(sessionData, session.Config.Name)
			if err := team.SaveSessionMD(session.Workspace, md); err != nil {
				stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
			}
			existingMD = md
			stderrLog("%s Resuming session (%d exchanges, since %s)\n",
				boldStyle.Render("→"),
				len(sessionData.Entries),
				sessionData.CreatedAt,
			)
		} else {
			if sessionData == nil {
				sessionData = team.NewSession()
			}
			if err := team.SaveSession(session.Workspace, sessionData); err != nil {
				stderrLog("%s Failed to save session: %v\n", errStyle.Render("⚠"), err)
			}
			if err := team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name)); err != nil {
				stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
			}
			stderrLog("%s Starting new session\n", boldStyle.Render("→"))
		}

		if showHistory && existingMD != "" {
			lines := strings.SplitN(existingMD, "\n", 30)
			preview := strings.Join(lines, "\n")
			stderrLog("\n%s\n%s\n\n",
				boldStyle.Render("─── Previous Session ───"),
				dimStyle.Render(preview),
			)
		}
	}

	stderrLog("%s %s\n", boldStyle.Render("Team:"), session.Config.Name)
	stderrLog("%s ", boldStyle.Render("Agents:"))
	var agentDisplayNames []string
	for _, def := range sortedAgents(session.Agents) {
		roleLabel := def.Role
		agentDisplayNames = append(agentDisplayNames, fmt.Sprintf("%s (%s)", agentStyle.Render(def.Name), dimStyle.Render(roleLabel)))
	}
	stderrLog("%s\n", strings.Join(agentDisplayNames, ", "))

	cfg := config.LoadConfig()

	// Default team has no MCP servers or mcp-tools, so skip MCP manager creation.
	var mcpManager *mcp.MCPToolManager

	var memStore *memory.MemoryStore
	if memoryEnabled && !tempWorkspace {
		ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedProviderURL)
		embedModel := config.ResolveEmbeddingModel(memoryModel)
		projectDir, _ := os.Getwd()
		var err error
		memStore, err = memory.NewMemoryStore(projectDir, ollamaAPIURL, embedModel)
		if err != nil {
			stderrLog("%s Memory store directory creation failed: %v\n", errStyle.Render("⚠"), err)
		}
		stderrLog("%s Memory: enabled (model: %s)\n", doneStyle.Render("✓"), embedModel)
	} else if !memoryEnabled {
		stderrLog("%s Memory: disabled\n", dimStyle.Render("○"))
	}

	resolvedModelList := cfg.ResolveModelList(session.Config.ModelList)
	resolvedSidecarModel := cfg.ResolveSidecarModel(session.Config.SidecarModel)
	resolvedGuardModel := cfg.ResolveGuardModel(session.Config.GuardModel, session.Config.SidecarModel)
	resolvedMaxConcurrent := cfg.ResolveMaxConcurrent(session.Config.MaxConcurrent)
	if resolvedMaxConcurrent <= 0 {
		resolvedMaxConcurrent = 8
	}
	session.Config.Generation.Model = cfg.ResolveModel(session.Config.Generation.Model)

	// For the default team there is no registry — only the workspace root
	// is allowed by default.
	var allowedPaths []string
	allowedPaths = append(allowedPaths, baseWorkspace)

	hookRegistry := hooks.NewHookRegistry()
	if configHooks := cfg.GetHooks(); len(configHooks) > 0 {
		if err := hooks.RegisterShellHooks(hookRegistry, configHooks); err != nil {
			stderrLog("%s Invalid hooks config: %v\n", errStyle.Render("⚠"), err)
		} else {
			for k := range configHooks {
				stderrLog("%s Hook: %s\n", dimStyle.Render("◆"), k)
			}
		}
	}

	resolvedRestrictedPath := cfg.RestrictedPath
	if session.Config.RestrictedPath != "" {
		resolvedRestrictedPath = session.Config.RestrictedPath
	}
	if rbashMode && resolvedRestrictedPath == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			rbashBin := filepath.Join(home, ".rbash-bin")
			if fi, err := os.Stat(rbashBin); err == nil && fi.IsDir() {
				resolvedRestrictedPath = rbashBin
			}
		}
	}

	resolvedNoNet := noNet || cfg.NoNet || session.Config.NoNet
	resolvedForceMCP := forceMCP || cfg.ForceMCP || session.Config.ForceMCP

	coordinator, err := team.NewCoordinator(session, resolvedProviderURL, resolvedProviderAPIKey, mcpManager, memStore, resolvedModelList, resolvedSidecarModel, resolvedGuardModel, resolvedMaxConcurrent, verbose, think, direnv, allowedPaths, pathConsent, hookRegistry, rbashMode, resolvedRestrictedPath, resolvedNoNet, resolvedForceMCP, forcedSkills, planMode, autoSkillsMode)
	if err != nil {
		return nil, fmt.Errorf("failed to create coordinator: %w", err)
	}
	coordinator.SetSessionData(sessionData)
	applyUnattendedAndBudget(coordinator, session)

	if memStore != nil && len(oldSessionEntries) > 0 {
		var summarizeFn memory.SummarizeFunc
		if s := coordinator.Sidecar(); s != nil {
			summarizeFn = s.Summarize
		}
		if err := memory.ArchiveSessionSummary(ctx, memStore, oldSessionEntries, session.Config.Name, summarizeFn); err != nil {
			stderrLog("%s Failed to archive session to memory: %v\n", errStyle.Render("⚠"), err)
		}
	}

	if len(session.Skills) > 0 {
		var skillNames []string
		for _, s := range session.Skills {
			skillNames = append(skillNames, s.Name)
		}
		stderrLog("%s %s\n", boldStyle.Render("Skills:"), strings.Join(skillNames, ", "))
	}

	if len(resolvedModelList) > 0 {
		var modelIDs []string
		for _, m := range resolvedModelList {
			modelIDs = append(modelIDs, m.ID)
		}
		stderrLog("%s %s\n", boldStyle.Render("Models:"), strings.Join(modelIDs, ", "))
	}

	if resolvedSidecarModel != "" {
		stderrLog("%s %s\n", boldStyle.Render("Sidecar:"), resolvedSidecarModel)
	}
	if resolvedGuardModel != "" {
		stderrLog("%s %s\n", boldStyle.Render("Guard:"), resolvedGuardModel)
	}
	if resolvedMaxConcurrent != 8 {
		stderrLog("%s %d\n", boldStyle.Render("Max concurrent:"), resolvedMaxConcurrent)
	}

	resolvedNotify := cfg.ResolveNotify(session.Config.Notify)
	var notifierInst *notify.Notifier
	if resolvedNotify.Enabled() {
		notifierInst = notify.NewNotifier(resolvedNotify, os.Stderr)
		if resolvedNotify.OSC {
			stderrLog("%s %s\n", dimStyle.Render("◆"), "Notify: OSC enabled")
		}
		if resolvedNotify.Command != "" {
			stderrLog("%s %s %s\n", dimStyle.Render("◆"), "Notify:", resolvedNotify.Command)
		}
		// Push a notification when an agent needs human input but none is
		// available (unattended) so an operator can follow up out-of-band.
		tools.SetOnNeedsHuman(func(question string) {
			notifierInst.Notify("needs_human", "", question, "")
		})
	}

	return &teamContext{
		session:     session,
		coordinator: coordinator,
		sessionData: sessionData,
		notifier:    notifierInst,
	}, nil
}

func buildAllowedPaths(session *team.TeamSession, registry *team.TeamRegistry, cfg *config.Config) []string {
	seen := make(map[string]bool)
	var paths []string

	projectDir, _ := os.Getwd()
	if projectDir != "" && !seen[projectDir] {
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

	skillDirs := []string{
		filepath.Join(session.Dir, "skills"),
		filepath.Join(session.Dir, ".agents", "skills"),
		filepath.Join(os.Getenv("HOME"), ".agents", "skills"),
	}
	for _, teamDir := range registry.TeamDirs() {
		skillDirs = append(skillDirs, filepath.Join(teamDir, "skills"))
	}
	migrateLegacyDrafts(skillDirs)
	for _, dir := range skillDirs {
		abs, err := filepath.Abs(dir)
		if err == nil && !seen[abs] {
			seen[abs] = true
			paths = append(paths, abs)
		}
	}

	for _, p := range cfg.AllowedPaths {
		expanded := os.ExpandEnv(p)
		if strings.HasPrefix(expanded, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				expanded = filepath.Join(home, expanded[1:])
			}
		}
		abs, err := filepath.Abs(expanded)
		if err == nil && !seen[abs] {
			seen[abs] = true
			paths = append(paths, abs)
		}
	}

	for _, p := range session.Config.AllowedPaths {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	return paths
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
			fmt.Fprintf(os.Stderr, "%s", boldStyle.Render("Execute? [Y/n]: "))
			if _, err := fmt.Scanln(&answer); err != nil {
				if err == io.EOF {
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
	for {
		select {
		case <-injector.wrapUpCh:
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
		case prompt, ok := <-injector.ch:
			if !ok {
				return result, nil
			}
			stderrLog("\n%s Injecting additional prompt...\n\n", boldStyle.Render("↩"))

			contResult, err := tc.coordinator.ContinueWithPrompt(ctx, prompt)
			if err != nil {
				return result, err
			}
			result += "\n\n---\n\n" + contResult
		default:
			return result, nil
		}
	}
}

func executeSegments(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry, defaultProviderURL string, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string) (string, error) {
	var results []string
	currentTeamName := ""

	for i, seg := range segments {
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

			if seg.Content == "" {
				continue
			}

			disp := newCoordDisplay(tc)

			stderrLog("\n%s Starting team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(teamName))

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, seg.Content)
			activeCoord.Clear()
			disp.stopTimer()

			if err != nil {
				if ctx.Err() == context.Canceled {
					_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
				_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q failed: %w", teamName, err)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				if ctx.Err() == context.Canceled {
					_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
				_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q continuation failed: %w", teamName, err)
			}

			disp.finalizeTasks()
			stderrLog("\n%s Team %s coordination complete.\n", doneStyle.Render("✓"), teamStyle.Render(teamName))
			results = append(results, fmt.Sprintf("## Team: %s\n%s", teamName, result))

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

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			directResult, err := tc.coordinator.RunDirectAgent(ctx, seg.Name, seg.Content)
			activeCoord.Clear()
			disp2.stopTimer()

			if err != nil {
				if ctx.Err() == context.Canceled {
					_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				return strings.Join(results, "\n\n"), fmt.Errorf("direct agent @%s failed: %w", seg.Name, err)
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
			} else {
				synthesisPrompt := fmt.Sprintf("A user directly asked @%s to do the following task:\n\n%s\n\nHere is what %s produced:\n\n---\n%s\n---\n\nPlease synthesize this into a final, well-organized answer for the user.",
					seg.Name, seg.Content, seg.Name, directResult.Output)
				activeCoord.Set(tc.coordinator)
				if injector.IsWrapUpRequested() {
					tc.coordinator.SetWrapUp()
				}
				synthResult, err := tc.coordinator.Run(ctx, synthesisPrompt)
				activeCoord.Clear()
				if err != nil {
					if ctx.Err() == context.Canceled {
						_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
						stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
						return "", errInterrupted{}
					}
					_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
					_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
					return strings.Join(results, "\n\n"), fmt.Errorf("synthesis for @%s failed: %w", seg.Name, err)
				}

				synthResult, err = runWithInjection(ctx, tc, synthResult, injector)
				if err != nil {
					if ctx.Err() == context.Canceled {
						_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
						stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
						return "", errInterrupted{}
					}
					_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
					_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
					return strings.Join(results, "\n\n"), fmt.Errorf("synthesis continuation for @%s failed: %w", seg.Name, err)
				}

				disp2.finalizeTasks()
				results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, synthResult))
			}

		case team.SegmentText:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("text segment with no active team — specify a team with --agent-team or @team-name first")
			}

			tc := loadedTeams[currentTeamName]

			disp3 := newCoordDisplay(tc)

			stderrLog("\n%s Team %s processing...\n\n", boldStyle.Render("→"), teamStyle.Render(currentTeamName))

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, seg.Content)
			activeCoord.Clear()
			disp3.stopTimer()

			if err != nil {
				if ctx.Err() == context.Canceled {
					_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
				_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q failed: %w", currentTeamName, err)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				if ctx.Err() == context.Canceled {
					_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
				_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q continuation failed: %w", currentTeamName, err)
			}

			disp3.finalizeTasks()
			results = append(results, fmt.Sprintf("## Team: %s\n%s", currentTeamName, result))
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
	var input string
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	fmt.Fprintf(os.Stderr, "%s ", boldStyle.Render(">"))
	_, _ = fmt.Fscanln(os.Stdin, &input)
	prompt := strings.TrimSpace(input)
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		return "", fmt.Errorf("no prompt provided")
	}
	return prompt, nil
}

func askUserForTeamFallback(teams []string) string {
	if len(teams) == 0 {
		return ""
	}
	sort.Strings(teams)
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Select Team ───"))
	for i, t := range teams {
		fmt.Fprintf(os.Stderr, "  %s %s\n", dimStyle.Render(fmt.Sprintf("%d.", i+1)), teamStyle.Render(t))
	}
	fmt.Fprintf(os.Stderr, "\n%s ", boldStyle.Render("Team name or number:>"))
	var input string
	_, _ = fmt.Fscanln(os.Stdin, &input)
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	var num int
	if n, err := fmt.Sscanf(input, "%d", &num); err == nil && n == 1 && num >= 1 && num <= len(teams) {
		return teams[num-1]
	}
	lower := strings.ToLower(input)
	for _, t := range teams {
		if strings.ToLower(t) == lower {
			return t
		}
	}
	return input
}

func readStdin() string {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
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
	if len(teams) == 0 {
		return "", nil
	}
	sort.Strings(teams)
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Select Team ───"))
	for i, t := range teams {
		fmt.Fprintf(os.Stderr, "  %s %s\n", dimStyle.Render(fmt.Sprintf("%d.", i+1)), teamStyle.Render(t))
	}
	input, err := pr.ReadLine(boldStyle.Render("Team name or number:>"))
	if err != nil {
		if err == ergoreadline.ErrInterrupt || err == io.EOF {
			fmt.Fprintf(os.Stderr, "\n")
			return "", errInterrupted{}
		}
		return "", fmt.Errorf("input error: %w", err)
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nil
	}
	var num int
	if n, err := fmt.Sscanf(input, "%d", &num); err == nil && n == 1 && num >= 1 && num <= len(teams) {
		return teams[num-1], nil
	}
	lower := strings.ToLower(input)
	for _, t := range teams {
		if strings.ToLower(t) == lower {
			return t, nil
		}
	}
	return input, nil
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
