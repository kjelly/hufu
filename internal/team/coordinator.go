package team

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/anomalyco/agent-team-cli/internal/agent"
	"github.com/anomalyco/agent-team-cli/internal/mcp"
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
	session      *TeamSession
	ollama       *agent.OllamaProvider
	mcpManager   *mcp.MCPToolManager
	coreTools    []fantasy.AgentTool
	agentCache   map[string]fantasy.Agent
	agentCacheMu sync.RWMutex
	round        int
	verbose      bool
	reportStatus StatusReporter
	sessionData  *SessionData
	taskTracker  *TaskTracker
}

func NewCoordinator(session *TeamSession, ollama *agent.OllamaProvider, mcpManager *mcp.MCPToolManager, verbose bool) *Coordinator {
	coreTools := agent.BuildAllAgentTools(session.Workspace)
	return &Coordinator{
		session:      session,
		ollama:       ollama,
		mcpManager:   mcpManager,
		coreTools:    coreTools,
		agentCache:   make(map[string]fantasy.Agent),
		verbose:      verbose,
		reportStatus: func(event StatusEvent) {},
		taskTracker:  NewTaskTracker(),
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
						"agent":         map[string]any{"type": "string", "description": "Agent name to delegate to"},
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

func (c *Coordinator) ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error) {
	c.round++
	if c.session.Config.MaxRounds > 0 && c.round > c.session.Config.MaxRounds {
		return "", fmt.Errorf("max rounds (%d) exceeded", c.session.Config.MaxRounds)
	}

	c.report(StatusEvent{Type: "step", Message: fmt.Sprintf("Round %d: delegating %d task(s)", c.round, len(tasks))})

	type taskResult struct {
		agentName string
		task      string
		output    string
		err       error
	}
	resultsCh := make(chan taskResult, len(tasks))

	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		go func(td TaskDef) {
			defer wg.Done()
			output, err := c.executeTask(ctx, td)
			resultsCh <- taskResult{agentName: td.Agent, task: td.Task, output: output, err: err}
		}(task)
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

	return b.String(), nil
}

func (c *Coordinator) executeTask(parentCtx context.Context, task TaskDef) (string, error) {
	agentName := strings.ToLower(task.Agent)
	agentDef, ok := c.session.Agents[agentName]
	if !ok {
		return "", fmt.Errorf("unknown agent: %q (available: %v)", task.Agent, c.agentNames())
	}

	if agentDef.Role == "orchestrator" || agentDef.Role == "coordinator" {
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

	c.taskTracker.Start(agentName, task.Task)
	c.report(StatusEvent{Type: "start", Agent: agentName, Message: task.Task})
	writeStatus(c.session.Workspace, agentName, "working", task.Task)
	writeInbox(c.session.Workspace, agentName, task.Task)

	ag, err := c.getOrCreateAgent(parentCtx, agentDef)
	if err != nil {
		c.report(StatusEvent{Type: "error", Agent: agentName, Message: err.Error()})
		c.taskTracker.Error(agentName, err.Error())
		writeStatus(c.session.Workspace, agentName, "error", task.Task)
		return "", err
	}

	prompt := task.Task
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
			c.report(StatusEvent{Type: "step", Agent: agentName, Message: fmt.Sprintf("retry %d/%d — continuing from previous progress", attempt, maxRetries)})
		}

		taskCtx, cancel := context.WithTimeout(context.Background(), agentTimeout)
		output, steps, err := c.runAgentWithStatusAndHistory(taskCtx, ag, agentName, prompt, conversationHistory)
		cancel()

		if err == nil {
			writeOutbox(c.session.Workspace, agentName, output)
			writeStatus(c.session.Workspace, agentName, "done", task.Task)
			c.taskTracker.Done(agentName)
			c.report(StatusEvent{Type: "done", Agent: agentName, Message: "completed"})
			return output, nil
		}

		for _, step := range steps {
			conversationHistory = append(conversationHistory, step.Messages...)
		}

		lastErr = err
		c.report(StatusEvent{Type: "error", Agent: agentName, Message: fmt.Sprintf("attempt %d failed: %v", attempt, err)})
		c.taskTracker.Error(agentName, fmt.Sprintf("attempt %d failed: %v", attempt, err))

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

	streamCall := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: history,
		OnStepStart: func(stepNumber int) error {
			reportFn(StatusEvent{Type: "step", Agent: agentName, Step: stepNumber, Message: fmt.Sprintf("step %d", stepNumber)})
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			argsPreview := tc.Input
			if len(argsPreview) > 200 {
				argsPreview = argsPreview[:200] + "..."
			}
			reportFn(StatusEvent{Type: "tool_call", Agent: agentName, ToolName: tc.ToolName, ToolArgs: argsPreview})
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			resultPreview := ""
			if tr.Result != nil {
				if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
					resultPreview = txt.Text
				}
			}
			reportFn(StatusEvent{Type: "tool_result", Agent: agentName, ToolName: tr.ToolName, ToolResult: resultPreview})
			return nil
		},
		OnTextDelta: func(id, text string) error {
			reportFn(StatusEvent{Type: "text", Agent: agentName, Message: text})
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

	ag, err := agent.CreateAgent(ctx, c.ollama, agent.AgentConfig{
		Def:        def,
		TeamConfig: &c.session.Config,
		WorkDir:    c.session.Workspace,
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

	return fmt.Sprintf(`You are the coordinator of team %q with %d members: %s.

You MUST delegate ALL work to your team members. You do NOT have tools to do work yourself.

## How to Coordinate

1. **Analyze** the user's request to identify which team members are needed
2. **Plan** your approach before delegating — think step by step
3. **Delegate** tasks using run_agents — this is the ONLY way to get work done
4. Run independent tasks in parallel by passing multiple tasks in one run_agents call
5. **Evaluate** results after each run_agents call — decide if more work is needed or if you can provide a final answer
6. **Synthesize** results into a coherent answer for the user
7. When satisfied, call the finish tool with your final response

## Available Agents

%s

## Tools

### run_agents
Delegate tasks to team workers. All tasks in one call run in parallel.
{
  "tasks": [
    {"agent": "agent-name", "task": "task description", "context_files": ["optional_file.txt"]}
  ]
}

### finish
Signal completion and provide your final answer to the user. ALWAYS call this when you are done.
{
  "response": "Your final synthesized answer to the user"
}

### ask_user
Ask the user a question when you need clarification before proceeding.

Team workspace: %s
`,
		c.session.Config.Name,
		len(workerNames),
		strings.Join(workerNames, ", "),
		strings.Join(workerDescs, "\n"),
		c.session.Workspace,
	)
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
	return nil
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

	c.taskTracker.Start(agentName, task)

	ag, err := c.getOrCreateAgent(ctx, agentDef)
	if err != nil {
		c.taskTracker.Error(agentName, err.Error())
		return nil, fmt.Errorf("failed to create agent %q: %w", agentName, err)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}

	taskCtx, cancel := context.WithTimeout(context.Background(), agentTimeout)
	defer cancel()

	writeInbox(c.session.Workspace, agentName, task)
	writeStatus(c.session.Workspace, agentName, "working", task)

	output, err := c.runAgentWithStatus(taskCtx, ag, agentName, task)
	if err != nil {
		writeStatus(c.session.Workspace, agentName, "error", task)
		c.taskTracker.Error(agentName, err.Error())
		return &DirectAgentResult{AgentName: agentName, Error: err}, nil
	}

	writeOutbox(c.session.Workspace, agentName, output)
	writeStatus(c.session.Workspace, agentName, "done", task)
	c.taskTracker.Done(agentName)

	return &DirectAgentResult{AgentName: agentName, Output: output}, nil
}

func (c *Coordinator) Run(ctx context.Context, userPrompt string) (string, error) {
	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	EnsureWorkspaceDirs(c.session.Workspace)

	if c.sessionData != nil {
		c.sessionData.AddEntry("user", userPrompt)
	}

	systemPrompt := orchDef.System
	if systemPrompt == "" {
		systemPrompt = defaultOrchestratorSystem
	}
	systemPrompt += "\n\n" + c.BuildOrchestratorPrompt()

	if c.sessionData != nil && len(c.sessionData.Entries) > 1 {
		contextSummary := c.sessionData.ContextSummary()
		if contextSummary != "" {
			systemPrompt += "\n\n---\n## Session Context\n\n" + contextSummary
		}
	}

	orchTools := []fantasy.AgentTool{c.RunAgentsTool(), &finishTool{}}
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

	c.report(StatusEvent{Type: "start", Agent: orchDef.Name, Message: "coordinator starting"})

	orch, err := agent.CreateAgent(orchCtx, c.ollama, agent.AgentConfig{
		Def:        orchDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.session.Workspace,
	}, orchTools)
	if err != nil {
		return "", fmt.Errorf("failed to create coordinator: %w", err)
	}

	result, err := c.runAgentWithStatus(orchCtx, orch, orchDef.Name, userPrompt)
	if err != nil {
		if c.sessionData != nil {
			SaveSession(c.session.Workspace, c.sessionData)
		}
		return "", fmt.Errorf("coordinator failed: %w", err)
	}

	finalResult := result
	if strings.HasPrefix(result, "FINISHED:") {
		finalResult = strings.TrimPrefix(result, "FINISHED:")
	}

	if c.sessionData != nil {
		c.sessionData.AddEntry("assistant", finalResult)
		c.sessionData.Rounds = c.round
		SaveSession(c.session.Workspace, c.sessionData)
	}

	c.report(StatusEvent{Type: "done", Agent: orchDef.Name, Message: "coordinator finished"})
	return finalResult, nil
}

const defaultOrchestratorSystem = `You are a team coordinator. You ONLY coordinate — you do NOT do implementation work yourself.

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
`