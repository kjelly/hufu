package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/skill"
)

type TaskDef struct {
	Agent        string   `json:"agent"`
	Task         string   `json:"task"`
	ContextFiles []string `json:"context_files,omitempty"`
}

type DirectAgentResult struct {
	AgentName string
	Output    string
	Error     error
}

type Coordinator struct {
	mu       sync.RWMutex
	session  *TeamSession
	provider *agent.OllamaProvider
	mcpManager *mcp.MCPToolManager
	coreTools           []fantasy.AgentTool
	agentCache          map[string]fantasy.Agent
	agentCacheMu        sync.RWMutex
	round               int
	verbose             bool
	reportStatus        StatusReporter
	sessionData         *SessionData
	taskTracker         *TaskTracker
	skills              []*skill.SkillDef
	conversationHistory   []fantasy.Message
	conversationHistoryMu sync.Mutex
	projectDir            string
	wrapUp              atomic.Int32
}

func NewCoordinator(session *TeamSession, defaultProviderURL string, mcpManager *mcp.MCPToolManager, verbose bool) *Coordinator {
	projectDir, _ := os.Getwd()
	coreTools := agent.BuildAllAgentTools(projectDir)
	prov, err := agent.NewOllamaProvider(defaultProviderURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ Failed to create Ollama provider: %v\n", err)
		os.Exit(1)
	}
	return &Coordinator{
		provider:     prov,
		session:      session,
		mcpManager:   mcpManager,
		coreTools:    coreTools,
		agentCache:   make(map[string]fantasy.Agent),
		verbose:      verbose,
		reportStatus: func(event StatusEvent) {},
		taskTracker:  NewTaskTracker(),
		skills:       session.Skills,
		projectDir:   projectDir,
	}
}

func (c *Coordinator) SetStatusReporter(fn StatusReporter) {
	if fn != nil {
		c.reportStatus = fn
	}
}

func (c *Coordinator) report(event StatusEvent) {
	c.reportStatus(event)
}

func (c *Coordinator) newEvent(eventType string) StatusEvent {
	return StatusEvent{Type: eventType, TeamName: c.session.Config.Name}
}

func (c *Coordinator) SetWrapUp() {
	c.wrapUp.Store(1)
	c.report(c.newEvent("wrap_up"))
}

func (c *Coordinator) IsWrapUp() bool {
	return c.wrapUp.Load() == 1
}

func (c *Coordinator) TaskTracker() *TaskTracker {
	return c.taskTracker
}

func (c *Coordinator) RunAgentsTool() fantasy.AgentTool {
	return &runAgentsTool{coordinator: c}
}

type runAgentsTool struct {
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
}

func (t *runAgentsTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "run_agents",
		Description: "Delegate tasks to team workers. Runs all tasks in parallel. Returns structured results from each agent.",
		Parameters: map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"agent":         map[string]any{"type": "string", "enum": t.coordinator.workerNameList(), "description": "Agent name to delegate to"},
						"task":          map[string]any{"type": "string", "description": "Task description for the agent"},
						"context_files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional files from the workspace shared/ directory to provide as context"},
					},
					"required": []string{"agent", "task"},
				},
			},
		},
		Required: []string{"tasks"},
	}
}

func (t *runAgentsTool) ProviderOptions() fantasy.ProviderOptions      { return t.pOpts }
func (t *runAgentsTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

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

	result, err := t.coordinator.ExecuteTasks(ctx, args.Tasks)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(result), nil
}

type finishTool struct {
	pOpts fantasy.ProviderOptions
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

func (t *finishTool) ProviderOptions() fantasy.ProviderOptions      { return t.pOpts }
func (t *finishTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *finishTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("FINISHED:%s", args.Response)), nil
}

type loadSkillTool struct {
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
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
		Required:  []string{"name"},
	}
}

func (t *loadSkillTool) ProviderOptions() fantasy.ProviderOptions      { return t.pOpts }
func (t *loadSkillTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

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

	nameLower := strings.ToLower(args.Name)
	for _, s := range t.coordinator.skills {
		if strings.ToLower(s.Name) == nameLower {
			return fantasy.NewTextResponse(fmt.Sprintf("Skill: %s\n\n%s", s.Name, s.Content)), nil
		}
	}

	available := make([]string, len(t.coordinator.skills))
	for i, s := range t.coordinator.skills {
		available[i] = s.Name
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("skill %q not found (available: %v)", args.Name, available)), nil
}

const maxConcurrentTasks = 8
const maxConversationHistory = 100

func (c *Coordinator) ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error) {
	if c.IsWrapUp() {
		c.report(c.newEvent("step").withMessage("Wrap-up: refusing to start new tasks"))
		return "", fmt.Errorf("wrap-up in progress: refusing to delegate new tasks. Call finish immediately with your best summary of work completed so far")
	}

	c.round++
	if c.session.Config.MaxRounds > 0 && c.round > c.session.Config.MaxRounds {
		return "", fmt.Errorf("max rounds (%d) exceeded", c.session.Config.MaxRounds)
	}

	c.report(c.newEvent("step").withMessage(fmt.Sprintf("Round %d: delegating %d task(s)", c.round, len(tasks))))

	todoItems := c.taskTracker.TodoList().AddBatch(func() []struct {
		Agent string
		Desc  string
	} {
		batch := make([]struct {
			Agent string
			Desc  string
		}, len(tasks))
		for i, t := range tasks {
			batch[i] = struct {
				Agent string
				Desc  string
			}{Agent: strings.ToLower(t.Agent), Desc: t.Task}
		}
		return batch
	}())
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	type taskResult struct {
		agentName string
		todoID    string
		task      string
		output    string
		err       error
	}
	resultsCh := make(chan taskResult, len(tasks))

	sem := make(chan struct{}, maxConcurrentTasks)
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(td TaskDef, tid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			output, err := c.executeTask(ctx, td, tid)
			resultsCh <- taskResult{agentName: td.Agent, todoID: tid, task: td.Task, output: output, err: err}
		}(task, todoItems[i].ID)
	}
	wg.Wait()
	close(resultsCh)

	var results []taskResult
	for r := range resultsCh {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].agentName < results[j].agentName
	})

	var b strings.Builder
	successCount := 0
	errorCount := 0
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		if r.err != nil {
			errorCount++
			b.WriteString(fmt.Sprintf("## Agent: %s\n**Status**: ERROR\n**Error**: %s", r.agentName, r.err))
		} else {
			successCount++
			b.WriteString(fmt.Sprintf("## Agent: %s\n**Status**: Success\n\n%s", r.agentName, r.output))
		}
	}

	summary := fmt.Sprintf("\n\n---\nSummary: %d/%d tasks completed successfully", successCount, len(tasks))
	if errorCount > 0 {
		summary += fmt.Sprintf(", %d failed", errorCount)
	}
	b.WriteString(summary)

	if successCount == 0 && len(results) > 0 {
		return b.String(), fmt.Errorf("all %d tasks failed", len(results))
	}
	return b.String(), nil
}

func (c *Coordinator) executeTask(parentCtx context.Context, task TaskDef, todoID string) (string, error) {
	agentName := strings.ToLower(task.Agent)
	agentDef, ok := c.session.Agents[agentName]
	if !ok {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, fmt.Sprintf("unknown agent: %q", task.Agent))
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", fmt.Errorf("unknown agent: %q (available: %v)", task.Agent, c.agentNames())
	}

	if agentDef.Role == "orchestrator" || agentDef.Role == "coordinator" {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, fmt.Sprintf("cannot delegate to coordinator %q", agentName))
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", fmt.Errorf("cannot delegate to coordinator agent %q", agentName)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}

	maxRetries := c.session.Config.MaxRetries
	if agentDef.MaxRetries >= 0 {
		maxRetries = agentDef.MaxRetries
	}
	if maxRetries < 1 {
		maxRetries = 1
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("start").withAgent(agentName).withMessage(task.Task))
	writeStatus(c.session.Workspace, agentName, "working", task.Task)
	writeInbox(c.session.Workspace, agentName, task.Task)

	ag, err := c.getOrCreateAgent(parentCtx, agentDef)
	if err != nil {
		c.report(c.newEvent("error").withAgent(agentName).withMessage(err.Error()))
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		writeStatus(c.session.Workspace, agentName, "error", task.Task)
		return "", err
	}

	prompt := task.Task

	agentSkillNames := skill.ParseSkillList(agentDef.Skills)
	if len(agentSkillNames) > 0 {
		var skillPrefix strings.Builder
		skillPrefix.WriteString("## Relevant Skills\n\n")
		matched := skill.SkillsByName(c.skills, agentSkillNames)
		for _, s := range matched {
			fmt.Fprintf(&skillPrefix, "**%s**: %s\n", s.Name, s.Summary)
			skillPath := skill.SkillWorkspacePath(c.session.Workspace, s.Name)
			fmt.Fprintf(&skillPrefix, "Full instructions: %s\n\n", skillPath)
		}
		skillPrefix.WriteString("Use the `read` tool to load the full skill instructions if you need detailed steps.\n\n---\n\n")
		prompt = skillPrefix.String() + prompt
	}

	if len(task.ContextFiles) > 0 {
		var contextBuilder strings.Builder
		contextBuilder.WriteString("Context files:\n\n")
		for _, f := range task.ContextFiles {
			content, err := readShared(c.session.Workspace, f)
			if err != nil {
				contextBuilder.WriteString(fmt.Sprintf("(could not read %s: %v)\n", f, err))
			} else {
				contextBuilder.WriteString(fmt.Sprintf("### %s\n```\n%s\n```\n\n", f, content))
			}
		}
		prompt = contextBuilder.String() + "\n---\n\n" + prompt
	}

	var conversationHistory []fantasy.Message
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("retry %d/%d — continuing from previous progress", attempt, maxRetries)))
		}

		taskCtx, cancel := context.WithTimeout(parentCtx, agentTimeout)
		output, steps, err := c.runAgentWithStatusAndHistory(taskCtx, ag, agentName, prompt, conversationHistory)
		cancel()

		if err == nil {
			writeOutbox(c.session.Workspace, agentName, output)
			writeStatus(c.session.Workspace, agentName, "done", task.Task)
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("done").withAgent(agentName).withMessage("completed"))
			return output, nil
		}

		for _, step := range steps {
			conversationHistory = append(conversationHistory, step.Messages...)
		}

		lastErr = err
		c.report(c.newEvent("error").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d failed: %v", attempt, err)))
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, fmt.Sprintf("attempt %d failed: %v", attempt, err))
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

		if parentCtx.Err() != nil {
			break
		}
	}

	writeStatus(c.session.Workspace, agentName, "error", task.Task)
	return "", fmt.Errorf("agent %q failed after %d attempts: %w", agentName, maxRetries, lastErr)
}

func (c *Coordinator) runAgentWithStatus(ctx context.Context, ag fantasy.Agent, agentName, prompt string) (string, error) {
	output, _, err := c.runAgentWithStatusAndHistory(ctx, ag, agentName, prompt, nil)
	return output, err
}

func (c *Coordinator) runAgentWithStatusAndHistory(ctx context.Context, ag fantasy.Agent, agentName, prompt string, history []fantasy.Message) (string, []fantasy.StepResult, error) {
	reportFn := c.reportStatus
	workspace := c.session.Workspace
	teamName := c.session.Config.Name
	logWrite := func(entry string) { writeLLMLog(workspace, teamName, agentName, entry) }

	streamCall := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: history,
		PrepareStep: func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
			llmLogRequest(logWrite, opts)
			return ctx, fantasy.PrepareStepResult{}, nil
		},
		OnStepStart: func(stepNumber int) error {
			reportFn(c.newEvent("step").withAgent(agentName).withStep(stepNumber).withMessage(fmt.Sprintf("step %d", stepNumber)))
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			argsPreview := tc.Input
			if len(argsPreview) > 200 {
				argsPreview = argsPreview[:200] + "..."
			}
			reportFn(c.newEvent("tool_call").withAgent(agentName).withTool(tc.ToolName, argsPreview))
			llmLogStreamEvent(logWrite, "tool_call", formatToolCallContent(tc))
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			resultPreview := ""
			if tr.Result != nil {
				if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
					resultPreview = txt.Text
				}
			}
			reportFn(c.newEvent("tool_result").withAgent(agentName).withToolResult(tr.ToolName, resultPreview))
			llmLogStreamEvent(logWrite, "tool_result", formatToolResultContent(tr))
			return nil
		},
		OnTextDelta: func(id, text string) error {
			reportFn(c.newEvent("text").withAgent(agentName).withMessage(text))
			logWrite(text)
			return nil
		},
		OnReasoningDelta: func(id, text string) error {
			logWrite(text)
			return nil
		},
		OnStreamFinish: func(usage fantasy.Usage, finishReason fantasy.FinishReason, providerMetadata fantasy.ProviderMetadata) error {
			llmLogStreamFinish(logWrite, finishReason, usage)
			return nil
		},
	}

	result, err := ag.Stream(ctx, streamCall)
	if err != nil {
		return "", nil, err
	}
	return result.Response.Content.Text(), result.Steps, nil
}

func (c *Coordinator) getOrCreateAgent(ctx context.Context, def *agent.AgentDef) (fantasy.Agent, error) {
	c.agentCacheMu.RLock()
	if ag, ok := c.agentCache[def.Name]; ok {
		c.agentCacheMu.RUnlock()
		return ag, nil
	}
	c.agentCacheMu.RUnlock()

	c.agentCacheMu.Lock()
	defer c.agentCacheMu.Unlock()

	if ag, ok := c.agentCache[def.Name]; ok {
		return ag, nil
	}

	agentTools := agent.SelectTools(c.coreTools, def.Tools)
	if c.mcpManager != nil {
		agentTools = append(agentTools, c.mcpManager.AsAgentTools()...)
	}

	ag, err := agent.CreateAgent(ctx, c.provider, agent.AgentConfig{
		Def:        def,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
	}, agentTools)
	if err != nil {
		return nil, err
	}

	c.agentCache[def.Name] = ag
	return ag, nil
}

func (c *Coordinator) agentNames() []string {
	var names []string
	for name, def := range c.session.Agents {
		if def.Role != "orchestrator" && def.Role != "coordinator" {
			names = append(names, name)
		}
	}
	return names
}

func (c *Coordinator) workerDescriptions() []string {
	var descs []string
	for _, def := range c.session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		desc := def.Name
		if def.Description != "" {
			desc += ": " + def.Description
		}
		if def.Tools != "" {
			desc += fmt.Sprintf(" (tools: %s)", def.Tools)
		}
		descs = append(descs, desc)
	}
	return descs
}

func (c *Coordinator) workerNameList() []string {
	var names []string
	for _, def := range c.session.Agents {
		if def.Role != "orchestrator" && def.Role != "coordinator" {
			names = append(names, def.Name)
		}
	}
	return names
}

func (c *Coordinator) BuildOrchestratorPrompt() string {
	workerNames := c.workerNameList()
	workerDescs := c.workerDescriptions()

	var b strings.Builder
	fmt.Fprintf(&b, "You are the coordinator of team %q with %d members: %s.\n\n", c.session.Config.Name, len(workerNames), strings.Join(workerNames, ", "))

	b.WriteString("You MUST delegate ALL work to your team members. You do NOT have tools to do work yourself.\n\n")

	b.WriteString("## How to Coordinate\n\n")
	b.WriteString("1. **Analyze** the user's request to identify which team members are needed\n")
	b.WriteString("2. **Check skills** — if any available skills are relevant to the user's task, call `load_skill` to get the full instructions\n")
	b.WriteString("3. **Plan** your approach before delegating — think step by step\n")
	b.WriteString("4. **Delegate** tasks using run_agents — this is the ONLY way to get work done\n")
	b.WriteString("5. Run independent tasks in parallel by passing multiple tasks in one run_agents call\n")
	b.WriteString("6. When delegating to a worker that needs skill knowledge, include the skill summary in the task description and mention the skill file path so the worker can read it if needed\n")
	b.WriteString("7. **Evaluate** results after each run_agents call — decide if more work is needed or if you can provide a final answer\n")
	b.WriteString("8. **Synthesize** results into a coherent answer for the user\n")
	b.WriteString("9. When satisfied, call the finish tool with your final response\n\n")

	b.WriteString("## Available Agents\n\n")
	fmt.Fprintf(&b, "IMPORTANT: You MUST use these exact agent names in run_agents: %s. Do NOT invent or modify agent names.\n\n", strings.Join(workerNames, ", "))
	for _, desc := range workerDescs {
		fmt.Fprintf(&b, "- %s\n", desc)
	}
	b.WriteString("\n")

	b.WriteString("## Available Skills\n\n")
	if len(c.skills) == 0 {
		b.WriteString("No skills are available for this team.\n\n")
	} else {
		b.WriteString("| Skill | Description |\n")
		b.WriteString("|-------|-------------|\n")
		for _, s := range c.skills {
			desc := s.Description
			if len(desc) > 80 {
				desc = desc[:80] + "..."
			}
			fmt.Fprintf(&b, "| %s | %s |\n", s.Name, desc)
		}
		b.WriteString("\n")
		b.WriteString("To get the full instructions for any skill, call the `load_skill` tool with the skill name.\n")
		b.WriteString("Before delegating tasks, consider loading relevant skills so you can include their key instructions in the task description.\n\n")
	}

	b.WriteString("## Tools\n\n")
	b.WriteString("### run_agents\n")
	b.WriteString("Delegate tasks to team workers. All tasks in one call run in parallel.\n")
	b.WriteString("```json\n")
	b.WriteString("{\n")
	b.WriteString("  \"tasks\": [\n")
	b.WriteString("    {\"agent\": \"agent-name\", \"task\": \"task description\", \"context_files\": [\"optional_file.txt\"]}\n")
	b.WriteString("  ]\n")
	b.WriteString("}\n```\n\n")
	b.WriteString("### load_skill\n")
	b.WriteString("Load the full content of a skill by name. Returns detailed instructions you can include in worker task descriptions.\n")
	b.WriteString("```json\n{\"name\": \"skill-name\"}\n```\n\n")
	b.WriteString("### finish\n")
	b.WriteString("Signal completion and provide your final answer to the user. ALWAYS call this when you are done.\n")
	b.WriteString("```json\n{\"response\": \"Your final synthesized answer to the user\"}\n```\n\n")
	b.WriteString("### ask_user\n")
	b.WriteString("Ask the user a question when you need clarification before proceeding.\n\n")

	fmt.Fprintf(&b, "Team workspace: %s\n", c.session.Workspace)

	return b.String()
}

func (c *Coordinator) GetOrchestratorDef() *agent.AgentDef {
	for _, def := range c.session.Agents {
		if def.Role == "coordinator" || def.Role == "orchestrator" {
			return def
		}
	}
	for _, def := range c.session.Agents {
		n := strings.ToLower(def.Name)
		if strings.Contains(n, "coordinat") || strings.Contains(n, "orchestr") {
			return def
		}
	}
	return &agent.AgentDef{
		Name:        "coordinator",
		Description: "Default team coordinator",
		Role:        "coordinator",
		Tools:       "ask_user",
		System:      "",
		MaxRetries:  -1,
		Generation:  c.session.Config.Generation,
		ProviderURL: c.session.Config.ProviderURL,
	}
}

func (c *Coordinator) expandDefaultOrchestratorTemplate(tmpl string) string {
	workerNames := c.workerNameList()
	s := strings.ReplaceAll(tmpl, "{{TEAM_NAME}}", c.session.Config.Name)
	s = strings.ReplaceAll(s, "{{AGENT_COUNT}}", fmt.Sprintf("%d", len(workerNames)))
	s = strings.ReplaceAll(s, "{{AGENT_NAMES}}", strings.Join(workerNames, ", "))
	return s
}

func (c *Coordinator) Round() int { return c.round }

func (c *Coordinator) SetSessionData(sd *SessionData) {
	c.sessionData = sd
}

func (c *Coordinator) SessionData() *SessionData {
	return c.sessionData
}

var directAgentPattern = regexp.MustCompile(`^@(\w[\w-]*)\s+(.+)$`)

func ParseDirectAgent(prompt string) (agentName string, task string, ok bool) {
	m := directAgentPattern.FindStringSubmatch(prompt)
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), m[2], true
}

func (c *Coordinator) RunDirectAgent(ctx context.Context, agentName string, task string) (*DirectAgentResult, error) {
	agentDef, ok := c.session.Agents[agentName]
	if !ok {
		return nil, fmt.Errorf("unknown agent: %q (available: %v)", agentName, c.agentNames())
	}

	if agentDef.Role == "orchestrator" || agentDef.Role == "coordinator" {
		return nil, fmt.Errorf("cannot directly invoke coordinator agent %q", agentName)
	}

	todoItems := c.taskTracker.TodoList().AddBatch([]struct {
		Agent string
		Desc  string
	}{{Agent: agentName, Desc: task}})
	todoID := todoItems[0].ID
	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	ag, err := c.getOrCreateAgent(ctx, agentDef)
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return nil, fmt.Errorf("failed to create agent %q: %w", agentName, err)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}

	taskCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()

	writeInbox(c.session.Workspace, agentName, task)
	writeStatus(c.session.Workspace, agentName, "working", task)

	prompt := task
	agentSkillNames := skill.ParseSkillList(agentDef.Skills)
	if len(agentSkillNames) > 0 {
		var skillPrefix strings.Builder
		skillPrefix.WriteString("## Relevant Skills\n\n")
		matched := skill.SkillsByName(c.skills, agentSkillNames)
		for _, s := range matched {
			fmt.Fprintf(&skillPrefix, "**%s**: %s\n", s.Name, s.Summary)
			skillPath := skill.SkillWorkspacePath(c.session.Workspace, s.Name)
			fmt.Fprintf(&skillPrefix, "Full instructions: %s\n\n", skillPath)
		}
		skillPrefix.WriteString("Use the `read` tool to load the full skill instructions if you need detailed steps.\n\n---\n\n")
		prompt = skillPrefix.String() + prompt
	}

	output, err := c.runAgentWithStatus(taskCtx, ag, agentName, prompt)
	if err != nil {
		writeStatus(c.session.Workspace, agentName, "error", task)
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return &DirectAgentResult{AgentName: agentName, Error: err}, nil
	}

	writeOutbox(c.session.Workspace, agentName, output)
	writeStatus(c.session.Workspace, agentName, "done", task)
	c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	return &DirectAgentResult{AgentName: agentName, Output: output}, nil
}

func (c *Coordinator) Run(ctx context.Context, userPrompt string) (string, error) {
	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	EnsureWorkspaceDirs(c.session.Workspace)

	if len(c.skills) > 0 {
		if err := skill.CopySkillsToWorkspace(c.skills, c.session.Workspace); err != nil {
			return "", fmt.Errorf("failed to copy skills to workspace: %w", err)
		}
	}

	if c.sessionData != nil {
		c.sessionData.AddEntry("user", userPrompt)
	}

	systemPrompt := orchDef.System
	if systemPrompt == "" {
		systemPrompt = c.expandDefaultOrchestratorTemplate(defaultOrchestratorSystem)
	}
	systemPrompt += "\n\n" + c.BuildOrchestratorPrompt()

	if c.sessionData != nil && len(c.sessionData.Entries) > 1 {
		contextSummary := c.sessionData.ContextSummary()
		if contextSummary != "" {
			systemPrompt += "\n\n---\n## Session Context\n\n" + contextSummary
		}
	}

	orchTools := []fantasy.AgentTool{c.RunAgentsTool(), &finishTool{}, &loadSkillTool{coordinator: c}}
	for _, t := range c.coreTools {
		if t.Info().Name == "ask_user" {
			orchTools = append(orchTools, t)
			break
		}
	}

	coordinatorTimeout := time.Duration(c.session.Config.Timeout) * time.Second * time.Duration(c.session.Config.MaxRounds+1)
	if orchDef.Timeout > 0 {
		coordinatorTimeout = time.Duration(orchDef.Timeout) * time.Second
	}

	orchCtx, cancel := context.WithTimeout(ctx, coordinatorTimeout)
	defer cancel()

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("coordinator starting"))

	orch, err := agent.CreateAgent(orchCtx, c.provider, agent.AgentConfig{
		Def:        orchDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
	}, orchTools)
	if err != nil {
		return "", fmt.Errorf("failed to create coordinator: %w", err)
	}

	c.conversationHistoryMu.Lock()
	historySnapshot := make([]fantasy.Message, len(c.conversationHistory))
	copy(historySnapshot, c.conversationHistory)
	c.conversationHistoryMu.Unlock()

	result, steps, err := c.runAgentWithStatusAndHistory(orchCtx, orch, orchDef.Name, userPrompt, historySnapshot)
	if err != nil {
		if c.sessionData != nil {
			SaveSession(c.session.Workspace, c.sessionData)
		}
		return "", fmt.Errorf("coordinator failed: %w", err)
	}

	c.conversationHistoryMu.Lock()
	for _, step := range steps {
		c.conversationHistory = append(c.conversationHistory, step.Messages...)
	}
	if len(c.conversationHistory) > maxConversationHistory {
		c.conversationHistory = c.conversationHistory[len(c.conversationHistory)-maxConversationHistory:]
	}
	c.conversationHistoryMu.Unlock()

	finalResult := result
	if strings.HasPrefix(result, "FINISHED:") {
		finalResult = strings.TrimPrefix(result, "FINISHED:")
	}

	if c.sessionData != nil {
		c.sessionData.AddEntry("assistant", finalResult)
		c.sessionData.Rounds = c.round
		SaveSession(c.session.Workspace, c.sessionData)
	}

	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator finished"))
	return finalResult, nil
}

func (c *Coordinator) ContinueWithPrompt(ctx context.Context, additionalPrompt string) (string, error) {
	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	var continuationPrompt string
	if c.IsWrapUp() {
		continuationPrompt = wrapUpPromptTemplate
		additionalPrompt = "wrap up now"
	} else {
		continuationPrompt = fmt.Sprintf(continuationPromptTemplate, additionalPrompt)
	}

	if c.sessionData != nil {
		c.sessionData.AddEntry("user", additionalPrompt)
	}

	orchTools := []fantasy.AgentTool{c.RunAgentsTool(), &finishTool{}, &loadSkillTool{coordinator: c}}
	for _, t := range c.coreTools {
		if t.Info().Name == "ask_user" {
			orchTools = append(orchTools, t)
			break
		}
	}

	coordinatorTimeout := time.Duration(c.session.Config.Timeout) * time.Second * time.Duration(c.session.Config.MaxRounds+1)
	if orchDef.Timeout > 0 {
		coordinatorTimeout = time.Duration(orchDef.Timeout) * time.Second
	}

	orchCtx, cancel := context.WithTimeout(ctx, coordinatorTimeout)
	defer cancel()

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("continuing with additional input"))

	orch, err := agent.CreateAgent(orchCtx, c.provider, agent.AgentConfig{
		Def:        orchDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
	}, orchTools)
	if err != nil {
		return "", fmt.Errorf("failed to create coordinator: %w", err)
	}

	c.conversationHistoryMu.Lock()
	historySnapshot := make([]fantasy.Message, len(c.conversationHistory))
	copy(historySnapshot, c.conversationHistory)
	c.conversationHistoryMu.Unlock()

	result, steps, err := c.runAgentWithStatusAndHistory(orchCtx, orch, orchDef.Name, continuationPrompt, historySnapshot)
	if err != nil {
		if c.sessionData != nil {
			SaveSession(c.session.Workspace, c.sessionData)
		}
		return "", fmt.Errorf("coordinator continuation failed: %w", err)
	}

	c.conversationHistoryMu.Lock()
	for _, step := range steps {
		c.conversationHistory = append(c.conversationHistory, step.Messages...)
	}
	if len(c.conversationHistory) > maxConversationHistory {
		c.conversationHistory = c.conversationHistory[len(c.conversationHistory)-maxConversationHistory:]
	}
	c.conversationHistoryMu.Unlock()

	finalResult := result
	if strings.HasPrefix(result, "FINISHED:") {
		finalResult = strings.TrimPrefix(result, "FINISHED:")
	}

	if c.sessionData != nil {
		c.sessionData.AddEntry("assistant", finalResult)
		c.sessionData.Rounds = c.round
		SaveSession(c.session.Workspace, c.sessionData)
	}

	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("continuation finished"))
	return finalResult, nil
}

const defaultOrchestratorSystem = `You are the orchestrator of "{{TEAM_NAME}}", a software development team with {{AGENT_COUNT}} members: {{AGENT_NAMES}}.

Your role is to coordinate the team: break down user requests into concrete tasks, delegate them to the right members, and synthesize the results into a coherent response.

Rules:
- You MUST use run_agents to delegate ALL work to team members
- Running independent tasks in parallel is preferred
- After receiving results from run_agents, evaluate whether more work is needed or if you can provide a final answer
- Synthesize results from workers into a coherent answer for the user
- NEVER attempt to do the work yourself — you do not have tools for that
- If a task fails, retry once with clearer instructions before giving up
- Break complex requests into smaller subtasks for appropriate workers
- Use ask_user when you need clarification from the user before proceeding
- When you have completed all coordination and have a final answer, call the finish tool with your response
- ALWAYS call finish when done — do not just output text as your final answer
- If the user's task relates to a skill, use load_skill to get the detailed instructions first, then include relevant parts in worker task descriptions
- Workers have limited context — include only the essential skill instructions in the task description, not the entire skill content. Mention the skill file path so workers can read it if they need more detail
`

const continuationPromptTemplate = `The user has sent an additional message while you were working:

"""
%s
"""

Please take this into account. You may need to:
- Add new tasks for your workers
- Modify tasks that haven't started yet
- Cancel tasks that are no longer needed

Continue coordinating. Call finish when you have a complete response that addresses both the original request and the new input.`

const wrapUpPromptTemplate = `The user has requested that you wrap up immediately.

IMPORTANT INSTRUCTIONS:
- Do NOT delegate any new tasks
- Do NOT call run_agents again
- Immediately summarize what has been accomplished so far based on all results you have received
- Call the finish tool RIGHT NOW with your best summary of the work completed

This is a wrap-up request. You MUST call finish immediately with whatever results are available.`
