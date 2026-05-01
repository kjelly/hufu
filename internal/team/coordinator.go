package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/audit"
	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/memory"
	"github.com/anomalyco/hufu/internal/sidecar"
	"github.com/anomalyco/hufu/internal/skill"
)

var skillSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

type TaskDef struct {
	Agent        string   `json:"agent"`
	Task         string   `json:"task"`
	Model        string   `json:"model,omitempty"`
	Sidecar      bool     `json:"sidecar,omitempty"`
	Summarize    bool     `json:"summarize,omitempty"`
	ContextFiles []string `json:"context_files,omitempty"`
}

type DirectAgentResult struct {
	AgentName string
	Output    string
	Error     error
}

type Coordinator struct {
	mu                    sync.RWMutex
	session               *TeamSession
	provider              *agent.OllamaProvider
	mcpManager            *mcp.MCPToolManager
	coreTools             []fantasy.AgentTool
	agentCache            map[string]fantasy.Agent
	agentCacheMu          sync.RWMutex
	round                 int
	verbose               bool
	reportStatus          StatusReporter
	sessionData           *SessionData
	taskTracker           *TaskTracker
	skills                []*skill.SkillDef
	conversationHistory   []fantasy.Message
	conversationHistoryMu sync.Mutex
	projectDir            string
	wrapUp                atomic.Int32
	currentAgentName      string
	currentAgentNameMu    sync.RWMutex
	auditLogger           *audit.AuditLogger
	skillUsage            map[string]*skillUsageState
	skillUsageMu          sync.Mutex
	delegatedTasks        map[string]int
	delegatedTasksMu      sync.Mutex
	memoryStore           *memory.MemoryStore
	skillsMu              sync.RWMutex
	modelList             []config.ModelEntry
	sidecarModel          string
	sidecarInst           *sidecar.Sidecar
	sidecarInitMu         sync.Mutex
	cachedWorkerContext   string
	workerCtxOnce        sync.Once
	autoLoadedSkills     []*skill.SkillDef
	sidecarInit          bool
}

// skillUsageState is the internal mutable record; Agents uses a map for O(1) dedup.
type skillUsageState struct {
	Name   string
	Count  int
	Agents map[string]bool
}

// SkillUsageEntry is the read-only snapshot returned by SkillUsage().
type SkillUsageEntry struct {
	Name   string
	Count  int
	Agents []string // sorted list of agent names
}

type taskTiming struct {
	mu        sync.Mutex
	taskStart time.Time
	toolTime  time.Duration
	toolStart time.Time
	counting  bool
}

func (t *taskTiming) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.taskStart = time.Now()
	t.toolTime = 0
	t.counting = true
}

func (t *taskTiming) beginTool() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counting {
		t.toolStart = time.Now()
	}
}

func (t *taskTiming) endTool() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counting && !t.toolStart.IsZero() {
		t.toolTime += time.Since(t.toolStart)
		t.toolStart = time.Time{}
	}
}

func (t *taskTiming) snapshot() (duration, modelTime, toolTime time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.taskStart.IsZero() {
		return 0, 0, 0
	}
	duration = time.Since(t.taskStart)
	toolTime = t.toolTime
	modelTime = duration - toolTime
	if modelTime < 0 {
		modelTime = 0
	}
	return
}

func NewCoordinator(session *TeamSession, defaultProviderURL string, mcpManager *mcp.MCPToolManager, memoryStore *memory.MemoryStore, modelList []config.ModelEntry, sidecarModel string, verbose bool) (*Coordinator, error) {
	projectDir, _ := os.Getwd()
	coreTools := agent.BuildAllAgentTools(projectDir)
	prov, err := agent.NewOllamaProvider(defaultProviderURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create Ollama provider: %w", err)
	}
	c := &Coordinator{
		provider:       prov,
		session:        session,
		mcpManager:     mcpManager,
		coreTools:      coreTools,
		agentCache:     make(map[string]fantasy.Agent),
		verbose:        verbose,
		reportStatus:   func(event StatusEvent) {},
		taskTracker:    NewTaskTracker(),
		skills:         session.Skills,
		projectDir:     projectDir,
		skillUsage:     make(map[string]*skillUsageState),
		delegatedTasks: make(map[string]int),
		memoryStore:    memoryStore,
		modelList:      modelList,
		sidecarModel:   sidecarModel,
	}

	auditLogger, err := audit.NewAuditLogger(session.Workspace, session.Config.Name)
	if err == nil {
		c.auditLogger = auditLogger
		audit.SetDefault(auditLogger)
	}

	c.coreTools = append(c.coreTools,
		&workerAgentTool{coordinator: c},
		&todoTool{coordinator: c},
		&loadSkillTool{coordinator: c},
		&saveSkillTool{coordinator: c},
	)

	if c.memoryStore != nil {
		c.coreTools = append(c.coreTools,
			memory.NewMemorySaveTool(c.memoryStore),
			memory.NewMemoryQueryTool(c.memoryStore),
		)
	}

	if history := LoadConversationHistory(session.Workspace); len(history) > 0 {
		c.conversationHistory = history
	}

	return c, nil
}

func (c *Coordinator) SetStatusReporter(fn StatusReporter) {
	if fn != nil {
		c.reportStatus = fn
	}
}

func (c *Coordinator) report(event StatusEvent) {
	c.reportStatus(event)
}

func (c *Coordinator) recordSkillUsage(name, agentName string) {
	key := strings.ToLower(name)
	func() {
		c.skillUsageMu.Lock()
		defer c.skillUsageMu.Unlock()
		entry, ok := c.skillUsage[key]
		if !ok {
			entry = &skillUsageState{
				Name:   name,
				Agents: make(map[string]bool),
			}
			c.skillUsage[key] = entry
		}
		entry.Count++
		entry.Agents[agentName] = true
	}()
	c.report(c.newEvent("skill_used").withSkillName(name).withAgent(agentName))
}

func (c *Coordinator) SkillUsage() []SkillUsageEntry {
	c.skillUsageMu.Lock()
	defer c.skillUsageMu.Unlock()
	result := make([]SkillUsageEntry, 0, len(c.skillUsage))
	for _, entry := range c.skillUsage {
		agents := make([]string, 0, len(entry.Agents))
		for k := range entry.Agents {
			agents = append(agents, k)
		}
		sort.Strings(agents)
		result = append(result, SkillUsageEntry{
			Name:   entry.Name,
			Count:  entry.Count,
			Agents: agents,
		})
	}
	return result
}

// getSkills returns a snapshot of the current skill list, safe for concurrent use.
func (c *Coordinator) getSkills() []*skill.SkillDef {
	c.skillsMu.RLock()
	defer c.skillsMu.RUnlock()
	return c.skills
}

func (c *Coordinator) skillDirs() []string {
	return []string{
		filepath.Join(c.session.Dir, ".agents", "skills"),
		filepath.Join(os.Getenv("HOME"), ".agents", "skills"),
	}
}

// saveAndReloadSkill writes a SKILL.md to the team's local skill directory and
// immediately hot-reloads c.skills so the new skill is available in the same session.
func (c *Coordinator) saveAndReloadSkill(name, description, content string) (string, error) {
	slug := strings.Trim(skillSlugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		return "", fmt.Errorf("invalid skill name %q", name)
	}

	skillDir := filepath.Join(c.session.Dir, ".agents", "skills", slug)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create skill directory: %w", err)
	}

	// Build YAML-safe description block.
	descLines := strings.Split(strings.TrimSpace(description), "\n")
	var descYAML string
	if len(descLines) == 1 {
		descYAML = "description: " + descLines[0]
	} else {
		var db strings.Builder
		db.WriteString("description: |\n")
		for _, l := range descLines {
			db.WriteString("  ")
			db.WriteString(l)
			db.WriteString("\n")
		}
		descYAML = strings.TrimRight(db.String(), "\n")
	}

	fileContent := fmt.Sprintf("---\nname: %s\n%s\n---\n\n%s\n", name, descYAML, strings.TrimSpace(content))
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(fileContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write skill file: %w", err)
	}

	// Hot-reload: rediscover and re-filter skills from all directories.
	allSkills := skill.DiscoverSkills(c.skillDirs())
	includeSkills := skill.ParseSkillList(c.session.Config.Skills)
	excludeSkills := skill.ParseSkillList(c.session.Config.SkillsExclude)
	newSkills := skill.FilterSkills(allSkills, includeSkills, excludeSkills)

	func() {
		c.skillsMu.Lock()
		defer c.skillsMu.Unlock()
		c.skills = newSkills
	}()

	return skillPath, nil
}

func (c *Coordinator) buildSkillPromptPrefix(agentDef *agent.AgentDef) string {
	agentSkillNames := skill.ParseSkillList(agentDef.Skills)
	if len(agentSkillNames) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Relevant Skills\n\n")
	for _, s := range skill.SkillsByName(c.getSkills(), agentSkillNames) {
		fmt.Fprintf(&b, "### %s\n%s\n\n", s.Name, s.Content)
	}
	b.WriteString("---\n\n")
	return b.String()
}

func (c *Coordinator) buildAutoSkillPrefix(agentDef *agent.AgentDef, agentName string, taskDesc string) string {
	if len(c.autoLoadedSkills) == 0 {
		return ""
	}

	existingSkills := skill.ParseSkillList(agentDef.Skills)
	existingSet := map[string]bool{}
	for _, name := range existingSkills {
		existingSet[strings.ToLower(strings.TrimSpace(name))] = true
	}

	agentText := strings.ToLower(agentDef.Name + " " + agentDef.Description + " " + taskDesc)

	var relevant []*skill.SkillDef
	for _, s := range c.autoLoadedSkills {
		if existingSet[strings.ToLower(s.Name)] {
			continue
		}
		for _, kw := range extractSkillKeywords(s) {
			if strings.Contains(agentText, kw) {
				relevant = append(relevant, s)
				break
			}
		}
	}

	if len(relevant) == 0 {
		return ""
	}

	for _, s := range relevant {
		c.report(c.newEvent("skill_auto_loaded").withAgent(agentName).withSkillName(s.Name))
	}

	var b strings.Builder
	b.WriteString("## Relevant Skills (auto-loaded)\n\n")
	for _, s := range relevant {
		fmt.Fprintf(&b, "### %s\n%s\n\n", s.Name, s.Content)
	}
	b.WriteString("---\n\n")
	return b.String()
}

func (c *Coordinator) extractSkillFromToolCall(toolName, input string) string {
	if toolName != "load_skill" {
		return ""
	}
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil || args.Name == "" {
		return ""
	}
	nameLower := strings.ToLower(args.Name)
	for _, s := range c.getSkills() {
		if strings.ToLower(s.Name) == nameLower {
			return s.Name
		}
	}
	return args.Name
}

func (c *Coordinator) matchSkillsForPrompt(prompt string) []*skill.SkillDef {
	skills := c.getSkills()
	if len(skills) == 0 {
		return nil
	}
	promptLower := strings.ToLower(prompt)
	var matched []*skill.SkillDef
	for _, s := range skills {
		for _, kw := range extractSkillKeywords(s) {
			if strings.Contains(promptLower, kw) {
				matched = append(matched, s)
				break
			}
		}
	}
	return matched
}

func extractSkillKeywords(s *skill.SkillDef) []string {
	stopWords := map[string]bool{
		"this": true, "that": true, "with": true, "from": true,
		"for": true, "the": true, "and": true, "its": true,
		"use": true, "like": true, "into": true, "over": true,
		"when": true, "also": true, "just": true, "than": true,
		"then": true, "will": true, "your": true, "both": true,
	}
	seen := map[string]bool{}
	var result []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !seen[s] && !stopWords[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	for _, part := range strings.Split(s.Name, "-") {
		if len(part) >= 3 {
			add(part)
		}
	}
	add(s.Name)
	add(strings.ReplaceAll(s.Name, "-", " "))
	for _, word := range strings.Fields(s.Description) {
		word = strings.Trim(word, ".,;:!?\"'()")
		if len(word) >= 4 {
			add(word)
		}
	}
	return result
}

func (c *Coordinator) Sidecar() *sidecar.Sidecar {
	if c.sidecarModel == "" {
		return nil
	}
	c.sidecarInitMu.Lock()
	defer c.sidecarInitMu.Unlock()
	if c.sidecarInit {
		return c.sidecarInst
	}
	ctx := context.Background()
	s, err := sidecar.NewSidecar(ctx, c.provider, c.sidecarModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to initialize sidecar model %q: %v\n", c.sidecarModel, err)
		return nil
	}
	c.sidecarInst = s
	c.sidecarInit = true
	return c.sidecarInst
}

func (c *Coordinator) matchSkillsWithSidecar(ctx context.Context, prompt string) []*skill.SkillDef {
	allSkills := c.getSkills()
	if len(allSkills) == 0 {
		return nil
	}

	s := c.Sidecar()
	if s != nil {
		summaries := make([]sidecar.SkillSummary, len(allSkills))
		for i, sk := range allSkills {
			summaries[i] = sidecar.SkillSummary{
				Name:        sk.Name,
				Description: sk.Description,
			}
		}
		names, err := s.MatchSkills(ctx, prompt, summaries)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: sidecar skill matching failed, using keyword fallback: %v\n", err)
		} else if len(names) > 0 {
			nameSet := map[string]bool{}
			for _, n := range names {
				nameSet[strings.ToLower(strings.TrimSpace(n))] = true
			}
			var matched []*skill.SkillDef
			for _, sk := range allSkills {
				if nameSet[strings.ToLower(sk.Name)] {
					matched = append(matched, sk)
				}
			}
			matchedNames := make([]string, len(matched))
			for i, sk := range matched {
				matchedNames[i] = sk.Name
			}
			c.report(c.newEvent("sidecar_call").withMessage("match_skills → " + strings.Join(matchedNames, ", ")))
			return matched
		}
		c.report(c.newEvent("sidecar_call").withMessage("match_skills → (no matches)"))
	}

	fallback := c.matchSkillsForPrompt(prompt)
	if len(fallback) > 0 {
		names := make([]string, len(fallback))
		for i, sk := range fallback {
			names[i] = sk.Name
		}
		c.report(c.newEvent("sidecar_call").withMessage("match_skills (keyword) → " + strings.Join(names, ", ")))
	}
	return fallback
}

func (c *Coordinator) newEvent(eventType string) StatusEvent {
	return StatusEvent{Type: eventType, TeamName: c.session.Config.Name}
}

func (c *Coordinator) updateTodoTiming(todoID string, modelTime, toolTime time.Duration) {
	c.taskTracker.TodoList().UpdateTodoTiming(todoID, modelTime, toolTime)
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

func (c *Coordinator) SetCurrentAgent(name string) {
	c.currentAgentNameMu.Lock()
	defer c.currentAgentNameMu.Unlock()
	c.currentAgentName = name
}

func (c *Coordinator) GetCurrentAgent() string {
	c.currentAgentNameMu.RLock()
	defer c.currentAgentNameMu.RUnlock()
	return c.currentAgentName
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
		Name:        "agent",
		Description: "Delegate tasks to team workers. Runs all tasks in parallel. Returns structured results from each agent.",
		Parameters: map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"agent":         map[string]any{"type": "string", "enum": t.coordinator.workerNameList(), "description": "Agent name to delegate to"},
						"task":          map[string]any{"type": "string", "description": "Task description for the agent"},
						"model":         map[string]any{"type": "string", "description": "Model ID from Available Models to use for this task. Select the model whose strengths best match this task. If empty, the default team model will be used."},
						"summarize":     map[string]any{"type": "boolean", "description": "If true, summarize the agent's output before returning. Use for tasks that produce verbose output where only key points matter."},
						"sidecar":       map[string]any{"type": "boolean", "description": "If true, execute this task directly via the sidecar model instead of an agent. Use for simple, tool-free tasks that need a quick response."},
						"context_files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional files from the workspace shared/ directory to provide as context"},
					},
					"required": []string{"agent", "task"},
				},
			},
		},
		Required: []string{"tasks"},
	}
}

func (t *runAgentsTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
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

func (t *finishTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
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
		Required: []string{"name"},
	}
}

func (t *loadSkillTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
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
	skills := t.coordinator.getSkills()
	for _, s := range skills {
		if strings.ToLower(s.Name) == nameLower {
			t.coordinator.recordSkillUsage(s.Name, "coordinator")
			return fantasy.NewTextResponse(fmt.Sprintf("Skill: %s\n\n%s", s.Name, s.Content)), nil
		}
	}

	available := make([]string, len(skills))
	for i, s := range skills {
		available[i] = s.Name
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("skill %q not found (available: %v)", args.Name, available)), nil
}

type workerAgentTool struct {
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
}

func (t *workerAgentTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "agent",
		Description: "Create a sub-agent to execute a specific task. The sub-agent inherits the same tool set as you. Returns the sub-agent's output.",
		Parameters: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "A descriptive name for this sub-agent invocation",
			},
			"task": map[string]any{
				"type":        "string",
				"description": "The task description for the sub-agent to execute",
			},
		},
		Required: []string{"name", "task"},
	}
}

func (t *workerAgentTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *workerAgentTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *workerAgentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Name string `json:"name"`
		Task string `json:"task"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Name == "" {
		return fantasy.NewTextErrorResponse("name is required"), nil
	}
	if args.Task == "" {
		return fantasy.NewTextErrorResponse("task is required"), nil
	}

	callerName := t.coordinator.GetCurrentAgent()
	subAgentLabel := callerName + "/" + args.Name

	result, err := t.coordinator.ExecuteSubAgent(ctx, subAgentLabel, args.Task)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(result), nil
}

func (c *Coordinator) ExecuteSubAgent(ctx context.Context, name string, task string) (string, error) {
	if c.IsWrapUp() {
		return "", fmt.Errorf("wrap-up in progress: cannot create sub-agent")
	}

	subModel := c.resolveAgentModel(&agent.AgentDef{
		Generation:  c.session.Config.Generation,
		ProviderURL: c.session.Config.ProviderURL,
	}, "")

	todoItems := c.taskTracker.TodoList().AddBatch([]struct {
		Agent string
		Desc  string
		Model string
	}{{Agent: name, Desc: task, Model: subModel}})
	todoID := todoItems[0].ID

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("start").withAgent(name).withMessage(task).withModel(subModel))

	def := &agent.AgentDef{
		Name:        name,
		Description: "Sub-agent for task: " + task,
		Role:        "worker",
		Tools:       "",
		System:      "You are a sub-agent. Complete the assigned task efficiently. Use the tools available to you.",
		MaxRetries:  -1,
		Generation:  c.session.Config.Generation,
		ProviderURL: c.session.Config.ProviderURL,
	}

	def = c.injectWorkerContext(ctx, def)

	agentTools := agent.SelectTools(c.coreTools, def.Tools)
	if c.mcpManager != nil {
		agentTools = append(agentTools, c.mcpManager.AsAgentTools()...)
	}

	ag, err := agent.CreateAgent(ctx, c.provider, agent.AgentConfig{
		Def:        def,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   agent.DefaultMaxSteps,
	}, agentTools)
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("error").withAgent(name).withMessage(err.Error()))
		return "", fmt.Errorf("failed to create sub-agent: %w", err)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	taskCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()

	taskPrompt := task
	if prefix := c.buildAutoSkillPrefix(def, name, task); prefix != "" {
		taskPrompt = prefix + taskPrompt
	}

	timing := &taskTiming{}
	timing.reset()

	currentAgent := c.GetCurrentAgent()
	c.SetCurrentAgent(name)
	output, _, err := c.runAgentWithStatusAndHistory(taskCtx, ag, name, taskPrompt, nil, timing)
	c.SetCurrentAgent(currentAgent)

	duration, modelTime, toolTime := timing.snapshot()
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.updateTodoTiming(todoID, modelTime, toolTime)
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("error").withAgent(name).withMessage(err.Error()).withModel(subModel).withTiming(duration, modelTime, toolTime))
		return "", fmt.Errorf("sub-agent failed (model: %s): %w", subModel, err)
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
	c.updateTodoTiming(todoID, modelTime, toolTime)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(name).withMessage("completed").withModel(subModel).withTiming(duration, modelTime, toolTime))
	return output, nil
}

type todoTool struct {
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
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

func (t *todoTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *todoTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

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

	callerName := t.coordinator.GetCurrentAgent()
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

	batch := make([]struct {
		Agent string
		Desc  string
		Model string
	}, len(items))
	for i, desc := range items {
		batch[i] = struct {
			Agent string
			Desc  string
			Model string
		}{Agent: callerName, Desc: desc, Model: resolvedModel}
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

const maxConcurrentTasks = 8
const maxConversationHistory = 100
const compactHistoryThreshold = 80
const maxMessageSize = 50000

type agentTaskResult struct {
	agentName string
	todoID    string
	task      string
	output    string
	err       error
}

func (c *Coordinator) checkDuplicateTasks(tasks []TaskDef) []string {
	var warnings []string
	c.delegatedTasksMu.Lock()
	defer c.delegatedTasksMu.Unlock()
	for _, t := range tasks {
		key := strings.ToLower(t.Agent) + ":" + truncateTaskDesc(t.Task)
		c.delegatedTasks[key]++
		if c.delegatedTasks[key] > 1 {
			warnings = append(warnings, fmt.Sprintf("%s (agent=%s, count=%d)", truncateTaskDesc(t.Task), t.Agent, c.delegatedTasks[key]))
		}
	}
	return warnings
}

func formatTaskResults(results []agentTaskResult, totalTasks int, duplicateWarnings []string) (string, error) {
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
	summary := fmt.Sprintf("\n\n---\nSummary: %d/%d tasks completed successfully", successCount, totalTasks)
	if errorCount > 0 {
		summary += fmt.Sprintf(", %d failed", errorCount)
	}
	b.WriteString(summary)
	if len(duplicateWarnings) > 0 {
		b.WriteString("\n\n**Warning**: You have delegated the same task to the same agent multiple times. This suggests you may be stuck in a loop. Consider using a different approach, agent, or calling `finish` with your best answer so far:\n")
		for _, w := range duplicateWarnings {
			b.WriteString(fmt.Sprintf("- %s\n", w))
		}
	}
	if successCount == 0 && len(results) > 0 {
		return b.String(), fmt.Errorf("all %d tasks failed", len(results))
	}
	return b.String(), nil
}

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

	todoBatch := make([]struct {
		Agent string
		Desc  string
		Model string
	}, len(tasks))
	for i, t := range tasks {
		agentDef, _, _ := c.resolveAgentName(t.Agent)
		var resolvedModel string
		if agentDef != nil {
			resolvedModel = c.resolveAgentModel(agentDef, t.Model)
		}
		todoBatch[i] = struct {
			Agent string
			Desc  string
			Model string
		}{Agent: strings.ToLower(t.Agent), Desc: t.Task, Model: resolvedModel}
	}
	todoItems := c.taskTracker.TodoList().AddBatch(todoBatch)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	duplicateWarnings := c.checkDuplicateTasks(tasks)
	if len(duplicateWarnings) > 0 {
		c.report(c.newEvent("loop_warning").withMessage(fmt.Sprintf("Duplicate task delegation detected: %v", duplicateWarnings)))
	}

	resultsCh := make(chan agentTaskResult, len(tasks))
	sem := make(chan struct{}, maxConcurrentTasks)
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(td TaskDef, tid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var output string
			var err error
			if td.Sidecar {
				output, err = c.executeSidecarTask(ctx, td, tid)
			} else {
				output, err = c.executeTask(ctx, td, tid)
			}
			resultsCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: td.Task, output: output, err: err}
		}(task, todoItems[i].ID)
	}
	wg.Wait()
	close(resultsCh)

	var results []agentTaskResult
	for r := range resultsCh {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].agentName < results[j].agentName
	})

	return formatTaskResults(results, len(tasks), duplicateWarnings)
}

func (c *Coordinator) executeTask(parentCtx context.Context, task TaskDef, todoID string) (string, error) {
	agentDef, _, err := c.resolveAgentName(task.Agent)
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", err
	}
	agentName := strings.ToLower(agentDef.Name)

	if task.Model != "" && len(c.modelList) > 0 {
		found := false
		for _, m := range c.modelList {
			if m.ID == task.Model {
				found = true
				break
			}
		}
		if !found {
			var validIDs []string
			for _, m := range c.modelList {
				validIDs = append(validIDs, m.ID)
			}
			return "", fmt.Errorf("unknown model %q for agent %q (valid models: %v)", task.Model, task.Agent, validIDs)
		}
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

	resolvedModel := c.resolveAgentModel(agentDef, task.Model)

	c.report(c.newEvent("start").withAgent(agentName).withMessage(task.Task).withModel(resolvedModel))
	c.SetCurrentAgent(agentName)
	_ = writeStatus(c.session.Workspace, agentName, "working", task.Task)
	_ = writeInbox(c.session.Workspace, agentName, task.Task)

	timing := &taskTiming{}
	timing.reset()

	ag, err := c.getOrCreateAgent(parentCtx, agentDef, task.Model)
	if err != nil {
		c.report(c.newEvent("error").withAgent(agentName).withMessage(err.Error()))
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		writeStatus(c.session.Workspace, agentName, "error", task.Task)
		return "", err
	}

	prompt := task.Task

	if prefix := c.buildSkillPromptPrefix(agentDef); prefix != "" {
		prompt = prefix + prompt
	}

	if prefix := c.buildAutoSkillPrefix(agentDef, agentName, task.Task); prefix != "" {
		prompt = prefix + prompt
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

		var output string
		var steps []fantasy.StepResult
		var err error
		func() {
			taskCtx, cancel := context.WithTimeout(parentCtx, agentTimeout)
			defer cancel()
			output, steps, err = c.runAgentWithStatusAndHistory(taskCtx, ag, agentName, prompt, conversationHistory, timing)
		}()

		if err == nil {
			_ = writeOutbox(c.session.Workspace, agentName, output)
			_ = writeStatus(c.session.Workspace, agentName, "done", task.Task)
			duration, modelTime, toolTime := timing.snapshot()
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
			c.updateTodoTiming(todoID, modelTime, toolTime)
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("done").withAgent(agentName).withMessage("completed").withModel(resolvedModel).withTiming(duration, modelTime, toolTime))
			if task.Summarize {
				output = c.summarizeOutput(parentCtx, output)
			}
			return output, nil
		}

		for _, step := range steps {
			conversationHistory = append(conversationHistory, step.Messages...)
		}

		lastErr = err
		c.report(c.newEvent("error").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d failed: %v", attempt, err)).withModel(resolvedModel))
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, fmt.Sprintf("attempt %d failed: %v", attempt, err))
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

		if parentCtx.Err() != nil {
			break
		}
	}

	_, modelTime, toolTime := timing.snapshot()
	c.updateTodoTiming(todoID, modelTime, toolTime)
	_ = writeStatus(c.session.Workspace, agentName, "error", task.Task)
	return "", fmt.Errorf("agent %q failed after %d attempts (model: %s): %w", agentName, maxRetries, resolvedModel, lastErr)
}

func (c *Coordinator) runAgentWithStatus(ctx context.Context, ag fantasy.Agent, agentName, prompt string, timing *taskTiming) (string, error) {
	output, _, err := c.runAgentWithStatusAndHistory(ctx, ag, agentName, prompt, nil, timing)
	return output, err
}

func (c *Coordinator) executeSidecarTask(ctx context.Context, task TaskDef, todoID string) (string, error) {
	s := c.Sidecar()
	if s == nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, "sidecar not configured")
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", fmt.Errorf("sidecar not configured: set sidecar-model in team.yaml or hufu.yaml")
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("sidecar_call").withAgent(task.Agent).withMessage(task.Task))

	sidecarTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if sidecarTimeout <= 0 {
		sidecarTimeout = 120 * time.Second
	}
	sidecarCtx, cancel := context.WithTimeout(ctx, sidecarTimeout)
	defer cancel()

	result, err := s.Execute(sidecarCtx, task.Task)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar execute failed for agent %q: %v\n", task.Agent, err)
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", fmt.Errorf("sidecar execution failed (model: %s): %w", c.sidecarModel, err)
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(task.Agent).withMessage("sidecar completed"))
	return result, nil
}

func (c *Coordinator) runAgentWithStatusAndHistory(ctx context.Context, ag fantasy.Agent, agentName, prompt string, history []fantasy.Message, timing *taskTiming) (string, []fantasy.StepResult, error) {
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
			timing.beginTool()
			argsPreview := tc.Input
			if len(argsPreview) > 200 {
				argsPreview = argsPreview[:200] + "..."
			}
			reportFn(c.newEvent("tool_call").withAgent(agentName).withTool(tc.ToolName, argsPreview))
			llmLogStreamEvent(logWrite, "tool_call", formatToolCallContent(tc))
			audit.LogToolCall(agentName, tc.ToolName, tc.Input)
			if skillName := c.extractSkillFromToolCall(tc.ToolName, tc.Input); skillName != "" {
				c.recordSkillUsage(skillName, agentName)
			}
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			timing.endTool()
			resultPreview := ""
			if tr.Result != nil {
				if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
					resultPreview = txt.Text
				}
			}
			reportFn(c.newEvent("tool_result").withAgent(agentName).withToolResult(tr.ToolName, resultPreview))
			llmLogStreamEvent(logWrite, "tool_result", formatToolResultContent(tr))
			_, isErrResult := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Result)
			audit.LogToolResult(agentName, tr.ToolName, resultPreview, isErrResult)
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

func agentCacheKey(def *agent.AgentDef, overrideModel string) string {
	if overrideModel != "" {
		return def.Name + "|" + overrideModel
	}
	return def.Name
}

func (c *Coordinator) getOrCreateAgent(ctx context.Context, def *agent.AgentDef, overrideModel string) (fantasy.Agent, error) {
	cacheKey := agentCacheKey(def, overrideModel)
	c.agentCacheMu.RLock()
	if ag, ok := c.agentCache[cacheKey]; ok {
		c.agentCacheMu.RUnlock()
		return ag, nil
	}
	c.agentCacheMu.RUnlock()

	c.agentCacheMu.Lock()
	defer c.agentCacheMu.Unlock()

	if ag, ok := c.agentCache[cacheKey]; ok {
		return ag, nil
	}

	agentDef := def
	if overrideModel != "" {
		overriddenDef := *def
		overriddenDef.Generation.Model = overrideModel
		agentDef = &overriddenDef
	}

	agentDef = c.injectWorkerContext(ctx, agentDef)

	agentTools := agent.SelectTools(c.coreTools, agentDef.Tools)
	if c.mcpManager != nil {
		agentTools = append(agentTools, c.mcpManager.AsAgentTools()...)
	}

	ag, err := agent.CreateAgent(ctx, c.provider, agent.AgentConfig{
		Def:        agentDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   agent.DefaultMaxSteps,
	}, agentTools)
	if err != nil {
		return nil, err
	}

	c.agentCache[cacheKey] = ag
	return ag, nil
}

func (c *Coordinator) resolveAgentName(input string) (*agent.AgentDef, string, error) {
	key := strings.ToLower(input)
	if def, ok := c.session.Agents[key]; ok {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			return nil, "", fmt.Errorf("cannot delegate to coordinator agent %q", input)
		}
		return def, key, nil
	}

	var matches []*agent.AgentDef
	seenNames := make(map[string]bool)
	for _, def := range c.session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		if seenNames[def.Name] {
			continue
		}
		for _, word := range strings.Fields(def.Name) {
			if strings.ToLower(word) == key {
				matches = append(matches, def)
				seenNames[def.Name] = true
				break
			}
		}
		if seenNames[def.Name] {
			continue
		}
		if def.FileAlias != "" {
			for _, seg := range strings.Split(strings.ToLower(def.FileAlias), "-") {
				if seg != "" && seg == key {
					matches = append(matches, def)
					seenNames[def.Name] = true
					break
				}
			}
		}
	}

	if len(matches) == 1 {
		def := matches[0]
		return def, strings.ToLower(def.Name), nil
	}
	if len(matches) > 1 {
		var names []string
		for _, m := range matches {
			names = append(names, m.Name)
		}
		sort.Strings(names)
		return nil, "", fmt.Errorf("ambiguous agent name %q matches multiple agents: %v", input, names)
	}

	var available []string
	seenAvail := make(map[string]bool)
	for _, def := range c.session.Agents {
		if def.Role != "orchestrator" && def.Role != "coordinator" && !seenAvail[def.Name] {
			seenAvail[def.Name] = true
			available = append(available, def.Name)
		}
	}
	sort.Strings(available)
	return nil, "", fmt.Errorf("unknown agent %q (available: %v)", input, available)
}

func (c *Coordinator) uniqueWorkerDefs() []*agent.AgentDef {
	seen := make(map[string]bool)
	var defs []*agent.AgentDef
	for _, def := range c.session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		defs = append(defs, def)
	}
	return defs
}

func (c *Coordinator) agentNames() []string {
	defs := c.uniqueWorkerDefs()
	names := make([]string, len(defs))
	for i, def := range defs {
		names[i] = def.Name
	}
	return names
}

func (c *Coordinator) buildWorkerNamesAndDescs() (names []string, descs []string) {
	for _, def := range c.uniqueWorkerDefs() {
		desc := def.Name
		aliases := c.agentAliasesFor(def)
		if aliases != "" {
			desc += " (aliases: " + aliases + ")"
		}
		if def.Description != "" {
			desc += ": " + def.Description
		}
		if def.Tools != "" {
			desc += fmt.Sprintf(" (tools: %s)", def.Tools)
		}
		names = append(names, def.Name)
		descs = append(descs, desc)
	}
	return names, descs
}

func (c *Coordinator) agentAliasesFor(def *agent.AgentDef) string {
	nameLower := strings.ToLower(def.Name)
	var parts []string
	if def.FileAlias != "" && strings.ToLower(def.FileAlias) != nameLower {
		parts = append(parts, def.FileAlias)
	}
	for _, word := range strings.Fields(def.Name) {
		w := strings.ToLower(word)
		if w != nameLower && !containsPart(parts, w) {
			parts = append(parts, w)
		}
	}
	if def.FileAlias != "" {
		for _, seg := range strings.Split(strings.ToLower(def.FileAlias), "-") {
			if seg != nameLower && seg != "" && !containsPart(parts, seg) {
				parts = append(parts, seg)
			}
		}
	}
	return strings.Join(parts, ", ")
}

func containsPart(parts []string, s string) bool {
	for _, p := range parts {
		if p == s {
			return true
		}
	}
	return false
}

func (c *Coordinator) workerNameList() []string {
	names, _ := c.buildWorkerNamesAndDescs()
	return names
}

const maxAgentsMDSize = 50000
const maxWorkerContextSize = 4000

func (c *Coordinator) loadProjectContext() string {
	path := filepath.Join(c.projectDir, "AGENTS.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if utf8.RuneCountInString(string(data)) > maxAgentsMDSize {
		runes := []rune(string(data))
		data = []byte(string(runes[:maxAgentsMDSize]) + "\n\n... [AGENTS.md truncated]")
	}
	return string(data)
}

func (c *Coordinator) getWorkerContext(ctx context.Context) string {
	c.workerCtxOnce.Do(func() {
		raw := c.loadProjectContext()
		if raw == "" {
			return
		}
		if s := c.Sidecar(); s != nil && utf8.RuneCountInString(raw) > maxWorkerContextSize/2 {
			compacted, err := s.Compact(ctx, raw, "Extract the essential project context: tech stack, language, framework, key conventions, and directory structure. Preserve all factual details but remove verbose descriptions.")
			if err == nil && compacted != "" {
				raw = compacted
			}
		}
		if utf8.RuneCountInString(raw) > maxWorkerContextSize {
			raw = string([]rune(raw)[:maxWorkerContextSize]) + "\n\n... [truncated]"
		}
		c.cachedWorkerContext = raw
	})
	return c.cachedWorkerContext
}

// injectWorkerContext prepends the project context to the agent's system prompt
// if the agent is not an orchestrator/coordinator and a worker context is available.
// It returns a new AgentDef pointer (either the original unchanged or a copy with
// the modified System field) to avoid mutating shared state.
func (c *Coordinator) injectWorkerContext(ctx context.Context, def *agent.AgentDef) *agent.AgentDef {
	if def.Role == "orchestrator" || def.Role == "coordinator" {
		return def
	}
	wc := c.getWorkerContext(ctx)
	if wc == "" {
		return def
	}
	injectedDef := *def
	injectedDef.System = "## Project Context\n\n" + wc + "\n\n---\n\n" + injectedDef.System
	return &injectedDef
}

func (c *Coordinator) BuildOrchestratorPrompt(autoSkills ...*skill.SkillDef) string {
	workerNames, workerDescs := c.buildWorkerNamesAndDescs()

	var b strings.Builder
	fmt.Fprintf(&b, "You are the coordinator of team %q with %d members: %s.\n\n", c.session.Config.Name, len(workerNames), strings.Join(workerNames, ", "))

	b.WriteString("You MUST delegate ALL work to your team members. You do NOT have tools to do work yourself.\n\n")

	b.WriteString("## How to Coordinate\n\n")
	b.WriteString("1. **Analyze** the user's request to identify which team members are needed\n")
	b.WriteString("2. **Check skills** — if any available skills are relevant to the user's task, call `load_skill` to get the full instructions\n")
	b.WriteString("3. **Plan** your approach before delegating — think step by step\n")
	b.WriteString("4. **Select model** — for each task, pick the model from Available Models whose strengths best match the task requirements. Using the right model improves quality and speed.\n")
	b.WriteString("5. **Delegate** tasks using agent — this is the ONLY way to get work done\n")
	b.WriteString("6. Run independent tasks in parallel by passing multiple tasks in one agent call\n")
	b.WriteString("7. When delegating to a worker that needs skill knowledge, include the skill summary in the task description and mention the skill file path so the worker can read it if needed\n")
	b.WriteString("8. **Include relevant details** — Workers already know the project context (tech stack, conventions, directory structure), but you should still specify file paths, function names, and specific constraints in your task descriptions to minimize exploration steps\n")
	b.WriteString("9. **Evaluate** results after each agent call — decide if more work is needed or if you can provide a final answer\n")
	b.WriteString("10. **Synthesize** results into a coherent answer for the user\n")
	b.WriteString("11. When satisfied, call the finish tool with your final response\n\n")

	b.WriteString("## Available Agents\n\n")
	fmt.Fprintf(&b, "IMPORTANT: You MUST use these agent names in the agent tool: %s. You can also use listed aliases. Do NOT invent agent names that are not listed.\n\n", strings.Join(workerNames, ", "))
	for _, desc := range workerDescs {
		fmt.Fprintf(&b, "- %s\n", desc)
	}
	b.WriteString("\n")

	b.WriteString("## Worker Tools\n\n")
	b.WriteString("Workers have access to the following special tools in addition to their configured toolset:\n\n")
	b.WriteString("- **agent**: Create a sub-agent to handle a specific sub-task. The sub-agent inherits the same toolset.\n")
	b.WriteString("- **todo**: Manage a task list to track progress. Workers can create, update, and list their own TODO items.\n\n")

	b.WriteString("## Available Skills\n\n")
	currentSkills := c.getSkills()
	if len(currentSkills) == 0 {
		b.WriteString("No skills are available for this team.\n\n")
	} else {
		b.WriteString("| Skill | Description |\n")
		b.WriteString("|-------|-------------|\n")
		for _, s := range currentSkills {
			desc := s.Description
			if utf8.RuneCountInString(desc) > 80 {
				runes := []rune(desc)
				desc = string(runes[:80]) + "..."
			}
			fmt.Fprintf(&b, "| %s | %s |\n", s.Name, desc)
		}
		b.WriteString("\n")
		b.WriteString("To get the full instructions for any skill, call the `load_skill` tool with the skill name.\n")
		b.WriteString("Before delegating tasks, consider loading relevant skills so you can include their key instructions in the task description.\n\n")
	}

	if len(autoSkills) > 0 {
		b.WriteString("## Auto-Loaded Skills\n\n")
		b.WriteString("The following skills were automatically loaded because they match the user's task. You already have their full instructions — include the relevant parts in worker task descriptions.\n\n")
		for _, s := range autoSkills {
			fmt.Fprintf(&b, "### %s\n%s\n\n", s.Name, s.Content)
		}
		b.WriteString("---\n\n")
	}

	b.WriteString("## Available Models\n\n")
	if len(c.modelList) == 0 {
		b.WriteString("No model list configured. The default team model will be used for all tasks.\n\n")
	} else {
		b.WriteString("IMPORTANT: Select the most appropriate model for each task based on its requirements. Each model has different strengths — match the task to the model best suited for it.\n\n")
		for _, m := range c.modelList {
			detail := strings.TrimSpace(m.Details)
			detail = strings.Join(strings.Split(detail, "\n"), " ")
			fmt.Fprintf(&b, "- **%s** — %s\n", m.ID, detail)
		}
		b.WriteString("\nIf no model is specified, the default team model will be used — but this is often suboptimal.\n\n")
	}

	b.WriteString("## Tools\n\n")
	b.WriteString("### agent\n")
	b.WriteString("Delegate tasks to team workers. All tasks in one call run in parallel.\n\n")
	b.WriteString("- **model**: Choose the model whose strengths best match each task — see Available Models above.\n")
	b.WriteString("- **summarize**: Set to `true` to condense the agent's output before returning. Use for tasks that may produce verbose output where only key points matter.\n")
	b.WriteString("```json\n")
	b.WriteString("{\n")
	b.WriteString("  \"tasks\": [\n")
	b.WriteString("    {\"agent\": \"agent-name\", \"task\": \"task description\", \"model\": \"model-id-from-available-models\", \"summarize\": false, \"context_files\": [\"optional_file.txt\"]}\n")
	b.WriteString("  ]\n")
	b.WriteString("}\n```\n\n")
	if len(c.modelList) >= 2 {
		b.WriteString("Example — if Available Models includes a fast model for simple tasks and a powerful model for complex reasoning, assign accordingly:\n```json\n")
		b.WriteString("{\n")
		b.WriteString("  \"tasks\": [\n")
		var fastModel, complexModel string
		for _, m := range c.modelList {
			if fastModel == "" {
				fastModel = m.ID
			}
			complexModel = m.ID
		}
		fmt.Fprintf(&b, "    {\"agent\": \"worker-name\", \"task\": \"fix a typo in README\", \"model\": \"%s\"},\n", fastModel)
		fmt.Fprintf(&b, "    {\"agent\": \"worker-name\", \"task\": \"design distributed consensus algorithm\", \"model\": \"%s\"}\n", complexModel)
		b.WriteString("  ]\n")
		b.WriteString("}\n```\n\n")
	}
	b.WriteString("### load_skill\n")
	b.WriteString("Load the full content of a skill by name. Returns detailed instructions you can include in worker task descriptions.\n")
	b.WriteString("```json\n{\"name\": \"skill-name\"}\n```\n\n")
	b.WriteString("### save_skill\n")
	b.WriteString("Save a reusable skill to disk and reload it immediately. Use this when you or a worker has solved a non-trivial problem and you want to encode the solution for future reuse.\n")
	b.WriteString("```json\n{\"name\": \"skill-name\", \"description\": \"what it does\", \"content\": \"# Skill\\n\\nStep-by-step workflow...\"}\n```\n\n")
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
			defCopy := *def
			c.ensureModelFallback(&defCopy)
			return &defCopy
		}
	}
	for _, def := range c.session.Agents {
		n := strings.ToLower(def.Name)
		if strings.Contains(n, "coordinat") || strings.Contains(n, "orchestr") {
			defCopy := *def
			c.ensureModelFallback(&defCopy)
			return &defCopy
		}
	}
	def := &agent.AgentDef{
		Name:        "coordinator",
		Description: "Default team coordinator",
		Role:        "coordinator",
		Tools:       "ask_user",
		System:      "",
		MaxRetries:  -1,
		Generation:  c.session.Config.Generation,
		ProviderURL: c.session.Config.ProviderURL,
	}
	c.ensureModelFallback(def)
	return def
}

func (c *Coordinator) ensureModelFallback(def *agent.AgentDef) {
	if def.Generation.Model == "" && c.session.Config.Generation.Model != "" {
		def.Generation.Model = c.session.Config.Generation.Model
	}
	if def.ProviderURL == "" && c.session.Config.ProviderURL != "" {
		def.ProviderURL = c.session.Config.ProviderURL
	}
}

func (c *Coordinator) resolveCurrentAgentModel(agentName string) string {
	agentDef, _, err := c.resolveAgentName(agentName)
	if err == nil && agentDef != nil {
		return c.resolveAgentModel(agentDef, "")
	}
	return c.session.Config.Generation.Model
}

func (c *Coordinator) resolveAgentModel(def *agent.AgentDef, overrideModel string) string {
	if overrideModel != "" {
		return overrideModel
	}
	if def.Generation.Model != "" {
		return def.Generation.Model
	}
	return c.session.Config.Generation.Model
}

func (c *Coordinator) summarizeOutput(ctx context.Context, text string) string {
	s := c.Sidecar()
	if s == nil {
		return text
	}
	c.report(c.newEvent("sidecar_call").withMessage("summarize"))
	summarized, err := s.Summarize(ctx, text, 2000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar summarize failed: %v\n", err)
		return text
	}
	return summarized
}

func (c *Coordinator) expandDefaultOrchestratorTemplate(tmpl string) string {
	workerNames := c.workerNameList()
	s := strings.ReplaceAll(tmpl, "{{TEAM_NAME}}", c.session.Config.Name)
	s = strings.ReplaceAll(s, "{{AGENT_COUNT}}", fmt.Sprintf("%d", len(workerNames)))
	s = strings.ReplaceAll(s, "{{AGENT_NAMES}}", strings.Join(workerNames, ", "))
	return s
}

func (c *Coordinator) appendHistory(ctx context.Context, steps []fantasy.StepResult) {
	for _, step := range steps {
		for _, msg := range step.Messages {
			if estimateMessageSize(msg) > maxMessageSize {
				continue
			}
			c.conversationHistory = append(c.conversationHistory, msg)
		}
	}
	if len(c.conversationHistory) <= maxConversationHistory {
		return
	}
	compactCount := len(c.conversationHistory) - compactHistoryThreshold
	if compactCount <= 0 {
		compactCount = len(c.conversationHistory) / 3
	}
	if compactCount <= 0 {
		trimmed := make([]fantasy.Message, maxConversationHistory)
		copy(trimmed, c.conversationHistory[len(c.conversationHistory)-maxConversationHistory:])
		c.conversationHistory = trimmed
		return
	}
	compacted := c.compactMessages(ctx, c.conversationHistory[:compactCount])
	c.conversationHistory = append(compacted, c.conversationHistory[compactCount:]...)
	if len(c.conversationHistory) > maxConversationHistory {
		trimmed := make([]fantasy.Message, maxConversationHistory)
		copy(trimmed, c.conversationHistory[len(c.conversationHistory)-maxConversationHistory:])
		c.conversationHistory = trimmed
	}
}

func (c *Coordinator) compactMessages(ctx context.Context, messages []fantasy.Message) []fantasy.Message {
	s := c.Sidecar()
	if s == nil || len(messages) < 2 {
		return messages
	}
	var b strings.Builder
	for _, msg := range messages {
		for _, part := range msg.Content {
			if txt, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				b.WriteString(txt.Text)
				b.WriteString("\n")
			}
		}
	}
	if b.Len() == 0 {
		return messages
	}
	result, err := s.Compact(ctx, b.String(), "Compress the following conversation into a concise summary while preserving key facts, decisions, and results.")
	if err != nil || result == "" {
		return messages
	}
	return []fantasy.Message{
		fantasy.NewUserMessage("[Compacted history]\n" + result),
	}
}

func estimateMessageSize(msg fantasy.Message) int {
	total := 0
	for _, part := range msg.Content {
		if txt, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			total += len(txt.Text)
		}
	}
	return total
}

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
	agentDef, _, err := c.resolveAgentName(agentName)
	if err != nil {
		return nil, err
	}

	resolvedName := strings.ToLower(agentDef.Name)
	directModel := c.resolveAgentModel(agentDef, "")

	todoItems := c.taskTracker.TodoList().AddBatch([]struct {
		Agent string
		Desc  string
		Model string
	}{{Agent: resolvedName, Desc: task, Model: directModel}})
	todoID := todoItems[0].ID
	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.SetCurrentAgent(resolvedName)

	ag, err := c.getOrCreateAgent(ctx, agentDef, "")
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return nil, fmt.Errorf("failed to create agent %q: %w", resolvedName, err)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}

	taskCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()

	timing := &taskTiming{}
	timing.reset()

	_ = writeInbox(c.session.Workspace, resolvedName, task)
	_ = writeStatus(c.session.Workspace, resolvedName, "working", task)

	prompt := task
	if prefix := c.buildSkillPromptPrefix(agentDef); prefix != "" {
		prompt = prefix + prompt
	}

	if prefix := c.buildAutoSkillPrefix(agentDef, resolvedName, task); prefix != "" {
		prompt = prefix + prompt
	}

	output, err := c.runAgentWithStatus(taskCtx, ag, resolvedName, prompt, timing)
	duration, modelTime, toolTime := timing.snapshot()
	if err != nil {
		_ = writeStatus(c.session.Workspace, resolvedName, "error", task)
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.updateTodoTiming(todoID, modelTime, toolTime)
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("error").withAgent(resolvedName).withMessage(err.Error()).withModel(directModel).withTiming(duration, modelTime, toolTime))
		return &DirectAgentResult{AgentName: resolvedName, Error: err}, nil
	}

	_ = writeOutbox(c.session.Workspace, resolvedName, output)
	_ = writeStatus(c.session.Workspace, resolvedName, "done", task)
	c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
	c.updateTodoTiming(todoID, modelTime, toolTime)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(resolvedName).withMessage("completed").withModel(directModel).withTiming(duration, modelTime, toolTime))

	return &DirectAgentResult{AgentName: resolvedName, Output: output}, nil
}

func (c *Coordinator) buildOrchestratorTools() []fantasy.AgentTool {
	orchTools := []fantasy.AgentTool{
		c.RunAgentsTool(),
		&finishTool{},
		&loadSkillTool{coordinator: c},
		&saveSkillTool{coordinator: c},
	}
	for _, t := range c.coreTools {
		if t.Info().Name == "ask_user" {
			orchTools = append(orchTools, t)
			break
		}
	}
	return orchTools
}

func (c *Coordinator) runOrchestrator(ctx context.Context, orchDef *agent.AgentDef, prompt string) (string, []fantasy.StepResult, error) {
	coordinatorTimeout := time.Duration(c.session.Config.Timeout) * time.Second * time.Duration(c.session.Config.MaxRounds+1)
	if orchDef.Timeout > 0 {
		coordinatorTimeout = time.Duration(orchDef.Timeout) * time.Second
	}

	orchCtx, cancel := context.WithTimeout(ctx, coordinatorTimeout)
	defer cancel()

	orch, err := agent.CreateAgent(orchCtx, c.provider, agent.AgentConfig{
		Def:        orchDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   agent.DefaultCoordinatorMaxSteps,
	}, c.buildOrchestratorTools())
	if err != nil {
		return "", nil, fmt.Errorf("failed to create coordinator: %w", err)
	}

	c.conversationHistoryMu.Lock()
	historySnapshot := make([]fantasy.Message, len(c.conversationHistory))
	copy(historySnapshot, c.conversationHistory)
	c.conversationHistoryMu.Unlock()

	return c.runAgentWithStatusAndHistory(orchCtx, orch, orchDef.Name, prompt, historySnapshot, &taskTiming{})
}

func (c *Coordinator) saveHistoryAndSession(ctx context.Context, steps []fantasy.StepResult) {
	c.conversationHistoryMu.Lock()
	c.appendHistory(ctx, steps)
	SaveConversationHistory(c.session.Workspace, c.conversationHistory)
	c.conversationHistoryMu.Unlock()
	if c.sessionData != nil {
		SaveSession(c.session.Workspace, c.sessionData)
	}
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
		systemPrompt = c.expandDefaultOrchestratorTemplate(defaultOrchestratorSystem)
	}
	matchedSkills := c.matchSkillsWithSidecar(ctx, userPrompt)
	c.autoLoadedSkills = matchedSkills
	systemPrompt += "\n\n" + c.BuildOrchestratorPrompt(matchedSkills...)

	if agentsMD := c.loadProjectContext(); agentsMD != "" {
		if s := c.Sidecar(); s != nil && len(agentsMD) > 4000 {
			compacted, err := s.Compact(ctx, agentsMD, "Compress this project context while preserving all key facts, patterns, conventions, and instructions.")
			if err == nil && compacted != "" {
				agentsMD = compacted
			}
		}
		systemPrompt += "\n\n---\n## Project Context (AGENTS.md)\n\n" + agentsMD
	}

	if c.memoryStore != nil {
		var compactFn memory.CompactFunc
		if s := c.Sidecar(); s != nil {
			compactFn = s.Compact
		}
		memCtx, err := memory.AutoQuery(ctx, c.memoryStore, userPrompt, compactFn)
		if err == nil && memCtx != "" {
			systemPrompt += "\n\n---\n" + memCtx
		}
	}

	if c.sessionData != nil && len(c.sessionData.Entries) > 1 && len(c.conversationHistory) == 0 {
		contextSummary := c.sessionData.ContextSummary()
		if contextSummary != "" {
			systemPrompt += "\n\n---\n## Session Context\n\n" + contextSummary
		}
	}

	// Apply the computed system prompt to a copy so shared state is not mutated.
	orchDefCopy := *orchDef
	orchDefCopy.System = systemPrompt

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("coordinator starting"))

	result, steps, err := c.runOrchestrator(ctx, &orchDefCopy, userPrompt)
	if err != nil {
		c.saveHistoryAndSession(ctx, steps)
		orchModel := c.resolveAgentModel(orchDef, "")
		return "", fmt.Errorf("coordinator failed (model: %s): %w", orchModel, err)
	}

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

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

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("continuing with additional input"))

	result, steps, err := c.runOrchestrator(ctx, orchDef, continuationPrompt)
	if err != nil {
		c.saveHistoryAndSession(ctx, steps)
		orchModel := c.resolveAgentModel(orchDef, "")
		return "", fmt.Errorf("coordinator continuation failed (model: %s): %w", orchModel, err)
	}

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

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
- You MUST use agent to delegate ALL work to team members
- Running independent tasks in parallel is preferred
- After receiving results from agent, evaluate whether more work is needed or if you can provide a final answer
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
- Do NOT call agent again
- Immediately summarize what has been accomplished so far based on all results you have received
- Call the finish tool RIGHT NOW with your best summary of the work completed

This is a wrap-up request. You MUST call finish immediately with whatever results are available.`

func truncateTaskDesc(task string) string {
	const maxLen = 80
	if utf8.RuneCountInString(task) > maxLen {
		runes := []rune(task)
		return string(runes[:maxLen])
	}
	return task
}
