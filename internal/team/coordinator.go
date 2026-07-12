package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
var taskStatusRe = regexp.MustCompile(`\*\*Status:\*\*\s*(\S+)`)
var extraWSSeq atomic.Uint64

// todoIDKey is a context key used to pass the current task's TodoItem ID
// down through executeTask → runAgentWithStatusAndHistory so that emitted
// StatusEvents can be attributed to a specific task for the TUI.
type todoIDKey struct{}

// modelKey is a context key used to pass the resolved model name down
// through executeTask → runAgentWithStatusAndHistory so that tool_result
// events can include the model for TUI display.
type modelKey struct{}

// delegationChainKey carries the "/"-joined chain of agent names that led to
// the current request_agent call, propagated through the context (the same
// way todoIDKey is) since the coordinator's mutable snapshot only ever holds
// the single currently-running agent's flat name.
type delegationChainKey struct{}

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
	DependsOn    []int    `json:"depends_on,omitempty"` // 0-based indices into the tasks array for this call
	// Pipeline is shorthand for depends_on:[i-1]: the task waits for the
	// immediately preceding task in the same batch. Ignored on the first task.
	Pipeline bool `json:"pipeline,omitempty"`
	// Verify is an optional shell command that objectively checks the task's
	// deliverable (e.g. "test -f report.pdf", "go build ./..."). It runs after
	// the agent reports success but before the task is marked done; a non-zero
	// exit makes the task fail and triggers a retry. This guards against agents
	// that claim completion without producing the expected artifact.
	Verify     string   `json:"verify,omitempty"`
	Requires   []string `json:"requires,omitempty"`
	MaxRetries int      `json:"max_retries,omitempty"` // Maximum number of retries if verify fails
	OnFailure  *int     `json:"on_failure,omitempty"`  // 0-based index of the task to jump back to if verify fails
	// Escalate makes each retry after a failure re-run the task on the next
	// stronger model in the model-list (ordered weakest→strongest).
	Escalate bool `json:"escalate,omitempty"`
	// AdversarialVerify is the number of skeptic LLM verifiers (0 = disabled,
	// capped at maxSkeptics) that independently try to refute the result after
	// the task succeeds; a majority refutation fails the task into the retry
	// path with the refutation as feedback.
	AdversarialVerify int `json:"adversarial_verify,omitempty"`
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

type agentResult struct {
	model  string
	output string
	err    error
}

const maxConcurrentModels = 3

// maxDraftsPerSession caps the number of skill draft candidates that
// checkSkillPatterns will surface per session. It is the last line of
// defense after upstream frequency, semantic, and prefix filters; even
// if all upstream filters let too many through, the user sees at most
// this many drafts in a session.
const maxDraftsPerSession = 3

// summaryMaxRunes is the character cap for the short task summary that
// the TODO panel shows alongside each task. The full output is kept in
// the task's Output field; only the summary is truncated to this size.
const summaryMaxRunes = 300

// coordToolBase provides the common ProviderOptions implementation for all
// coordinator tools, eliminating the repeated pOpts field + ProviderOptions()
// + SetProviderOptions() boilerplate in each tool type.
type coordToolBase struct {
	opts fantasy.ProviderOptions
}

func (b *coordToolBase) ProviderOptions() fantasy.ProviderOptions        { return b.opts }
func (b *coordToolBase) SetProviderOptions(opts fantasy.ProviderOptions) { b.opts = opts }

type Coordinator struct {
	mu                    sync.RWMutex
	session               *TeamSession
	providerManager       *agent.ProviderManager
	mcpManager            *mcp.MCPToolManager
	coreTools             []fantasy.AgentTool
	agentCache            map[string]fantasy.Agent
	agentCacheMu          sync.RWMutex
	round                 int
	baseRounds            int // rounds completed before the last round-state reset (resume/continue)
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
	finishCalled          atomic.Bool // set when the finish tool completes; cleared per orchestrator run
	current               atomic.Pointer[currentSnapshot]
	currentStageStart     time.Time
	currentStageStartMu   sync.RWMutex
	auditLogger           *audit.AuditLogger
	sshSessionMgr         *tools.SSHSessionManager
	skillUsage            map[string]*skillUsageState
	skillUsageMu          sync.Mutex
	delegatedTasks        map[string]int
	delegatedTasksMu      sync.Mutex
	taskResultCache       map[string][]cachedTaskEntry // agent → ordered list of past results
	taskResultCacheMu     sync.RWMutex
	capabilityCache       map[string]CapabilityResult
	capabilityCacheMu     sync.Mutex
	capabilityInflight    map[string]chan CapabilityResult
	cacheGeneration       atomic.Int64 // bumped each time coordinator starts a new delegation round
	journal               *taskJournal // persistent task-result journal (nil when disabled)
	noJournal             bool
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
	judgeModel            string
	judgeInst             *sidecar.Sidecar
	judgeInitMu           sync.Mutex
	judgeInit             bool
	planReviewerModel     string
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

	// Skill pattern detection
	skillDetector         *skill.SkillPatternDetector
	skillGenerator        *skill.AutoSkillGenerator
	skillPatternsDetected int // count of patterns detected in current session
	maxDrafts             int // per-session cap on skill draft candidates (0 disables)

	// stepConfirmFn must be set before Run() or protected by stepConfirmFnMu.
	stepConfirmFn       func(context.Context, []TaskDef) (bool, error)
	stepConfirmFnMu     sync.RWMutex
	hooks               *hooks.HookRegistry
	rbashMode           bool
	restrictedPath      string
	noNet               bool
	forceMCP            bool
	workerSummariesOnce sync.Once
	workerSummaries     map[string]string
	workerSummariesMu   sync.Mutex
	pendingPlans        map[string]*PlanEntry
	lastFailureMu       sync.RWMutex
	lastFailureAgent    string
	lastFailureTask     string
	lastFailureTodoID   string
	lastFailureDetail   string
	// approvedOutputs stores actual task output once autoApprovePlan executes.
	// CRITICAL: Always access under pendingPlansMu. All access points:
	//   - review() lines 246-248: read + delete under lock
	//   - autoApprovePlan() line 289: write under lock
	// Do NOT read or write without holding pendingPlansMu.
	approvedOutputs   map[string]string
	approvedErrors    map[string]error
	pendingPlansMu    sync.Mutex
	forcePlanFirst    bool
	autoSkillsEnabled bool

	sessionToolPermissions   map[string]bool // toolName -> allowed (permanent session decision)
	sessionToolPermissionsMu sync.RWMutex

	// executionEvents is initialized for each top-level Run/Continue call and
	// receives attempt-level telemetry for `hufu improve`.
	executionEventsMu sync.RWMutex
	executionEvents   *executionEventLogger
	executionRunID    string

	// One-shot startup validation of configured model names.
	validateModelsOnce sync.Once
	validateModelsErr  error

	// Unattended / budget controls for no-human-watching operation.
	unattended          bool
	autoApprove         bool
	maxWallClock        time.Duration // 0 = unlimited
	tokenBudget         int64         // 0 = unlimited; cumulative LLM tokens
	tokensUsed          atomic.Int64
	acceptanceCmd       string // optional shell command run at finish
	rollbackCmd         string // optional shell command run on acceptance failure
	selfHealingAttempts int
	budgetTripped       atomic.Bool
}

// SetUnattended enables unattended (no-human) mode: ask_user returns safe
// defaults, and only explicitly-allowed tools may run.
func (c *Coordinator) SetUnattended(v bool) { c.unattended = v }

// SetAutoApprove enables automatic selection of clearly safe ask_user options
// when one is available.
func (c *Coordinator) SetAutoApprove(v bool) { c.autoApprove = v }

// SetNoJournal disables the persistent task-result journal.
func (c *Coordinator) SetNoJournal(v bool) { c.noJournal = v }

// IsUnattended reports whether the coordinator is in unattended mode.
func (c *Coordinator) IsUnattended() bool { return c.unattended }

// IsAutoApprove reports whether ask_user should auto-select safe options when
// possible.
func (c *Coordinator) IsAutoApprove() bool { return c.autoApprove }

// SetBudget configures the run's wall-clock and cumulative-token ceilings.
// Zero values mean unlimited.
func (c *Coordinator) SetBudget(maxWallClockSeconds, maxTotalTokens int64) {
	if maxWallClockSeconds > 0 {
		c.maxWallClock = time.Duration(maxWallClockSeconds) * time.Second
	}
	if maxTotalTokens > 0 {
		c.tokenBudget = maxTotalTokens
	}
}

// SetAcceptance sets an optional shell command run when the coordinator
// finishes; a non-zero exit marks the run as not-accepted.
func (c *Coordinator) SetAcceptance(cmd string) { c.acceptanceCmd = cmd }

// SetRollback sets an optional shell command run on acceptance failure in unattended mode.
func (c *Coordinator) SetRollback(cmd string) { c.rollbackCmd = cmd }

func (c *Coordinator) chooseAskUserResponse(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (tools.AskUserResponse, error) {
	s := c.Sidecar()
	if s == nil {
		return tools.AskUserResponse{}, fmt.Errorf("no sidecar configured")
	}
	return s.ChooseAskUserResponse(ctx, question, qtype, opts, allowAny)
}

// TokensUsed returns the cumulative LLM token count observed so far.
func (c *Coordinator) TokensUsed() int64 { return c.tokensUsed.Load() }

// addStepTokens accumulates token usage from a set of agent steps. Some
// providers report no usage at all (observed: minimax via ollama returned
// tokens_in/out=0 for every response); estimate from message bytes in that
// case so token budgets are not silently blind for most of a run.
func (c *Coordinator) addStepTokens(steps []fantasy.StepResult) {
	var total int64
	for _, s := range steps {
		if s.Usage.TotalTokens > 0 {
			total += s.Usage.TotalTokens
			continue
		}
		est := 0
		for _, m := range s.Messages {
			est += messageTextSize(m)
		}
		total += int64(est / 4)
	}
	if total > 0 {
		c.tokensUsed.Add(total)
	}
}

// budgetExceeded reports whether any configured budget (wall-clock or tokens)
// has been exceeded, along with a human-readable reason.
func (c *Coordinator) budgetExceeded() (bool, string) {
	if c.maxWallClock > 0 {
		if elapsed := time.Since(c.sessionTime); elapsed > c.maxWallClock {
			return true, fmt.Sprintf("wall-clock budget exceeded (%s > %s)", elapsed.Round(time.Second), c.maxWallClock)
		}
	}
	if c.tokenBudget > 0 {
		if used := c.tokensUsed.Load(); used >= c.tokenBudget {
			return true, fmt.Sprintf("token budget exceeded (%d >= %d)", used, c.tokenBudget)
		}
	}
	return false, ""
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
	if !t.counting {
		return duration, 0, toolTime
	}
	modelTime = duration - toolTime
	if modelTime < 0 {
		modelTime = 0
	}
	return
}

// RoleModels groups the resolved model IDs for the sidecar, guard, judge,
// and plan-reviewer roles. Passing this instead of adjacent string
// parameters prevents NewCoordinator callers from silently transposing
// same-typed positional arguments.
type RoleModels struct {
	Sidecar      string
	Guard        string
	Judge        string
	PlanReviewer string
}

func NewCoordinator(session *TeamSession, defaultProviderURL, defaultProviderAPIKey string, mcpManager *mcp.MCPToolManager, memoryStore *memory.MemoryStore, modelList []config.ModelEntry, roleModels RoleModels, maxConcurrent int, verbose bool, think bool, direnv bool, allowedPaths []string, pathConsent *tools.PathConsent, hookRegistry *hooks.HookRegistry, rbashMode bool, restrictedPath string, noNet bool, forceMCP bool, forcedSkillNames []string, planMode bool, autoSkillsMode bool) (*Coordinator, error) {
	projectDir, _ := os.Getwd()
	coreTools := agent.BuildAllAgentTools(projectDir, tools.WithAllowedPaths(allowedPaths), tools.WithPathConsent(pathConsent), tools.WithWorkspaceName(filepath.Base(session.Workspace)), tools.WithHooks(hookRegistry), tools.WithRestrictedBash(rbashMode), tools.WithRestrictedPath(restrictedPath), tools.WithNetworkBlock(noNet), tools.WithForceMCP(forceMCP), tools.WithDirenv(direnv))
	pm, err := agent.NewProviderManager(defaultProviderURL, defaultProviderAPIKey, session.Config.Providers)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider manager: %w", err)
	}
	c := &Coordinator{
		providerManager:    pm,
		session:            session,
		mcpManager:         mcpManager,
		coreTools:          coreTools,
		agentCache:         make(map[string]fantasy.Agent),
		verbose:            verbose,
		think:              think,
		reportStatus:       func(event StatusEvent) {},
		taskTracker:        NewTaskTracker(),
		skills:             session.Skills,
		projectDir:         projectDir,
		skillUsage:         make(map[string]*skillUsageState),
		delegatedTasks:     make(map[string]int),
		pendingPlans:       make(map[string]*PlanEntry),
		approvedOutputs:    make(map[string]string),
		approvedErrors:     make(map[string]error),
		taskResultCache:    make(map[string][]cachedTaskEntry),
		capabilityCache:    make(map[string]CapabilityResult),
		capabilityInflight: make(map[string]chan CapabilityResult),
		memoryStore:        memoryStore,
		modelList:          modelList,
		sidecarModel:       roleModels.Sidecar,
		guardModel:         roleModels.Guard,
		judgeModel:         roleModels.Judge,
		planReviewerModel:  roleModels.PlanReviewer,
		maxConcurrent:      maxConcurrent,
		sessionTime:        time.Now(),
		hooks:              hookRegistry,
		rbashMode:          rbashMode,
		restrictedPath:     restrictedPath,
		noNet:              noNet,
		forceMCP:           forceMCP,
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
		forcePlanFirst:         planMode,
		autoSkillsEnabled:      autoSkillsMode,
		sessionToolPermissions: make(map[string]bool),
		skillDetector:          skill.NewSkillPatternDetector(5, 3, 10), // minFrequency=5, windowMin=3, windowMax=10
		skillGenerator:         skill.NewAutoSkillGenerator(filepath.Join(session.Dir, "skills")),
		skillPatternsDetected:  0,
		maxDrafts:              maxDraftsPerSession,
	}

	// Enable sidecar for skill pattern detection
	if s := c.Sidecar(); s != nil {
		c.skillDetector.SetSidecar(s)
	}

	auditLogger, err := audit.NewAuditLogger(session.Workspace, session.Config.Name)
	if err == nil {
		c.auditLogger = auditLogger
		audit.SetDefault(auditLogger)
	}

	// Initialize SSH session manager
	c.sshSessionMgr = tools.NewSSHSessionManager()

	c.coreTools = append(c.coreTools,
		&requestAgentTool{coordinator: c},
		&todoTool{coordinator: c},
		&loadSkillTool{coordinator: c},
		&saveSkillTool{coordinator: c},
		&stmWriteTool{coordinator: c},
		&ltmUpdateTool{coordinator: c},
		&teamInfoTool{coordinator: c},
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
			return false, "", err
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

	if !planMode {
		if history := LoadConversationHistory(session.Workspace); len(history) > 0 {
			c.conversationHistory = history
		}
	}

	if pathConsent != nil {
		coordinator := c
		pathConsent.SetAgentInfoSource(func() tools.AgentInfo {
			return coordinator.GetCurrentAgentInfo()
		})
	}

	return c, nil
}

// ResetConversation clears the accumulated coordinator conversation history so
// the next Run/ContinueWithPrompt starts fresh. Used by the chat REPL's /reset.
func (c *Coordinator) ResetConversation() {
	c.conversationHistoryMu.Lock()
	c.conversationHistory = nil
	c.conversationHistoryMu.Unlock()

	c.resetRoundState()
}

// totalRounds returns the round count across the whole session, including
// rounds run before a continue/resume reset the per-run counter.
func (c *Coordinator) totalRounds() int {
	return c.baseRounds + c.round
}

func (c *Coordinator) resetRoundState() {
	// Rounds already run must survive the reset: session.json's round count and
	// stm snapshots previously restarted at 0 on every continue, overwriting
	// history from earlier segments of the same session.
	c.baseRounds += c.round
	c.round = 0
	c.wrapUp.Store(0)
	c.finishCalled.Store(false)
	c.delegatedTasksMu.Lock()
	c.delegatedTasks = make(map[string]int)
	c.delegatedTasksMu.Unlock()
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
	// Populate SSH session count automatically
	if c.sshSessionMgr != nil {
		event.SSHSessions = c.sshSessionMgr.Count()
	}
	c.reportStatus(event)
}

func (c *Coordinator) newEvent(eventType string) StatusEvent {
	return StatusEvent{Type: eventType, TeamName: c.session.Config.Name}
}

func (c *Coordinator) updateTodoTiming(todoID string, modelTime, toolTime time.Duration) {
	c.taskTracker.TodoList().UpdateTodoTiming(todoID, modelTime, toolTime)
}

func (c *Coordinator) SetWrapUp() {
	c.wrapUp.Store(1)
	c.SetCurrentStage("wrapping_up")
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
		"depends_on": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "integer"},
			"description": "0-based indices of tasks in this call's tasks array that must complete before this task starts. Example: [{agent:\"researcher\",goal:\"find X\"},{agent:\"coder\",goal:\"implement X\",depends_on:[0]}] — the coder waits for the researcher to finish.",
		},
		"pipeline": map[string]any{
			"type":        "boolean",
			"description": "If true, this task waits for the immediately previous task in this tasks array (shorthand for depends_on:[i-1]). Ignored on the first task. Use for simple linear chains A→B→C instead of writing depends_on indices.",
		},
		"verify": map[string]any{
			"type":        "string",
			"description": "Optional shell command that objectively verifies the task's deliverable exists/works (e.g. 'test -f workspace/report.md', 'go build ./...'). It runs after the agent reports success; a non-zero exit fails the task and triggers a retry. Use it for tasks with a checkable artifact so the agent cannot falsely claim completion.",
		},
		"requires": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Optional capability names that must be available before this task starts. Each name must match a capability declared in team.yaml `preflight`.",
		},
		"adversarial_verify": map[string]any{
			"type":        "integer",
			"description": "Optional number of skeptic LLM verifiers (1-3, odd recommended) that independently try to refute the result after the task succeeds. If a majority refutes, the task fails and retries with the refutation as feedback. Use for high-stakes tasks where 'verify' alone cannot check quality.",
		},
	}
	if hasModelList {
		props["model"] = map[string]any{"type": "string", "description": "Model ID from Available Models to use for this task. Select the model whose strengths best match this task. If empty, the default team model will be used."}
		props["escalate"] = map[string]any{"type": "boolean", "description": "If true, each retry after a failure re-runs this task on the next stronger model in Available Models (ordered weakest→strongest). Start cheap tasks on a fast model with escalate:true so only failures pay for a stronger model. Not applicable to agents with extra-models."}
	}
	return props
}

func (c *Coordinator) RunAgentsTool() fantasy.AgentTool {
	return &runAgentsTool{coordinator: c}
}
