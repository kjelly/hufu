package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	ergoreadline "github.com/ergochat/readline"
	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
)

var (
	providerURL         string
	verbose             bool
	workspace           string
	newSession          bool
	tempWorkspace       bool
	agentTeamName       string
	agentTeamSearchPath string
	globalPromptReader  atomic.Pointer[readline.PromptReader]
)

type errInterrupted struct{}

func (errInterrupted) Error() string { return "interrupted" }

type errForced struct{}

func (errForced) Error() string { return "force quit" }

func main() {
	exitCode := 0
	rootCmd := &cobra.Command{
		Use:   "hufu [prompt]",
		Short: "Run an agent team to accomplish a task",
		Long:  "hufu discovers and runs agent teams by name. Use --agent-team or @team-name in the prompt to select a team.",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runTeam,
	}

	rootCmd.Flags().StringVar(&providerURL, "provider-url", "http://localhost:11434/v1", "Ollama API base URL")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show full agent text output in real-time")
	rootCmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	rootCmd.Flags().BoolVarP(&newSession, "new", "n", false, "Archive old session and start fresh")
	rootCmd.Flags().BoolVarP(&tempWorkspace, "temp", "t", false, "Use a temporary directory for workspace")
	rootCmd.Flags().StringVar(&agentTeamName, "agent-team", "", "Agent team name to load")
	rootCmd.Flags().StringVar(&agentTeamSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams (default: .agent-teams/,~/.agent-teams/)")

	if err := rootCmd.Execute(); err != nil {
		var interrupted errInterrupted
		if errors.Is(err, interrupted) {
			exitCode = 130
		} else {
			exitCode = 1
		}
	}
	if pr := globalPromptReader.Load(); pr != nil {
		pr.Close()
	}
	os.Exit(exitCode)
}

type teamContext struct {
	session     *team.TeamSession
	coordinator *team.Coordinator
	sessionData *team.SessionData
}

func runTeam(cmd *cobra.Command, args []string) error {
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
		defer os.RemoveAll(tmpDir)
		workspace = filepath.Join(tmpDir, "workspace")
		fmt.Fprintf(os.Stderr, "%s Temp workspace: %s\n", stepStyle.Render("⟳"), workspace)
	}

	prompt := ""
	if len(args) > 0 {
		prompt = args[0]
	}
	if prompt == "" {
		prompt = readStdin()
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

	sigIntCh := make(chan os.Signal, 1)
	signal.Notify(sigIntCh, os.Interrupt)
	sigIntDone := make(chan struct{})
	go func() {
		defer close(sigIntDone)
		first := true
		for range sigIntCh {
			if first {
				fmt.Fprintf(os.Stderr, "\n%s Wrapping up... (press Ctrl+C again to force quit)\n", boldStyle.Render("⏹"))
				if c := activeCoord.Get(); c != nil {
					c.SetWrapUp()
				}
				injector.injectWrapUp()
				first = false
			} else {
				fmt.Fprintf(os.Stderr, "\n%s Force quit\n", errStyle.Render("✗"))
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
	if err := registry.Discover(); err != nil {
		return fmt.Errorf("failed to discover teams: %w", err)
	}

	if registry.TeamCount() == 0 {
		return fmt.Errorf("no agent teams found in search paths: %s", strings.Join(searchPaths, ", "))
	}

	fmt.Fprintf(os.Stderr, "%s Available teams: %s\n", boldStyle.Render("Teams:"), strings.Join(registry.ListTeams(), ", "))

	initialTeam := strings.ToLower(agentTeamName)

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

	loadedTeams := map[string]*teamContext{}
	for _, seg := range initialSegments {
		if seg.Type == team.SegmentSwitchTeam {
			if _, ok := loadedTeams[seg.Name]; !ok {
				tc, err := loadTeamByName(ctx, seg.Name, registry, providerURL)
				if err != nil {
					return fmt.Errorf("failed to load team %q: %w", seg.Name, err)
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

	result, err := executeSegments(ctx, segments, registry, providerURL, loadedTeams, injector, activeCoord)
	if err != nil {
		return err
	}
	fmt.Println(result)

	if tempWorkspace {
		absWS, _ := filepath.Abs(workspace)
		fmt.Fprintf(os.Stderr, "\n%s\n  Path: %s\n  Reuse: hufu -w %s [prompt]\n",
			boldStyle.Render("─── Temporary Workspace ───"),
			absWS, absWS)
	}

	return nil
}

func loadTeamByName(ctx context.Context, teamName string, registry *team.TeamRegistry, defaultProviderURL string) (*teamContext, error) {
	teamDir, err := registry.Resolve(teamName)
	if err != nil {
		return nil, err
	}

	session, err := team.LoadTeam(teamDir)
	if err != nil {
		return nil, err
	}

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

	team.EnsureWorkspaceDirs(session.Workspace)

	var sessionData *team.SessionData
	if newSession {
		existingMD := team.LoadSessionMD(session.Workspace)
		if existingMD != "" {
			fmt.Fprintf(os.Stderr, "%s Archiving previous session...\n", stepStyle.Render("⟳"))
			archivedPath, err := team.ArchiveSessionMD(session.Workspace)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
			} else {
				fmt.Fprintf(os.Stderr, "%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
			}
		} else if team.HasSession(session.Workspace) {
			oldSession := team.LoadSession(session.Workspace)
			if oldSession != nil && len(oldSession.Entries) > 0 {
				fmt.Fprintf(os.Stderr, "%s Archiving previous session...\n", stepStyle.Render("⟳"))
				md := team.GenerateSessionMD(oldSession, session.Config.Name)
				team.SaveSessionMD(session.Workspace, md)
				archivedPath, err := team.ArchiveSessionMD(session.Workspace)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
				} else {
					fmt.Fprintf(os.Stderr, "%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
				}
			}
			os.Remove(filepath.Join(session.Workspace, "session.json"))
			team.DeleteConversationHistory(session.Workspace)
		}
		if err := team.CleanRunDirs(session.Workspace); err != nil {
			fmt.Fprintf(os.Stderr, "%s Failed to clean workspace: %v\n", errStyle.Render("⚠"), err)
		}
		team.EnsureWorkspaceDirs(session.Workspace)
		sessionData = team.NewSession()
		team.SaveSession(session.Workspace, sessionData)
		team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name))
		fmt.Fprintf(os.Stderr, "%s Started new session\n", boldStyle.Render("→"))
	} else {
		sessionData = team.LoadSession(session.Workspace)
		existingMD := team.LoadSessionMD(session.Workspace)
		if existingMD != "" {
			fmt.Fprintf(os.Stderr, "%s Resuming session\n", boldStyle.Render("→"))
		} else if sessionData != nil && len(sessionData.Entries) > 0 {
			md := team.GenerateSessionMD(sessionData, session.Config.Name)
			team.SaveSessionMD(session.Workspace, md)
			existingMD = md
			fmt.Fprintf(os.Stderr, "%s Resuming session (%d exchanges, since %s)\n",
				boldStyle.Render("→"),
				len(sessionData.Entries),
				sessionData.CreatedAt,
			)
		} else {
			if sessionData == nil {
				sessionData = team.NewSession()
			}
			team.SaveSession(session.Workspace, sessionData)
			team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name))
			fmt.Fprintf(os.Stderr, "%s Starting new session\n", boldStyle.Render("→"))
		}

		if existingMD != "" {
			lines := strings.SplitN(existingMD, "\n", 30)
			preview := strings.Join(lines, "\n")
			fmt.Fprintf(os.Stderr, "\n%s\n%s\n\n",
				boldStyle.Render("─── Previous Session ───"),
				dimStyle.Render(preview),
			)
		}
	}

	fmt.Fprintf(os.Stderr, "%s %s\n", boldStyle.Render("Team:"), session.Config.Name)
	fmt.Fprintf(os.Stderr, "%s ", boldStyle.Render("Agents:"))
	var agentDisplayNames []string
	for _, def := range sortedAgents(session.Agents) {
		roleLabel := def.Role
		agentDisplayNames = append(agentDisplayNames, fmt.Sprintf("%s (%s)", agentStyle.Render(def.Name), dimStyle.Render(roleLabel)))
	}
	fmt.Fprintf(os.Stderr, "%s\n", strings.Join(agentDisplayNames, ", "))

	var mcpManager *mcp.MCPToolManager
	if len(session.MCPServers) > 0 {
		mcpManager = mcp.NewMCPToolManager()
		fmt.Fprintf(os.Stderr, "%s Loading MCP servers...\n", stepStyle.Render("⟳"))
		if err := mcpManager.LoadTools(ctx, session.MCPServers); err != nil {
			fmt.Fprintf(os.Stderr, "%s MCP loading failed: %v\n", errStyle.Render("⚠"), err)
		} else {
			fmt.Fprintf(os.Stderr, "%s MCP tools: %d loaded\n", doneStyle.Render("✓"), len(mcpManager.GetTools()))
		}
	}

	coordinator, err := team.NewCoordinator(session, defaultProviderURL, mcpManager, verbose)
	if err != nil {
		return nil, fmt.Errorf("failed to create coordinator: %w", err)
	}
	coordinator.SetSessionData(sessionData)

	if len(session.Skills) > 0 {
		var skillNames []string
		for _, s := range session.Skills {
			skillNames = append(skillNames, s.Name)
		}
		fmt.Fprintf(os.Stderr, "%s %s\n", boldStyle.Render("Skills:"), strings.Join(skillNames, ", "))
	}

	return &teamContext{
		session:     session,
		coordinator: coordinator,
		sessionData: sessionData,
	}, nil
}

func runWithInjection(ctx context.Context, tc *teamContext, initialResult string, injector *promptInjector) (string, error) {
	result := initialResult
	for {
		select {
		case <-injector.wrapUpCh:
			fmt.Fprintf(os.Stderr, "\n%s Wrapping up — coordinator will summarize and finish.\n\n", boldStyle.Render("⏹"))
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
			fmt.Fprintf(os.Stderr, "\n%s Injecting additional prompt...\n\n", boldStyle.Render("↩"))

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

func executeSegments(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry, defaultProviderURL string, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator) (string, error) {
	var results []string
	currentTeamName := ""

	for i, seg := range segments {
		switch seg.Type {
		case team.SegmentSwitchTeam:
			teamName := seg.Name
			tc, ok := loadedTeams[teamName]
			if !ok {
				loaded, err := loadTeamByName(ctx, teamName, registry, providerURL)
				if err != nil {
					return strings.Join(results, "\n\n"), fmt.Errorf("failed to load team %q: %w", teamName, err)
				}
				tc = loaded
				loadedTeams[teamName] = tc
			}

			if currentTeamName != "" && currentTeamName != teamName {
				prevTC := loadedTeams[currentTeamName]
				if prevTC != nil {
					team.SaveSessionMD(prevTC.session.Workspace, team.GenerateSessionMD(prevTC.sessionData, prevTC.session.Config.Name))
				}
				fmt.Fprintf(os.Stderr, "\n%s Switching team: %s → %s\n\n", boldStyle.Render("⇒"), teamStyle.Render(currentTeamName), teamStyle.Render(teamName))
			}

			currentTeamName = teamName

			if seg.Content == "" {
				continue
			}

			w := &lineWriter{}
			idleTimer := newIdleWarningTimer(w, 30*time.Second)
			taskDisp := newTaskDisplay(w, tc.coordinator.TaskTracker())
			skillDisp := newSkillDisplay(w)
			setupStatusReporter(w, tc.coordinator, taskDisp, skillDisp, idleTimer)
			setStatusFlusher(w, taskDisp, skillDisp)

			fmt.Fprintf(os.Stderr, "\n%s Starting team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(teamName))

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, seg.Content)
			activeCoord.Clear()
			idleTimer.stop()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				team.SaveSession(tc.session.Workspace, tc.sessionData)
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q failed: %w", teamName, err)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				team.SaveSession(tc.session.Workspace, tc.sessionData)
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q continuation failed: %w", teamName, err)
			}

			taskDisp.update()
			fmt.Fprintf(os.Stderr, "\n%s Team %s coordination complete.\n", doneStyle.Render("✓"), teamStyle.Render(teamName))
			results = append(results, fmt.Sprintf("## Team: %s\n%s", teamName, result))

		case team.SegmentInvokeAgent:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("@%s — no active team. Specify a team with --agent-team or @team-name first", seg.Name)
			}

			tc := loadedTeams[currentTeamName]
			if tc == nil {
				return strings.Join(results, "\n\n"), fmt.Errorf("@%s — team %q not loaded", seg.Name, currentTeamName)
			}

			w := &lineWriter{}
			idleTimer := newIdleWarningTimer(w, 30*time.Second)
			taskDisp := newTaskDisplay(w, tc.coordinator.TaskTracker())
			skillDisp := newSkillDisplay(w)
			setupStatusReporter(w, tc.coordinator, taskDisp, skillDisp, idleTimer)
			setStatusFlusher(w, taskDisp, skillDisp)

			fmt.Fprintf(os.Stderr, "\n%s Direct invocation: @%s (team: %s)\n\n", boldStyle.Render("→"), agentStyle.Render(seg.Name), teamStyle.Render(currentTeamName))

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			directResult, err := tc.coordinator.RunDirectAgent(ctx, seg.Name, seg.Content)
			activeCoord.Clear()
			idleTimer.stop()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				return strings.Join(results, "\n\n"), fmt.Errorf("direct agent @%s failed: %w", seg.Name, err)
			}

			if directResult.Error != nil {
				fmt.Fprintf(os.Stderr, "\n%s %s failed: %s\n", errStyle.Render("✗"), agentStyle.Render(seg.Name), errStyle.Render(directResult.Error.Error()))
				results = append(results, fmt.Sprintf("## Agent: @%s\n**ERROR**: %s", seg.Name, directResult.Error))
				continue
			}

			fmt.Fprintf(os.Stderr, "\n%s %s completed, synthesizing...\n\n", doneStyle.Render("✓"), agentStyle.Render(seg.Name))

			orchDef := tc.coordinator.GetOrchestratorDef()
			if orchDef == nil {
				taskDisp.update()
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
						team.SaveSession(tc.session.Workspace, tc.sessionData)
						fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
						return "", errInterrupted{}
					}
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
					return strings.Join(results, "\n\n"), fmt.Errorf("synthesis for @%s failed: %w", seg.Name, err)
				}

				synthResult, err = runWithInjection(ctx, tc, synthResult, injector)
				if err != nil {
					if ctx.Err() == context.Canceled {
						team.SaveSession(tc.session.Workspace, tc.sessionData)
						fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
						return "", errInterrupted{}
					}
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
					return strings.Join(results, "\n\n"), fmt.Errorf("synthesis continuation for @%s failed: %w", seg.Name, err)
				}

				taskDisp.update()
				results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, synthResult))
			}

		case team.SegmentText:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("text segment with no active team — specify a team with --agent-team or @team-name first")
			}

			tc := loadedTeams[currentTeamName]

			w := &lineWriter{}
			idleTimer := newIdleWarningTimer(w, 30*time.Second)
			taskDisp := newTaskDisplay(w, tc.coordinator.TaskTracker())
			skillDisp := newSkillDisplay(w)
			setupStatusReporter(w, tc.coordinator, taskDisp, skillDisp, idleTimer)
			setStatusFlusher(w, taskDisp, skillDisp)

			fmt.Fprintf(os.Stderr, "\n%s Team %s processing...\n\n", boldStyle.Render("→"), teamStyle.Render(currentTeamName))

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, seg.Content)
			activeCoord.Clear()
			idleTimer.stop()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				team.SaveSession(tc.session.Workspace, tc.sessionData)
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q failed: %w", currentTeamName, err)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				team.SaveSession(tc.session.Workspace, tc.sessionData)
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q continuation failed: %w", currentTeamName, err)
			}

			taskDisp.update()
			results = append(results, fmt.Sprintf("## Team: %s\n%s", currentTeamName, result))
		}

		if i == len(segments)-1 {
			if tc, ok := loadedTeams[currentTeamName]; ok {
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
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

func agentNamesFromSession(session *team.TeamSession) []string {
	var names []string
	for name, def := range session.Agents {
		if def.Role != "orchestrator" && def.Role != "coordinator" {
			names = append(names, name)
		}
	}
	return names
}

func sortedAgents(agents map[string]*agent.AgentDef) []*agent.AgentDef {
	var result []*agent.AgentDef
	for _, def := range agents {
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
	os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "prompt_history")
}

func askUserForPromptFallback() (string, error) {
	var input string
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	fmt.Fprintf(os.Stderr, "%s ", boldStyle.Render(">"))
	fmt.Scanln(&input)
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
	fmt.Scanln(&input)
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	if idx, err := fmt.Sscanf(input, "%d", new(int)); err == nil && idx == 1 {
		var num int
		fmt.Sscanf(input, "%d", &num)
		if num >= 1 && num <= len(teams) {
			return teams[num-1]
		}
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
	if idx, err := fmt.Sscanf(input, "%d", new(int)); err == nil && idx == 1 {
		var num int
		fmt.Sscanf(input, "%d", &num)
		if num >= 1 && num <= len(teams) {
			return teams[num-1], nil
		}
	}
	lower := strings.ToLower(input)
	for _, t := range teams {
		if strings.ToLower(t) == lower {
			return t, nil
		}
	}
	return input, nil
}
