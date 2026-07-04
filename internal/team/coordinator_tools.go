package team

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"os/exec"

	"encoding/json"
	"path/filepath"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
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
		Description: "Signal that you have completed the user's request and provide your final answer. Call this when you are done coordinating and have a complete response for the user. You MUST call this instead of just outputting text — your final answer goes in the response field.",
		Parameters: map[string]any{
			"response": map[string]any{
				"type":        "string",
				"description": "Your final answer to the user",
			},
		},
		Required: []string{"response"},
	}
}

func (t *finishTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	t.coordinator.lastStmWriteMu.Lock()
	workspace := t.coordinator.session.Workspace
	todoList := t.coordinator.taskTracker.TodoList()
	completed := todoList.CompletedCount()
	failed := todoList.ErrorCount()
	summary := fmt.Sprintf("[summary] %d/%d tasks done, %d rounds, %s elapsed",
		completed, completed+failed, t.coordinator.round,
		time.Since(t.coordinator.sessionTime).Round(time.Second))
	existing := LoadSTM(workspace)
	if existing == "" {
		existing = fmt.Sprintf("Session started at %s.", t.coordinator.sessionTime.Format(time.RFC3339))
	}
	newContent := appendSTMEntry(existing, summary, stmSectionProgress)
	_ = SaveSTM(workspace, TruncateSTM(newContent))
	t.coordinator.lastStmWrite = time.Now()
	t.coordinator.lastStmWriteMu.Unlock()

	t.coordinator.AutoExtractLTM(ctx)

	// Team-level acceptance check: an objective gate over the whole run. A
	// non-zero exit does not block finishing (the work is already done) but is
	// surfaced in the result and via a notifiable event so an unattended run's
	// failure is not silent.
	response := args.Response
	if accErr := t.coordinator.runAcceptance(ctx); accErr != nil {
		if t.coordinator.IsUnattended() {
			if t.coordinator.selfHealingAttempts < 2 {
				t.coordinator.selfHealingAttempts++
				msg := fmt.Sprintf("Acceptance check failed (attempt %d/2). Initiating self-healing. Error: %v", t.coordinator.selfHealingAttempts, accErr)
				t.coordinator.report(t.coordinator.newEvent("error").withMessage(msg))
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Acceptance check failed: %v. Please analyze the failure log, modify files/re-run tasks to fix the issues, and call finish again.", accErr)), nil
			}
			// Self-healing attempts exhausted, run rollback
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
		} else {
			// Interactive mode: preserve standard behavior
			note := fmt.Sprintf("\n\n⚠️ ACCEPTANCE CHECK FAILED: %v", accErr)
			response += note
			t.coordinator.report(t.coordinator.newEvent("error").withMessage("acceptance check failed: " + accErr.Error()))
		}
	}

	return fantasy.NewTextResponse(fmt.Sprintf("FINISHED:%s", response)), nil
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

const maxDelegationDepth = 5

// delegationChain returns the ancestor chain of agent names that led to the
// current request_agent call. The chain is propagated through the context
// (see delegationChainKey) rather than the coordinator's mutable snapshot,
// since the snapshot only ever holds the single currently-running agent's
// flat name and gets overwritten on every nested agent run.
func delegationChain(ctx context.Context, callerName string) []string {
	if raw, ok := ctx.Value(delegationChainKey{}).(string); ok && raw != "" {
		return strings.Split(raw, "/")
	}
	if callerName == "" {
		return nil
	}
	return []string{callerName}
}

// checkDelegationLimits rejects a delegation to selected if it would exceed
// the maximum chain depth or would re-introduce an agent already present in
// the chain (a delegation cycle).
func checkDelegationLimits(chain []string, selected string) error {
	if len(chain) >= maxDelegationDepth {
		return fmt.Errorf("maximum delegation depth (%d) reached to prevent infinite recursion", maxDelegationDepth)
	}
	for _, a := range chain {
		if strings.EqualFold(a, selected) {
			return fmt.Errorf("circular delegation detected: agent '%s' is already in the delegation chain (%s)", selected, strings.Join(chain, "/"))
		}
	}
	return nil
}

type requestAgentTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *requestAgentTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "request_agent",
		Description: "Request the coordinator to delegate a task to another agent. Describe what needs to be done (goal) and any constraints. The coordinator will select the best agent and return the result. You are paused until the result is ready.",
		Parameters: map[string]any{
			"goal": map[string]any{
				"type":        "string",
				"description": "The goal of the task — what should be achieved",
			},
			"constraints": map[string]any{
				"type":        "string",
				"description": "Non-obvious restrictions the sub-agent must respect",
			},
			"agent": map[string]any{
				"type":        "string",
				"description": "Name of the specific agent to assign this task to. If omitted, the best available agent is selected automatically.",
			},
		},
		Required: []string{"goal"},
	}
}

func (t *requestAgentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Goal        string `json:"goal"`
		Constraints string `json:"constraints"`
		Agent       string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Goal == "" {
		return fantasy.NewTextErrorResponse("goal is required"), nil
	}

	callerName := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
	parentID := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.TodoID })

	taskDesc := args.Goal
	if args.Constraints != "" {
		taskDesc += "\nconstraints: " + args.Constraints
	}

	c := t.coordinator

	var selected string
	if args.Agent != "" {
		def, _, err := c.resolveAgentName(args.Agent)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown agent %q: %v", args.Agent, err)), nil
		}
		selected = def.Name
	} else {
		var err error
		selected, err = c.selectAgentForGoal(ctx, args.Goal)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("could not select agent: %v", err)), nil
		}
	}

	chainAgents := delegationChain(ctx, callerName)
	if err := checkDelegationLimits(chainAgents, selected); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	subLabel := strings.Join(append(chainAgents, selected), "/")
	agentKey := strings.ToLower(selected)
	if cachedOutput, cachedDesc, ok := c.lookupTaskCacheAllGenerations(ctx, agentKey, taskDesc); ok {
		log.Printf("[INFO] request_agent cache hit: agent=%q, task=%q, matched=%q", selected, taskDesc, cachedDesc)
		return fantasy.NewTextResponse(fmt.Sprintf("[CACHED RESULT] Task: '%s'\n\n%s", truncateTaskDesc(cachedDesc), cachedOutput)), nil
	}

	todoItems := c.taskTracker.TodoList().AddBatch([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{{Agent: subLabel, Desc: taskDesc, Model: "", Source: TaskSourceSubagent, ParentID: parentID}})
	subTodoID := todoItems[0].ID

	c.taskTracker.TodoList().UpdateStatus(subTodoID, TaskInProgress, "")
	if parentID != "" {
		c.taskTracker.TodoList().UpdateStatus(parentID, TaskPaused, "")
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("start").withAgent(subLabel).withMessage(taskDesc).withTodoID(subTodoID))

	// Inject subTodoID so events from runAgentWithStatusAndHistory attribute to the right item.
	execCtx := context.WithValue(ctx, todoIDKey{}, subTodoID)
	execCtx = context.WithValue(execCtx, delegationChainKey{}, subLabel)
	output, err := c.ExecuteSubAgent(execCtx, selected, args.Goal, args.Constraints)
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(subTodoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	c.storeTaskCache(agentKey, taskDesc, output)

	if parentID != "" {
		c.taskTracker.TodoList().UpdateStatus(parentID, TaskInProgress, "")
	}
	c.taskTracker.TodoList().UpdateStatusAndOutput(subTodoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	return fantasy.NewTextResponse(output), nil
}

func (c *Coordinator) selectAgentForGoal(ctx context.Context, goal string) (string, error) {
	s := c.Sidecar()
	workers := c.uniqueWorkerDefs()
	if len(workers) == 0 {
		return "", fmt.Errorf("no workers available")
	}
	if len(workers) == 1 {
		return workers[0].Name, nil
	}

	var workersList strings.Builder
	for _, w := range workers {
		fmt.Fprintf(&workersList, "- %s", w.Name)
		if w.Description != "" {
			fmt.Fprintf(&workersList, ": %s", w.Description)
		}
		if w.Tools != "" {
			fmt.Fprintf(&workersList, " (tools: %s)", w.Tools)
		}
		workersList.WriteString("\n")
	}

	if s != nil {
		prompt := fmt.Sprintf("Select the single best agent name for this task:\n\nGoal: %s\n\nAvailable agents:\n%s\nReturn ONLY the agent name.", goal, workersList.String())
		selection, err := s.Execute(ctx, prompt)
		if err == nil {
			selection = strings.TrimSpace(selection)
			for _, w := range workers {
				if strings.EqualFold(w.Name, selection) {
					return w.Name, nil
				}
			}
		}
	}

	for _, w := range workers {
		if strings.Contains(strings.ToLower(w.Description), "helper") || strings.Contains(strings.ToLower(w.Name), "helper") {
			return w.Name, nil
		}
	}
	return workers[0].Name, nil
}

func (c *Coordinator) ExecuteSubAgent(ctx context.Context, name string, task string, constraints string) (string, error) {
	if c.IsWrapUp() {
		return "", fmt.Errorf("wrap-up in progress: cannot create sub-agent")
	}

	agentDef, _, err := c.resolveAgentName(name)
	if err != nil {
		agentDef = &agent.AgentDef{
			Name:        name,
			Description: "Sub-agent for: " + task,
			Role:        "worker",
			Tools:       "",
			MaxRetries:  -1,
		}
	}

	agentDef = c.injectWorkerContext(ctx, agentDef)

	subAgModelID := c.resolveAgentModel(agentDef, "")
	ag, err := agent.CreateAgent(ctx, c.providerManager.GetProvider(subAgModelID), agent.AgentConfig{
		Def:        agentDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   agent.DefaultMaxSteps,
	}, agent.SelectTools(c.coreTools, agentDef.Tools))
	if err != nil {
		return "", fmt.Errorf("failed to create sub-agent %q: %w", name, err)
	}

	prompt := "## Goal\n\n" + task
	if constraints != "" {
		prompt += "\n\n## Constraints\n\n" + constraints
	}
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	prompt = c.appendSkillContext(prompt, agentDef, agentDef.Name, task, todoID)

	timing := &taskTiming{}
	timing.reset()

	output, _, err := c.runAgentWithStatusAndHistory(ctx, ag, name, prompt, nil, timing)
	if err != nil {
		return "", err
	}
	return output, nil
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

	batch := make([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}, len(items))
	for i, desc := range items {
		batch[i] = struct {
			Agent    string
			Desc     string
			Model    string
			Source   string
			ParentID string
		}{Agent: callerName, Desc: desc, Model: resolvedModel, Source: TaskSourceAgent, ParentID: parentID}
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

type memorySaveLTMWrapper struct {
	original    fantasy.AgentTool
	coordinator *Coordinator
}

func (t *memorySaveLTMWrapper) Info() fantasy.ToolInfo {
	return t.original.Info()
}

func (t *memorySaveLTMWrapper) ProviderOptions() fantasy.ProviderOptions {
	return t.original.ProviderOptions()
}

func (t *memorySaveLTMWrapper) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.original.SetProviderOptions(opts)
}

func (t *memorySaveLTMWrapper) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := t.original.Run(ctx, call)
	if err != nil || resp.IsError {
		return resp, err
	}

	var args struct {
		Content  string `json:"content"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil || args.Content == "" {
		return resp, nil
	}

	section := ClassifyLTMEntry(args.Content, "finding")
	if section == "" {
		section = ltmSectionPatterns
	}

	t.coordinator.ltmWriteMu.Lock()
	defer t.coordinator.ltmWriteMu.Unlock()

	workspace := t.coordinator.session.Workspace
	existingLTM := LoadLTM(workspace, t.coordinator.session.Config.Name)
	entry := formatLTMEntry(args.Content)
	existingLTMSections := ParseSTMSections(existingLTM)
	if hasLTREntry(existingLTMSections, section, entry) {
		return resp, nil
	}

	newLTM := appendSTMEntry(existingLTM, entry, section)
	pruned := PruneLTM(newLTM)
	if err := SaveLTM(workspace, t.coordinator.session.Config.Name, TruncateLTM(pruned)); err != nil {
		log.Printf("warning: memory_save LTM write-back failed: %v", err)
	}

	return resp, nil
}

type submitPlanTool struct {
	coordinator *Coordinator
	todoID      string
}

func (t *submitPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *submitPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *submitPlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "submit_plan",
		Description: "Submit your task execution plan for coordinator review. The plan should be a numbered list of concrete steps with brief descriptions. Do NOT include any execution results — only the plan. After submitting, wait for the coordinator to approve, modify, or reject your plan before executing.",
		Parameters: map[string]any{
			"plan": map[string]any{
				"type":        "string",
				"description": "The task execution plan as a numbered list of steps with descriptions.",
			},
		},
		Required: []string{"plan"},
	}
}

func (t *submitPlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Plan == "" {
		return fantasy.NewTextErrorResponse("plan is required"), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	existing := t.coordinator.pendingPlans[t.todoID]
	if existing != nil {
		existing.PlanText = args.Plan
		existing.Status = "submitted"
	} else {
		t.coordinator.pendingPlans[t.todoID] = &PlanEntry{
			TodoID:   t.todoID,
			PlanText: args.Plan,
			Status:   "submitted",
		}
	}
	t.coordinator.pendingPlansMu.Unlock()
	if t.coordinator.forcePlanFirst {
		return fantasy.NewTextResponse("Plan submitted. Awaiting review."), nil
	}
	return fantasy.NewTextResponse("Plan submitted. Await coordinator review."), nil
}

type stmWriteTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *stmWriteTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "stm_write",
		Description: "Write to short-term memory (stm.md), a shared workspace file visible to all agents in the current session. Use append mode to add new information, or replace mode to overwrite. This memory is session-scoped and will be archived when the session ends.",
		Parameters: map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The content to write to short-term memory",
			},
			"mode": map[string]any{
				"type":        "string",
				"description": "Write mode: \"append\" (add to end, default) or \"replace\" (overwrite entire file)",
				"enum":        []string{"append", "replace"},
			},
		},
		Required: []string{"content"},
	}
}

func (t *stmWriteTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Content string `json:"content"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	mode := args.Mode
	if mode == "" {
		mode = "append"
	}

	workspace := t.coordinator.session.Workspace
	var newContent string
	switch mode {
	case "replace":
		newContent = TruncateSTM(args.Content)
	default:
		existing := LoadSTM(workspace)
		if existing == "" {
			newContent = TruncateSTM(args.Content)
		} else {
			newContent = TruncateSTM(existing + "\n" + args.Content)
		}
	}

	if err := SaveSTM(workspace, newContent); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write stm.md: %v", err)), nil
	}
	t.coordinator.lastStmWriteMu.Lock()
	t.coordinator.lastStmWrite = time.Now()
	t.coordinator.lastStmWriteMu.Unlock()

	verb := "Appended to"
	if mode == "replace" {
		verb = "Replaced"
	}
	return fantasy.NewTextResponse(fmt.Sprintf("%s short-term memory (stm.md)", verb)), nil
}

type ltmUpdateTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *ltmUpdateTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "ltm_update",
		Description: "Update long-term memory (ltm.md), a persistent file shared across sessions for this team. Each entry is appended to the specified section so it can be retrieved in future sessions.",
		Parameters: map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The knowledge to record (one concise fact, decision, or pattern per call)",
			},
			"section": map[string]any{
				"type":        "string",
				"description": "Which long-term memory section to append to",
				"enum": []string{
					ltmSectionConventions,
					ltmSectionArchitecture,
					ltmSectionPatterns,
					ltmSectionIssues,
					ltmSectionFiles,
					ltmSectionTools,
				},
			},
		},
		Required: []string{"content", "section"},
	}
}

func (t *ltmUpdateTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Content string `json:"content"`
		Section string `json:"section"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}
	if args.Section == "" {
		return fantasy.NewTextErrorResponse("section is required"), nil
	}

	// Validate section against the enum defined in Info()
	validSections := map[string]bool{
		ltmSectionConventions:  true,
		ltmSectionArchitecture: true,
		ltmSectionPatterns:     true,
		ltmSectionIssues:       true,
		ltmSectionFiles:        true,
		ltmSectionTools:        true,
	}
	if !validSections[args.Section] {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid section %q; must be one of: %s, %s, %s, %s, %s, %s",
			args.Section,
			ltmSectionConventions, ltmSectionArchitecture, ltmSectionPatterns,
			ltmSectionIssues, ltmSectionFiles, ltmSectionTools)), nil
	}

	entry := formatLTMEntry(args.Content)
	workspace := t.coordinator.session.Workspace
	t.coordinator.ltmWriteMu.Lock()
	existing := LoadLTM(workspace, t.coordinator.session.Config.Name)
	newContent := TruncateLTM(appendLTMEntry(existing, entry, args.Section))
	err := SaveLTM(workspace, t.coordinator.session.Config.Name, newContent)
	t.coordinator.ltmWriteMu.Unlock()
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write ltm.md: %v", err)), nil
	}

	return fantasy.NewTextResponse(fmt.Sprintf("Appended to long-term memory section %q", args.Section)), nil
}

type approvePlanTool struct {
	coordinator *Coordinator
}

func (t *approvePlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *approvePlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *approvePlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "approve_plan",
		Description: "Approve a submitted task plan and execute it immediately. The plan must have been submitted by an agent via submit_plan. The agent that submitted the plan will execute the approved plan.",
		Parameters: map[string]any{
			"todo_id": map[string]any{
				"type":        "string",
				"description": "The todo ID of the submitted plan to approve.",
			},
		},
		Required: []string{"todo_id"},
	}
}

func (t *approvePlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TodoID string `json:"todo_id"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.TodoID == "" {
		return fantasy.NewTextErrorResponse("todo_id is required"), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	entry, ok := t.coordinator.pendingPlans[args.TodoID]
	if !ok {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse("plan not found for todo_id: " + args.TodoID), nil
	}
	if entry.Status != "submitted" {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse(fmt.Sprintf("plan already %s", entry.Status)), nil
	}
	entry.Status = "approved"
	var approvedTask TaskDef
	if entry.Task.Agent != "" {
		approvedTask = cloneTaskDef(entry.Task)
	} else {
		approvedTask = TaskDef{
			Agent: entry.Agent,
			Goal:  entry.Goal,
		}
	}
	approvedTask.PlanFirst = true
	approvedTask.PlanID = entry.TodoID
	todoID := entry.TodoID
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.taskTracker.TodoList().UpdateStatus(todoID, TaskPlanned, "")
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))

	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{approvedTask})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Plan approved and executed.\n\n%s", result)), nil
}

type modifyPlanTool struct {
	coordinator *Coordinator
}

func (t *modifyPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *modifyPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *modifyPlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "modify_plan",
		Description: "Modify a submitted task plan and execute the modified version. Provide the corrected plan steps. The agent will execute the modified plan.",
		Parameters: map[string]any{
			"todo_id": map[string]any{
				"type":        "string",
				"description": "The todo ID of the submitted plan to modify.",
			},
			"plan": map[string]any{
				"type":        "string",
				"description": "The modified task execution plan as a numbered list of steps.",
			},
		},
		Required: []string{"todo_id", "plan"},
	}
}

func (t *modifyPlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TodoID string `json:"todo_id"`
		Plan   string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.TodoID == "" || args.Plan == "" {
		return fantasy.NewTextErrorResponse("todo_id and plan are required"), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	entry, ok := t.coordinator.pendingPlans[args.TodoID]
	if !ok {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse("plan not found for todo_id: " + args.TodoID), nil
	}
	entry.Status = "modified"
	entry.PlanText = args.Plan
	var modifiedTask TaskDef
	if entry.Task.Agent != "" {
		modifiedTask = cloneTaskDef(entry.Task)
	} else {
		modifiedTask = TaskDef{
			Agent: entry.Agent,
			Goal:  entry.Goal,
		}
	}
	modifiedTask.PlanFirst = true
	modifiedTask.PlanID = entry.TodoID
	todoID := entry.TodoID
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.report(t.coordinator.newEvent("step").withMessage(fmt.Sprintf("plan %s modified by coordinator", todoID)))

	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{modifiedTask})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Plan modified and executed.\n\n%s", result)), nil
}

type rejectPlanTool struct {
	coordinator *Coordinator
}
