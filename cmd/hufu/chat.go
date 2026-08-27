package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	ergoreadline "github.com/ergochat/readline"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/readline"
	"github.com/kjelly/hufu/internal/team"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

var replCmd = &cobra.Command{
	Use:     "chat",
	Aliases: []string{"repl"},
	Short:   "Interactive REPL that keeps one team loaded across prompts",
	Long: `Start an interactive session with a single team. The team, its MCP servers
and memory are loaded once and reused for every prompt, so iterating is fast and
the coordinator keeps full conversation context between turns.

Pick the team with --agent-team, --default, or interactively. Commands inside the
REPL: /exit (or /quit) to leave, /reset to clear conversation context.

Examples:
  hufu chat --agent-team dev-team
  hufu chat --default --helper-tools bash`,
	RunE: runChat,
}

func init() {
	// Bind the subset of root flags that make sense for an interactive chat
	// session to the same global vars the root command uses.
	f := replCmd.Flags()
	f.StringVar(&opts.agentTeamName, "agent-team", "", "Agent team name to chat with")
	f.StringVar(&opts.agentTeamSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams")
	f.BoolVar(&opts.defaultTeam, "default", false, "Use the built-in default team (coordinator + Helper)")
	f.StringVar(&opts.helperTools, "helper-tools", "", "Extra tools for the default Helper (e.g. bash,sudo)")
	f.StringSliceVar(&opts.allowPaths, "allow-path", nil, "Additional filesystem paths to allow for the active team")
	f.BoolVar(&opts.autoApprove, "auto-approve", false, "Automatically choose clearly safe ask_user options; dangerous or ambiguous choices still prompt the user")
	f.StringVar(&opts.providerURL, "provider-url", "", "Provider API base URL")
	f.StringVar(&opts.providerAPIKey, "provider-api-key", "", "Provider API key")
	f.StringVar(&opts.modelOverride, "model", "", "Override default model (e.g. local-model or lemonade/model)")
	f.StringVarP(&opts.workspace, "workspace", "w", "", "Workspace directory")
	f.BoolVarP(&opts.newSession, "new", "n", false, "Archive old session and start fresh")
	f.BoolVarP(&opts.verbose, "verbose", "v", false, "Show full agent text output in real-time")
	f.BoolVar(&opts.memoryEnabled, "memory", false, "Enable long-term memory (RAG with vector search)")
	f.StringArrayVar(&opts.forcedSkills, "skill", nil, "Force-load specific skills (repeatable)")
	f.StringArrayVar(&opts.varFlags, "var", nil, "Set template variable key=value (repeatable)")
	f.StringArrayVar(&opts.varFiles, "var-file", nil, "Read template variables from a file (repeatable)")
	f.BoolVar(&opts.planMode, "plan", false, "Force plan-first mode")
	f.BoolVar(&opts.autoSkills, "auto-skills", false, "Enable automatic skill detection")
	f.BoolVar(&opts.projectContext, "project-context", false, "Inject Git Status and Project Directory Structure into prompt context")
	f.BoolVar(&opts.think, "think", false, "Show coordinator decision reasoning")
	f.BoolVar(&opts.tuiMode, "tui", false, "Show a Bubble Tea TUI for real-time task tracking")
	f.Int64Var(&opts.timeoutOverride, "timeout", 0, "Override agent/coordinator timeout in seconds")
	f.IntVar(&opts.maxRoundsOverride, "max-rounds", 0, "Override team.yaml max-rounds. 0 = use team default.")
	f.IntVar(&opts.maxConcurrentOverride, "max-concurrent", 0, "Override team.yaml max-concurrent. 0 = use team default.")
	f.IntVar(&opts.maxStepsOverride, "max-steps", 0, "Override team.yaml max-steps. 0 = use team/agent default.")
	f.IntVar(&opts.compactionMaxHistoryMessages, "compaction-max-history-messages", 0, "Override compaction max history messages; zero keeps the safety default")
	f.IntVar(&opts.compactionRetainHistoryMessages, "compaction-retain-history-messages", 0, "Override compaction retained history messages; zero keeps the safety default")
	f.IntVar(&opts.compactionVerifiedHistoryTargetTokens, "compaction-verified-history-target-tokens", 0, "Override verified history target tokens; zero keeps the safety default")
	f.IntVar(&opts.compactionToolOutputMaxBytes, "compaction-tool-output-max-bytes", 0, "Override normalized tool output byte cap; zero keeps the safety default")
	f.IntVar(&opts.compactionToolOutputMaxRunes, "compaction-tool-output-max-runes", 0, "Override normalized tool output rune cap; zero keeps the safety default")
	f.IntVar(&opts.compactionToolOutputMaxTokens, "compaction-tool-output-max-tokens", 0, "Override normalized tool output token cap; zero keeps the safety default")
	f.IntVar(&opts.compactionDiagnosticMaxLines, "compaction-diagnostic-max-lines", 0, "Override preserved diagnostic line cap; zero keeps the safety default")
	f.IntVar(&opts.compactionDiagnosticMaxTokens, "compaction-diagnostic-max-tokens", 0, "Override preserved diagnostic token cap; zero keeps the safety default")
}

func runChat(cmd *cobra.Command, args []string) error {
	if err := applyProfile(cmd); err != nil {
		return err
	}
	configureOutputRendering()
	if opts.defaultTeam && opts.agentTeamName != "" {
		return fmt.Errorf("cannot use --default with --agent-team; pick one")
	}

	pr, err := readline.NewPromptReader(defaultHistoryPath())
	if err != nil {
		pr = nil
	}
	globalPromptReader.Store(pr)
	defer func() {
		if pr != nil {
			_ = pr.Close()
		}
	}()

	vars, err := buildVars()
	if err != nil {
		return err
	}

	pathConsent := newPathConsent()
	rootCtx := context.Background()

	// Resolve which team to load.
	var tc *teamContext
	var teamName string
	var registry *team.TeamRegistry
	if opts.defaultTeam {
		teamName = "default"
		tc, err = loadDefaultTeam(rootCtx, opts.providerURL, opts.providerAPIKey, pathConsent, vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	} else {
		teamName, registry, err = pickChatTeam(pr)
		if err != nil {
			return err
		}
		tc, err = loadTeamByName(rootCtx, teamName, registry, opts.providerURL, opts.providerAPIKey, pathConsent, vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	}
	if err != nil {
		return fmt.Errorf("failed to load team %q: %w", teamName, err)
	}

	// Recreate the prompt reader with a team-aware tab completer. The
	// first reader (line 71) had no completer because we did not yet
	// know the team. Recreating is cheaper than supporting mutable
	// completer state in the underlying readline library.
	if pr != nil {
		_ = pr.Close()
		var agentNames []string
		for name := range tc.session.Agents {
			agentNames = append(agentNames, name)
		}
		newPr, perr := readline.NewPromptReaderWithCompleter(
			defaultHistoryPath(),
			newChatCompleter(teamName, []string{teamName}, agentNames),
		)
		if perr == nil {
			pr = newPr
			globalPromptReader.Store(pr)
			defer func() { _ = pr.Close() }()
		}
	}

	if opts.tuiMode {
		opts.isChatTUI = true
		var teamInfo tuipkg.TeamInfo
		if registry != nil {
			teamInfo.AvailableTeams = registry.ListTeams()
		}
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
		teamInfo.MemoryEnabled = opts.memoryEnabled && !opts.tempWorkspace
		if teamInfo.MemoryEnabled {
			teamInfo.MemoryModel = config.ResolveEmbeddingModel(opts.memoryModel)
		}
		teamInfo.SSHSessions = 0
		teamInfo.PTYEnabled = opts.enablePTYTerminal
		teamInfo.HufuBinary, _ = os.Executable()
		teamInfo.IsChat = true

		segments := []team.PromptSegment{
			{
				Type:    team.SegmentSwitchTeam,
				Name:    teamName,
				Content: "",
			},
		}

		injector := newPromptInjector(nil)
		activeCoord := new(atomic.Pointer[team.Coordinator])

		loadedTeams := map[string]*teamContext{
			teamName: tc,
		}

		turnCtx, cancel := signal.NotifyContext(rootCtx, os.Interrupt)
		defer cancel()

		_, runErr := runWithTUI(turnCtx, cancel, "", segments, registry, loadedTeams, injector, activeCoord, pathConsent, vars, teamInfo, RouteDecision{Route: RouteTeam, Team: teamName})
		return runErr
	}

	fmt.Fprintf(os.Stderr, "\n%s Chatting with team %s. Type %s to leave, %s to clear context.\n\n",
		boldStyle.Render("💬"), teamStyle.Render(teamName),
		dimStyle.Render("/exit"), dimStyle.Render("/reset"))
	turn := 0
	for {
		line, err := readChatLine(pr)
		if err != nil {
			if err == io.EOF || err == ergoreadline.ErrInterrupt {
				fmt.Fprintln(os.Stderr)
				return nil
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		command, commandArg := splitChatCommand(line)
		if command == "/team" && commandArg != "" {
			newTc, terr := switchChatTeamByName(rootCtx, commandArg, registry)
			if terr != nil {
				fmt.Fprintf(os.Stderr, "%s %v\n", errStyle.Render("✗"), terr)
				continue
			}
			tc, teamName, turn = newTc, newTc.teamName, 0
			fmt.Fprintf(os.Stderr, "%s Switched to team %s. Conversation context cleared.\n\n", doneStyle.Render("✓"), teamStyle.Render(teamName))
			continue
		}
		switch command {
		case "/exit", "/quit", ":q", "exit", "quit":
			return nil
		case "/reset":
			tc.coordinator.ResetConversation()
			turn = 0
			fmt.Fprintf(os.Stderr, "%s Conversation context cleared.\n\n", dimStyle.Render("·"))
			continue
		case "/help", "help", "?":
			fmt.Fprint(os.Stderr, chatHelpText(tc.teamName))
			continue
		case "/team":
			newTc, newName, terr := switchChatTeam(rootCtx, pr, tc, registry)
			if terr != nil {
				fmt.Fprintf(os.Stderr, "%s %v\n", errStyle.Render("✗"), terr)
				continue
			}
			if newTc == nil {
				continue
			}
			tc = newTc
			teamName = newName
			turn = 0
			fmt.Fprintf(os.Stderr, "%s Switched to team %s. Conversation context cleared.\n\n",
				doneStyle.Render("✓"), teamStyle.Render(teamName))
			continue
		case "/agents":
			printChatAgents(tc)
			continue
		case "/skills":
			printChatSkills(tc)
			continue
		case "/config":
			fmt.Fprintf(os.Stderr, "Team: %s\nWorkspace: %s\nModel: %s\n\n", tc.session.Config.Name, tc.session.Workspace, tc.session.Config.Generation.Model)
			continue
		case "/save":
			if commandArg == "" {
				fmt.Fprintln(os.Stderr, "usage: /save <path>")
				continue
			}
			if err := os.MkdirAll(filepath.Dir(commandArg), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "%s %v\n", errStyle.Render("✗"), err)
				continue
			}
			if err := os.WriteFile(commandArg, []byte(team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name)), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "%s %v\n", errStyle.Render("✗"), err)
				continue
			}
			fmt.Fprintf(os.Stderr, "%s Saved conversation to %s\n\n", doneStyle.Render("✓"), commandArg)
			continue
		}

		// Inject file/project contexts
		promptToRun, _ := injectFileContexts(line)
		cfg := config.LoadConfig()
		if opts.projectContext || cfg.ProjectContext || tc.session.Config.ProjectContext {
			promptToRun = injectProjectContext(promptToRun)
		}

		// Each turn is independently cancellable with Ctrl+C; cancelling a turn
		// returns to the prompt rather than exiting the REPL.
		turnCtx, cancel := signal.NotifyContext(rootCtx, os.Interrupt)
		disp := newCoordDisplay(tc)
		var result string
		if turn == 0 {
			result, err = tc.coordinator.Run(turnCtx, promptToRun)
		} else {
			result, err = tc.coordinator.ContinueWithPrompt(turnCtx, promptToRun)
		}
		disp.stopTimer()
		cancel()

		if err != nil {
			if turnCtx.Err() == context.Canceled {
				fmt.Fprintf(os.Stderr, "\n%s Turn cancelled.\n\n", errStyle.Render("⚠"))
				// A cancelled first turn still established history inside the
				// coordinator, so next turn must use ContinueWithPrompt.
				turn++
				continue
			}
			fmt.Fprintf(os.Stderr, "%s Error executing: %v\n\n", errStyle.Render("✗"), err)
			continue
		}

		disp.finalizeTasks()
		fmt.Println(result)
		fmt.Fprintln(os.Stderr)
		turn++

		if line != "" {
			go savePromptToHistory(context.Background(), line, opts.providerURL)
		}

		// Persist after every turn so the session survives a crash mid-chat.
		_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
		_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
	}
}

func splitChatCommand(line string) (string, string) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return "", ""
	}
	return strings.ToLower(parts[0]), strings.TrimSpace(strings.TrimPrefix(line, parts[0]))
}

func printChatAgents(tc *teamContext) {
	agents := sortedAgents(tc.session.Agents)
	for _, ag := range agents {
		fmt.Fprintf(os.Stderr, "  @%s [%s] %s\n", ag.Name, ag.Role, ag.Generation.Model)
	}
	fmt.Fprintln(os.Stderr)
}

func printChatSkills(tc *teamContext) {
	names := make([]string, 0, len(tc.session.Skills))
	for _, skill := range tc.session.Skills {
		names = append(names, skill.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "  (no skills loaded)")
	} else {
		for _, name := range names {
			fmt.Fprintf(os.Stderr, "  %s\n", name)
		}
	}
	fmt.Fprintln(os.Stderr)
}

// chatHelpText returns a multi-line help block shown by the /help command in
// the chat REPL.
func chatHelpText(teamName string) string {
	return fmt.Sprintf(`%s Chatting with team %s

Commands:
  /help, help, ?       show this help
  /reset               clear the conversation context (start fresh)
  /team [name]         switch teams (no name opens the picker)
  /agents              list current team agents
  /skills              list loaded skills
  /config              show current team settings
  /save <path>         export the current conversation
  /exit, /quit, :q     leave the chat

Prompt features:
  @<agent-name> ...    target a specific agent in the current team
  #filename            inject the file contents into the prompt
  Tab                  autocomplete @names and /commands

Tips:
  - Type your message and press Enter to send it
  - Each turn is independently cancellable with Ctrl+C
  - Use ↑/↓ to recall previous prompts
  - Run with --tui for a 6-column dashboard

`,
		boldStyle.Render("💬"), teamStyle.Render(teamName))
}

// switchChatTeam allows the user to switch to a different team mid-REPL.
// If the user types a team name, it switches. If they just press enter,
// they can pick from a menu.
func switchChatTeam(rootCtx context.Context, pr *readline.PromptReader, current *teamContext, registry *team.TeamRegistry) (*teamContext, string, error) {
	if registry == nil {
		return nil, "", fmt.Errorf("team switching not available with the default team (exit and re-run with --agent-team)")
	}
	var others []string
	for _, name := range registry.ListTeams() {
		if name != current.teamName {
			others = append(others, name)
		}
	}
	if len(others) == 0 {
		return nil, "", fmt.Errorf("no other teams available in %s", strings.Join(registry.SearchPaths(), ", "))
	}
	chosen, err := askUserForTeamWithPromptUI(others)
	if err != nil {
		if _, ok := err.(errInterrupted); ok {
			return nil, "", nil
		}
		return nil, "", err
	}
	if chosen == "" {
		return nil, "", nil
	}
	tc, err := loadTeamByName(rootCtx, chosen, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), buildVarsOrNil(), opts.forcedSkills, opts.planMode, opts.autoSkills)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load team %q: %w", chosen, err)
	}
	return tc, chosen, nil
}

func switchChatTeamByName(rootCtx context.Context, name string, registry *team.TeamRegistry) (*teamContext, error) {
	if registry == nil {
		return nil, fmt.Errorf("team switching not available with the default team")
	}
	name = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	if !registry.HasTeam(name) {
		return nil, fmt.Errorf("team %q not found. Available: %s", name, strings.Join(registry.ListTeams(), ", "))
	}
	tc, err := loadTeamByName(rootCtx, name, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), buildVarsOrNil(), opts.forcedSkills, opts.planMode, opts.autoSkills)
	if err != nil {
		return nil, fmt.Errorf("failed to load team %q: %w", name, err)
	}
	return tc, nil
}

// buildVarsOrNil returns the merged template variables, or nil on error.
// Unlike buildVars(), it does not error out so the REPL can stay alive.
func buildVarsOrNil() map[string]string {
	vars, _ := buildVars()
	return vars
}

// pickChatTeam discovers teams and resolves which one to chat with, from the
// --agent-team flag or an interactive menu.
func pickChatTeam(pr *readline.PromptReader) (string, *team.TeamRegistry, error) {
	searchPaths := resolveSearchPaths()
	registry := team.NewTeamRegistry(searchPaths)
	if err := registry.Discover(); err != nil {
		return "", nil, fmt.Errorf("failed to discover teams: %w", err)
	}
	if registry.TeamCount() == 0 {
		return "", nil, fmt.Errorf("no teams found in %s — use --default or run `hufu init <team>`", strings.Join(searchPaths, ", "))
	}

	if opts.agentTeamName != "" {
		name := strings.ToLower(opts.agentTeamName)
		if !registry.HasTeam(name) {
			return "", nil, fmt.Errorf("team %q not found. Available: %s", opts.agentTeamName, strings.Join(registry.ListTeams(), ", "))
		}
		return name, registry, nil
	}

	teams := registry.ListTeams()
	if len(teams) == 1 {
		return teams[0], registry, nil
	}

	var chosen string
	var err error
	if pr != nil {
		chosen, err = askUserForTeam(teams, pr)
	} else {
		chosen = askUserForTeamFallback(teams)
	}
	if err != nil {
		return "", nil, err
	}
	if chosen == "" {
		return "", nil, fmt.Errorf("no team selected (use --agent-team, @<team>, or 'hufu init <name>')")
	}
	return strings.ToLower(chosen), registry, nil
}

func readChatLine(pr *readline.PromptReader) (string, error) {
	if pr != nil {
		return pr.ReadLine(boldStyle.Render("hufu> "))
	}
	prompt := promptui.Prompt{
		Label: "hufu",
	}
	res, err := prompt.Run()
	if err != nil {
		return "", io.EOF
	}
	return res, nil
}

// buildVars resolves template variables the same way the root command does:
// --var-file + --var, merged on top of hufu.yaml vars.
func buildVars() (map[string]string, error) {
	vars, err := team.ResolveVars(opts.varFiles, opts.varFlags)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve template variables: %w", err)
	}
	cfgVars := config.LoadConfig().GetVars()
	if len(cfgVars) > 0 || len(vars) > 0 {
		vars = team.MergeVars(cfgVars, vars)
	}
	return vars, nil
}
