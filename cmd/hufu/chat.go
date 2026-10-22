package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/readline"
	"github.com/kjelly/hufu/internal/team"
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

Choose the interface with --ui: plain (default line REPL), dashboard (the
task dashboard, also --tui), or chat (full-screen chat-first TUI).

Examples:
  hufu chat --agent-team dev-team
  hufu chat --agent-team dev-team --ui chat
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
	f.StringVar(&opts.modelOverride, "model", "", "Override worker execution target (e.g. local-model or codex/model)")
	f.StringSliceVar(&opts.workerModelOverrides, "worker-model", nil, "Override the execution target of specific worker(s) as agent=target; repeatable")
	f.StringVarP(&opts.coordinatorModelOverride, "coordinator-model", "c", "", "Override coordinator LLM target")
	f.IntVar(&opts.contextWindowOverride, "context-window", 0, "Override model context window in tokens (0 = use team/provider metadata)")
	f.StringVarP(&opts.workspace, "workspace", "w", "", "Workspace directory")
	f.BoolVarP(&opts.newSession, "new", "n", false, "Archive old session and start fresh")
	f.BoolVarP(&opts.verbose, "verbose", "v", false, "Show full agent text output in real-time")
	f.BoolVar(&opts.memoryEnabled, "memory", false, "Deprecated: no effect; runs always use canonical context memory")
	cobra.CheckErr(f.MarkDeprecated("memory", memoryFlagDeprecation))
	f.StringArrayVar(&opts.forcedSkills, "skill", nil, "Force-load specific skills (repeatable)")
	f.StringArrayVar(&opts.varFlags, "var", nil, "Set template variable key=value (repeatable)")
	f.StringArrayVar(&opts.varFiles, "var-file", nil, "Read template variables from a file (repeatable)")
	f.StringArrayVar(&opts.inputFlags, "input", nil, "Set a typed run input (name=value; repeatable)")
	f.StringArrayVar(&opts.inputFiles, "input-file", nil, "Read typed run inputs from JSON or YAML (repeatable)")
	f.BoolVar(&opts.requestAcceptance, "request-acceptance", false, "Validate each request against frozen requirements")
	f.BoolVar(&opts.planMode, "plan", false, "Force plan-first mode")
	f.BoolVar(&opts.autoSkills, "auto-skills", false, "Enable automatic skill detection")
	f.BoolVar(&opts.projectContext, "project-context", false, "Inject Git Status and Project Directory Structure into prompt context")
	f.BoolVar(&opts.think, "think", false, "Show coordinator decision reasoning")
	f.BoolVar(&opts.tuiMode, "tui", false, "Show the task dashboard TUI (alias for --ui dashboard)")
	f.StringVar(&opts.chatUI, "ui", string(chatUIPlain), "Chat interface: plain, dashboard, or chat")
	f.StringVar(&opts.themeMode, "theme", "", "Display theme: auto, light, dark, or mono (default: config or auto)")
	f.StringVar(&opts.displayPreset, "display-preset", "", "Display preset: default or epaper (default: config or default)")
	f.BoolVar(&opts.noSpinner, "no-spinner", false, "Disable the TUI waiting spinner (also honors NO_SPINNER)")
	registerStaticFlagCompletion(replCmd, "ui", []string{string(chatUIPlain), string(chatUIDashboard), string(chatUIChat)})
	registerStaticFlagCompletion(replCmd, "theme", []string{"auto", "light", "dark", "mono"})
	registerStaticFlagCompletion(replCmd, "display-preset", []string{"default", "epaper"})
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

func runChat(cmd *cobra.Command, args []string) (runErr error) {
	if err := applyProfile(cmd); err != nil {
		return err
	}
	opts.requestAcceptanceSet = cmd.Flags().Changed("request-acceptance")
	configureOutputRendering()
	mode, err := selectChatUIMode(cmd.Flags().Changed("ui"))
	if err != nil {
		return err
	}
	if opts.defaultTeam && opts.agentTeamName != "" {
		return fmt.Errorf("cannot use --default with --agent-team; pick one")
	}
	session, err := openChatSession()
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, session.close())
	}()
	switch mode {
	case chatUIDashboard:
		return session.runDashboard()
	case chatUIChat:
		return session.runChatUI()
	default:
		return session.runPlain()
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
  - Run with --ui chat for the full-screen chat interface,
    or --ui dashboard (--tui) for the task dashboard

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
		chosen, err = askUserForTeamWithPromptUI(teams)
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
