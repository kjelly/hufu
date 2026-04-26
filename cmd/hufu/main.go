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
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	ergoreadline "github.com/ergochat/readline"
	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
)

var (
	ollamaURL           string
	verbose             bool
	workspace           string
	newSession          bool
	agentTeamName       string
	agentTeamSearchPath string
)

var (
	boldStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle     = lipgloss.NewStyle().Faint(true)
	agentStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	toolStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	resultStyle  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("7"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	stepStyle    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
	doneStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	textStyle    = lipgloss.NewStyle().Faint(true)
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	pendingIcon  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	progressIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	doneIcon     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errorIcon    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	teamStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
)

type lineWriter struct {
	mu sync.Mutex
}

func (w *lineWriter) write(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprint(os.Stderr, s)
}

type taskDisplay struct {
	mu      sync.Mutex
	w       *lineWriter
	tracker *team.TaskTracker
	lines   int
}

func newTaskDisplay(w *lineWriter, tracker *team.TaskTracker) *taskDisplay {
	return &taskDisplay{w: w, tracker: tracker}
}

func (d *taskDisplay) render() {
	d.mu.Lock()
	defer d.mu.Unlock()

	todoItems := d.tracker.TodoList().Items()
	if len(todoItems) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── TODO ───"))
	b.WriteString("\n")

	for _, t := range todoItems {
		var icon string
		var desc string
		switch t.Status {
		case team.TaskPending:
			icon = pendingIcon.Render("○")
			desc = dimStyle.Render(t.Desc)
		case team.TaskInProgress:
			icon = progressIcon.Render("◑")
			taskPreview := t.Desc
			if len(taskPreview) > 60 {
				taskPreview = taskPreview[:60] + "..."
			}
			desc = taskPreview
		case team.TaskDone:
			icon = doneIcon.Render("●")
			desc = dimStyle.Render(t.Desc)
		case team.TaskError:
			icon = errorIcon.Render("✗")
			if t.Detail != "" {
				desc = errStyle.Render(t.Detail)
			} else {
				desc = dimStyle.Render(t.Desc)
			}
		}
		b.WriteString(fmt.Sprintf("  %s %s %s %s\n", icon, dimStyle.Render(t.ID+"."), agentStyle.Render(t.Agent), desc))
	}

	d.w.write(b.String())
	d.lines = len(todoItems) + 2
}

func (d *taskDisplay) clear() {
	if d.lines > 0 {
		d.w.write(fmt.Sprintf("\033[%dA\033[J", d.lines))
		d.lines = 0
	}
}

func (d *taskDisplay) update() {
	d.clear()
	d.render()
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "hufu [prompt]",
		Short: "Run an agent team to accomplish a task",
		Long:  "hufu discovers and runs agent teams by name. Use --agent-team or @team-name in the prompt to select a team.",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runTeam,
	}

	rootCmd.Flags().StringVar(&ollamaURL, "ollama-url", "http://localhost:11434/v1", "Ollama API base URL")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show full agent text output in real-time")
	rootCmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	rootCmd.Flags().BoolVarP(&newSession, "new", "n", false, "Archive old session and start fresh")
	rootCmd.Flags().StringVar(&agentTeamName, "agent-team", "", "Agent team name to load")
	rootCmd.Flags().StringVar(&agentTeamSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams (default: .agent-teams/,~/.agent-teams/)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
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
	defer func() {
		if pr != nil {
			pr.Close()
		}
	}()

	prompt := ""
	if len(args) > 0 {
		prompt = args[0]
	}
	if prompt == "" {
		prompt = readStdin()
	}
	if prompt == "" {
		if pr != nil {
			prompt = askUserForPrompt(pr)
		} else {
			prompt = askUserForPromptFallback()
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	injector := newPromptInjector(pr)
	defer setupPromptSignals(injector)()

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

	ollama, err := agent.NewOllamaProvider(ollamaURL)
	if err != nil {
		return fmt.Errorf("failed to connect to Ollama: %w", err)
	}

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
				chosen = askUserForTeam(registry.ListTeams(), pr)
			} else {
				chosen = askUserForTeamFallback(registry.ListTeams())
			}
			if chosen == "" {
				fmt.Fprintf(os.Stderr, "%s No team selected.\n", errStyle.Render("✗"))
				os.Exit(1)
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
				tc, err := loadTeamByName(ctx, seg.Name, registry, ollama)
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

	result, err := executeSegments(ctx, segments, registry, ollama, loadedTeams, injector)
	if err != nil {
		return err
	}
	fmt.Println(result)
	return nil
}

func loadTeamByName(ctx context.Context, teamName string, registry *team.TeamRegistry, ollama *agent.OllamaProvider) (*teamContext, error) {
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

	coordinator := team.NewCoordinator(session, ollama, mcpManager, verbose)
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
		additionalPrompt, ok := injector.poll()
		if !ok {
			break
		}
		fmt.Fprintf(os.Stderr, "\n%s Injecting additional prompt...\n\n", boldStyle.Render("↩"))

		contResult, err := tc.coordinator.ContinueWithPrompt(ctx, additionalPrompt)
		if err != nil {
			return result, err
		}
		result += "\n\n---\n\n" + contResult
	}
	return result, nil
}

func executeSegments(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry, ollama *agent.OllamaProvider, loadedTeams map[string]*teamContext, injector *promptInjector) (string, error) {
	var results []string
	currentTeamName := ""

	for i, seg := range segments {
		switch seg.Type {
		case team.SegmentSwitchTeam:
			teamName := seg.Name
			tc, ok := loadedTeams[teamName]
			if !ok {
				loaded, err := loadTeamByName(ctx, teamName, registry, ollama)
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
			setupStatusReporter(w, tc.coordinator, taskDisp, idleTimer)

			fmt.Fprintf(os.Stderr, "\n%s Starting team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(teamName))

			result, err := tc.coordinator.Run(ctx, seg.Content)
			idleTimer.stop()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					os.Exit(130)
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
					os.Exit(130)
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
			setupStatusReporter(w, tc.coordinator, taskDisp, idleTimer)

			fmt.Fprintf(os.Stderr, "\n%s Direct invocation: @%s (team: %s)\n\n", boldStyle.Render("→"), agentStyle.Render(seg.Name), teamStyle.Render(currentTeamName))

			directResult, err := tc.coordinator.RunDirectAgent(ctx, seg.Name, seg.Content)
			idleTimer.stop()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					os.Exit(130)
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
				synthResult, err := tc.coordinator.Run(ctx, synthesisPrompt)
				if err != nil {
					if ctx.Err() == context.Canceled {
						team.SaveSession(tc.session.Workspace, tc.sessionData)
						fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
						os.Exit(130)
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
						os.Exit(130)
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
			setupStatusReporter(w, tc.coordinator, taskDisp, idleTimer)

			fmt.Fprintf(os.Stderr, "\n%s Team %s processing...\n\n", boldStyle.Render("→"), teamStyle.Render(currentTeamName))

			result, err := tc.coordinator.Run(ctx, seg.Content)
			idleTimer.stop()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					os.Exit(130)
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
					os.Exit(130)
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

func setupStatusReporter(w *lineWriter, coordinator *team.Coordinator, taskDisp *taskDisplay, idleTimer *idleWarningTimer) {
	currentAgent := ""
	textBuf := ""

	coordinator.SetStatusReporter(func(event team.StatusEvent) {
		idleTimer.reset()

		switch event.Type {
		case "start":
			if currentAgent != "" && textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			currentAgent = event.Agent
			desc := event.Message
			if len(desc) > 120 {
				desc = desc[:120] + "..."
			}
			w.write(fmt.Sprintf("\n%s %s %s\n",
				headerStyle.Render("▶"),
				agentStyle.Render(event.Agent),
				dimStyle.Render("— "+desc),
			))
			taskDisp.update()

		case "step":
			label := event.Message
			if event.Step > 0 {
				label = fmt.Sprintf("step %d", event.Step)
			}
			w.write(fmt.Sprintf("  %s %s\n",
				stepStyle.Render("│"),
				stepStyle.Render(label),
			))

		case "tool_call":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			argsPreview := formatToolArgs(event.ToolName, event.ToolArgs)
			icon := "⟹"
			w.write(fmt.Sprintf("  %s %s %s\n",
				toolStyle.Render(icon),
				toolStyle.Render(event.ToolName),
				dimStyle.Render(argsPreview),
			))

		case "tool_result":
			resultLine := ""
			if event.ToolResult != "" {
				lines := strings.Split(event.ToolResult, "\n")
				maxLines := 3
				if len(lines) > maxLines {
					resultLine = strings.Join(lines[:maxLines], "\n    ") + fmt.Sprintf("  ... (%d more lines)", len(lines)-maxLines)
				} else {
					resultLine = strings.Join(lines, "\n    ")
				}
				maxChars := 200
				if len(resultLine) > maxChars {
					resultLine = resultLine[:maxChars] + "..."
				}
				w.write(fmt.Sprintf("  %s %s\n    %s\n",
					doneStyle.Render("✓"),
					toolStyle.Render(event.ToolName),
					resultStyle.Render(resultLine),
				))
			} else {
				w.write(fmt.Sprintf("  %s %s\n",
					doneStyle.Render("✓"),
					toolStyle.Render(event.ToolName),
				))
			}

		case "text":
			textBuf += event.Message

		case "done":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("%s %s %s\n",
				doneStyle.Render("✓"),
				agentStyle.Render(event.Agent),
				doneStyle.Render("done"),
			))
			currentAgent = ""
			taskDisp.update()

		case "error":
			if textBuf != "" {
				w.write(flushText(currentAgent, textBuf))
				textBuf = ""
			}
			w.write(fmt.Sprintf("%s %s: %s\n",
				errStyle.Render("✗"),
				agentStyle.Render(event.Agent),
				errStyle.Render(event.Message),
			))
			taskDisp.update()

		case "todos_updated":
			taskDisp.update()
		}
	})
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

func flushText(agentName, text string) string {
	if text == "" {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	maxLen := 300
	display := text
	if len(display) > maxLen {
		display = display[:maxLen] + "..."
	}
	return fmt.Sprintf("  %s %s\n",
		textStyle.Render("💬"),
		textStyle.Render(display),
	)
}

func formatToolArgs(toolName, args string) string {
	if args == "" || args == "{}" {
		return ""
	}
	args = strings.ReplaceAll(args, "\n", " ")
	args = strings.TrimSpace(args)
	maxLen := 80
	if toolName == "run_agents" {
		maxLen = 200
	}
	if toolName == "finish" {
		maxLen = 120
	}
	if len(args) > maxLen {
		return args[:maxLen] + "..."
	}
	return args
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

func askUserForPromptFallback() string {
	var input string
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	fmt.Fprintf(os.Stderr, "%s ", boldStyle.Render(">"))
	fmt.Scanln(&input)
	prompt := strings.TrimSpace(input)
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		os.Exit(1)
	}
	return prompt
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

func askUserForPrompt(pr *readline.PromptReader) string {
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	prompt, err := pr.ReadLine(boldStyle.Render("> "))
	if err != nil {
		if err == ergoreadline.ErrInterrupt || err == io.EOF {
			fmt.Fprintf(os.Stderr, "\n")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "%s Input error: %v\n", errStyle.Render("✗"), err)
		os.Exit(1)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		os.Exit(1)
	}
	return prompt
}

func askUserForTeam(teams []string, pr *readline.PromptReader) string {
	if len(teams) == 0 {
		return ""
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
			os.Exit(130)
		}
		return ""
	}
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

type idleWarningTimer struct {
	w        *lineWriter
	interval time.Duration
	timer    *time.Timer
	mu       sync.Mutex
	warned   bool
	deadline time.Time
}

func newIdleWarningTimer(w *lineWriter, interval time.Duration) *idleWarningTimer {
	t := &idleWarningTimer{
		w:        w,
		interval: interval,
	}
	t.timer = time.AfterFunc(interval, t.warn)
	t.deadline = time.Now().Add(interval)
	return t
}

func (t *idleWarningTimer) warn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer == nil {
		return
	}
	elapsed := time.Since(t.deadline.Add(-t.interval))
	t.w.write(fmt.Sprintf("\n%s Waiting for LLM response... (%v elapsed)\n",
		stepStyle.Render("⏳"),
		elapsed.Truncate(time.Second),
	))
	t.warned = true
	t.timer = time.AfterFunc(t.interval, t.warn)
	t.deadline = time.Now().Add(t.interval)
}

func (t *idleWarningTimer) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer == nil {
		return
	}
	t.timer.Stop()
	if t.warned {
		t.w.write(fmt.Sprintf("\n%s Activity resumed\n", doneStyle.Render("↻")))
		t.warned = false
	}
	t.timer = time.AfterFunc(t.interval, t.warn)
	t.deadline = time.Now().Add(t.interval)
}

func (t *idleWarningTimer) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

type promptInjector struct {
	ch           chan string
	mu           sync.Mutex
	promptReader *readline.PromptReader
}

func newPromptInjector(pr *readline.PromptReader) *promptInjector {
	return &promptInjector{
		ch:           make(chan string, 16),
		promptReader: pr,
	}
}

func (p *promptInjector) enqueue(prompt string) {
	select {
	case p.ch <- prompt:
	default:
	}
}

func (p *promptInjector) poll() (string, bool) {
	select {
	case prompt := <-p.ch:
		return prompt, true
	default:
		return "", false
	}
}

func (p *promptInjector) promptAndEnqueue() {
	tools.StdinMu.Lock()
	defer tools.StdinMu.Unlock()

	if p.promptReader == nil {
		return
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Additional Prompt ───"))
	line, err := p.promptReader.ReadLine(boldStyle.Render("> "))
	if err != nil {
		if err == ergoreadline.ErrInterrupt || err == io.EOF {
			return
		}
		return
	}
	prompt := strings.TrimSpace(line)
	if prompt == "" {
		return
	}
	p.enqueue(prompt)
	fmt.Fprintf(os.Stderr, "%s Prompt enqueued, will be processed after current task completes.\n", doneStyle.Render("✓"))
}

func setupPromptSignals(injector *promptInjector) func() {
	sigTstp := make(chan os.Signal, 1)
	signal.Notify(sigTstp, syscall.SIGTSTP)
	go func() {
		for range sigTstp {
			injector.promptAndEnqueue()
		}
	}()

	sigUsr1 := make(chan os.Signal, 1)
	signal.Notify(sigUsr1, syscall.SIGUSR1)
	go func() {
		for range sigUsr1 {
			injector.promptAndEnqueue()
		}
	}()

	return func() {
		signal.Stop(sigTstp)
		close(sigTstp)
		signal.Stop(sigUsr1)
		close(sigUsr1)
	}
}