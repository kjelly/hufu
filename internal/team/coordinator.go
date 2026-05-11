package team

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	"github.com/anomalyco/hufu/internal/hooks"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/memory"
	"github.com/anomalyco/hufu/internal/sidecar"
	"github.com/anomalyco/hufu/internal/skill"
	"github.com/anomalyco/hufu/internal/tools"
)

// CoordTodoID is the special TodoItem ID used for the coordinator/orchestrator
// pseudo-task that appears in the TUI and status reporting.
const CoordTodoID = "__coord__"

var skillSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// todoIDKey is a context key used to pass the current task's TodoItem ID
// down through executeTask → runAgentWithStatusAndHistory so that emitted
// StatusEvents can be attributed to a specific task for the TUI.
type todoIDKey struct{}

// modelKey is a context key used to pass the resolved model name down
// through executeTask → runAgentWithStatusAndHistory so that tool_result
// events can include the model for TUI display.
type modelKey struct{}

type TaskDef struct {
	Agent        string   `json:"agent"`
	Goal         string   `json:"goal"`
	Constraints  string   `json:"constraints,omitempty"`
	Model        string   `json:"model,omitempty"`
	Sidecar      bool     `json:"sidecar,omitempty"`
	Summarize    bool     `json:"summarize,omitempty"`
	ContextFiles []string `json:"context_files,omitempty"`
	PlanFirst    bool     `json:"plan_first,omitempty"`
	PlanID       string   `json:"plan_id,omitempty"`
}

// UnmarshalJSON handles legacy "task" field by mapping it to Goal.
func (t *TaskDef) UnmarshalJSON(data []byte) error {
	type Alias TaskDef
	aux := &struct {
		Task *string `json:"task"`
		*Alias
	}{Alias: (*Alias)(t)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if t.Goal == "" && aux.Task != nil {
		t.Goal = *aux.Task
	}
	return nil
}

type DirectAgentResult struct {
	AgentName string
	Output    string
	Error     error
}

type DryRunAgentInfo struct {
	Name   string
	Role   string
	Model  string
	Tools  []string
	Skills []string
}

type DryRunSkillInfo struct {
	Name        string
	Description string
}

type DryRunResult struct {
	TeamName           string
	Model              string
	SidecarModel       string
	Agents             []DryRunAgentInfo
	AllSkills          []DryRunSkillInfo
	MatchedSkillNames  []string
	OrchestratorPrompt string
	FirstRoundTasks    []TaskDef
	Error              string
}

type PlanEntry struct {
	TodoID      string
	Agent       string
	Goal        string
	PlanText    string
	Status      string // "submitted", "approved", "modified", "rejected"
	ReviewCount int
}

const planReviewerMaxReviews = 3

const planReviewerSystemPrompt = `You are a Plan Reviewer. Review agent-submitted execution plans against the original user requirement.

Input:
- USER REQUIREMENT: The original task goal
- COMPLETED TASKS: Previously completed tasks with their results (to detect actual duplication)
- PLAN: The agent's proposed execution plan

Rules:
1. APPROVE: The plan is clear, addresses the USER REQUIREMENT, and is not a true duplicate of completed work. Approval is the DEFAULT — only reject when there is clear evidence of duplication.
2. REJECT only when: the plan repeats the EXACT SAME work that was already completed (same deliverable, same file, same outcome). Do NOT reject because tasks share a category (e.g., "writing files" is normal — multiple files are different work). Provide a SPECIFIC reason referencing which completed task it duplicates.

Creating files, writing documents, generating code — these are legitimate execution plans. APPROVE them.

You MUST call one of:
- approve_plan(todo_id) → execute the plan immediately
- reject_plan(todo_id, reason) → agent re-plans with your feedback

No other response format. Do NOT do the work yourself — only approve or reject.`

// planReviewer implements an autonomous plan review agent using a sidecar model.
// It is NOT user-configurable — it only activates when forcePlanFirst is set.
type planReviewer struct {
	coordinator *Coordinator
	modelID     string
	agent       fantasy.Agent
	mu          sync.Mutex
	initialized bool
	todoID      string
}

func (c *Coordinator) getPlanReviewer(ctx context.Context, todoID string) (*planReviewer, error) {
	pr := &planReviewer{coordinator: c, modelID: c.sidecarModel, todoID: todoID}
	if c.sidecarModel == "" {
		return nil, fmt.Errorf("plan review requires a sidecar-model in team.yaml")
	}
	ag, err := agent.CreateAgent(ctx, c.provider, agent.AgentConfig{
		Def: &agent.AgentDef{
			Name:    "plan-reviewer",
			System:  planReviewerSystemPrompt,
			Role:    "plan_reviewer",
		},
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   1,
	}, []fantasy.AgentTool{
		&reviewerApprovePlanTool{coordinator: c, todoID: todoID},
		&reviewerRejectPlanTool{coordinator: c, todoID: todoID},
	})
	if err != nil {
		return nil, err
	}
	pr.agent = ag
	pr.initialized = true
	return pr, nil
}

func (pr *planReviewer) review(ctx context.Context, planText string) (string, bool, error) {
	c := pr.coordinator
	c.pendingPlansMu.Lock()
	entry := c.pendingPlans[pr.todoID]
	if entry == nil {
		c.pendingPlansMu.Unlock()
		return "", false, fmt.Errorf("plan entry not found for %s", pr.todoID)
	}
	goal := entry.Goal
	entry.ReviewCount++
	forceApprove := entry.ReviewCount > planReviewerMaxReviews
	c.pendingPlansMu.Unlock()

	if forceApprove {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("plan %s: auto-approving after %d reviews", pr.todoID, planReviewerMaxReviews)).withTodoID(pr.todoID))
		approved := c.autoApprovePlan(ctx, pr.todoID)
		return approved, true, nil
	}

	completedTasks := c.buildCompletedTasksSummary()
	prompt := fmt.Sprintf("## USER REQUIREMENT\n\n%s\n\n## COMPLETED TASKS\n\n%s\n\n## PLAN\n\n%s", goal, completedTasks, planText)

	c.report(c.newEvent("step").withMessage("plan reviewer evaluating plan").withTodoID(pr.todoID))
	result, _, err := c.runAgentWithStatusAndHistory(ctx, pr.agent, "plan-reviewer", prompt, nil, &taskTiming{})
	if err != nil {
		return "", false, err
	}
	return result, false, nil
}

func (c *Coordinator) autoApprovePlan(ctx context.Context, todoID string) string {
	c.pendingPlansMu.Lock()
	entry := c.pendingPlans[todoID]
	if entry == nil {
		c.pendingPlansMu.Unlock()
		return ""
	}
	entry.Status = "approved"
	agentName := entry.Agent
	goal := entry.Goal
	c.pendingPlansMu.Unlock()

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskPlanned, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	output, err := c.executeTask(ctx, TaskDef{
		Agent:     agentName,
		Goal:      goal,
		PlanFirst: true,
		PlanID:    todoID,
	}, todoID)
	if err != nil {
		return fmt.Sprintf("Plan execution failed: %v", err)
	}
	return output
}

func (c *Coordinator) buildCompletedTasksSummary() string {
	items := c.taskTracker.TodoList().Items()
	var parts []string
	for _, item := range items {
		if item.Status == TaskDone {
			parts = append(parts, fmt.Sprintf("- [Done] %s: %s", item.Agent, item.Desc))
		}
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, "\n")
}

type reviewerApprovePlanTool struct {
	coordinator *Coordinator
	todoID      string
}

func (t *reviewerApprovePlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "approve_plan",
		Description: "Approve the plan and execute it immediately.",
		Parameters: map[string]any{
			"todo_id": map[string]any{"type": "string", "description": "The todo ID of the plan to approve."},
		},
		Required: []string{"todo_id"},
	}
}
func (t *reviewerApprovePlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *reviewerApprovePlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}
func (t *reviewerApprovePlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	result := t.coordinator.autoApprovePlan(ctx, t.todoID)
	return fantasy.NewTextResponse("Plan approved and executed.\n\n" + result), nil
}

type reviewerRejectPlanTool struct {
	coordinator *Coordinator
	todoID      string
}

func (t *reviewerRejectPlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "reject_plan",
		Description: "Reject the plan with a reason. The agent will re-plan based on your feedback.",
		Parameters: map[string]any{
			"todo_id": map[string]any{"type": "string", "description": "The todo ID of the plan to reject."},
			"reason":  map[string]any{"type": "string", "description": "Why the plan was rejected."},
		},
		Required: []string{"todo_id", "reason"},
	}
}
func (t *reviewerRejectPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *reviewerRejectPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}
func (t *reviewerRejectPlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TodoID string `json:"todo_id"`
		Reason string `json:"reason"`
	}
	json.Unmarshal([]byte(call.Input), &args)

	t.coordinator.pendingPlansMu.Lock()
	entry := t.coordinator.pendingPlans[t.todoID]
	if entry == nil {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse("plan not found"), nil
	}
	entry.Status = "rejected"
	agentName := entry.Agent
	goal := entry.Goal
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.report(t.coordinator.newEvent("step").withMessage(fmt.Sprintf("plan %s rejected: %s", t.todoID, args.Reason)).withTodoID(t.todoID))

	output, err := t.coordinator.executeTask(ctx, TaskDef{
		Agent:     agentName,
		Goal:      fmt.Sprintf("%s\n\n## Plan Rejected\nReason: %s\n\nRevise your plan and submit a new one.", goal, args.Reason),
		PlanFirst: true,
	}, t.todoID)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(output), nil
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
	think                 bool
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
	currentTask           string
	currentTaskMu         sync.RWMutex
	currentTodoID         string
	currentTodoIDMu       sync.RWMutex
	auditLogger           *audit.AuditLogger
	skillUsage            map[string]*skillUsageState
	skillUsageMu          sync.Mutex
	delegatedTasks        map[string]int
	delegatedTasksMu      sync.Mutex
	taskResultCache       map[string][]cachedTaskEntry // agent → ordered list of past results
	taskResultCacheMu     sync.RWMutex
	cacheGeneration       atomic.Int64 // bumped each time coordinator starts a new delegation round
	memoryStore           *memory.MemoryStore
	skillsMu              sync.RWMutex
	modelList             []config.ModelEntry
	sidecarModel          string
	sidecarInst           *sidecar.Sidecar
	sidecarInitMu         sync.Mutex
	sidecarInit           bool
	guardModel            string
	guardInst             *sidecar.Sidecar
	guardInitMu           sync.Mutex
	guardInit             bool
	cachedWorkerContext   string
	workerCtxOnce         sync.Once
	autoLoadedSkills      []*skill.SkillDef
	autoLoadedSkillsMu    sync.RWMutex
	forcedSkillNames      map[string]bool // set of skill names specified via --skill
	maxConcurrent         int
	sessionTime           time.Time
	lastStmWrite          time.Time // tracks when stm_write was last called for finish enforcement
	lastStmWriteMu        sync.Mutex
	ltmWriteMu            sync.Mutex // Protect LTM file reads and writes

	// stepConfirmFn must be set before Run() or protected by stepConfirmFnMu.
	stepConfirmFn   func(context.Context, []TaskDef) (bool, error)
	stepConfirmFnMu sync.RWMutex
	// dryRun is a forward-looking feature for short-circuiting ExecuteTasks
	// when dry-run mode is active. Currently, the CLI's --dry-run flag calls
	// DryRun() directly and returns early before reaching ExecuteTasks, so
	// this field is not yet exercised in the main CLI flow.
	dryRun atomic.Bool
	hooks  *hooks.HookRegistry
	rbashMode      bool
	restrictedPath string
	noNet                bool
	workerSummariesOnce  sync.Once
	workerSummaries      map[string]string
	workerSummariesMu    sync.Mutex
	pendingPlans          map[string]*PlanEntry
	pendingPlansMu        sync.Mutex
	forcePlanFirst        bool
	autoSkillsEnabled     bool
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

func NewCoordinator(session *TeamSession, defaultProviderURL, defaultProviderAPIKey string, mcpManager *mcp.MCPToolManager, memoryStore *memory.MemoryStore, modelList []config.ModelEntry, sidecarModel string, guardModel string, maxConcurrent int, verbose bool, think bool, direnv bool, allowedPaths []string, pathConsent *tools.PathConsent, hookRegistry *hooks.HookRegistry, rbashMode bool, restrictedPath string, noNet bool, forcedSkillNames []string, planMode bool, autoSkillsMode bool) (*Coordinator, error) {
	projectDir, _ := os.Getwd()
	coreTools := agent.BuildAllAgentTools(projectDir, tools.WithAllowedPaths(allowedPaths), tools.WithPathConsent(pathConsent), tools.WithWorkspaceName(filepath.Base(session.Workspace)), tools.WithHooks(hookRegistry), tools.WithRestrictedBash(rbashMode), tools.WithRestrictedPath(restrictedPath), tools.WithNetworkBlock(noNet), tools.WithDirenv(direnv))
	prov, err := agent.NewOllamaProvider(defaultProviderURL, defaultProviderAPIKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create Ollama provider: %w", err)
	}
	c := &Coordinator{
		provider:        prov,
		session:         session,
		mcpManager:      mcpManager,
		coreTools:       coreTools,
		agentCache:      make(map[string]fantasy.Agent),
		verbose:         verbose,
		think:           think,
		reportStatus:    func(event StatusEvent) {},
		taskTracker:     NewTaskTracker(),
		skills:          session.Skills,
		projectDir:      projectDir,
		skillUsage:      make(map[string]*skillUsageState),
		delegatedTasks:  make(map[string]int),
		pendingPlans:    make(map[string]*PlanEntry),
		taskResultCache: make(map[string][]cachedTaskEntry),
		memoryStore:     memoryStore,
		modelList:       modelList,
		sidecarModel:    sidecarModel,
		guardModel:      guardModel,
		maxConcurrent:   maxConcurrent,
		sessionTime:     time.Now(),
		hooks:           hookRegistry,
		rbashMode:       rbashMode,
		restrictedPath:  restrictedPath,
		noNet:           noNet,
		forcedSkillNames: func() map[string]bool {
			m := make(map[string]bool)
			for _, n := range forcedSkillNames {
				trimmed := strings.TrimSpace(n)
				if trimmed != "" {
					m[strings.ToLower(trimmed)] = true
				}
			}
			return m
		}(),
		forcePlanFirst: planMode,
		autoSkillsEnabled: autoSkillsMode,
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
		&stmWriteTool{coordinator: c},
		&ltmUpdateTool{coordinator: c},
	)

	if c.memoryStore != nil {
		c.coreTools = append(c.coreTools,
			&memorySaveLTMWrapper{original: memory.NewMemorySaveTool(c.memoryStore), coordinator: c},
			memory.NewMemoryQueryTool(c.memoryStore),
		)
	}

	guardReviewer := func(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
		s := c.GuardSidecar()
		if s == nil {
			return true, "", nil
		}
		agentName, _ := ctx.Value(tools.AgentNameKey).(string)
		result, err := s.ReviewToolCall(ctx, agentName, toolName, args, rules)
		if err != nil {
			return true, "", err
		}
		return result.Approved, result.Reason, nil
	}
	tools.SetGuardReviewer(c.coreTools, guardReviewer)

	pathReviewer := func(ctx context.Context, command string, path string) (bool, error) {
		s := c.Sidecar()
		if s == nil {
			return true, nil
		}
		return s.ReviewPathAccess(ctx, command, path)
	}
	tools.SetPathReviewer(c.coreTools, pathReviewer)

	if history := LoadConversationHistory(session.Workspace); len(history) > 0 {
		c.conversationHistory = history
	}

	if pathConsent != nil {
		coordinator := c
		pathConsent.SetAgentInfoSource(func() tools.AgentInfo {
			return coordinator.GetCurrentAgentInfo()
		})
	}

	return c, nil
}

func (c *Coordinator) SetStatusReporter(fn StatusReporter) {
	if fn != nil {
		c.reportStatus = fn
	}
}

func (c *Coordinator) Hooks() *hooks.HookRegistry {
	return c.hooks
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

func (c *Coordinator) setAutoLoadedSkills(skills []*skill.SkillDef) {
	c.autoLoadedSkillsMu.Lock()
	defer c.autoLoadedSkillsMu.Unlock()
	c.autoLoadedSkills = skills
}

func (c *Coordinator) getAutoLoadedSkills() []*skill.SkillDef {
	c.autoLoadedSkillsMu.RLock()
	defer c.autoLoadedSkillsMu.RUnlock()
	return c.autoLoadedSkills
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
	cachedSkills := c.getSkills()
	foundMap := map[string]bool{}
	var b strings.Builder
	b.WriteString("## Relevant Skills\n\n")
	for _, s := range skill.SkillsByName(cachedSkills, agentSkillNames) {
		fmt.Fprintf(&b, "### %s\n*File: %s*\n\n%s\n\n", s.Name, s.Path, s.Content)
		foundMap[strings.ToLower(s.Name)] = true
	}
	for _, name := range agentSkillNames {
		if !foundMap[strings.ToLower(strings.TrimSpace(name))] {
			fmt.Fprintf(os.Stderr, "warning: agent %q requests skill %q which is not available (check team skills-exclude config)\n", agentDef.Name, name)
		}
	}
	b.WriteString("---\n\n")
	return b.String()
}

func (c *Coordinator) buildSuggestedSkillsText(agentDef *agent.AgentDef, agentName string, taskDesc string) (string, []string) {
	relevant := c.computeRelevantSkills(agentDef, taskDesc)
	if len(relevant) == 0 {
		return "", nil
	}

	names := make([]string, len(relevant))
	for i, s := range relevant {
		names[i] = s.Name
		c.report(c.newEvent("skill_auto_loaded").withAgent(agentName).withSkillName(s.Name))
		c.recordSkillUsage(s.Name, agentName)
	}

	var b strings.Builder
	b.WriteString("## Suggested Skills\n\n")
	b.WriteString("The following skills are relevant to your task. Call `load_skill` to load ALL of them before starting work:\n\n")
	for _, s := range relevant {
		desc := s.Description
		if utf8.RuneCountInString(desc) > 80 {
			runes := []rune(desc)
			desc = string(runes[:80]) + "..."
		}
		fmt.Fprintf(&b, "- **%s**: %s\n", s.Name, desc)
	}
	b.WriteString("\n")
	return b.String(), names
}

func (c *Coordinator) computeRelevantSkills(agentDef *agent.AgentDef, taskDesc string) []*skill.SkillDef {
	autoSkills := c.getAutoLoadedSkills()
	if len(autoSkills) == 0 && len(c.forcedSkillNames) == 0 {
		return nil
	}

	existingSkills := skill.ParseSkillList(agentDef.Skills)
	existingSet := map[string]bool{}
	for _, name := range existingSkills {
		existingSet[strings.ToLower(strings.TrimSpace(name))] = true
	}

	agentText := strings.ToLower(agentDef.Name + " " + agentDef.Description + " " + agentDef.Role)
	taskText := strings.ToLower(taskDesc)

	var relevant []*skill.SkillDef
	addedSet := map[string]bool{}
	for _, s := range autoSkills {
		if existingSet[strings.ToLower(s.Name)] {
			continue
		}
		keywords := extractSkillKeywords(s)
		if !containsAny(keywords, agentText) {
			continue
		}
		if !containsAny(keywords, taskText) {
			continue
		}
		addedSet[strings.ToLower(s.Name)] = true
		relevant = append(relevant, s)
	}

	if len(c.forcedSkillNames) > 0 {
		allSkills := c.getSkills()
		for _, s := range allSkills {
			if c.forcedSkillNames[strings.ToLower(s.Name)] && !existingSet[strings.ToLower(s.Name)] && !addedSet[strings.ToLower(s.Name)] {
				relevant = append(relevant, s)
				addedSet[strings.ToLower(s.Name)] = true
			}
		}
	}

	return relevant
}

func containsAny(keywords []string, text string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

const maxSTMAutoInject = 2000
const maxLTMAutoInject = 3000

func (c *Coordinator) buildMemorySuffix(agentRole string) string {
	var b strings.Builder

	rawSTM := LoadSTM(c.session.Workspace)
	if rawSTM != "" {
		sections := ParseSTMSections(rawSTM)
		filtered := filterSTMSectionsByRole(sections, agentRole)
		stm := FormatSTMSections(filtered)
		if stm != "" {
			runes := []rune(stm)
			if len(runes) > maxSTMAutoInject {
				stm = string(runes[len(runes)-maxSTMAutoInject:])
			}
			b.WriteString("--- Short-term memory (stm.md) ---\n")
			b.WriteString(stm)
			b.WriteString("\n--- End stm.md ---")
		}
	}

	if rawLTM := LoadLTM(c.session.Dir); rawLTM != "" {
		sections := ParseSTMSections(rawLTM)
		if len(sections) > 0 {
			for i, s := range sections {
				if len(s.Entries) > 3 {
					sections[i].Entries = s.Entries[:3]
				}
			}
			ltm := FormatSTMSections(sections)
			runes := []rune(ltm)
			if len(runes) > maxLTMAutoInject {
				ltm = string(runes[len(runes)-maxLTMAutoInject:])
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("--- Long-term memory (ltm.md) ---\n")
			b.WriteString(ltm)
			b.WriteString("\n--- End ltm.md ---")
		}
	}

	if b.Len() == 0 {
		return ""
	}

	b.WriteString("## Memory & Context\n\n")
	b.WriteString("Review the following memory to understand the current state and prior knowledge before proceeding.\n\n")
	stmLtmContent := b.String()
	b.Reset()
	b.WriteString("## Memory & Context\n\n")
	b.WriteString("Review the following memory to understand the current state and prior knowledge before proceeding.\n\n\n")
	b.WriteString(stmLtmContent)
	b.WriteString("\n")
	return b.String()
}

func (c *Coordinator) autoWriteSTM(agentName, taskDesc, output, errMsg string, success bool) {
	workspace := c.session.Workspace
	existing := LoadSTM(workspace)

	var entry string
	if success {
		summary := extractSummary(output, 300)
		entry = formatSTMDoneEntry(agentName, taskDesc, summary)
	} else {
		entry = formatSTMErrorEntry(agentName, taskDesc, errMsg)
	}

	newContent := appendSTMEntry(existing, entry, stmSectionProgress)
	if err := SaveSTM(workspace, TruncateSTM(newContent)); err != nil {
		log.Printf("warning: auto STM write failed: %v", err)
	}
	c.lastStmWriteMu.Lock()
	c.lastStmWrite = time.Now()
	c.lastStmWriteMu.Unlock()
}

func extractSummary(output string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(output))
	if len(runes) == 0 {
		return ""
	}
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}

func (c *Coordinator) AutoExtractLTM() {
	teamDir := c.session.Dir
	workspace := c.session.Workspace
	stmContent := LoadSTM(workspace)
	if stmContent == "" {
		return
	}

	existingLTM := LoadLTM(teamDir)
	sections := ParseSTMSections(stmContent)
	existingLTMSections := ParseSTMSections(existingLTM)

	var newEntries []struct {
		sectionTitle string
		entry        string
	}

	for _, s := range sections {
		switch s.Title {
		case stmSectionDecisions:
			for _, e := range s.Entries {
				section := classifyLTMEntry(e, "decision")
				if section != "" {
					newEntries = append(newEntries, struct {
						sectionTitle string
						entry        string
					}{section, formatLTMEntry(stripSTMListItem(e))})
				}
			}
		case stmSectionFindings:
			for _, e := range s.Entries {
				section := classifyLTMEntry(e, "finding")
				if section != "" {
					newEntries = append(newEntries, struct {
						sectionTitle string
						entry        string
					}{section, formatLTMEntry(stripSTMListItem(e))})
				}
			}
		case stmSectionErrors:
			for _, e := range s.Entries {
				section := classifyLTMEntry(e, "error")
				if section != "" {
					newEntries = append(newEntries, struct {
						sectionTitle string
						entry        string
					}{section, formatLTMEntry(stripSTMListItem(e))})
				}
			}
		}
	}

	if len(newEntries) == 0 {
		return
	}

	for _, ne := range newEntries {
		if hasLTREntry(existingLTMSections, ne.sectionTitle, ne.entry) {
			continue
		}
		existingLTM = appendSTMEntry(existingLTM, ne.entry, ne.sectionTitle)
	}

	pruned := PruneLTM(existingLTM)
	if err := SaveLTM(teamDir, TruncateLTM(pruned)); err != nil {
		log.Printf("warning: auto LTM extraction failed: %v", err)
	}

	if c.memoryStore != nil {
		saveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, ne := range newEntries {
			if hasLTREntry(existingLTMSections, ne.sectionTitle, ne.entry) {
				continue
			}
			id := fmt.Sprintf("ltm-%d", time.Now().UnixNano())
			metadata := map[string]string{
				"category": ne.sectionTitle,
				"source":   "auto-extract",
			}
			if err := c.memoryStore.Save(saveCtx, id, ne.entry, metadata); err != nil {
				log.Printf("warning: memory store save failed for LTM entry: %v", err)
			}
		}
	}
}

func classifyLTMEntry(entry string, source string) string {
	lower := strings.ToLower(entry)
	hasFilePath := strings.Contains(lower, ".go") || strings.Contains(lower, ".yaml") ||
		strings.Contains(lower, ".yml") || strings.Contains(lower, ".md") ||
		strings.Contains(lower, ".json") || strings.Contains(lower, ".sh") ||
		strings.Contains(lower, ".py") || strings.Contains(lower, ".js") ||
		strings.Contains(lower, ".ts") || strings.Contains(lower, "/")

	if source == "finding" && hasFilePath {
		return ltmSectionFiles
	}

	if strings.Contains(lower, "always") || strings.Contains(lower, "never") ||
		strings.Contains(lower, "must ") || strings.Contains(lower, "should ") ||
		strings.Contains(lower, "convention") || strings.Contains(lower, "rule") ||
		strings.Contains(lower, "standard") || strings.Contains(lower, "guideline") ||
		strings.Contains(lower, "every time") {
		return ltmSectionConventions
	}

	if strings.Contains(lower, "pattern") || strings.Contains(lower, "approach") ||
		strings.Contains(lower, "strategy") || strings.Contains(lower, "workflow") ||
		strings.Contains(lower, "pipeline") || strings.Contains(lower, "template") {
		return ltmSectionPatterns
	}

	if source == "error" && strings.Contains(lower, "fix") ||
		strings.Contains(lower, "solved") || strings.Contains(lower, "resolved") ||
		strings.Contains(lower, "workaround") || strings.Contains(lower, "solution") {
		return ltmSectionIssues
	}

	if source == "decision" {
		if strings.Contains(lower, "use ") || strings.Contains(lower, "switch") ||
			strings.Contains(lower, "migrate") || strings.Contains(lower, "replace") ||
			strings.Contains(lower, "upgrade") || strings.Contains(lower, "choose") ||
			strings.Contains(lower, "select") || strings.Contains(lower, "adopt") {
			return ltmSectionArchitecture
		}
		return ltmSectionArchitecture
	}

	if strings.Contains(lower, "tool") || strings.Contains(lower, "command") ||
		strings.Contains(lower, "script") || strings.Contains(lower, "cli ") ||
		strings.Contains(lower, "run ") || strings.Contains(lower, "install ") ||
		strings.Contains(lower, "build ") || strings.Contains(lower, "test ") {
		return ltmSectionTools
	}

	if source == "finding" {
		return ltmSectionIssues
	}

	return ""
}

func stripSTMListItem(entry string) string {
	s := strings.TrimSpace(entry)
	for _, prefix := range []string{"- [FAILED] ", "- ", "* "} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	return s
}

func hasLTREntry(sections []STMSection, sectionTitle, entry string) bool {
	for _, s := range sections {
		if s.Title == sectionTitle {
			normalized := normalizeLTREntry(entry)
			for _, e := range s.Entries {
				if normalizeLTREntry(e) == normalized {
					return true
				}
			}
		}
	}
	return false
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

func (c *Coordinator) GuardSidecar() *sidecar.Sidecar {
	if c.guardModel == "" {
		return nil
	}
	c.guardInitMu.Lock()
	defer c.guardInitMu.Unlock()
	if c.guardInit {
		return c.guardInst
	}
	ctx := context.Background()
	s, err := sidecar.NewSidecar(ctx, c.provider, c.guardModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to initialize guard model %q: %v\n", c.guardModel, err)
		return nil
	}
	c.guardInst = s
	c.guardInit = true
	return c.guardInst
}

func (c *Coordinator) matchSkillsWithSidecar(ctx context.Context, prompt string) []*skill.SkillDef {
	if !c.autoSkillsEnabled {
		return nil
	}
	allSkills := c.getSkills()
	if len(allSkills) == 0 {
		return nil
	}

	var matched []*skill.SkillDef

	s := c.Sidecar()
	if s != nil {
		summaries := make([]sidecar.SkillSummary, len(allSkills))
		for i, sk := range allSkills {
			summaries[i] = sidecar.SkillSummary{
				Name:        sk.Name,
				Description: sk.Description,
			}
		}
		if c.think {
			c.emitThinkSidecar("MatchSkills", fmt.Sprintf("matching %d skills against prompt", len(allSkills)))
		}
		names, err := s.MatchSkills(ctx, prompt, summaries)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: sidecar skill matching failed, using keyword fallback: %v\n", err)
		} else if len(names) > 0 {
			nameSet := map[string]bool{}
			for _, n := range names {
				nameSet[strings.ToLower(strings.TrimSpace(n))] = true
			}
			for _, sk := range allSkills {
				if nameSet[strings.ToLower(sk.Name)] {
					matched = append(matched, sk)
				}
			}
			matchedNames := make([]string, len(matched))
			for i, sk := range matched {
				matchedNames[i] = sk.Name
			}
			if c.think {
				c.emitThinkSidecar("MatchSkills", fmt.Sprintf("matched: %s", strings.Join(matchedNames, ", ")))
			}
			c.report(c.newEvent("sidecar_call").withMessage("match_skills → " + strings.Join(matchedNames, ", ")))
		}
		if len(matched) == 0 {
			if c.think {
				c.emitThinkSidecar("MatchSkills", "no matches")
			}
			c.report(c.newEvent("sidecar_call").withMessage("match_skills → (no matches)"))
		}
	} else {
		fallback := c.matchSkillsForPrompt(prompt)
		if len(fallback) > 0 {
			names := make([]string, len(fallback))
			for i, sk := range fallback {
				names[i] = sk.Name
			}
			if c.think {
				c.emitThinkSidecar("MatchSkills(keyword)", fmt.Sprintf("fallback matched: %s", strings.Join(names, ", ")))
			}
			c.report(c.newEvent("sidecar_call").withMessage("match_skills (keyword) → " + strings.Join(names, ", ")))
		}
		matched = fallback
	}

	return matched
}

func (c *Coordinator) newEvent(eventType string) StatusEvent {
	return StatusEvent{Type: eventType, TeamName: c.session.Config.Name}
}

func (c *Coordinator) updateTodoTiming(todoID string, modelTime, toolTime time.Duration) {
	c.taskTracker.TodoList().UpdateTodoTiming(todoID, modelTime, toolTime)
}

func (c *Coordinator) SetWrapUp() {
	c.wrapUp.Store(1)
	c.report(c.newEvent("wrap_up_phase").withMessage("finishing active tasks"))
}

func (c *Coordinator) IsWrapUp() bool {
	return c.wrapUp.Load() == 1
}

func (c *Coordinator) TaskTracker() *TaskTracker {
	return c.taskTracker
}

func (c *Coordinator) SetStepConfirmFn(fn func(context.Context, []TaskDef) (bool, error)) {
	c.stepConfirmFnMu.Lock()
	defer c.stepConfirmFnMu.Unlock()
	c.stepConfirmFn = fn
}

// SetDryRun enables or disables dry-run mode on the coordinator.
// When enabled, ExecuteTasks will short-circuit and return a plan
// without actually executing any agent tasks. This is a forward-looking
// feature; the current CLI --dry-run flag calls DryRun() directly and
// returns early, so SetDryRun is not yet exercised in the main CLI flow.
func (c *Coordinator) SetDryRun(v bool) {
	c.dryRun.Store(v)
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

func (c *Coordinator) SetCurrentTask(task string) {
	c.currentTaskMu.Lock()
	defer c.currentTaskMu.Unlock()
	c.currentTask = task
}

func (c *Coordinator) GetCurrentTask() string {
	c.currentTaskMu.RLock()
	defer c.currentTaskMu.RUnlock()
	return c.currentTask
}

func (c *Coordinator) SetCurrentTodoID(id string) {
	c.currentTodoIDMu.Lock()
	defer c.currentTodoIDMu.Unlock()
	c.currentTodoID = id
}

func (c *Coordinator) GetCurrentTodoID() string {
	c.currentTodoIDMu.RLock()
	defer c.currentTodoIDMu.RUnlock()
	return c.currentTodoID
}

func (c *Coordinator) GetCurrentAgentInfo() tools.AgentInfo {
	return tools.AgentInfo{
		Name: c.GetCurrentAgent(),
		Task: c.GetCurrentTask(),
	}
}

// buildAgentTaskProperties returns the JSON schema properties map for a task
// item in the "agent" tool. When hasModelList is false the "model" field is
// omitted so the coordinator cannot (and does not need to) specify a model;
// each agent's model is determined by its own configuration instead.
// sharedDir is the absolute path to the workspace shared/ directory.
func buildAgentTaskProperties(workerNames []string, hasModelList bool, sharedDirPath string) map[string]any {
	contextFilesDesc := "Optional files from the shared directory to provide as context"
	if sharedDirPath != "" {
		contextFilesDesc = fmt.Sprintf("Optional files from the shared directory (%s) to provide as context", sharedDirPath)
	}
	props := map[string]any{
		"agent":         map[string]any{"type": "string", "enum": workerNames, "description": "Agent name to delegate to"},
		"goal":          map[string]any{"type": "string", "description": "The desired OUTCOME — what should be achieved. Do NOT include implementation details (file paths, function names, step-by-step instructions). Workers are specialists who determine their own approach."},
		"constraints":   map[string]any{"type": "string", "description": "Non-obvious constraints the worker MUST respect (e.g., 'must use Python 3.11', 'cannot modify the public API'). Do NOT include obvious project conventions."},
		"plan_first":    map[string]any{"type": "boolean", "description": "If true, the agent must draft a task execution plan and call submit_plan before doing any work. Use this for complex tasks where you want to review the approach before execution. After receiving the plan, call approve_plan, modify_plan, or reject_plan."},
		"summarize":     map[string]any{"type": "boolean", "description": "If true, summarize the agent's output before returning. Use for tasks that produce verbose output where only key points matter."},
		"sidecar":       map[string]any{"type": "boolean", "description": "If true, execute this task directly via the sidecar model instead of an agent. Use for simple, tool-free tasks that need a quick response."},
		"context_files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": contextFilesDesc},
	}
	if hasModelList {
		props["model"] = map[string]any{"type": "string", "description": "Model ID from Available Models to use for this task. Select the model whose strengths best match this task. If empty, the default team model will be used."}
	}
	return props
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
					"type":                 "object",
					"properties":          buildAgentTaskProperties(t.coordinator.workerNameList(), len(t.coordinator.modelList) > 0, filepath.Join(t.coordinator.session.Workspace, sharedDir)),
					"required":            []string{"agent"},
					"additionalProperties": false,
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
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
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

	t.coordinator.AutoExtractLTM()

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

	parentID := c.GetCurrentTodoID()
	todoItems := c.taskTracker.TodoList().AddBatch([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{{Agent: name, Desc: task, Model: subModel, Source: TaskSourceSubagent, ParentID: parentID}})
	todoID := todoItems[0].ID

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("start").withAgent(name).withMessage(task).withModel(subModel).withTodoID(todoID))

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
		c.report(c.newEvent("error").withAgent(name).withMessage(err.Error()).withTodoID(todoID))
		return "", fmt.Errorf("failed to create sub-agent: %w", err)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	taskCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()

	taskPrompt := task
	skillSuggestion, skillNames := c.buildSuggestedSkillsText(def, name, task)
	if skillSuggestion != "" {
		c.taskTracker.TodoList().SetInjectedSkills(todoID, skillNames)
		taskPrompt = taskPrompt + "\n\n" + skillSuggestion
	}

	if suffix := c.buildMemorySuffix("worker"); suffix != "" {
		taskPrompt = taskPrompt + "\n\n" + suffix
	}

	timing := &taskTiming{}
	timing.reset()

	prevAgent := c.GetCurrentAgent()
	prevTask := c.GetCurrentTask()
	prevTodoID := c.GetCurrentTodoID()
	c.SetCurrentAgent(name)
	c.SetCurrentTask(task)
	c.SetCurrentTodoID(todoID)
	defer func() {
		c.SetCurrentAgent(prevAgent)
		c.SetCurrentTask(prevTask)
		c.SetCurrentTodoID(prevTodoID)
	}()
	taskCtx = context.WithValue(taskCtx, modelKey{}, subModel)
	output, _, err := c.runAgentWithStatusAndHistory(taskCtx, ag, name, taskPrompt, nil, timing)

	duration, modelTime, toolTime := timing.snapshot()
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.updateTodoTiming(todoID, modelTime, toolTime)
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("error").withAgent(name).withMessage(err.Error()).withModel(subModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
		return "", fmt.Errorf("sub-agent failed (model: %s): %w", subModel, err)
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
	c.updateTodoTiming(todoID, modelTime, toolTime)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(name).withOutput(output).withMessage("completed").withModel(subModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
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
	parentID := t.coordinator.GetCurrentTodoID()

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

	section := classifyLTMEntry(args.Content, "finding")
	if section == "" {
		return resp, nil
	}

	t.coordinator.ltmWriteMu.Lock()
	defer t.coordinator.ltmWriteMu.Unlock()

	teamDir := t.coordinator.session.Dir
	existingLTM := LoadLTM(teamDir)
	entry := formatLTMEntry(args.Content)
	existingLTMSections := ParseSTMSections(existingLTM)
	if hasLTREntry(existingLTMSections, section, entry) {
		return resp, nil
	}

	newLTM := appendSTMEntry(existingLTM, entry, section)
	pruned := PruneLTM(newLTM)
	if err := SaveLTM(teamDir, TruncateLTM(pruned)); err != nil {
		log.Printf("warning: memory_save LTM write-back failed: %v", err)
	}

	return resp, nil
}

type submitPlanTool struct {
	coordinator *Coordinator
	todoID      string
}

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

func (t *submitPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *submitPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

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
	t.coordinator.pendingPlans[t.todoID] = &PlanEntry{
		TodoID:      t.todoID,
		PlanText:    args.Plan,
		Status:      "submitted",
		ReviewCount: func() int { if existing != nil { return existing.ReviewCount }; return 0 }(),
	}
	t.coordinator.pendingPlansMu.Unlock()
	if t.coordinator.forcePlanFirst {
		return fantasy.NewTextResponse("Plan submitted. Awaiting review."), nil
	}
	return fantasy.NewTextResponse("Plan submitted. Await coordinator review."), nil
}

type stmWriteTool struct {
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
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

func (t *stmWriteTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *stmWriteTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

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
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
}

func (t *ltmUpdateTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "ltm_update",
		Description: "Update long-term memory (ltm.md), a persistent file shared across sessions for this team. Use append mode to add new knowledge, or replace mode to overwrite. This memory persists between sessions and is available to future runs.",
		Parameters: map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The content to write to long-term memory",
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

func (t *ltmUpdateTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *ltmUpdateTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *ltmUpdateTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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

	teamDir := t.coordinator.session.Dir
	t.coordinator.ltmWriteMu.Lock()
	var newContent string
	switch mode {
	case "replace":
		existing := LoadLTM(teamDir)
		newContent = TruncateLTM(args.Content)
		if existing != "" {
			newContent = TruncateLTM(existing + "\n" + newContent)
		}
	default:
		existing := LoadLTM(teamDir)
		if existing == "" {
			newContent = TruncateLTM(args.Content)
		} else {
			newContent = TruncateLTM(existing + "\n" + args.Content)
		}
	}
	err := SaveLTM(teamDir, newContent)
	t.coordinator.ltmWriteMu.Unlock()
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write ltm.md: %v", err)), nil
	}

	verb := "Appended to"
	if mode == "replace" {
		verb = "Replaced"
	}
	return fantasy.NewTextResponse(fmt.Sprintf("%s long-term memory (ltm.md)", verb)), nil
}

type approvePlanTool struct {
	coordinator *Coordinator
}

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

func (t *approvePlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *approvePlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

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
	agent := entry.Agent
	goal := entry.Goal
	todoID := entry.TodoID
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.taskTracker.TodoList().UpdateStatus(todoID, TaskPlanned, "")
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))

	task := TaskDef{
		Agent:  agent,
		Goal:   goal,
		PlanFirst: true,
		PlanID: todoID,
	}
	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{task})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Plan approved and executed.\n\n%s", result)), nil
}

type modifyPlanTool struct {
	coordinator *Coordinator
}

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

func (t *modifyPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *modifyPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

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
	agent := entry.Agent
	goal := entry.Goal
	todoID := entry.TodoID
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.report(t.coordinator.newEvent("step").withMessage(fmt.Sprintf("plan %s modified by coordinator", todoID)))

	task := TaskDef{
		Agent:  agent,
		Goal:   goal,
		PlanFirst: true,
		PlanID: todoID,
	}
	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{task})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Plan modified and executed.\n\n%s", result)), nil
}

type rejectPlanTool struct {
	coordinator *Coordinator
}

func (t *rejectPlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "reject_plan",
		Description: "Reject a submitted task plan and ask the agent to re-plan. Include the reason for rejection so the agent can improve their plan.",
		Parameters: map[string]any{
			"todo_id": map[string]any{
				"type":        "string",
				"description": "The todo ID of the submitted plan to reject.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Why the plan was rejected. The agent will see this and re-plan accordingly.",
			},
		},
		Required: []string{"todo_id", "reason"},
	}
}

func (t *rejectPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *rejectPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *rejectPlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TodoID string `json:"todo_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.TodoID == "" || args.Reason == "" {
		return fantasy.NewTextErrorResponse("todo_id and reason are required"), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	entry, ok := t.coordinator.pendingPlans[args.TodoID]
	if !ok {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse("plan not found for todo_id: " + args.TodoID), nil
	}
	entry.Status = "rejected"
	agent := entry.Agent
	goal := entry.Goal
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.report(t.coordinator.newEvent("step").withMessage(fmt.Sprintf("plan %s rejected: %s", args.TodoID, args.Reason)))

	task := TaskDef{
		Agent:     agent,
		Goal:      fmt.Sprintf("%s\n\n## Plan Rejected\nYour previous plan was rejected for the following reason:\n\n%s\n\nPlease re-plan and submit a new plan.", goal, args.Reason),
		PlanFirst: true,
	}
	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{task})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Plan rejected. Agent re-planned.\n\n%s", result)), nil
}

const maxConversationHistory = 100
const compactHistoryThreshold = 80
const maxMessageSize = 50000

type agentTaskResult struct {
	agentName string
	todoID    string
	task      string
	output    string
	err       error
	planText  string
}

// cachedTaskEntry stores a previously completed task and its output for dedup.
type cachedTaskEntry struct {
	taskDesc   string
	output     string
	generation int64 // cacheGeneration at time of storage
}

// lookupTaskCache checks whether newTask has a semantically equivalent prior
// result for agentKey (lowercase agent name). Only entries from the current
// cacheGeneration are considered — this ensures results from a previous
// coordinator round (where workspace state may have changed) are never reused.
// It first does a fast exact-match check; if that misses and a sidecar is
// configured, it uses the sidecar to judge semantic similarity.
// Returns (cachedOutput, true) on a hit.
func (c *Coordinator) lookupTaskCache(ctx context.Context, agentKey, newTask string) (string, bool) {
	gen := c.cacheGeneration.Load()

	c.taskResultCacheMu.RLock()
	all := c.taskResultCache[agentKey]
	var entries []cachedTaskEntry
	for _, e := range all {
		if e.generation == gen {
			entries = append(entries, e)
		}
	}
	c.taskResultCacheMu.RUnlock()

	if len(entries) == 0 {
		return "", false
	}

	normalizedNew := strings.ToLower(strings.Join(strings.Fields(newTask), " "))
	for _, e := range entries {
		norm := strings.ToLower(strings.Join(strings.Fields(e.taskDesc), " "))
		if norm == normalizedNew {
			return e.output, true
		}
	}

	s := c.Sidecar()
	if s == nil {
		return "", false
	}

	pastDescs := make([]string, len(entries))
	for i, e := range entries {
		pastDescs[i] = e.taskDesc
	}

	sidecarCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if c.think {
		c.emitThinkSidecar("SimilarTask", fmt.Sprintf("checking cache for semantically similar task: %.50s", newTask))
	}
	idx, err := s.SimilarTask(sidecarCtx, newTask, pastDescs)
	if err != nil {
		return "", false
	}
	if idx >= 0 && idx < len(entries) {
		return entries[idx].output, true
	}
	return "", false
}

const maxTaskCacheEntries = 50

// storeTaskCache saves a completed task result so future similar tasks within
// the same coordinator round (same cacheGeneration) can skip re-execution.
func (c *Coordinator) storeTaskCache(agentKey, taskDesc, output string) {
	gen := c.cacheGeneration.Load()
	c.taskResultCacheMu.Lock()
	defer c.taskResultCacheMu.Unlock()
	c.taskResultCache[agentKey] = append(c.taskResultCache[agentKey], cachedTaskEntry{
		taskDesc:   taskDesc,
		output:     output,
		generation: gen,
	})
	if len(c.taskResultCache[agentKey]) > maxTaskCacheEntries {
		c.taskResultCache[agentKey] = c.taskResultCache[agentKey][1:]
	}
}

func (c *Coordinator) checkDuplicateTasks(tasks []TaskDef) []string {
	var warnings []string
	c.delegatedTasksMu.Lock()
	defer c.delegatedTasksMu.Unlock()
	for _, t := range tasks {
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := strings.ToLower(t.Agent) + ":" + truncateTaskDesc(desc)
		c.delegatedTasks[key]++
		if c.delegatedTasks[key] > 1 {
			warnings = append(warnings, fmt.Sprintf("%s (agent=%s, count=%d)", truncateTaskDesc(desc), t.Agent, c.delegatedTasks[key]))
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
		} else if r.planText != "" {
			b.WriteString(fmt.Sprintf("## Agent: %s\n**Status**: PLAN SUBMITTED\n**Todo ID**: %s\n\n%s", r.agentName, r.todoID, r.planText))
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

func (c *Coordinator) checkpointSTM() {
	workspace := c.session.Workspace
	content := LoadSTM(workspace)
	if content == "" {
		return
	}
	histDir := filepath.Join(workspace, "stm_history")
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		log.Printf("warning: stm checkpoint dir creation failed: %v", err)
		return
	}
	fname := fmt.Sprintf("stm_r%d.md", c.round)
	path := filepath.Join(histDir, fname)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		log.Printf("warning: stm checkpoint write failed: %v", err)
	}
}

func (c *Coordinator) ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error) {
	if c.IsWrapUp() {
		c.report(c.newEvent("step").withMessage("Wrap-up: refusing to start new tasks"))
		return "", fmt.Errorf("wrap-up in progress: refusing to delegate new tasks. Call finish immediately with your best summary of work completed so far")
	}

	if c.forcePlanFirst {
		for i := range tasks {
			if tasks[i].PlanID == "" {
				tasks[i].PlanFirst = true
			}
		}
	}

	c.round++
	if c.session.Config.MaxRounds > 0 && c.round > c.session.Config.MaxRounds {
		return "", fmt.Errorf("max rounds (%d) exceeded", c.session.Config.MaxRounds)
	}

	// Bump the cache generation when the coordinator starts a new delegation
	// round. Worker-level ExecuteTasks calls carry a worker todoID in context,
	// not CoordTodoID, so they do NOT bump the generation. This means all
	// sub-tasks spawned by workers within the same coordinator round share the
	// same generation and can deduplicate against each other. When the
	// coordinator starts a new round (new agent call), the generation bumps,
	// making all previous cached results invalid — ensuring stale workspace
	// state is never reused.
	callerID, _ := ctx.Value(todoIDKey{}).(string)
	if callerID == "" || callerID == CoordTodoID {
		newGen := c.cacheGeneration.Add(1)
		c.taskResultCacheMu.Lock()
		for key, entries := range c.taskResultCache {
			var fresh []cachedTaskEntry
			for _, e := range entries {
				if e.generation == newGen {
					fresh = append(fresh, e)
				}
			}
			c.taskResultCache[key] = fresh
		}
		c.taskResultCacheMu.Unlock()
	}

	c.report(c.newEvent("step").withMessage(fmt.Sprintf("Round %d: delegating %d task(s)", c.round, len(tasks))))

	todoBatch := make([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}, len(tasks))
	for i, t := range tasks {
		agentDef, _, _ := c.resolveAgentName(t.Agent)
		var resolvedModel string
		if agentDef != nil {
			overrideModel := t.Model
			if len(c.modelList) == 0 {
				overrideModel = ""
			}
			resolvedModel = c.resolveAgentModel(agentDef, overrideModel)
		}
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		todoBatch[i] = struct {
			Agent    string
			Desc     string
			Model    string
			Source   string
			ParentID string
		}{Agent: strings.ToLower(t.Agent), Desc: desc, Model: resolvedModel, Source: TaskSourceCoordinator, ParentID: ""}
	}
	todoItems := c.taskTracker.TodoList().AddBatch(todoBatch)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	if c.dryRun.Load() {
		c.report(c.newEvent("step").withMessage("Dry run: task plan shown, agents not executed"))
		c.wrapUp.Store(1)
		return "Dry run: the above tasks have been planned but will not be executed. Call finish immediately.", nil
	}

	c.stepConfirmFnMu.RLock()
	stepFn := c.stepConfirmFn
	c.stepConfirmFnMu.RUnlock()
	if stepFn != nil {
		approved, err := stepFn(ctx, tasks)
		if err != nil {
			return "", err
		}
		if !approved {
			for _, item := range todoItems {
				c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "cancelled by user")
			}
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("step").withMessage("Steps: user declined task execution"))
			c.wrapUp.Store(1)
			return "", fmt.Errorf("user declined task execution: call finish immediately with your best summary of work completed so far")
		}
	}

	duplicateWarnings := c.checkDuplicateTasks(tasks)
	if len(duplicateWarnings) > 0 {
		c.report(c.newEvent("loop_warning").withMessage(fmt.Sprintf("Duplicate task delegation detected: %v", duplicateWarnings)))
	}

	resultsCh := make(chan agentTaskResult, len(tasks))
	sem := make(chan struct{}, c.maxConcurrent)
	var wg sync.WaitGroup

	// in-flight dedup: prevents duplicate tasks in the same batch from running concurrently.
	// First task to acquire a key runs; subsequent tasks wait for and share the result.
	var inflightMu sync.Mutex
	inflight := make(map[string]chan agentTaskResult)

	for i, task := range tasks {
		wg.Add(1)
		go func(td TaskDef, tid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Compute effective description (goal takes precedence over task)
			desc := td.Goal
			if td.Constraints != "" {
				desc += "\nconstraints: " + td.Constraints
			}
			agentKey := strings.ToLower(td.Agent)
			cacheKey := agentKey + ":" + truncateTaskDesc(desc)

			// Check in-flight dedup map — wait for first task with same key to complete
			inflightMu.Lock()
			if ch, ok := inflight[cacheKey]; ok {
				inflightMu.Unlock()
				result := <-ch // wait for first task
				resultsCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: result.output, err: result.err}
				return
			}
			inflight[cacheKey] = make(chan agentTaskResult, 1)
			inflightMu.Unlock()

			// Check task result cache before running. Sidecar tasks and tasks
			// that explicitly request summarize always run fresh.
			if !td.Sidecar && !td.Summarize {
				if cached, ok := c.lookupTaskCache(ctx, agentKey, desc); ok {
					c.report(c.newEvent("cache_hit").withAgent(td.Agent).withMessage(desc).withTodoID(tid))
					c.taskTracker.TodoList().UpdateStatus(tid, TaskDone, "")
					c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					result := agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: cached}
					inflightMu.Lock()
					inflight[cacheKey] <- result
					inflightMu.Unlock()
					resultsCh <- result
					return
				}
			}

			var output string
			var err error
			if td.Sidecar {
				output, err = c.executeSidecarTask(ctx, td, tid)
			} else {
				output, err = c.executeTask(ctx, td, tid)
			}
			if err == nil && !td.Sidecar {
				c.storeTaskCache(agentKey, desc, output)
			}
			result := agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: output, err: err}
			if err == nil && td.PlanFirst && td.PlanID == "" {
				c.pendingPlansMu.Lock()
				if pe := c.pendingPlans[tid]; pe != nil && pe.Status == "submitted" {
					result.planText = pe.PlanText
				}
				c.pendingPlansMu.Unlock()
			}
			inflightMu.Lock()
			inflight[cacheKey] <- result
			inflightMu.Unlock()
			resultsCh <- result
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

	if c.forcePlanFirst {
		for i := range results {
			r := &results[i]
			if r.planText == "" {
				continue
			}
			for reviewCycle := 0; reviewCycle <= planReviewerMaxReviews+1; reviewCycle++ {
				pr, err := c.getPlanReviewer(ctx, r.todoID)
				if err != nil {
					r.planText = ""
					r.err = fmt.Errorf("plan reviewer failed: %w", err)
					break
				}
				output, approved, err := pr.review(ctx, r.planText)
				if err != nil {
					r.planText = ""
					r.err = fmt.Errorf("plan review failed: %w", err)
					break
				}
				if approved {
					r.planText = ""
					r.output = output
					break
				}
				c.pendingPlansMu.Lock()
				entry := c.pendingPlans[r.todoID]
				if entry != nil {
					r.planText = entry.PlanText
				} else {
					r.planText = ""
				}
				c.pendingPlansMu.Unlock()
				if r.planText == "" {
					r.err = fmt.Errorf("plan rejected but no new plan submitted")
					break
				}
			}
		}
	}

	c.checkpointSTM()

	return formatTaskResults(results, len(tasks), duplicateWarnings)
}

func (c *Coordinator) executeTask(parentCtx context.Context, task TaskDef, todoID string) (string, error) {
	taskDesc := task.Goal
	if task.Constraints != "" {
		taskDesc += "\nconstraints: " + task.Constraints
	}

	agentDef, _, err := c.resolveAgentName(task.Agent)
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", err
	}
	agentName := strings.ToLower(agentDef.Name)

	// When a model-list is configured, validate the requested model is in it.
	// When no model-list is configured, ignore task.Model entirely — the agent's
	// own model (from agent.md > team.yaml) is used via resolveAgentModel below.
	if len(c.modelList) == 0 {
		task.Model = ""
	} else if task.Model != "" {
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
	if agentDef.Skills != "" {
		skills := strings.Split(agentDef.Skills, ",")
		for i, s := range skills {
			skills[i] = strings.TrimSpace(s)
		}
		c.taskTracker.TodoList().SetSkills(todoID, skills)
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	resolvedModel := c.resolveAgentModel(agentDef, task.Model)

	if c.think {
		c.emitThinkDelegation(agentName, taskDesc, resolvedModel)
	}

	c.report(c.newEvent("start").withAgent(agentName).withMessage(taskDesc).withModel(resolvedModel).withTodoID(todoID))
	prevAgent := c.GetCurrentAgent()
	prevTask := c.GetCurrentTask()
	prevTodoID := c.GetCurrentTodoID()
	c.SetCurrentAgent(agentName)
	c.SetCurrentTask(taskDesc)
	c.SetCurrentTodoID(todoID)
	defer func() {
		c.SetCurrentAgent(prevAgent)
		c.SetCurrentTask(prevTask)
		c.SetCurrentTodoID(prevTodoID)
	}()
	taskTS := time.Now().Format("20060102-150405")
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "working", taskDesc, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, agentName, "working", taskDesc)

	timing := &taskTiming{}
	timing.reset()

	if task.PlanFirst && task.PlanID == "" {
		c.pendingPlansMu.Lock()
		existing := c.pendingPlans[todoID]
		c.pendingPlans[todoID] = &PlanEntry{
			TodoID:      todoID,
			Agent:       agentName,
			Goal:        task.Goal,
			Status:      "",
			ReviewCount: func() int { if existing != nil { return existing.ReviewCount }; return 0 }(),
		}
		c.pendingPlansMu.Unlock()
	}

	var ag fantasy.Agent
	if task.PlanFirst && task.PlanID == "" {
		planAg, planErr := agent.CreateAgent(parentCtx, c.provider, agent.AgentConfig{
			Def:        agentDef,
			TeamConfig: &c.session.Config,
			WorkDir:    c.projectDir,
			MaxSteps:   agent.DefaultMaxSteps,
		}, append(agent.SelectTools(c.coreTools, agentDef.Tools), &submitPlanTool{coordinator: c, todoID: todoID}))
		if planErr != nil {
			c.report(c.newEvent("error").withAgent(agentName).withMessage(planErr.Error()).withTodoID(todoID))
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, planErr.Error())
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "error", taskDesc, "")
			writeStatus(c.session.Workspace, agentName, "error", taskDesc)
			return "", planErr
		}
		ag = planAg
	} else {
		var err error
		ag, err = c.getOrCreateAgent(parentCtx, agentDef, task.Model)
		if err != nil {
			c.report(c.newEvent("error").withAgent(agentName).withMessage(err.Error()).withTodoID(todoID))
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "error", taskDesc, "")
			writeStatus(c.session.Workspace, agentName, "error", taskDesc)
			return "", err
		}
	}

	var prompt string
	if task.PlanFirst && task.PlanID != "" {
		c.pendingPlansMu.Lock()
		entry := c.pendingPlans[task.PlanID]
		if entry == nil {
			c.pendingPlansMu.Unlock()
			return "", fmt.Errorf("plan not found for id %s", task.PlanID)
		}
		planText := entry.PlanText
		c.pendingPlansMu.Unlock()

		prompt = "## Goal\n\n" + task.Goal
		if task.Constraints != "" {
			prompt += "\n\n## Constraints\n\n" + task.Constraints
		}
		prompt += "\n\n## Approved Plan\n\n" + planText
		prompt += "\n\n## Instructions\n\nExecute the approved plan above. You have already planned — now implement each step. Call finish when done."
	} else if task.PlanFirst {
		prompt = "## Goal\n\n" + task.Goal
		if task.Constraints != "" {
			prompt += "\n\n## Constraints\n\n" + task.Constraints
		}
		prompt += "\n\n## Instructions\n\nDraft a detailed task execution plan before doing any work. Your plan should be a numbered list of concrete, actionable steps with brief descriptions. Consider your skills, available tools, and the project context. Call `submit_plan` with your complete plan when ready. Do NOT execute any steps yet — only plan."
	} else {
		prompt = "## Goal\n\n" + task.Goal
		if task.Constraints != "" {
			prompt += "\n\n## Constraints\n\n" + task.Constraints
		}
		prompt += "\n\nYou are a domain expert. Determine your own implementation approach based on the goal above."
	}

	if suffix := c.buildSkillPromptPrefix(agentDef); suffix != "" {
		prompt = prompt + "\n\n" + suffix
	}

	skillSuggestion, skillNames := c.buildSuggestedSkillsText(agentDef, agentName, task.Goal)
	if skillSuggestion != "" {
		c.taskTracker.TodoList().SetInjectedSkills(todoID, skillNames)
		prompt = prompt + "\n\n" + skillSuggestion
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

	if suffix := c.buildMemorySuffix(agentDef.Role); suffix != "" {
		prompt = prompt + "\n\n" + suffix
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
			taskCtx = context.WithValue(taskCtx, todoIDKey{}, todoID)
			taskCtx = context.WithValue(taskCtx, modelKey{}, resolvedModel)
			taskCtx = context.WithValue(taskCtx, tools.AgentNameKey, agentName)
			taskCtx = context.WithValue(taskCtx, hooks.AgentNameKey, agentName)
			taskCtx = context.WithValue(taskCtx, hooks.TeamNameKey, c.session.Config.Name)
			taskCtx = context.WithValue(taskCtx, hooks.TaskDescKey, taskDesc)
			if len(agentDef.Guard) > 0 {
				taskCtx = context.WithValue(taskCtx, tools.GuardRulesKey, agentDef.Guard)
			}
			if len(agentDef.AllowedPaths) > 0 {
				taskCtx = context.WithValue(taskCtx, tools.AgentAllowedPathsKey, agentDef.AllowedPaths)
			}
			if agentDef.RestrictedPath != "" {
				taskCtx = context.WithValue(taskCtx, tools.AgentRestrictedPathKey, agentDef.RestrictedPath)
			}
			if c.noNet || agentDef.NoNet {
				taskCtx = context.WithValue(taskCtx, tools.AgentNetworkBlockKey, true)
			}
			output, steps, err = c.runAgentWithStatusAndHistory(taskCtx, ag, agentName, prompt, conversationHistory, timing)
		}()

		if err == nil {
			c.pendingPlansMu.Lock()
			planEntry := c.pendingPlans[todoID]
			c.pendingPlansMu.Unlock()
			if planEntry != nil && planEntry.Status == "submitted" {
				c.pendingPlansMu.Lock()
				planEntry.Agent = agentName
				planEntry.Goal = task.Goal
				c.pendingPlansMu.Unlock()
				c.taskTracker.TodoList().UpdateStatus(todoID, TaskPlanned, "")
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("step").withAgent(agentName).withMessage("plan submitted").withTodoID(todoID))
				c.report(c.newEvent("done").withAgent(agentName).withMessage("plan submitted").withTodoID(todoID))
				if c.forcePlanFirst {
					return "", nil
				}
				return planEntry.PlanText, nil
			}
			if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "done", taskDesc, output); err != nil {
				log.Printf("warning: failed to write task file: %v", err)
			}
			_ = writeStatus(c.session.Workspace, agentName, "done", taskDesc)
			duration, modelTime, toolTime := timing.snapshot()
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
			c.updateTodoTiming(todoID, modelTime, toolTime)
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("done").withAgent(agentName).withOutput(output).withMessage("completed").withModel(resolvedModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
			if task.Summarize {
				output = c.summarizeOutput(parentCtx, output)
			}
			c.autoWriteSTM(agentName, taskDesc, output, "", true)
			return output, nil
		}

		for _, step := range steps {
			conversationHistory = append(conversationHistory, step.Messages...)
		}

		lastErr = err
		c.report(c.newEvent("error").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d failed: %v", attempt, err)).withModel(resolvedModel).withTodoID(todoID))
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, fmt.Sprintf("attempt %d failed: %v", attempt, err))
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

		if parentCtx.Err() != nil {
			break
		}
	}

	_, modelTime, toolTime := timing.snapshot()
	c.updateTodoTiming(todoID, modelTime, toolTime)
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "error", taskDesc, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, agentName, "error", taskDesc)
	c.autoWriteSTM(agentName, taskDesc, "", lastErr.Error(), false)
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

	taskDesc := task.Goal
	if task.Constraints != "" {
		taskDesc += "\nconstraints: " + task.Constraints
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("sidecar_call").withAgent(task.Agent).withMessage(taskDesc))

	if c.think {
		c.emitThinkDelegation(task.Agent, taskDesc, c.sidecarModel)
		c.emitThinkSidecar("Execute", fmt.Sprintf("running task via sidecar model: %s", c.sidecarModel))
	}

	sidecarTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if sidecarTimeout <= 0 {
		sidecarTimeout = 120 * time.Second
	}
	sidecarCtx, cancel := context.WithTimeout(ctx, sidecarTimeout)
	defer cancel()

	result, err := s.Execute(sidecarCtx, taskDesc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar execute failed for agent %q: %v\n", task.Agent, err)
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", fmt.Errorf("sidecar execution failed (model: %s): %w", c.sidecarModel, err)
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(task.Agent).withOutput(result).withMessage("sidecar completed").withTodoID(todoID))
	return result, nil
}

func (c *Coordinator) runAgentWithStatusAndHistory(ctx context.Context, ag fantasy.Agent, agentName, prompt string, history []fantasy.Message, timing *taskTiming, extraStop ...fantasy.StopCondition) (string, []fantasy.StepResult, error) {
	reportFn := c.reportStatus
	workspace := c.session.Workspace
	teamName := c.session.Config.Name
	logWrite := func(entry string) { writeLLMLog(workspace, teamName, agentName, entry) }

	// Pick up the TodoItem ID injected by executeTask so events can be attributed to a task.
	todoID, _ := ctx.Value(todoIDKey{}).(string)

	streamCall := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: history,
		StopWhen: extraStop,
		PrepareStep: func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
			llmLogRequest(logWrite, opts)
			return ctx, fantasy.PrepareStepResult{}, nil
		},
		OnStepStart: func(stepNumber int) error {
			reportFn(c.newEvent("step").withAgent(agentName).withTodoID(todoID).withStep(stepNumber).withMessage(fmt.Sprintf("step %d", stepNumber)))
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			timing.beginTool()
			argsPreview := tc.Input
			if len(argsPreview) > 10000 {
				r := []rune(argsPreview)
				if len(r) > 10000 {
					argsPreview = string(r[:10000]) + "..."
				}
			}
			reportFn(c.newEvent("tool_call").withAgent(agentName).withTodoID(todoID).withTool(tc.ToolName, argsPreview))
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
			resolvedModel, _ := ctx.Value(modelKey{}).(string)
			reportFn(c.newEvent("tool_result").withAgent(agentName).withTodoID(todoID).withToolResult(tr.ToolName, resultPreview).withModel(resolvedModel))
			llmLogStreamEvent(logWrite, "tool_result", formatToolResultContent(tr))
			_, isErrResult := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Result)
			audit.LogToolResult(agentName, tr.ToolName, resultPreview, isErrResult)
			return nil
		},
		OnTextDelta: func(id, text string) error {
			eventType := "text"
			if c.think {
				eventType = "think_text"
			}
			reportFn(c.newEvent(eventType).withAgent(agentName).withTodoID(todoID).withMessage(text))
			logWrite(text)
			return nil
		},
		OnReasoningDelta: func(id, text string) error {
			logWrite(text)
			return nil
		},
		OnStreamFinish: func(usage fantasy.Usage, finishReason fantasy.FinishReason, providerMetadata fantasy.ProviderMetadata) error {
			llmLogStreamFinish(logWrite, finishReason, usage)

			if c.hooks != nil && c.hooks.HasHooks("after_llm_step") {
				resolvedModel, _ := ctx.Value(modelKey{}).(string)
				hookCtx := hooks.MakeContext(teamName, agentName, todoID, "", resolvedModel, "")
				hookPayload := hooks.HookPayload{
					HookPoint: "after_llm_step",
					Context:   hookCtx,
					Model:     resolvedModel,
					Usage: hooks.UsageSummary{
						PromptTokens:     int(usage.InputTokens),
						CompletionTokens: int(usage.OutputTokens),
						TotalTokens:      int(usage.TotalTokens),
					},
				}
				c.hooks.Dispatch(ctx, "after_llm_step", hookPayload)
			}

			return nil
		},
	}

	if c.hooks != nil && c.hooks.HasHooks("before_llm_step") {
		resolvedModel, _ := ctx.Value(modelKey{}).(string)
		hookCtx := hooks.MakeContext(teamName, agentName, todoID, "", resolvedModel, "")
		hookPayload := hooks.HookPayload{
			HookPoint: "before_llm_step",
			Context:   hookCtx,
			Model:     resolvedModel,
		}
		resp := c.hooks.Dispatch(ctx, "before_llm_step", hookPayload)
		if resp.Result == hooks.HookSkip {
			return "", nil, nil
		}
		if resp.Result == hooks.HookError {
			return "", nil, fmt.Errorf("hook error: %s", resp.ErrorMessage)
		}
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

func extractCapabilities(system string) string {
	if system == "" {
		return ""
	}
	lines := strings.Split(system, "\n")
	var caps []string
	inList := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			inList = false
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			item := strings.TrimPrefix(strings.TrimPrefix(trimmed, "- "), "* ")
			if len(item) > 3 {
				caps = append(caps, item)
				inList = true
			}
		} else if inList && len(caps) > 0 && len(trimmed) > 3 {
			caps[len(caps)-1] += " " + trimmed
		} else {
			inList = false
		}
	}
	for i := range caps {
		caps[i] = strings.TrimSpace(caps[i])
	}
	if len(caps) == 0 {
		sentences := strings.Split(system, ". ")
		for _, s := range sentences {
			s = strings.TrimSpace(s)
			if len(s) > 10 && len(caps) < 5 {
				caps = append(caps, s)
			}
		}
	}
	var result []string
	seen := map[string]bool{}
	for _, c := range caps {
		lower := strings.ToLower(c)
		if !seen[lower] && len(c) > 5 {
			seen[lower] = true
			result = append(result, c)
		}
	}
	return strings.Join(result, "\n")
}

const maxAgentsMDSize = 50000
const defaultWorkerContextSize = 12000

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
		ctxSize := c.getWorkerContextSize()
		if s := c.Sidecar(); s != nil && utf8.RuneCountInString(raw) > ctxSize/2 {
			if c.think {
				c.emitThinkSidecar("Compact", "compacting worker context (AGENTS.md)")
			}
			compacted, err := s.Compact(ctx, raw, "Extract the essential project context: tech stack, language, framework, key conventions, and directory structure. Preserve all factual details but remove verbose descriptions.")
			if err == nil && compacted != "" {
				raw = compacted
			}
		}
		if utf8.RuneCountInString(raw) > ctxSize {
			raw = string([]rune(raw)[:ctxSize]) + "\n\n... [truncated]"
		}
		c.cachedWorkerContext = raw
	})
	return c.cachedWorkerContext
}

func (c *Coordinator) getWorkerContextSize() int {
	if c.session != nil && c.session.Config.WorkerContextSize > 0 {
		return c.session.Config.WorkerContextSize
	}
	return defaultWorkerContextSize
}

func (c *Coordinator) getWorkerSummary(name string) string {
	c.workerSummariesMu.Lock()
	defer c.workerSummariesMu.Unlock()
	if c.workerSummaries == nil {
		return ""
	}
	return c.workerSummaries[name]
}

func (c *Coordinator) computeWorkerSummaries(ctx context.Context) {
	c.workerSummariesOnce.Do(func() {
		c.workerSummaries = make(map[string]string)
		for _, def := range c.uniqueWorkerDefs() {
			if def.System == "" {
				continue
			}
			c.workerSummariesMu.Lock()
			summary := c.summarizeSystem(ctx, def.System)
			c.workerSummaries[def.Name] = summary
			c.workerSummariesMu.Unlock()
		}
	})
}

func (c *Coordinator) summarizeSystem(ctx context.Context, system string) string {
	if s := c.Sidecar(); s != nil {
		if c.think {
			c.emitThinkSidecar("Compact", "summarizing worker system prompt for coordinator")
		}
		compacted, err := s.Compact(ctx, system, "Summarize this agent's role, key behavioral guidelines, and unique instructions in 2-3 concise sentences. Preserve what makes this agent distinct.")
		if err == nil && compacted != "" {
			return compacted
		}
	}
	if utf8.RuneCountInString(system) > 500 {
		runes := []rune(system)
		return string(runes[:500]) + "..."
	}
	return system
}

func (c *Coordinator) buildCoreReminder(def *agent.AgentDef) string {
	if def.System == "" {
		return ""
	}
	lines := strings.Split(def.System, "\n")
	var keyLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "---") {
			continue
		}
		keyLines = append(keyLines, trimmed)
		if len(keyLines) >= 5 {
			break
		}
	}
	if len(keyLines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Core Instructions Reminder\n\n")
	b.WriteString(fmt.Sprintf("You are **%s**", def.Name))
	if def.Description != "" {
		b.WriteString(fmt.Sprintf(": %s", def.Description))
	}
	b.WriteString(". Your core directives:\n")
	for _, line := range keyLines {
		b.WriteString(fmt.Sprintf("- %s\n", line))
	}
	b.WriteString("\nFollow these as your primary directive — they define your identity and priorities.\n")
	return b.String()
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

	var b strings.Builder
	b.WriteString("## Project Context\n\n")
	if wc != "" {
		b.WriteString(wc)
		b.WriteString("\n\n---\n\n")
	}
	wsPath := c.session.Workspace
	sharedPath := filepath.Join(wsPath, sharedDir)
	b.WriteString("## Environment & Rules\n\n")
	fmt.Fprintf(&b, "- CWD: %s | Workspace: %s | Shared: %s | Time: %s\n", c.projectDir, wsPath, sharedPath, c.sessionTime.Format(time.RFC3339))
	fmt.Fprintf(&b, "- ALL intermediate files (drafts, logs, notes, etc.) MUST be written to workspace: %s\n", wsPath)
	fmt.Fprintf(&b, "- Use %s to share files between agents. NEVER write outside workspace.\n\n", sharedPath)
	b.WriteString("---\n\n")

	if memSuffix := c.buildMemorySuffix(def.Role); memSuffix != "" {
		b.WriteString(memSuffix)
		b.WriteString("\n")
	}

	injectedDef := *def
	injectedDef.System = injectedDef.System + "\n\n---\n\n" + b.String()

	if reminder := c.buildCoreReminder(def); reminder != "" {
		injectedDef.System += "\n\n" + reminder
	}

	return &injectedDef
}

func (c *Coordinator) BuildOrchestratorPrompt(autoSkills ...*skill.SkillDef) string {
	workerNames, _ := c.buildWorkerNamesAndDescs()

	var b strings.Builder
	fmt.Fprintf(&b, "You are the coordinator of team %q with %d members: %s.\n\n", c.session.Config.Name, len(workerNames), strings.Join(workerNames, ", "))

	b.WriteString("You MUST delegate ALL work to your team members. You do NOT have tools to do work yourself.\n\n")

	b.WriteString("## How to Coordinate\n\n")
	b.WriteString("0. **Check memory first** — Review the Memory & Context section below. STM (# 進度, # 發現, # 決策, # 錯誤與修復, # 待解決) tracks current session state. LTM (# 專案慣例, # 架構決策, # 常見模式, # 已知問題與解法, # 關鍵檔案, # 工具與指令) records cross-session knowledge. This helps you understand ongoing work and past decisions.\n")
	b.WriteString("1. **Analyze** the user's request to identify which team members are needed\n")
	b.WriteString("2. **Check skills** — if any available skills are relevant to the user's task, call `load_skill` to get the full instructions. Include the relevant skill summary in task descriptions so workers know which skills to load\n")
	b.WriteString("3. **Plan** your approach before delegating — think step by step\n")
	b.WriteString("4. **Select model** — for each task, pick the model from Available Models whose strengths best match the task requirements. Using the right model improves quality and speed.\n")
	b.WriteString("5. **Delegate goals** using agent — describe WHAT outcome each worker should achieve. Use the 'goal' field for the desired outcome and 'constraints' for non-obvious restrictions. Workers are domain experts who determine their own implementation approach.\n\n")
	b.WriteString("   Examples:\n")
	b.WriteString("   - ❌ BAD: \"search src/main.go line 42 for parseUser and fix the nil check\"\n")
	b.WriteString("   - ✅ GOOD: goal=\"Fix nil pointer dereference in user parsing\", constraints=\"Must maintain backward compatibility with existing callers\"\n\n")
	b.WriteString("6. Run independent tasks in parallel by passing multiple tasks in one agent call\n")
	b.WriteString("7. When delegating to a worker that needs skill knowledge, include ALL relevant skill summaries (name, file path) in the task description so the worker can call `load_skill` if needed\n")
	b.WriteString("8. **Trust worker expertise** — Workers have access to the full project context (AGENTS.md, tech stack, conventions, directory structure). They will explore the codebase, identify relevant files, and determine the best implementation approach. Do NOT pre-specify file paths, function names, or implementation steps unless they are non-obvious constraints.\n")
	b.WriteString("9. **Evaluate** results after each agent call — decide if more work is needed or if you can provide a final answer\n")
	b.WriteString("10. **Synthesize** results into a coherent answer for the user\n")
	b.WriteString("11. When satisfied, call the finish tool with your final response\n\n")
	b.WriteString("12. **Deduplicate** — do not delegate conceptually identical tasks multiple times. If the user mentions the same action (e.g., \"dry-run\", \"deploy\", \"analyze\") in multiple parts of their request, delegate it ONCE and reuse the result for both mentions.\n\n")
	b.WriteString("   Examples:\n")
	b.WriteString("   - ❌ BAD: Delegating \"run dry-run\" to worker A, then \"compare diff\" where the worker ALSO runs dry-run internally\n")
	b.WriteString("   - ✅ GOOD: Delegate \"run dry-run\" once and pass the result to the worker doing the comparison\n\n")

	b.WriteString("## Available Agents\n\n")
	fmt.Fprintf(&b, "IMPORTANT: You MUST use these agent names in the agent tool: %s. You can also use listed aliases. Do NOT invent agent names that are not listed.\n\n", strings.Join(workerNames, ", "))
	for _, def := range c.uniqueWorkerDefs() {
		fmt.Fprintf(&b, "### %s\n", def.Name)
		if def.Description != "" {
			fmt.Fprintf(&b, "**Description:** %s\n", def.Description)
		}
		if instr := c.getWorkerSummary(def.Name); instr != "" {
			fmt.Fprintf(&b, "**Instructions:** %s\n", instr)
		}
		if def.Tools != "" {
			fmt.Fprintf(&b, "**Tools:** %s\n", def.Tools)
		}
		if caps := extractCapabilities(def.System); caps != "" {
			fmt.Fprintf(&b, "**Capabilities:**\n")
			for _, line := range strings.Split(caps, "\n") {
				fmt.Fprintf(&b, "- %s\n", line)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Worker Tools\n\n")
	b.WriteString("Workers have access to the following special tools in addition to their configured toolset:\n\n")
	b.WriteString("- **agent**: Create a sub-agent to handle a specific sub-task. The sub-agent inherits the same toolset.\n")
	b.WriteString("- **todo**: Manage a task list to track progress. Workers can create, update, and list their own TODO items.\n\n")

	b.WriteString("## Available Skills\n\n")
	currentSkills := c.getSkills()
	if len(currentSkills) == 0 {
		b.WriteString("No skills are available for this team.\n\n")
	} else {
		b.WriteString("| Skill | File | Description |\n")
		b.WriteString("|-------|------|-------------|\n")
		for _, s := range currentSkills {
			desc := s.Description
			if utf8.RuneCountInString(desc) > 80 {
				runes := []rune(desc)
				desc = string(runes[:80]) + "..."
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", s.Name, s.Path, desc)
		}
		b.WriteString("\nTo get the full instructions for any skill, call the `load_skill` tool with the skill name.\n\n")
	}

	if len(autoSkills) > 0 {
		b.WriteString("## Auto-Loaded Skills\n\n")
		b.WriteString("The following skills were automatically matched to your task. Include the skill name and file path in worker task descriptions so workers can load them if needed.\n\n")
		for _, s := range autoSkills {
			fmt.Fprintf(&b, "- **%s** (`%s`)\n", s.Name, s.Path)
		}
		b.WriteString("\n")
	}

	if len(c.modelList) > 0 {
		b.WriteString("## Available Models\n\n")
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
	b.WriteString("Use **goal** to describe what outcome the worker should achieve. Use **constraints** for non-obvious restrictions. Workers are domain experts who determine their own implementation approach.\n\n")
	if len(c.modelList) > 0 {
		b.WriteString("- **model**: Choose the model whose strengths best match each task — see Available Models above.\n")
	}
	b.WriteString("- **goal**: The desired outcome — what should be achieved (do NOT include file paths or implementation steps)\n")
	b.WriteString("- **constraints**: Non-obvious restrictions the worker must respect (e.g., 'must maintain backward compatibility')\n")
	b.WriteString("- **task**: DEPRECATED — use 'goal' instead. Legacy task description.\n")
	b.WriteString("- **summarize**: Set to `true` to condense the agent's output before returning. Use for tasks that may produce verbose output where only key points matter.\n")
	if c.forcePlanFirst {
		b.WriteString("- **plan_first**: ALWAYS `true` — the system handles plan review automatically. Agents will submit plans, a Plan Reviewer will approve or reject them, and you will only receive the final executed results. You never need to call approve_plan, modify_plan, or reject_plan — these are handled by the system.\n")
	} else {
		b.WriteString("- **plan_first**: Set to `true` for complex tasks where you want the agent to draft a plan before executing. The agent will call submit_plan with their plan. You MUST then review it and call approve_plan, modify_plan, or reject_plan. The plan submission includes a todo ID — use this ID for your review call.\n")
	}
	b.WriteString("```json\n")
	b.WriteString("{\n")
	b.WriteString("  \"tasks\": [\n")
	if len(c.modelList) > 0 {
		b.WriteString("    {\"agent\": \"agent-name\", \"goal\": \"fix the authentication bug\", \"constraints\": \"must not break existing user sessions\", \"model\": \"model-id\", \"summarize\": false}\n")
	} else {
		b.WriteString("    {\"agent\": \"agent-name\", \"goal\": \"fix the authentication bug\", \"constraints\": \"must not break existing user sessions\", \"summarize\": false}\n")
	}
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
		fmt.Fprintf(&b, "    {\"agent\": \"worker-name\", \"goal\": \"fix typo in README\", \"model\": \"%s\"},\n", fastModel)
		fmt.Fprintf(&b, "    {\"agent\": \"worker-name\", \"goal\": \"design distributed consensus algorithm\", \"model\": \"%s\"}\n", complexModel)
		b.WriteString("  ]\n")
		b.WriteString("}\n```\n\n")
	}
	b.WriteString("### load_skill\n")
	b.WriteString("Load the full content of a skill by name. You and your workers can call `load_skill` multiple times to load all relevant skills — include ALL skill names and file paths in worker task descriptions so workers can load them if needed.\n")
	b.WriteString("```json\n{\"name\": \"skill-name\"}\n```\n\n")
	b.WriteString("### save_skill\n")
	b.WriteString("Save a reusable skill to disk and reload it immediately. Use this when you or a worker has solved a non-trivial problem and you want to encode the solution for future reuse.\n")
	b.WriteString("```json\n{\"name\": \"skill-name\", \"description\": \"what it does\", \"content\": \"# Skill\\n\\nStep-by-step workflow...\"}\n```\n\n")
	b.WriteString("### finish\n")
	b.WriteString("Signal completion and provide your final answer to the user. ALWAYS call this when you are done.\n**Important: Call stm_write with a session summary BEFORE calling finish.**\n")
	b.WriteString("```json\n{\"response\": \"Your final synthesized answer to the user\"}\n```\n\n")

	b.WriteString("### approve_plan\n")
	b.WriteString("Approve a submitted task plan and execute it. The plan must have been submitted by an agent via submit_plan. The agent will immediately execute the approved plan.\n")
	b.WriteString("```json\n{\"todo_id\": \"the-plan-todo-id\"}\n```\n\n")

	b.WriteString("### modify_plan\n")
	b.WriteString("Modify a submitted plan (correct or improve it) and then execute the modified version. Provide the corrected plan as a numbered list.\n")
	b.WriteString("```json\n{\"todo_id\": \"the-plan-todo-id\", \"plan\": \"1. First step\\n2. Second step\\n...\"}\n```\n\n")

	b.WriteString("### reject_plan\n")
	b.WriteString("Reject a submitted plan with a reason. The agent will see your reason and re-plan accordingly.\n")
	b.WriteString("```json\n{\"todo_id\": \"the-plan-todo-id\", \"reason\": \"why the plan was rejected and what needs to change\"}\n```\n\n")
	b.WriteString("### ask_user\n")
	b.WriteString("Ask the user a question when you need clarification before proceeding.\n\n")

	b.WriteString("### stm_write\n")
	b.WriteString("Write to short-term memory (stm.md), a shared workspace file visible to all agents in the current session. Use **append** mode to add new information, or **replace** mode to overwrite entirely.\n")
	b.WriteString("**You MUST use stm_write before calling finish** to save a concise session summary (key decisions, findings, errors, and outcomes) so that future agents in this session can build on prior work.\n")
	b.WriteString("```json\n{\"content\": \"concise summary of what happened\", \"mode\": \"append\"}\n```\n\n")

	b.WriteString("### ltm_update\n")
	b.WriteString("Update long-term memory (ltm.md), a persistent file shared across sessions for this team. Use **append** mode to add knowledge, or **replace** mode to overwrite.\n")
	b.WriteString("Use ltm_update to save important cross-session knowledge: project conventions, discovered APIs, recurring patterns, architecture decisions, and lessons learned.\n")
	b.WriteString("```json\n{\"content\": \"discovered API endpoint /api/v2/...\", \"mode\": \"append\"}\n```\n\n")

	wsPath := c.session.Workspace
	sharedPath := filepath.Join(wsPath, sharedDir)
	b.WriteString("\n## Environment & Rules\n\n")
	fmt.Fprintf(&b, "- CWD: %s | Workspace: %s | Shared: %s | Time: %s\n", c.projectDir, wsPath, sharedPath, c.sessionTime.Format(time.RFC3339))
	fmt.Fprintf(&b, "- ALL intermediate files go to workspace: %s. Use %s for inter-agent sharing. NEVER write outside workspace.\n", wsPath, sharedPath)
	b.WriteString("- stm_write before finish. ltm_update for cross-session knowledge (conventions, APIs, patterns, decisions).\n\n")

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
	if c.think {
		c.emitThinkSidecar("Summarize", "summarizing agent output")
	}
	summarized, err := s.Summarize(ctx, text, 2000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar summarize failed: %v\n", err)
		return text
	}
	return summarized
}

func (c *Coordinator) expandOrchestratorTemplate(tmpl string) string {
	workerNames := c.workerNameList()

	// Use session.Config.Vars as base (contains team.yaml vars + CLI --var + built-in)
	vars := make(map[string]string)
	if c.session.Config.Vars != nil {
		for k, v := range c.session.Config.Vars {
			vars[k] = fmt.Sprintf("%v", v)
		}
	}

	// Ensure built-in vars exist (fallback if not in config.Vars)
	if _, ok := vars["TEAM_NAME"]; !ok {
		vars["TEAM_NAME"] = c.session.Config.Name
	}
	if _, ok := vars["AGENT_COUNT"]; !ok {
		vars["AGENT_COUNT"] = fmt.Sprintf("%d", len(workerNames))
	}
	if _, ok := vars["AGENT_NAMES"]; !ok {
		vars["AGENT_NAMES"] = strings.Join(workerNames, ", ")
	}

	result, err := applyTemplate(tmpl, "orchestrator-system", vars)
	if err != nil {
		s := strings.ReplaceAll(tmpl, "{@ .TEAM_NAME @}", c.session.Config.Name)
		s = strings.ReplaceAll(s, "{@ .AGENT_COUNT @}", fmt.Sprintf("%d", len(workerNames)))
		s = strings.ReplaceAll(s, "{@ .AGENT_NAMES @}", strings.Join(workerNames, ", "))
		return s
	}
	return result
}

// expandDefaultOrchestratorTemplate expands the default orchestrator system prompt
// using the team's runtime variables.
func (c *Coordinator) expandDefaultOrchestratorTemplate(tmpl string) string {
	return c.expandOrchestratorTemplate(tmpl)
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
	if c.think {
		c.emitThinkSidecar("Compact", fmt.Sprintf("compacting %d messages", len(messages)))
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

	c.setAutoLoadedSkills(c.matchSkillsWithSidecar(ctx, task))

	resolvedName := strings.ToLower(agentDef.Name)
	directModel := c.resolveAgentModel(agentDef, "")

	todoItems := c.taskTracker.TodoList().AddBatch([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{{Agent: resolvedName, Desc: task, Model: directModel, Source: TaskSourceCoordinator, ParentID: ""}})
	todoID := todoItems[0].ID
	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("start").withAgent(resolvedName).withMessage(task).withModel(directModel).withTodoID(todoID))
	prevAgent := c.GetCurrentAgent()
	prevTask := c.GetCurrentTask()
	prevTodoID := c.GetCurrentTodoID()
	c.SetCurrentAgent(resolvedName)
	c.SetCurrentTask(task)
	c.SetCurrentTodoID(todoID)
	defer func() {
		c.SetCurrentAgent(prevAgent)
		c.SetCurrentTask(prevTask)
		c.SetCurrentTodoID(prevTodoID)
	}()

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

	taskCtx = context.WithValue(taskCtx, todoIDKey{}, todoID)
	taskCtx = context.WithValue(taskCtx, modelKey{}, directModel)
	taskCtx = context.WithValue(taskCtx, tools.AgentNameKey, resolvedName)
	taskCtx = context.WithValue(taskCtx, hooks.AgentNameKey, resolvedName)
	taskCtx = context.WithValue(taskCtx, hooks.TeamNameKey, c.session.Config.Name)
	taskCtx = context.WithValue(taskCtx, hooks.TaskDescKey, task)
	if len(agentDef.Guard) > 0 {
		taskCtx = context.WithValue(taskCtx, tools.GuardRulesKey, agentDef.Guard)
	}
	if len(agentDef.AllowedPaths) > 0 {
		taskCtx = context.WithValue(taskCtx, tools.AgentAllowedPathsKey, agentDef.AllowedPaths)
	}
	if agentDef.RestrictedPath != "" {
		taskCtx = context.WithValue(taskCtx, tools.AgentRestrictedPathKey, agentDef.RestrictedPath)
	}
	if c.noNet || agentDef.NoNet {
		taskCtx = context.WithValue(taskCtx, tools.AgentNetworkBlockKey, true)
	}

	timing := &taskTiming{}
	timing.reset()

	taskTS := time.Now().Format("20060102-150405")
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "working", task, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, resolvedName, "working", task)

	prompt := task
	if suffix := c.buildSkillPromptPrefix(agentDef); suffix != "" {
		prompt = prompt + "\n\n" + suffix
	}

	skillSuggestion, skillNames := c.buildSuggestedSkillsText(agentDef, resolvedName, task)
	if skillSuggestion != "" {
		c.taskTracker.TodoList().SetInjectedSkills(todoID, skillNames)
		prompt = prompt + "\n\n" + skillSuggestion
	}

	if suffix := c.buildMemorySuffix(agentDef.Role); suffix != "" {
		prompt = prompt + "\n\n" + suffix
	}

	output, err := c.runAgentWithStatus(taskCtx, ag, resolvedName, prompt, timing)
	duration, modelTime, toolTime := timing.snapshot()
	if err != nil {
		if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "error", task, ""); err != nil {
			log.Printf("warning: failed to write task file: %v", err)
		}
		_ = writeStatus(c.session.Workspace, resolvedName, "error", task)
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.updateTodoTiming(todoID, modelTime, toolTime)
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("error").withAgent(resolvedName).withMessage(err.Error()).withModel(directModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
		return &DirectAgentResult{AgentName: resolvedName, Error: err}, nil
	}

	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "done", task, output); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, resolvedName, "done", task)
	c.taskTracker.TodoList().UpdateStatus(todoID, TaskDone, "")
	c.updateTodoTiming(todoID, modelTime, toolTime)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(resolvedName).withOutput(output).withMessage("completed").withModel(directModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))

	return &DirectAgentResult{AgentName: resolvedName, Output: output}, nil
}

func (c *Coordinator) buildOrchestratorTools() []fantasy.AgentTool {
	if c.forcePlanFirst {
		orchTools := []fantasy.AgentTool{
			c.RunAgentsTool(),
			&finishTool{coordinator: c},
			&loadSkillTool{coordinator: c},
			&saveSkillTool{coordinator: c},
		}
		for _, t := range c.coreTools {
			name := t.Info().Name
			if name == "ask_user" || name == "stm_write" || name == "ltm_update" {
				orchTools = append(orchTools, t)
			}
		}
		return orchTools
	}
	orchTools := []fantasy.AgentTool{
		c.RunAgentsTool(),
		&finishTool{coordinator: c},
		&approvePlanTool{coordinator: c},
		&modifyPlanTool{coordinator: c},
		&rejectPlanTool{coordinator: c},
		&loadSkillTool{coordinator: c},
		&saveSkillTool{coordinator: c},
	}
	for _, t := range c.coreTools {
		name := t.Info().Name
		if name == "ask_user" || name == "stm_write" || name == "ltm_update" {
			orchTools = append(orchTools, t)
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
	orchCtx = context.WithValue(orchCtx, todoIDKey{}, CoordTodoID)

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

func (c *Coordinator) finalizeRemainingTasks() {
	items := c.taskTracker.TodoList().Items()
	changed := false
	for _, item := range items {
		switch item.Status {
		case TaskInProgress:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "coordinator ended unexpectedly")
			changed = true
		case TaskPending:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "")
			changed = true
		}
	}
	if changed {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
}

// finalizeNormalCompletion marks tasks that are still pending as skipped when
// the coordinator finishes successfully. Tasks in progress should not exist at
// this point since ExecuteTasks waits for all goroutines, but we handle them as
// done out of caution.
func (c *Coordinator) finalizeNormalCompletion() {
	items := c.taskTracker.TodoList().Items()
	changed := false
	for _, item := range items {
		switch item.Status {
		case TaskPending:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "")
			changed = true
		case TaskInProgress:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "still in progress at completion")
			changed = true
		}
	}
	if changed {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
}

func (c *Coordinator) emitThinkSkills(matched []*skill.SkillDef) {
	if len(matched) == 0 {
		c.report(c.newEvent("think_skills").withMessage("no skills matched (keyword fallback used)"))
		return
	}
	names := make([]string, len(matched))
	for i, s := range matched {
		names[i] = s.Name
	}
	c.report(c.newEvent("think_skills").withMessage(fmt.Sprintf("matched %d skills: %s", len(matched), strings.Join(names, ", "))))
	for _, s := range matched {
		c.report(c.newEvent("think_skill_detail").withAgent(s.Name).withMessage(s.Description))
	}
}

func (c *Coordinator) emitThinkAgents() {
	workers := c.uniqueWorkerDefs()
	var b strings.Builder
	fmt.Fprintf(&b, "available agents (%d): ", len(workers))
	for _, def := range workers {
		fmt.Fprintf(&b, "%s(%s), ", def.Name, def.Description)
	}
	msg := strings.TrimSuffix(b.String(), ", ")
	c.report(c.newEvent("think_agents").withMessage(msg))
}

func (c *Coordinator) emitThinkPrompt(systemPrompt string) {
	n := utf8.RuneCountInString(systemPrompt)
	c.report(c.newEvent("think_prompt").withMessage(fmt.Sprintf("system prompt assembled (%d chars)", n)))

	dumpPath := filepath.Join(c.session.Workspace, "think-prompt.md")
	if err := os.WriteFile(dumpPath, []byte(systemPrompt), 0o644); err == nil {
		c.report(c.newEvent("think_prompt_dump").withMessage("saved to " + dumpPath))
	}
}

func (c *Coordinator) emitThinkDelegation(agent, task, model string) {
	msg := fmt.Sprintf("delegating → %s ← %q [model: %s]", agent, task, model)
	c.report(c.newEvent("think_delegation").withAgent(agent).withMessage(msg))
}

func (c *Coordinator) emitThinkSidecar(action, detail string) {
	msg := fmt.Sprintf("%s: %s", action, detail)
	c.report(c.newEvent("think_sidecar").withMessage(msg))
}

func (c *Coordinator) emitThinkText(text string) {
	c.report(c.newEvent("think_text").withMessage(text))
}

func (c *Coordinator) Run(ctx context.Context, userPrompt string) (string, error) {
	c.lastStmWrite = time.Time{}

	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	EnsureWorkspaceDirs(c.session.Workspace)

	c.report(c.newEvent("step").withMessage("coordinator preparing"))

	if c.sessionData != nil {
		c.sessionData.AddEntry("user", userPrompt)
	}

	systemPrompt := c.expandOrchestratorTemplate(orchDef.System)
	if systemPrompt == "" {
		systemPrompt = c.expandDefaultOrchestratorTemplate(defaultOrchestratorSystem)
	}
	matchedSkills := c.matchSkillsWithSidecar(ctx, userPrompt)
	c.setAutoLoadedSkills(matchedSkills)
	c.computeWorkerSummaries(ctx)

	if c.think {
		c.emitThinkSkills(matchedSkills)
	}

	systemPrompt += "\n\n" + c.BuildOrchestratorPrompt(matchedSkills...)

	if c.think {
		c.emitThinkAgents()
	}

	if agentsMD := c.loadProjectContext(); agentsMD != "" {
		if s := c.Sidecar(); s != nil && len(agentsMD) > 4000 {
			if c.think {
				c.emitThinkSidecar("Compact", "compacting AGENTS.md for coordinator prompt")
			}
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

	if suffix := c.buildMemorySuffix("coordinator"); suffix != "" {
		systemPrompt += "\n\n" + suffix
	}

	if reminder := c.buildCoreReminder(orchDef); reminder != "" {
		systemPrompt += "\n\n" + reminder
	}

	if c.think {
		c.emitThinkPrompt(systemPrompt)
	}

	// Apply the computed system prompt to a copy so shared state is not mutated.
	orchDefCopy := *orchDef
	orchDefCopy.System = systemPrompt

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("coordinator starting").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	result, steps, err := c.runOrchestrator(ctx, &orchDefCopy, userPrompt)
	if err != nil {
		c.finalizeRemainingTasks()
		c.saveHistoryAndSession(ctx, steps)
		orchModel := c.resolveAgentModel(orchDef, "")
		c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator failed").withTodoID(CoordTodoID))
		return "", fmt.Errorf("coordinator failed (model: %s): %w", orchModel, err)
	}

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

	if c.sessionData != nil {
		c.sessionData.AddEntry("assistant", finalResult)
		c.sessionData.Rounds = c.round
		SaveSession(c.session.Workspace, c.sessionData)
	}

	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator finished").withTodoID(CoordTodoID))
	return finalResult, nil
}

func (c *Coordinator) ContinueWithPrompt(ctx context.Context, additionalPrompt string) (string, error) {
	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	memorySuffix := c.buildMemorySuffix("coordinator")

	var continuationPrompt string
	if c.IsWrapUp() {
		continuationPrompt = wrapUpPromptTemplate + "\n\n" + memorySuffix
		additionalPrompt = "wrap up now"
		c.report(c.newEvent("wrap_up_phase").withMessage("coordinator summarizing").withTodoID(CoordTodoID))
	} else {
		continuationPrompt = fmt.Sprintf(continuationPromptTemplate, additionalPrompt) + "\n\n" + memorySuffix
		c.report(c.newEvent("step").withMessage("coordinator preparing").withTodoID(CoordTodoID))
	}

	if c.sessionData != nil {
		c.sessionData.AddEntry("user", additionalPrompt)
	}

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("continuing with additional input").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	result, steps, err := c.runOrchestrator(ctx, orchDef, continuationPrompt)
	if err != nil {
		c.finalizeRemainingTasks()
		c.saveHistoryAndSession(ctx, steps)
		orchModel := c.resolveAgentModel(orchDef, "")
		c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator continuation failed").withTodoID(CoordTodoID))
		return "", fmt.Errorf("coordinator continuation failed (model: %s): %w", orchModel, err)
	}

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

	if c.sessionData != nil {
		c.sessionData.AddEntry("assistant", finalResult)
		c.sessionData.Rounds = c.round
		SaveSession(c.session.Workspace, c.sessionData)
	}

	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("continuation finished").withTodoID(CoordTodoID))
	return finalResult, nil
}

const defaultOrchestratorSystem = `You are the orchestrator of "{@ .TEAM_NAME @}", a software development team with {@ .AGENT_COUNT @} members: {@ .AGENT_NAMES @}.

Your role is to coordinate the team: break down user requests into concrete tasks, delegate them to the right members, and synthesize the results into a coherent response.

Rules:
- You MUST use agent to delegate ALL work to team members
- Running independent tasks in parallel is preferred
- After receiving results from agent, evaluate whether more work is needed or if you can provide a final answer
- Synthesize results from workers into a coherent answer for the user
- NEVER attempt to do the work yourself — you do not have tools for that
- If a task fails, provide clearer GOALS or add missing CONSTRAINTS — do NOT add implementation details
- Break complex requests into smaller subtasks for appropriate workers
- Use ask_user when you need clarification from the user before proceeding
- When you have completed all coordination and have a final answer, call the finish tool with your response
- ALWAYS call finish when done — do not just output text as your final answer
- If the user's task relates to a skill, use load_skill to get the detailed instructions. Include the skill name and file path in worker task descriptions so workers can load it themselves if needed
- Workers have access to load_skill — include the skill name and path in the task description rather than the full skill content

Delegation Guidelines:
- Break down user requests into outcome-oriented goals for each worker
- Describe WHAT to achieve (goal), not HOW to achieve it
- Only specify constraints that are non-obvious or user-mandated (e.g., "must not break public API")
- Workers will determine file paths, tool selection, and implementation approach based on the goal
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

type dryRunAgentsTool struct {
	coordinator *Coordinator
	captured    *[]TaskDef
	pOpts       fantasy.ProviderOptions
}

func (t *dryRunAgentsTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "agent",
		Description: "Delegate tasks to team workers. Runs all tasks in parallel. Returns structured results from each agent.",
		Parameters: map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"properties":          buildAgentTaskProperties(t.coordinator.workerNameList(), len(t.coordinator.modelList) > 0, filepath.Join(t.coordinator.session.Workspace, sharedDir)),
					"required":            []string{"agent"},
					"additionalProperties": false,
				},
			},
		},
		Required: []string{"tasks"},
	}
}

func (t *dryRunAgentsTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *dryRunAgentsTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *dryRunAgentsTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Tasks []TaskDef `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if len(args.Tasks) == 0 {
		return fantasy.NewTextErrorResponse("no tasks provided"), nil
	}

	*t.captured = append(*t.captured, args.Tasks...)

	var descs []string
	for _, td := range args.Tasks {
		desc := td.Goal
		if td.Constraints != "" {
			desc += "\nconstraints: " + td.Constraints
		}
		descs = append(descs, fmt.Sprintf("  - %s → %s", td.Agent, desc))
	}
	return fantasy.NewTextResponse(fmt.Sprintf("[DRY RUN] Tasks recorded (not executed):\n%s\n\nDry run complete. Call finish immediately.", strings.Join(descs, "\n"))), nil
}

type dryRunFinishTool struct {
	pOpts fantasy.ProviderOptions
}

func (t *dryRunFinishTool) Info() fantasy.ToolInfo {
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

func (t *dryRunFinishTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *dryRunFinishTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *dryRunFinishTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("FINISHED:%s", args.Response)), nil
}

func (c *Coordinator) DryRun(ctx context.Context, userPrompt string) (*DryRunResult, error) {
	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return nil, fmt.Errorf("no coordinator agent found in team")
	}

	EnsureWorkspaceDirs(c.session.Workspace)

	matchedSkills := c.matchSkillsWithSidecar(ctx, userPrompt)
	c.setAutoLoadedSkills(matchedSkills)

	if c.think {
		c.emitThinkSkills(matchedSkills)
		c.computeWorkerSummaries(ctx)
		c.emitThinkAgents()
	}

	systemPrompt := c.expandOrchestratorTemplate(orchDef.System)
	if systemPrompt == "" {
		systemPrompt = c.expandDefaultOrchestratorTemplate(defaultOrchestratorSystem)
	}
	orchestratorPrompt := c.BuildOrchestratorPrompt(matchedSkills...)
	systemPrompt += "\n\n" + orchestratorPrompt

	if agentsMD := c.loadProjectContext(); agentsMD != "" {
		if s := c.Sidecar(); s != nil && len(agentsMD) > 4000 {
			if c.think {
				c.emitThinkSidecar("Compact", "compacting AGENTS.md")
			}
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

	if suffix := c.buildMemorySuffix("coordinator"); suffix != "" {
		systemPrompt += "\n\n" + suffix
	}

	orchDefCopy := *orchDef
	orchDefCopy.System = systemPrompt

	var capturedTasks []TaskDef

	dryRunAgentTool := &dryRunAgentsTool{
		coordinator: c,
		captured:    &capturedTasks,
	}

	orchTools := []fantasy.AgentTool{
		dryRunAgentTool,
		&dryRunFinishTool{},
		&loadSkillTool{coordinator: c},
		&saveSkillTool{coordinator: c},
	}
	for _, t := range c.coreTools {
		if t.Info().Name == "ask_user" {
			orchTools = append(orchTools, t)
			break
		}
	}

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("dry-run coordinator starting").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	orch, err := agent.CreateAgent(ctx, c.provider, agent.AgentConfig{
		Def:        &orchDefCopy,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   agent.DefaultCoordinatorMaxSteps,
	}, orchTools)
	if err != nil {
		return nil, fmt.Errorf("failed to create coordinator for dry-run: %w", err)
	}

	coordinatorTimeout := time.Duration(c.session.Config.Timeout) * time.Second * time.Duration(c.session.Config.MaxRounds+1)
	if orchDef.Timeout > 0 {
		coordinatorTimeout = time.Duration(orchDef.Timeout) * time.Second
	}
	dryRunCtx, cancel := context.WithTimeout(ctx, coordinatorTimeout)
	defer cancel()
	dryRunCtx = context.WithValue(dryRunCtx, todoIDKey{}, CoordTodoID)

	_, _, err = c.runAgentWithStatusAndHistory(dryRunCtx, orch, orchDef.Name, userPrompt, nil, &taskTiming{},
		fantasy.HasToolCall("agent"),
		fantasy.StepCountIs(agent.DefaultCoordinatorMaxSteps),
	)

	result := &DryRunResult{
		TeamName:           c.session.Config.Name,
		Model:              c.resolveAgentModel(orchDef, ""),
		SidecarModel:       c.sidecarModel,
		OrchestratorPrompt: orchestratorPrompt,
		FirstRoundTasks:    capturedTasks,
	}
	if err != nil {
		result.Error = err.Error()
	}

	for _, sk := range matchedSkills {
		result.MatchedSkillNames = append(result.MatchedSkillNames, sk.Name)
	}

	allSkills := c.getSkills()
	for _, sk := range allSkills {
		result.AllSkills = append(result.AllSkills, DryRunSkillInfo{
			Name:        sk.Name,
			Description: sk.Description,
		})
	}

	for _, def := range c.session.Agents {
		role := def.Role
		if role == "" {
			role = "worker"
		}
		model := c.resolveAgentModel(def, "")
		var tools []string
		if def.Tools != "" {
			tools = strings.Split(def.Tools, ",")
			for i, t := range tools {
				tools[i] = strings.TrimSpace(t)
			}
		}
		var skills []string
		if def.Skills != "" {
			skills = strings.Split(def.Skills, ",")
			for i, s := range skills {
				skills[i] = strings.TrimSpace(s)
			}
		}
		if role == "coordinator" || role == "orchestrator" {
			tools = []string{"agent", "finish", "load_skill", "save_skill", "ask_user"}
		}
		result.Agents = append(result.Agents, DryRunAgentInfo{
			Name:   def.Name,
			Role:   role,
			Model:  model,
			Tools:  tools,
			Skills: skills,
		})
	}

	// sidecarModel is already resolved at coordinator creation time
	// (via config.ResolveSidecarModel in main.go). If it's still empty,
	// resolve it here as a fallback using the same resolution path.
	if result.SidecarModel == "" {
		resolvedConfig := config.LoadConfig()
		result.SidecarModel = resolvedConfig.ResolveSidecarModel(c.session.Config.SidecarModel)
	}

	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("dry-run coordinator finished").withTodoID(CoordTodoID))

	return result, nil
}
