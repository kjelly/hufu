package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/anomalyco/agent-team-cli/internal/agent"
	"github.com/anomalyco/agent-team-cli/internal/mcp"
	"github.com/anomalyco/agent-team-cli/internal/team"
)

var (
	ollamaURL string
	verbose   bool
	workspace string
	newSession bool
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
)

type lineWriter struct {
	mu sync.Mutex
}

func (w *lineWriter) write(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprint(os.Stderr, s)
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

	coordinator := team.NewCoordinator(session, ollama, mcpManager, verbose)
	coordinator.SetSessionData(sessionData)
	coordinator.SetStatusReporter(func(event team.StatusEvent) {
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
		}
	})

	fmt.Fprintf(os.Stderr, "\n%s Starting team coordination...\n\n", boldStyle.Render("→"))

	result, err := coordinator.Run(ctx, prompt)
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

	if sessionData != nil {
		team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name))
	}

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
	fmt.Fprintf(os.Stderr, "Please describe the task you want the team to perform:\n")
	fmt.Fprintf(os.Stderr, "%s ", boldStyle.Render(">"))
	input, _ := reader.ReadString('\n')
	prompt := strings.TrimRight(input, "\r\n")
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		os.Exit(1)
	}
	return prompt
}