package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/readline"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

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
	tuipkg.SetSpinnerEnabled(!opts.noSpinner)
	tuipkg.SetCompactMode(opts.tuiCompact)

	pr, err := readline.NewPromptReader(defaultHistoryPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s Readline initialization failed, falling back to basic input: %v\n", errStyle.Render("⚠"), err)
		pr = nil
	}
	globalPromptReader.Store(pr)
	if opts.tempWorkspace {
		tmpDir, err := os.MkdirTemp("", "hufu-*")
		if err != nil {
			return fmt.Errorf("failed to create temp directory: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmpDir) }()
		opts.workspace = filepath.Join(tmpDir, "workspace")
		fmt.Fprintf(os.Stderr, "%s Temp workspace: %s\n", stepStyle.Render("⟳"), opts.workspace)
	}

	vars, err := team.ResolveVars(opts.varFiles, opts.varFlags)
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

	if opts.archiveMemory && prompt == "" && opts.templateName == "" {
		registry := team.NewTeamRegistry(resolveSearchPaths())
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

	searchPaths := resolveSearchPaths()
	registry := team.NewTeamRegistry(searchPaths)
	if !opts.defaultTeam {
		if err := registry.Discover(); err != nil {
			return fmt.Errorf("failed to discover teams: %w", err)
		}

		if registry.TeamCount() == 0 {
			return offerFirstTimeWizard(searchPaths)
		}

		fmt.Fprintf(os.Stderr, "%s Available teams: %s\n", boldStyle.Render("Teams:"), strings.Join(registry.ListTeams(), ", "))
	}

	if opts.archiveMemory && prompt == "" && !opts.newSession {
		return runArchiveMemory(context.Background(), registry, vars)
	}

	if opts.fixQuestion != "" {
		fixPC := newPathConsent()
		return runFixMode(ctx, prompt, opts.fixQuestion, registry, opts.providerURL, opts.providerAPIKey, fixPC, vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	}

	initialTeam := strings.ToLower(opts.agentTeamName)
	if opts.defaultTeam {
		initialTeam = "default"
	}

	routeDecision := maybeAutoSelectTeam(ctx, prompt, initialTeam, registry)
	initialTeam = routeDecision.Team

	initialSegments, err := resolveInitialSegments(prompt, initialTeam, registry, pr)
	if err != nil {
		return err
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

	if opts.dryRun {
		return executeDryRun(ctx, segments, prompt, loadedTeams)
	}

	return executeAndReport(ctx, cancel, prompt, originalPrompt, segments, registry, loadedTeams, injector, activeCoord, pathConsent, vars, routeDecision)
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

		if opts.isChatTUI {
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
		if opts.isChatTUI && turn == 0 {
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

// maybeAutoSelectTeam implements execution route selection and --auto-team.
// It uses ExecutionRouter (evaluating deterministic signals and sidecar classifier)
// to pick the route (fast vs team) and appropriate team, and returns the full
// RouteDecision so the caller can thread the chosen route into execution.
func maybeAutoSelectTeam(ctx context.Context, prompt, initialTeam string, registry *team.TeamRegistry) RouteDecision {
	router := NewExecutionRouter(registry, buildSelectionSidecar(ctx))
	decision := router.Route(ctx, prompt, initialTeam)

	if decision.Team != "" && (opts.autoTeam || opts.routeMode != "auto" || opts.defaultTeam || initialTeam != "") {
		if opts.verbose || opts.autoTeam || opts.routeMode != "auto" {
			reasonsStr := strings.Join(decision.Reasons, "; ")
			fmt.Fprintf(os.Stderr, "%s Route decision: %s (team: %s, confidence: %.2f) [%s]\n",
				boldStyle.Render("→"), teamStyle.Render(string(decision.Route)), teamStyle.Render(decision.Team), decision.Confidence, reasonsStr)
		}
		return RouteDecision{Route: decision.Route, Team: strings.ToLower(decision.Team), Confidence: decision.Confidence, Reasons: decision.Reasons}
	}

	if !opts.autoTeam || initialTeam != "" || team.HasAtName(prompt) || (registry != nil && registry.TeamCount() == 0) {
		return RouteDecision{Route: decision.Route, Team: initialTeam, Confidence: decision.Confidence, Reasons: decision.Reasons}
	}

	if decision.Team != "" {
		fmt.Fprintf(os.Stderr, "%s Auto-selected route %s (team: %s) [%s]\n",
			boldStyle.Render("→"), teamStyle.Render(string(decision.Route)), teamStyle.Render(decision.Team), strings.Join(decision.Reasons, "; "))
		return RouteDecision{Route: decision.Route, Team: strings.ToLower(decision.Team), Confidence: decision.Confidence, Reasons: decision.Reasons}
	}

	picked, method := autoSelectTeam(ctx, prompt, registry)
	if picked == "" {
		fmt.Fprintf(os.Stderr, "%s --auto-team could not confidently pick a team; falling back to selection.\n", dimStyle.Render("·"))
		return RouteDecision{Route: RouteTeam, Team: initialTeam, Confidence: 0, Reasons: []string{"auto-team fallback to manual selection"}}
	}
	fmt.Fprintf(os.Stderr, "%s Auto-selected team %s (%s)\n", boldStyle.Render("→"), teamStyle.Render(picked), method)
	return RouteDecision{Route: RouteTeam, Team: strings.ToLower(picked), Confidence: 0.6, Reasons: []string{"auto-team keyword match: " + method}}
}

// resolveInitialSegments parses the prompt into team/agent segments. When the
// parse fails only because no team was named or found, it asks the user to
// pick one and retries once.
func resolveInitialSegments(prompt, initialTeam string, registry *team.TeamRegistry, pr *readline.PromptReader) ([]team.PromptSegment, error) {
	segments, err := team.ParsePromptWithLazyAgents(prompt, registry, initialTeam)
	if err == nil {
		return segments, nil
	}

	teamNotFound := initialTeam == "" && !team.HasAtName(prompt)
	if !teamNotFound {
		for _, prefix := range []string{"no team found", "no team specified"} {
			if strings.HasPrefix(err.Error(), prefix) {
				teamNotFound = true
				break
			}
		}
	}
	if !teamNotFound {
		return nil, err
	}

	var chosen string
	if pr != nil {
		chosen, err = askUserForTeam(registry.ListTeams(), pr)
		if err != nil {
			return nil, err
		}
	} else {
		chosen = askUserForTeamFallback(registry.ListTeams())
	}
	if chosen == "" {
		fmt.Fprintf(os.Stderr, "%s No team selected. Pass --agent-team <name>, use @<team> in the prompt, or run 'hufu init <name>' to create one.\n", errStyle.Render("✗"))
		return nil, fmt.Errorf("no team selected")
	}
	return team.ParsePromptWithLazyAgents(prompt, registry, chosen)
}

// validateRunFlags checks for invalid flag combinations and adjusts
// flags that conflict with --unattended. Returns an error if the
// command cannot run, otherwise nil (with side effects on stepsMode
// and tuiMode as needed).
func validateRunFlags() error {
	switch opts.outputFormat {
	case "", "text", "json":
	default:
		return fmt.Errorf("invalid --output %q: use 'text' or 'json'", opts.outputFormat)
	}
	switch opts.displayMode {
	case "auto", "terminal", "plain":
	default:
		return fmt.Errorf("invalid --display-mode %q: use 'auto', 'terminal', or 'plain'", opts.displayMode)
	}
	if opts.eventFormat != "text" && opts.eventFormat != "jsonl" {
		return fmt.Errorf("invalid --event-format %q: use 'text' or 'jsonl'", opts.eventFormat)
	}
	// JSON output implies quiet: stdout must carry only the JSON document.
	if opts.outputFormat == "json" {
		opts.quietMode = true
	}
	// Unattended mode is inherently non-interactive: silently disable the
	// human-in-the-loop features rather than erroring, so the same command
	// line works whether or not a human is present.
	if opts.unattended {
		if opts.stepsMode {
			fmt.Fprintln(os.Stderr, "note: --unattended disables --steps (no human to confirm)")
			opts.stepsMode = false
		}
		if opts.tuiMode {
			fmt.Fprintln(os.Stderr, "note: --unattended disables --tui (no human to watch)")
			opts.tuiMode = false
		}
	}
	if opts.stepsMode && opts.tuiMode {
		return fmt.Errorf("cannot use --steps (step confirmation) with --tui (TUI mode); remove one flag")
	}
	if opts.defaultTeam && opts.agentTeamName != "" {
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
	if prompt == "" && opts.templateName == "" {
		prompt = readStdin()
	}
	if opts.templateName != "" {
		templated, err := loadPromptTemplate(opts.templateName, vars)
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
	if opts.projectContext || cfg.ProjectContext {
		prompt = injectProjectContext(prompt)
	}
	return prompt, nil
}
