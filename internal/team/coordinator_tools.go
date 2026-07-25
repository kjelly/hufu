package team

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"os/exec"

	"encoding/json"
	"path/filepath"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/tools"
	"github.com/anomalyco/hufu/internal/utils"
)

type runAgentsTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *runAgentsTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "agent",
		Description: "Delegate tasks to team workers. Runs all tasks in parallel. Returns structured results from each agent.",
		Parameters: map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"properties":           buildAgentTaskProperties(t.coordinator.workerNameList(), len(t.coordinator.modelList) > 0, filepath.Join(t.coordinator.session.Workspace, sharedDir)),
					"required":             []string{"agent"},
					"additionalProperties": false,
				},
			},
		},
		Required: []string{"tasks"},
	}
}

func (t *runAgentsTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Tasks []TaskDef `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if len(args.Tasks) == 0 {
		return fantasy.NewTextErrorResponse("no tasks provided"), nil
	}
	for _, t := range args.Tasks {
		if t.Goal == "" {
			return fantasy.NewTextErrorResponse("each task requires 'goal'"), nil
		}
	}

	result, err := t.coordinator.ExecuteTasks(ctx, args.Tasks)
	if err != nil {
		// Keep the per-task detail: formatTaskResults returns the full report
		// even on "all tasks failed", and discarding it left the coordinator
		// blind to why tasks failed, forcing guesswork re-delegation.
		if strings.TrimSpace(result) != "" {
			return fantasy.NewTextErrorResponse(result + "\n\nERROR: " + err.Error()), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(result), nil
}

type finishTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *finishTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "finish",
		Description: "Signal that you have completed the user's request and provide your final answer. Call this when you are done coordinating and have a complete response for the user. You MUST call this instead of just outputting text — your final answer goes in the response field. If worker tasks failed or were blocked, first fix them; only set acknowledge_failed_tasks when you must end with a clearly disclosed partial result.",
		Parameters: map[string]any{
			"response": map[string]any{
				"type":        "string",
				"description": "Your final answer to the user",
			},
			"acknowledge_failed_tasks": map[string]any{
				"type":        "boolean",
				"description": "Set true only when ending with unresolved failed or blocked tasks. The final response will include their IDs and errors.",
			},
		},
		Required: []string{"response"},
	}
}

func (t *finishTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Response               string `json:"response"`
		AcknowledgeFailedTasks bool   `json:"acknowledge_failed_tasks"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	todoList := t.coordinator.taskTracker.TodoList()
	failedTasks := failedTodoItems(todoList.Items())
	prof := t.coordinator.ExecutionProfile()

	if len(failedTasks) > 0 {
		if prof.RequireEvidenceManifest || prof.StrictPolicy {
			return fantasy.NewTextErrorResponse("RequireEvidenceManifest policy violation: cannot finish while worker tasks failed or were blocked:\n" + formatFailedTasks(failedTasks)), nil
		}
		if !args.AcknowledgeFailedTasks {
			return fantasy.NewTextErrorResponse("cannot finish successfully while worker tasks failed or were blocked:\n" + formatFailedTasks(failedTasks) + "\nFix or re-delegate these tasks. If the user needs a partial result, call finish again with acknowledge_failed_tasks:true; hufu will append the unresolved-task warning."), nil
		}
	}

	if t.coordinator.terminalSessionMgr != nil {
		// A resumed terminal can belong to a prior execution run. It remains an
		// unresolved resource, so a new run must not sidestep the gate by using
		// a new executionRunID.
		if err := t.coordinator.terminalSessionMgr.RequireNoLeaks(""); err != nil {
			return fantasy.NewTextErrorResponse("cannot finish while terminal sessions remain unresolved: " + err.Error()), nil
		}
	}

	completed := todoList.CompletedCount()
	failed := todoList.ErrorCount()
	summary := fmt.Sprintf("[summary] %d/%d tasks done, %d rounds, %s elapsed",
		completed, completed+failed, t.coordinator.totalRounds(),
		time.Since(t.coordinator.sessionTime).Round(time.Second))
	_ = t.coordinator.updateSTM(func(existing string) string {
		if existing == "" {
			existing = fmt.Sprintf("Session started at %s.", t.coordinator.sessionTime.Format(time.RFC3339))
		}
		newContent := appendSTMEntry(existing, summary, stmSectionProgress)
		return TruncateSTM(newContent)
	})

	t.coordinator.AutoExtractLTM(ctx)

	// Team-level acceptance check: an objective gate over the whole run. A
	// non-zero exit does not block finishing (the work is already done) but is
	// surfaced in the result and via a notifiable event so an unattended run's
	// failure is not silent.
	response := args.Response
	if len(failedTasks) > 0 {
		response += "\n\n⚠️ UNRESOLVED TASKS\n" + formatFailedTasks(failedTasks)
	}

	if prof.RequireClosedTerminals && t.coordinator.terminalSessionMgr != nil {
		if err := t.coordinator.terminalSessionMgr.RequireNoLeaks(""); err != nil {
			return fantasy.NewTextErrorResponse("RequireClosedTerminals policy violation: active terminal sessions remain open. Close all terminal sessions before finishing: " + err.Error()), nil
		}
	}

	if prof.RequireEvidenceManifest {
		items := t.coordinator.TaskTracker().TodoList().Items()
		completedCount := 0
		for _, item := range items {
			if item != nil && item.Status == TaskDone {
				completedCount++
				hasEvidence := item.VerifyResult != nil || (item.TypedResult != nil && len(item.TypedResult.Evidence) > 0)
				if !hasEvidence {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("RequireEvidenceManifest policy violation: completed task %q (%s) is missing verification evidence.", item.ID, item.Desc)), nil
				}
			}
		}
		if completedCount == 0 {
			return fantasy.NewTextErrorResponse("RequireEvidenceManifest policy violation: no completed tasks with verification evidence recorded."), nil
		}
	}

	if accErr := t.coordinator.runAcceptance(ctx); accErr != nil {
		if prof.AcceptanceMode == AcceptanceBlocking || t.coordinator.IsUnattended() {
			if t.coordinator.selfHealingAttempts < 2 {
				t.coordinator.selfHealingAttempts++
				msg := fmt.Sprintf("Acceptance check failed (attempt %d/2). Initiating self-healing. Error: %v", t.coordinator.selfHealingAttempts, accErr)
				t.coordinator.report(t.coordinator.newEvent("error").withMessage(msg))
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Acceptance check failed: %v. Please analyze the failure log, modify files/re-run tasks to fix the issues, and call finish again.", accErr)), nil
			}
			if t.coordinator.IsUnattended() {
				msg := fmt.Sprintf("Acceptance check failed after %d self-healing attempts. Initiating rollback...", t.coordinator.selfHealingAttempts)
				t.coordinator.report(t.coordinator.newEvent("error").withMessage(msg))
				if rollErr := t.coordinator.runRollback(ctx); rollErr != nil {
					rollMsg := fmt.Sprintf("Rollback failed: %v", rollErr)
					t.coordinator.report(t.coordinator.newEvent("error").withMessage(rollMsg))
					response += fmt.Sprintf("\n\n⚠️ ACCEPTANCE CHECK FAILED: %v\n⚠️ ROLLBACK FAILED: %v", accErr, rollErr)
				} else {
					t.coordinator.report(t.coordinator.newEvent("error").withMessage("Workspace rolled back successfully due to acceptance check failure."))
					response += fmt.Sprintf("\n\n⚠️ ACCEPTANCE CHECK FAILED: %v\n✓ Workspace rolled back successfully.", accErr)
				}
			}
			if prof.AcceptanceMode == AcceptanceBlocking {
				t.coordinator.report(t.coordinator.newEvent("error").withMessage("acceptance check failed (blocking): " + accErr.Error()))
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Acceptance check failed (blocking): %v", accErr)), nil
			}
		} else {
			// Interactive mode: preserve standard behavior
			note := fmt.Sprintf("\n\n⚠️ ACCEPTANCE CHECK FAILED: %v", accErr)
			response += note
			t.coordinator.report(t.coordinator.newEvent("error").withMessage("acceptance check failed: " + accErr.Error()))
		}
	}

	t.coordinator.finishCalled.Store(true)
	return fantasy.NewTextResponse(fmt.Sprintf("FINISHED:%s", response)), nil
}

func failedTodoItems(items []*TodoItem) []*TodoItem {
	failed := make([]*TodoItem, 0)
	for _, item := range items {
		if item != nil && (item.Status == TaskError || item.Status == TaskBlocked) {
			failed = append(failed, item)
		}
	}
	return failed
}

func formatFailedTasks(items []*TodoItem) string {
	var b strings.Builder
	for _, item := range items {
		detail := strings.TrimSpace(item.Detail)
		if detail == "" {
			detail = "no failure detail recorded"
		}
		fmt.Fprintf(&b, "- Task %s (%s, %s): %s\n", item.ID, item.Agent, item.Status, utils.TruncateString(detail, 500))
	}
	return strings.TrimRight(b.String(), "\n")
}

// runAcceptance runs the team's optional acceptance command in the project dir
// and returns a non-nil error if it exits non-zero. No-op when unset.
func (c *Coordinator) runAcceptance(parentCtx context.Context) error {
	cmd := strings.TrimSpace(c.acceptanceCmd)
	if cmd == "" {
		return nil
	}
	shell := "sh"
	if c.session != nil && c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}
	timeout := time.Duration(c.session.Config.Timeout) * time.Second
	if timeout <= 0 || timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	ex := exec.CommandContext(ctx, shell, "-c", cmd)
	if c.projectDir != "" {
		ex.Dir = c.projectDir
	}
	out, err := ex.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + utils.TruncateString(detail, 500)
		}
		return fmt.Errorf("%v%s", err, detail)
	}
	return nil
}

// runRollback runs the team's optional rollback command or default git rollback.
func (c *Coordinator) runRollback(parentCtx context.Context) error {
	cmd := strings.TrimSpace(c.rollbackCmd)
	shell := "sh"
	if c.session != nil && c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}
	timeout := time.Duration(c.session.Config.Timeout) * time.Second
	if timeout <= 0 || timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	if cmd == "" {
		// Default rollback: git reset --hard and git clean -fd if it's a git repo
		gitDir := filepath.Join(c.projectDir, ".git")
		if _, err := os.Stat(gitDir); err == nil {
			cmd = "git reset --hard && git clean -fd"
		} else {
			return fmt.Errorf("no custom rollback command set and no git repository found in workspace")
		}
	}

	ex := exec.CommandContext(ctx, shell, "-c", cmd)
	if c.projectDir != "" {
		ex.Dir = c.projectDir
	}
	out, err := ex.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + utils.TruncateString(detail, 500)
		}
		return fmt.Errorf("%v%s", err, detail)
	}
	return nil
}

type loadSkillTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *loadSkillTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "load_skill",
		Description: "Load the full content of a skill by name. Use this when you need detailed instructions from a skill before planning delegation. The skill content will help you understand how to instruct workers properly.",
		Parameters: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The skill name to load (e.g. 'git-commit')",
			},
		},
		Required: []string{"name"},
	}
}

func (t *loadSkillTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Name == "" {
		return fantasy.NewTextErrorResponse("skill name is required"), nil
	}

	agentName := "coordinator"
	if name, _ := ctx.Value(tools.AgentNameKey).(string); name != "" {
		agentName = name
	}

	nameLower := strings.ToLower(args.Name)
	skills := t.coordinator.getSkills()
	for _, s := range skills {
		if strings.ToLower(s.Name) == nameLower {
			t.coordinator.recordSkillUsage(s.Name, agentName)

			if todoID, _ := ctx.Value(todoIDKey{}).(string); todoID != "" {
				t.coordinator.taskTracker.TodoList().AddLoadedSkill(todoID, s.Name)
				t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))
			}

			return fantasy.NewTextResponse(fmt.Sprintf("Skill: %s\nFile: %s\n\n%s", s.Name, s.Path, s.Content)), nil
		}
	}

	available := make([]string, len(skills))
	for i, s := range skills {
		available[i] = s.Name
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("skill %q not found (available: %v)", args.Name, available)), nil
}

type todoTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *todoTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "todo",
		Description: "Manage your task list to track progress. Create items to plan your work, update their status as you progress, and list your items to review.",
		Parameters: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Action to perform: create, update, or list",
			},
			"items": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Task descriptions to create (used with action=create)",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "The TODO item ID to update (used with action=update)",
			},
			"status": map[string]any{
				"type":        "string",
				"description": "New status: in_progress or done (used with action=update)",
			},
			"detail": map[string]any{
				"type":        "string",
				"description": "Optional detail or note (used with action=update)",
			},
		},
		Required: []string{"action"},
	}
}

func (t *todoTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Action string   `json:"action"`
		Items  []string `json:"items"`
		ID     string   `json:"id"`
		Status string   `json:"status"`
		Detail string   `json:"detail"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	callerName := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
	if callerName == "" {
		callerName = "agent"
	}

	switch args.Action {
	case "create":
		return t.handleCreate(callerName, args.Items)
	case "update":
		return t.handleUpdate(callerName, args.ID, args.Status, args.Detail)
	case "list":
		return t.handleList(callerName)
	default:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown action %q (valid: create, update, list)", args.Action)), nil
	}
}

func (t *todoTool) handleCreate(callerName string, items []string) (fantasy.ToolResponse, error) {
	if len(items) == 0 {
		return fantasy.NewTextErrorResponse("items is required for create action"), nil
	}

	resolvedModel := t.coordinator.resolveCurrentAgentModel(callerName)
	parentID := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.TodoID })

	batch := make([]TodoSpec, len(items))
	for i, desc := range items {
		batch[i] = TodoSpec{Agent: callerName, Desc: desc, Model: resolvedModel, Source: TaskSourceAgent, ParentID: parentID}
	}

	added := t.coordinator.taskTracker.TodoList().AddBatch(batch)
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))

	var b strings.Builder
	b.WriteString("Created TODO items:\n")
	for _, item := range added {
		fmt.Fprintf(&b, "- %s: %s [%s]\n", item.ID, item.Desc, item.Status)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func (t *todoTool) handleUpdate(callerName string, id string, status string, detail string) (fantasy.ToolResponse, error) {
	if id == "" {
		return fantasy.NewTextErrorResponse("id is required for update action"), nil
	}

	todoItems := t.coordinator.taskTracker.TodoList().Items()
	var targetItem *TodoItem
	for _, item := range todoItems {
		if item.ID == id {
			targetItem = item
			break
		}
	}
	if targetItem == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("TODO item %q not found", id)), nil
	}
	if targetItem.Agent != callerName {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot update TODO item %q: it belongs to agent %q", id, targetItem.Agent)), nil
	}

	var taskStatus TaskStatus
	switch status {
	case "in_progress":
		taskStatus = TaskInProgress
	case "done":
		taskStatus = TaskDone
	default:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid status %q (valid: in_progress, done)", status)), nil
	}

	// TaskDone and TaskSkipped are terminal states - cannot be updated.
	// TaskError can only be updated to in_progress (for retries).
	if targetItem.Status == TaskDone || targetItem.Status == TaskSkipped {
		return fantasy.NewTextErrorResponse(
			fmt.Sprintf("cannot update completed TODO %q (status: %s): create a new task instead", id, targetItem.Status),
		), nil
	}
	if targetItem.Status == TaskError && status != "in_progress" {
		return fantasy.NewTextErrorResponse(
			fmt.Sprintf("cannot update error TODO %q to %s: only in_progress is allowed for retries", id, status),
		), nil
	}

	t.coordinator.taskTracker.TodoList().UpdateStatus(id, taskStatus, detail)
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))

	return fantasy.NewTextResponse(fmt.Sprintf("Updated TODO %s to %s", id, taskStatus)), nil
}

func (t *todoTool) handleList(callerName string) (fantasy.ToolResponse, error) {
	todoItems := t.coordinator.taskTracker.TodoList().Items()
	var myItems []*TodoItem
	for _, item := range todoItems {
		if item.Agent == callerName {
			myItems = append(myItems, item)
		}
	}
	if len(myItems) == 0 {
		return fantasy.NewTextResponse("No TODO items."), nil
	}

	var b strings.Builder
	for _, item := range myItems {
		fmt.Fprintf(&b, "- %s: %s [%s]", item.ID, item.Desc, item.Status)
		if item.Detail != "" {
			fmt.Fprintf(&b, " (%s)", item.Detail)
		}
		b.WriteString("\n")
	}
	return fantasy.NewTextResponse(b.String()), nil
}
