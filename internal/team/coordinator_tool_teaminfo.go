package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/utils"
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
			"task_contains": map[string]any{
				"type":        "string",
				"description": "Optional case-sensitive substring of the task description for task_result. Use this when the agent has multiple completed tasks and one exact result is required; omit it for the most recent result.",
			},
			"task_id": map[string]any{
				"type":        "string",
				"description": "Stable TODO task ID for task_result. Prefer this over agent/task_contains: it returns the authoritative typed result and is not affected by task-description escaping or Markdown publication timing.",
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
		Action       string `json:"action"`
		Agent        string `json:"agent"`
		TaskContains string `json:"task_contains"`
		TaskID       string `json:"task_id"`
		Limit        int    `json:"limit"`
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
		if c.ExecutionProfile().DisableHistoricalTaskReuse {
			return fantasy.NewTextResponse("Historical task inspection is disabled by the active execution profile. Use todo_status or task_result for tasks created in this run."), nil
		}
		return t.handleTaskHistory(workspace, teamName, args.Agent, args.Limit)
	case "task_result":
		if args.Agent == "" && args.TaskID == "" {
			return fantasy.NewTextErrorResponse("agent or task_id is required for task_result action"), nil
		}
		return t.handleTaskResult(c, workspace, teamName, args.Agent, args.TaskContains, args.TaskID, c.ExecutionProfile().DisableHistoricalTaskReuse)
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
	entries, err := taskHistoryEntries(workspace, teamName, agentName)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot read task dir: %v", err)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Task history for %s:\n\n", agentName)
	count := 0
	for _, entry := range entries {
		if count >= limit {
			fmt.Fprintf(&b, "\n... (%d more tasks)", len(entries)-count)
			break
		}
		data, err := os.ReadFile(entry.path)
		if err != nil {
			continue
		}

		ts := strings.TrimSuffix(entry.name, ".md")
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

func (t *teamInfoTool) handleTaskResult(c *Coordinator, workspace, teamName, name, taskContains, taskID string, disableHistoricalFallback bool) (fantasy.ToolResponse, error) {
	if taskID != "" {
		return taskResultByID(c, taskID)
	}
	agentDef, _, err := c.AgentPool().ResolveAgentName(name)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("agent %q not found: %v", name, err)), nil
	}
	resolvedName := strings.ToLower(agentDef.Name)

	// The TodoList is reconstructed from session state on restart and holds the
	// typed terminal result. It is therefore the API authority; task Markdown
	// is only a human-readable compatibility projection.
	if response, matched, candidates := completedTaskResultForAgent(c, resolvedName, taskContains); matched {
		return fantasy.NewTextResponse(response), nil
	} else if taskContains != "" && len(candidates) > 1 {
		return fantasy.NewTextResponse(fmt.Sprintf("task_result selector is ambiguous for agent %q; matching task IDs: %s. Retry with task_id.", resolvedName, strings.Join(candidates, ", "))), nil
	}
	if disableHistoricalFallback {
		return fantasy.NewTextResponse(fmt.Sprintf("No completed task matched agent %q and selector %q in this run. Historical task transcripts are disabled by the active execution profile.", resolvedName, taskContains)), nil
	}

	entries, err := taskHistoryEntries(workspace, teamName, resolvedName)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot read task dir: %v", err)), nil
	}

	for _, entry := range entries {
		data, err := os.ReadFile(entry.path)
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
		task := taskDescription(content)
		if !taskDescriptionMatches(task, taskContains) {
			continue
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

	return fantasy.NewTextResponse(fmt.Sprintf("No completed task matched agent %q and selector %q. Use task_id for an exact lookup.", resolvedName, taskContains)), nil
}

func taskResultByID(c *Coordinator, taskID string) (fantasy.ToolResponse, error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return fantasy.NewTextErrorResponse("task result store is unavailable"), nil
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.ID != taskID {
			continue
		}
		if item.Status != TaskDone {
			return fantasy.NewTextResponse(fmt.Sprintf("Task %q is %s, not completed.", taskID, item.Status)), nil
		}
		return fantasy.NewTextResponse(formatCompletedTaskResult(item)), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Task %q was not found. Use todo_status or task_history to discover available task IDs.", taskID)), nil
}

func completedTaskResultForAgent(c *Coordinator, agentName, taskContains string) (string, bool, []string) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return "", false, nil
	}
	items := c.taskTracker.TodoList().Items()
	var matches []*TodoItem
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item == nil || !strings.EqualFold(strings.TrimSpace(item.Agent), strings.TrimSpace(agentName)) || item.Status != TaskDone {
			continue
		}
		if !taskDescriptionMatches(item.Desc, taskContains) {
			continue
		}
		matches = append(matches, item)
	}
	if len(matches) == 0 {
		return "", false, nil
	}
	if taskContains != "" && len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, item := range matches {
			ids = append(ids, item.ID)
		}
		return "", false, ids
	}
	return formatCompletedTaskResult(matches[0]), true, nil
}

// inMemoryCompletedTaskResult remains for compatibility with callers and
// tests added during the Markdown-publication-race fix. New code should use
// completedTaskResultForAgent or taskResultByID.
func inMemoryCompletedTaskResult(c *Coordinator, agentName, taskContains string) (string, bool) {
	result, ok, _ := completedTaskResultForAgent(c, agentName, taskContains)
	return result, ok
}

func formatCompletedTaskResult(item *TodoItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Completed task %s by %s:\n\n**Task:** %s\n\n", item.ID, item.Agent, item.Desc)
	b.WriteString("**Result:**\n\n")
	if result := item.TypedResult; result != nil {
		// A sealed transcript supplements the typed result; it must not hide
		// the summary, findings, decisions, or stable handoff data. Consumers
		// use this API rather than constructing a task-output path and reading
		// it through a filesystem tool.
		b.WriteString(result.FormatForContext())
		if result.RawOutputRef != nil && (result.RawOutputRef.ID != "" || result.RawOutputRef.Path != "") {
			b.WriteString("\n\n")
			b.WriteString(formatVerbatimTranscriptManifest(result.RawOutputRef))
		}
	} else if strings.TrimSpace(item.Output) != "" {
		b.WriteString(item.Output)
	} else {
		b.WriteString("(completed without a recorded typed result)")
	}
	return b.String()
}

func taskDescription(content string) string {
	task := "(no description)"
	if idx := strings.Index(content, "## Task Description"); idx >= 0 {
		rest := strings.TrimSpace(content[idx+len("## Task Description"):])
		if firstLine := strings.SplitN(rest, "\n", 2); len(firstLine) > 0 && firstLine[0] != "" {
			task = firstLine[0]
		}
	}
	return task
}

func taskDescriptionMatches(task, selector string) bool {
	if selector == "" {
		return true
	}
	// Legacy callers historically copied a JSON-escaped description into the
	// selector. Normalizing only escaped quotes preserves the documented
	// substring behavior without making identity depend on serialization.
	selector = strings.ReplaceAll(selector, `\"`, `"`)
	task = strings.ReplaceAll(task, `\"`, `"`)
	return strings.Contains(task, selector)
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
			TaskError: "✗", TaskBlocked: "⚠", TaskProtocolIncomplete: "⚠", TaskSkipped: "—", TaskPlanned: "◎", TaskPaused: "◐",
		}[item.Status]
		desc := item.Desc
		if item.Status == TaskError || item.Status == TaskBlocked || item.Status == TaskProtocolIncomplete {
			if failure := FailureDisplayText(item); failure != "" {
				desc = failure
			}
		}
		fmt.Fprintf(&b, "- %s %s: %s\n", icon, item.ID, utils.TruncateLine(desc, 100))
	}

	return fantasy.NewTextResponse(b.String()), nil
}

func (t *teamInfoTool) handleSessionSummary(c *Coordinator) (fantasy.ToolResponse, error) {
	ctx := c.buildTaskStatusContext()
	return fantasy.NewTextResponse(ctx), nil
}
