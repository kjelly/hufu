package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/anomalyco/agent-team-cli/internal/agent"
	"github.com/anomalyco/agent-team-cli/internal/mcp"
	"github.com/anomalyco/agent-team-cli/internal/team"
)

var (
	ollamaURL   string
	verbose     bool
	workspace   string
	newSession  bool
)

var (
	boldStyle   = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Faint(true)
	agentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	toolStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	resultStyle = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("7"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	stepStyle   = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
	doneStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	textStyle   = lipgloss.NewStyle().Faint(true)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	pendingIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	progressIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	doneIcon    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errorIcon   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
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
	mu     sync.Mutex
	w      *lineWriter
	tracker *team.TaskTracker
	lines  int
}

func newTaskDisplay(w *lineWriter, tracker *team.TaskTracker) *taskDisplay {
	return &taskDisplay{w: w, tracker: tracker}
}

func (d *taskDisplay) render() {
	d.mu.Lock()
	defer d.mu.Unlock()

	tasks := d.tracker.Tasks()
	if len(tasks) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── Tasks ───"))
	b.WriteString("\n")

	for _, t := range tasks {
		var icon string
		var desc string
		switch t.Status {
		case team.TaskPending:
			icon = pendingIcon.Render("○")
			desc = dimStyle.Render(t.Task)
		case team.TaskInProgress:
			icon = progressIcon.Render("◑")
			taskPreview := t.Task
			if len(taskPreview) > 60 {
				taskPreview = taskPreview[:60] + "..."
			}
			desc = taskPreview
		case team.TaskDone:
			icon = doneIcon.Render("●")
			desc = dimStyle.Render(t.Task)
		case team.TaskError:
			icon = errorIcon.Render("✗")
			if t.Detail != "" {
				desc = errStyle.Render(t.Detail)
			} else {
				desc = dimStyle.Render(t.Task)
			}
		}
		b.WriteString(fmt.Sprintf("  %s %s %s\n", icon, agentStyle.Render(t.Agent), desc))
	}

	d.w.write(b.String())
	d.lines = len(tasks) + 2
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
		Use:   "agent-team-cli <team-dir> [prompt]",
		Short: "Run an agent team to accomplish a task",
		Long:  "agent-team-cli loads a team directory, creates a coordinator agent with workers, and delegates tasks using Ollama.",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runTeam,
	}

	rootCmd.Flags().StringVar(&ollamaURL, "ollama-url", "http://localhost:11434/v1", "Ollama API base URL")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show full agent text output in real-time")
	rootCmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	rootCmd.Flags().BoolVarP(&newSession, "new", "n", false, "Archive old session and start fresh")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runTeam(cmd *cobra.Command, args []string) error {
	teamDir := args[0]
	prompt := ""
	if len(args) > 1 {
		prompt = strings.Join(args[1:], " ")
	}

	if prompt == "" {
		prompt = readStdin()
	}
	if prompt == "" {
		prompt = askUserForPrompt()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	session, err := team.LoadTeam(teamDir)
	if err != nil {
		return fmt.Errorf("failed to load team: %w", err)
	}

	if workspace != "" {
		absWorkspace, err := filepath.Abs(workspace)
		if err != nil {
			return fmt.Errorf("invalid workspace path: %w", err)
		}
		if err := os.MkdirAll(absWorkspace, 0o755); err != nil {
			return fmt.Errorf("failed to create workspace: %w", err)
		}
		session.Workspace = absWorkspace
		session.Config.WorkspaceDir = absWorkspace
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
			lines := strings.SplitN(existingMD, "\n", 8)
			preview := strings.Join(lines, "\n")
			fmt.Fprintf(os.Stderr, "\n%s\n%s\n\n",
				boldStyle.Render("─── Previous Session ───"),
				dimStyle.Render(preview),
			)
		}
	}

	fmt.Fprintf(os.Stderr, "%s %s\n", boldStyle.Render("Team:"), session.Config.Name)
	fmt.Fprintf(os.Stderr, "%s ", boldStyle.Render("Agents:"))
	var agentNames []string
	for _, def := range sortedAgents(session.Agents) {
		roleLabel := def.Role
		agentNames = append(agentNames, fmt.Sprintf("%s (%s)", agentStyle.Render(def.Name), dimStyle.Render(roleLabel)))
	}
	fmt.Fprintf(os.Stderr, "%s\n", strings.Join(agentNames, ", "))

	ollama, err := agent.NewOllamaProvider(ollamaURL)
	if err != nil {
		return fmt.Errorf("failed to connect to Ollama: %w", err)
	}

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

	w := &lineWriter{}
	currentAgent := ""
	textBuf := ""

	idleTimer := newIdleWarningTimer(w, 30*time.Second)

	coordinator := team.NewCoordinator(session, ollama, mcpManager, verbose)
	coordinator.SetSessionData(sessionData)

	if len(session.Skills) > 0 {
		var skillNames []string
		for _, s := range session.Skills {
			skillNames = append(skillNames, s.Name)
		}
		fmt.Fprintf(os.Stderr, "%s %s\n", boldStyle.Render("Skills:"), strings.Join(skillNames, ", "))
	}

	taskDisp := newTaskDisplay(w, coordinator.TaskTracker())

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
		}
	})

	var result string

	directAgent, directTask, isDirect := team.ParseDirectAgent(prompt)
	if isDirect {
		fmt.Fprintf(os.Stderr, "\n%s Direct invocation: @%s\n\n", boldStyle.Render("→"), directAgent)

		directResult, err := coordinator.RunDirectAgent(ctx, directAgent, directTask)
		if err != nil {
			if ctx.Err() == context.Canceled {
				if sessionData != nil {
					team.SaveSession(session.Workspace, sessionData)
				}
				fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
				if mcpManager != nil {
					mcpManager.Close()
				}
				os.Exit(130)
			}
			return fmt.Errorf("direct agent execution failed: %w", err)
		}

		if directResult.Error != nil {
			fmt.Fprintf(os.Stderr, "\n%s %s failed: %s\n", errStyle.Render("✗"), directResult.AgentName, errStyle.Render(directResult.Error.Error()))
			if sessionData != nil {
				team.SaveSession(session.Workspace, sessionData)
			}
			return fmt.Errorf("direct agent %s failed: %w", directResult.AgentName, directResult.Error)
		}

		fmt.Fprintf(os.Stderr, "\n%s %s completed, synthesizing...\n\n", doneStyle.Render("✓"), agentStyle.Render(directResult.AgentName))

		orchDef := coordinator.GetOrchestratorDef()
		if orchDef == nil {
			result = directResult.Output
		} else {
			synthesisPrompt := fmt.Sprintf("A user directly asked @%s to do the following task:\n\n%s\n\nHere is what %s produced:\n\n---\n%s\n---\n\nPlease synthesize this into a final, well-organized answer for the user. If the output is already complete, you can present it as-is. If there are any issues or follow-up needed, address them.",
				directAgent, directTask, directAgent, directResult.Output)

			result, err = coordinator.Run(ctx, synthesisPrompt)
			if err != nil {
				if ctx.Err() == context.Canceled {
					if sessionData != nil {
						team.SaveSession(session.Workspace, sessionData)
					}
					fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
					if mcpManager != nil {
						mcpManager.Close()
					}
					os.Exit(130)
				}
				if sessionData != nil {
					team.SaveSession(session.Workspace, sessionData)
					team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name))
				}
				return fmt.Errorf("coordinator synthesis failed: %w", err)
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "\n%s Starting team coordination...\n\n", boldStyle.Render("→"))

		result, err = coordinator.Run(ctx, prompt)
		if err != nil {
			if ctx.Err() == context.Canceled {
				if sessionData != nil {
					team.SaveSession(session.Workspace, sessionData)
				}
				fmt.Fprintf(os.Stderr, "\n%s Interrupted\n", errStyle.Render("⚠"))
				if mcpManager != nil {
					mcpManager.Close()
				}
				os.Exit(130)
			}
			if sessionData != nil {
				team.SaveSession(session.Workspace, sessionData)
				team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name))
			}
			return fmt.Errorf("team execution failed: %w", err)
		}
	}

	if sessionData != nil {
		team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name))
	}

	taskDisp.update()

	idleTimer.stop()

	fmt.Fprintf(os.Stderr, "\n%s Coordination complete.\n\n", doneStyle.Render("✓"))

	fmt.Println(result)

	if mcpManager != nil {
		mcpManager.Close()
	}

	return nil
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

func readStdin() string {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return ""
	}
	data, _ := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(data))
}

func askUserForPrompt() string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @agent-name for direct agent invocation):\n")
	fmt.Fprintf(os.Stderr, "%s ", boldStyle.Render(">"))
	input, _ := reader.ReadString('\n')
	prompt := strings.TrimRight(input, "\r\n")
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		os.Exit(1)
	}
	return prompt
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