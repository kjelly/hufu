package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"

	ergoreadline "github.com/ergochat/readline"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	tuipkg "github.com/anomalyco/hufu/internal/tui"
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
	f.StringVar(&agentTeamName, "agent-team", "", "Agent team name to chat with")
	f.StringVar(&agentTeamSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams")
	f.BoolVar(&defaultTeam, "default", false, "Use the built-in default team (coordinator + Helper)")
	f.StringVar(&helperTools, "helper-tools", "", "Extra tools for the default Helper (e.g. bash,sudo)")
	f.StringSliceVar(&allowPaths, "allow-path", nil, "Additional filesystem paths to allow for the active team")
	f.BoolVar(&autoApprove, "auto-approve", false, "Automatically choose clearly safe ask_user options; dangerous or ambiguous choices still prompt the user")
	f.StringVar(&providerURL, "provider-url", "", "Provider API base URL")
	f.StringVar(&providerAPIKey, "provider-api-key", "", "Provider API key")
	f.StringVar(&modelOverride, "model", "", "Override default model (e.g. ollama/qwen3:8b)")
	f.StringVarP(&workspace, "workspace", "w", "", "Workspace directory")
	f.BoolVarP(&newSession, "new", "n", false, "Archive old session and start fresh")
	f.BoolVarP(&verbose, "verbose", "v", false, "Show full agent text output in real-time")
	f.BoolVar(&memoryEnabled, "memory", false, "Enable long-term memory (RAG with vector search)")
	f.StringArrayVar(&forcedSkills, "skill", nil, "Force-load specific skills (repeatable)")
	f.StringArrayVar(&varFlags, "var", nil, "Set template variable key=value (repeatable)")
	f.StringArrayVar(&varFiles, "var-file", nil, "Read template variables from a file (repeatable)")
	f.BoolVar(&planMode, "plan", false, "Force plan-first mode")
	f.BoolVar(&autoSkills, "auto-skills", false, "Enable automatic skill detection")
	f.BoolVar(&projectContext, "project-context", false, "Inject Git Status and Project Directory Structure into prompt context")
	f.BoolVar(&think, "think", false, "Show coordinator decision reasoning")
	f.BoolVar(&tuiMode, "tui", false, "Show a Bubble Tea TUI for real-time task tracking")
	f.Int64Var(&timeoutOverride, "timeout", 0, "Override agent/coordinator timeout in seconds")
	f.IntVar(&maxRoundsOverride, "max-rounds", 0, "Override team.yaml max-rounds. 0 = use team default.")
	f.IntVar(&maxConcurrentOverride, "max-concurrent", 0, "Override team.yaml max-concurrent. 0 = use team default.")
	f.IntVar(&maxStepsOverride, "max-steps", 0, "Override team.yaml max-steps. 0 = use team/agent default.")
}

func runChat(cmd *cobra.Command, args []string) error {
	if err := applyProfile(cmd); err != nil {
		return err
	}
	configureOutputRendering()
	if defaultTeam && agentTeamName != "" {
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
	if defaultTeam {
		teamName = "default"
		tc, err = loadDefaultTeam(rootCtx, providerURL, providerAPIKey, pathConsent, vars, forcedSkills, planMode, autoSkills)
	} else {
		teamName, registry, err = pickChatTeam(pr)
		if err != nil {
			return err
		}
		tc, err = loadTeamByName(rootCtx, teamName, registry, providerURL, providerAPIKey, pathConsent, vars, forcedSkills, planMode, autoSkills)
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

	if tuiMode {
		isChatTUI = true
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
		teamInfo.MemoryEnabled = memoryEnabled && !tempWorkspace
		if teamInfo.MemoryEnabled {
			teamInfo.MemoryModel = config.ResolveEmbeddingModel(memoryModel)
		}
		teamInfo.SSHSessions = 0
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

		_, runErr := runWithTUI(turnCtx, cancel, "", segments, registry, loadedTeams, injector, activeCoord, pathConsent, vars, teamInfo)
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
		switch strings.ToLower(line) {
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
		}

		// Inject file/project contexts
		promptToRun, _ := injectFileContexts(line)
		cfg := config.LoadConfig()
		if projectContext || cfg.ProjectContext || tc.session.Config.ProjectContext {
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
			go savePromptToHistory(context.Background(), line, providerURL)
		}

		// Persist after every turn so the session survives a crash mid-chat.
		_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
		_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
	}
}

// chatHelpText returns a multi-line help block shown by the /help command in
// the chat REPL.
func chatHelpText(teamName string) string {
	return fmt.Sprintf(`%s Chatting with team %s

Commands:
  /help, help, ?       show this help
  /reset               clear the conversation context (start fresh)
  /team                switch to a different team mid-session
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
	tc, err := loadTeamByName(rootCtx, chosen, registry, providerURL, providerAPIKey, newPathConsent(), buildVarsOrNil(), forcedSkills, planMode, autoSkills)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load team %q: %w", chosen, err)
	}
	return tc, chosen, nil
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

	if agentTeamName != "" {
		name := strings.ToLower(agentTeamName)
		if !registry.HasTeam(name) {
			return "", nil, fmt.Errorf("team %q not found. Available: %s", agentTeamName, strings.Join(registry.ListTeams(), ", "))
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
	vars, err := team.ResolveVars(varFiles, varFlags)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve template variables: %w", err)
	}
	cfgVars := config.LoadConfig().GetVars()
	if len(cfgVars) > 0 || len(vars) > 0 {
		vars = team.MergeVars(cfgVars, vars)
	}
	return vars, nil
}
