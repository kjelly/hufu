package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/utils"
)

type teamInfoTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *teamInfoTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "team_info",
		Description: "Access team member information, task history, full task results, and session status. Use this to understand what other agents are doing, retrieve the complete output of a completed task (so you don't redo their work), and see the current state of the team.",
		Parameters: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Action: list_agents, agent_info, task_history, task_result, todo_status, session_summary. Use task_result to read the full output of another agent's most recently completed task.",
			},
			"agent": map[string]any{
				"type":        "string",
				"description": "Agent name for agent_info, task_history, task_result, todo_status actions",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results for task_history (default 10)",
			},
		},
		Required: []string{"action"},
		Parallel: true,
	}
}

func (t *teamInfoTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Action string `json:"action"`
		Agent  string `json:"agent"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Limit <= 0 || args.Limit > 50 {
		args.Limit = 10
	}

	c := t.coordinator
	workspace := c.session.Workspace
	teamName := c.session.Config.Name

	switch args.Action {
	case "list_agents":
		return t.handleListAgents(c)
	case "agent_info":
		if args.Agent == "" {
			return fantasy.NewTextErrorResponse("agent name is required for agent_info action"), nil
		}
		return t.handleAgentInfo(c, args.Agent)
	case "task_history":
		if args.Agent == "" {
			return fantasy.NewTextErrorResponse("agent name is required for task_history action"), nil
		}
		return t.handleTaskHistory(workspace, teamName, args.Agent, args.Limit)
	case "task_result":
		if args.Agent == "" {
			return fantasy.NewTextErrorResponse("agent name is required for task_result action"), nil
		}
		return t.handleTaskResult(c, workspace, teamName, args.Agent)
	case "todo_status":
		if args.Agent == "" {
			return fantasy.NewTextErrorResponse("agent name is required for todo_status action"), nil
		}
		return t.handleTodoStatus(c, args.Agent)
	case "session_summary":
		return t.handleSessionSummary(c)
	default:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown action %q (valid: list_agents, agent_info, task_history, task_result, todo_status, session_summary)", args.Action)), nil
	}
}

func (t *teamInfoTool) handleListAgents(c *Coordinator) (fantasy.ToolResponse, error) {
	workers := c.uniqueWorkerDefs()
	var b strings.Builder
	fmt.Fprintf(&b, "%d team members:\n\n", len(workers))
	for _, def := range workers {
		aliasesStr := ""
		if def.FileAlias != "" {
			aliasesStr = fmt.Sprintf(" (alias: %s)", def.FileAlias)
		}
		fmt.Fprintf(&b, "### %s%s\n", def.Name, aliasesStr)
		if def.Description != "" {
			fmt.Fprintf(&b, "**Description:** %s\n", def.Description)
		}
		if def.Role != "" {
			fmt.Fprintf(&b, "**Role:** %s\n", def.Role)
		}
		if def.Tools != "" {
			fmt.Fprintf(&b, "**Tools:** %s\n", def.Tools)
		}
		if def.Skills != "" {
			fmt.Fprintf(&b, "**Skills:** %s\n", def.Skills)
		}
		b.WriteString("\n")
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func (t *teamInfoTool) handleAgentInfo(c *Coordinator, name string) (fantasy.ToolResponse, error) {
	agentDef, _, err := c.AgentPool().ResolveAgentName(name)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("agent %q not found: %v", name, err)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n", agentDef.Name)
	if agentDef.FileAlias != "" {
		fmt.Fprintf(&b, "**File:** %s\n", agentDef.FileAlias)
	}
	if agentDef.Description != "" {
		fmt.Fprintf(&b, "**Description:** %s\n", agentDef.Description)
	}
	if agentDef.Role != "" {
		fmt.Fprintf(&b, "**Role:** %s\n", agentDef.Role)
	}
	if agentDef.Tools != "" {
		fmt.Fprintf(&b, "**Tools:** %s\n", agentDef.Tools)
	}
	if agentDef.Skills != "" {
		fmt.Fprintf(&b, "**Skills:** %s\n", agentDef.Skills)
	}

	instr := c.getWorkerSummary(agentDef.Name)
	if instr != "" {
		fmt.Fprintf(&b, "**Instructions:** %s\n", instr)
	}

	caps := ExtractCapabilitiesFromSystem(agentDef.System)
	if caps != "" {
		fmt.Fprintf(&b, "**Capabilities:**\n")
		for _, line := range strings.Split(caps, "\n") {
			if line != "" {
				fmt.Fprintf(&b, "- %s\n", line)
			}
		}
	}

	return fantasy.NewTextResponse(b.String()), nil
}

func (t *teamInfoTool) handleTaskHistory(workspace, teamName, agentName string, limit int) (fantasy.ToolResponse, error) {
	dir := filepath.Join(workspace, tasksDir, teamName, agentName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fantasy.NewTextResponse(fmt.Sprintf("No task history for agent %q.", agentName)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot read task dir: %v", err)), nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() > entries[j].Name()
	})

	// count only valid .md files for correct "remaining" display
	totalMD := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			totalMD++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Task history for %s:\n\n", agentName)
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if count >= limit {
			fmt.Fprintf(&b, "\n... (%d more tasks)", totalMD-count)
			break
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		ts := strings.TrimSuffix(entry.Name(), ".md")
		content := string(data)

		// Extract status
		status := "unknown"
		if m := taskStatusRe.FindStringSubmatch(content); len(m) > 1 {
			status = m[1]
		}

		// Extract task desc (first line after "## Task Description")
		task := "(no description)"
		if idx := strings.Index(content, "## Task Description"); idx >= 0 {
			rest := content[idx+len("## Task Description"):]
			if firstLine := strings.SplitN(strings.TrimSpace(rest), "\n", 2); len(firstLine) > 0 && firstLine[0] != "" {
				task = firstLine[0]
			}
		}

		fmt.Fprintf(&b, "- [%s] %s — %s\n", status, ts, utils.TruncateLine(task, 80))
		count++
	}

	if count == 0 {
		b.WriteString("(no tasks found)")
	}

	return fantasy.NewTextResponse(b.String()), nil
}

func (t *teamInfoTool) handleTaskResult(c *Coordinator, workspace, teamName, name string) (fantasy.ToolResponse, error) {
	agentDef, _, err := c.AgentPool().ResolveAgentName(name)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("agent %q not found: %v", name, err)), nil
	}
	resolvedName := strings.ToLower(agentDef.Name)

	dir := filepath.Join(workspace, tasksDir, teamName, resolvedName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fantasy.NewTextResponse(fmt.Sprintf("No completed tasks for agent %q yet.", resolvedName)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot read task dir: %v", err)), nil
	}

	// Newest first (timestamps sort lexicographically).
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		content := string(data)
		status := "unknown"
		if m := taskStatusRe.FindStringSubmatch(content); len(m) > 1 {
			status = m[1]
		}
		if status != "done" {
			continue
		}
		task := "(no description)"
		if idx := strings.Index(content, "## Task Description"); idx >= 0 {
			rest := strings.TrimSpace(content[idx+len("## Task Description"):])
			if firstLine := strings.SplitN(rest, "\n", 2); len(firstLine) > 0 && firstLine[0] != "" {
				task = firstLine[0]
			}
		}
		result := ""
		if idx := strings.Index(content, "## Result"); idx >= 0 {
			result = strings.TrimSpace(content[idx+len("## Result"):])
		}
		if result == "" || result == "(pending)" {
			result = "(no result recorded)"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Most recent completed task by %s:\n\n", resolvedName)
		fmt.Fprintf(&b, "**Task:** %s\n\n", task)
		b.WriteString("**Result:**\n\n")
		b.WriteString(utils.TruncateString(result, 8000))
		return fantasy.NewTextResponse(b.String()), nil
	}

	return fantasy.NewTextResponse(fmt.Sprintf("Agent %q has task records but none are completed yet.", resolvedName)), nil
}

func (t *teamInfoTool) handleTodoStatus(c *Coordinator, name string) (fantasy.ToolResponse, error) {
	agentDef, _, err := c.AgentPool().ResolveAgentName(name)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("agent %q not found: %v", name, err)), nil
	}

	allItems := c.taskTracker.TodoList().Items()
	var matched []*TodoItem
	for _, item := range allItems {
		if strings.EqualFold(item.Agent, agentDef.Name) {
			matched = append(matched, item)
		}
	}

	if len(matched) == 0 {
		return fantasy.NewTextResponse(fmt.Sprintf("No TODOs for agent %q.", agentDef.Name)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "TODO items for %s:\n\n", agentDef.Name)
	for _, item := range matched {
		icon := map[TaskStatus]string{
			TaskPending: "○", TaskInProgress: "◑", TaskVerifying: "◔", TaskDone: "●",
			TaskError: "✗", TaskBlocked: "⚠", TaskSkipped: "—", TaskPlanned: "◎", TaskPaused: "◐",
		}[item.Status]
		desc := item.Desc
		if item.Detail != "" && (item.Status == TaskError || item.Status == TaskBlocked) {
			desc = item.Detail
		}
		fmt.Fprintf(&b, "- %s %s: %s\n", icon, item.ID, utils.TruncateLine(desc, 100))
	}

	return fantasy.NewTextResponse(b.String()), nil
}

func (t *teamInfoTool) handleSessionSummary(c *Coordinator) (fantasy.ToolResponse, error) {
	ctx := c.buildTaskStatusContext()
	return fantasy.NewTextResponse(ctx), nil
}
