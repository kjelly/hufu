package team

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	"github.com/anomalyco/hufu/internal/utils"
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
	// Verify is an optional shell command that objectively checks the task's
	// deliverable (e.g. "test -f report.pdf", "go build ./..."). It runs after
	// the agent reports success but before the task is marked done; a non-zero
	// exit makes the task fail and triggers a retry. This guards against agents
	// that claim completion without producing the expected artifact.
	Verify string `json:"verify,omitempty"`
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
	UserPrompt         string
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

type Coordinator struct {
	mu                    sync.RWMutex
	session               *TeamSession
	providerManager       *agent.ProviderManager
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
	current atomic.Pointer[currentSnapshot]
	currentStageStart   time.Time
	currentStageStartMu sync.RWMutex
	auditLogger           *audit.AuditLogger
	sshSessionMgr         *tools.SSHSessionManager
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

	// Unattended / budget controls for no-human-watching operation.
	unattended          bool
	maxWallClock        time.Duration // 0 = unlimited
	tokenBudget         int64         // 0 = unlimited; cumulative LLM tokens
	tokensUsed          atomic.Int64
	acceptanceCmd       string // optional shell command run at finish
	rollbackCmd         string // optional shell command run on acceptance failure
	selfHealingAttempts int
	budgetTripped       atomic.Bool
}


func (c *Coordinator) getSnapshotField(getter func(*currentSnapshot) string) string {
	s := c.current.Load()
	if s == nil {
		return ""
	}
	return getter(s)
}

func (c *Coordinator) updateSnapshot(updater func(*currentSnapshot)) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	updater(newS)
	c.current.Store(newS)
}

// currentSnapshot is an atomic snapshot of the coordinator's active state.
// Replaces 8 separate Sync.RWMutex-guarded fields with a single atomic.Pointer
// for lock-free, contention-free reads by the SIGINT handler and status reporter.
type currentSnapshot struct {
	Agent  string
	Task   string
	TodoID string
	Stage  string // "model", "tool", "wrapping_up", "idle"
	Step   int
	Tool   string
	Model  string
	// stageStart is NOT in the snapshot; it is only written/read within the
	// SetCurrentStage method which still uses its own lightweight mutex.
}


// SetUnattended enables unattended (no-human) mode: ask_user returns safe
// defaults, and only explicitly-allowed tools may run.
func (c *Coordinator) SetUnattended(v bool) { c.unattended = v }

// IsUnattended reports whether the coordinator is in unattended mode.
func (c *Coordinator) IsUnattended() bool { return c.unattended }

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

// TokensUsed returns the cumulative LLM token count observed so far.
func (c *Coordinator) TokensUsed() int64 { return c.tokensUsed.Load() }

// addStepTokens accumulates token usage from a set of agent steps.
func (c *Coordinator) addStepTokens(steps []fantasy.StepResult) {
	var total int64
	for _, s := range steps {
		total += s.Usage.TotalTokens
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
	if !t.counting {
		return duration, 0, toolTime
	}
	modelTime = duration - toolTime
	if modelTime < 0 {
		modelTime = 0
	}
	return
}

func NewCoordinator(session *TeamSession, defaultProviderURL, defaultProviderAPIKey string, mcpManager *mcp.MCPToolManager, memoryStore *memory.MemoryStore, modelList []config.ModelEntry, sidecarModel string, guardModel string, maxConcurrent int, verbose bool, think bool, direnv bool, allowedPaths []string, pathConsent *tools.PathConsent, hookRegistry *hooks.HookRegistry, rbashMode bool, restrictedPath string, noNet bool, forceMCP bool, forcedSkillNames []string, planMode bool, autoSkillsMode bool) (*Coordinator, error) {
	projectDir, _ := os.Getwd()
	coreTools := agent.BuildAllAgentTools(projectDir, tools.WithAllowedPaths(allowedPaths), tools.WithPathConsent(pathConsent), tools.WithWorkspaceName(filepath.Base(session.Workspace)), tools.WithHooks(hookRegistry), tools.WithRestrictedBash(rbashMode), tools.WithRestrictedPath(restrictedPath), tools.WithNetworkBlock(noNet), tools.WithForceMCP(forceMCP), tools.WithDirenv(direnv))
	pm, err := agent.NewProviderManager(defaultProviderURL, defaultProviderAPIKey, session.Config.Providers)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider manager: %w", err)
	}
	c := &Coordinator{
		providerManager: pm,
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
		approvedOutputs: make(map[string]string),
		approvedErrors:  make(map[string]error),
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
		forceMCP:        forceMCP,
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

	if c.session != nil && c.session.Workspace != "" {
		if err := skill.RecordUsage(c.session.Workspace, name, agentName); err != nil {
			log.Printf("[WARN] failed to persist skill usage: %v", err)
		}
	}
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
		filepath.Join(c.session.Dir, "skills"),
		filepath.Join(c.session.Dir, ".agents", "skills"), // Fallback for old path
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
// When asDraft is true, the file is written under skills/drafts/ instead of skills/.
func (c *Coordinator) saveAndReloadSkill(name, description, content string, asDraft bool) (string, error) {
	slug := strings.Trim(skillSlugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		return "", fmt.Errorf("invalid skill name %q", name)
	}

	skillDir := filepath.Join(c.session.Dir, "skills", slug)
	if asDraft {
		skillDir = filepath.Join(c.session.Dir, "skills", "drafts", slug)
	}
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

	var fileContent string
	if asDraft {
		now := time.Now().UTC().Format(time.RFC3339)
		fileContent = fmt.Sprintf("---\nname: %s\n%s\ncreated_at: %s\nlast_modified: %s\n---\n\n%s\n",
			name, descYAML, now, now, strings.TrimSpace(content))
	} else {
		fileContent = fmt.Sprintf("---\nname: %s\n%s\n---\n\n%s\n", name, descYAML, strings.TrimSpace(content))
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(fileContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write skill file: %w", err)
	}

	// Hot-reload: rediscover and re-filter skills from all directories.
	allSkills := skill.DiscoverSkills(c.skillDirs(), false)
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

// appendSkillContext appends skill prefix and auto-matched skill suggestions to
// prompt. todoID may be empty; SetInjectedSkills is skipped when it is.
func (c *Coordinator) appendSkillContext(prompt string, agentDef *agent.AgentDef, agentName, goal, todoID string) string {
	if prefix := c.buildSkillPromptPrefix(agentDef); prefix != "" {
		prompt += "\n\n" + prefix
	}
	if suggestion, names := c.buildSuggestedSkillsText(agentDef, agentName, goal); suggestion != "" {
		if todoID != "" {
			c.taskTracker.TodoList().SetInjectedSkills(todoID, names)
		}
		prompt += "\n\n" + suggestion
	}
	return prompt
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
const maxTaskSTMContextChars = 1500

// maxWorkerAuxContextChars caps the combined size of the auxiliary context
// blocks (prior-agent STM, concurrent tasks, LTM background) appended to a
// worker prompt, so the total injected context cannot grow unbounded and
// overflow a small model's window.
const maxWorkerAuxContextChars = 5000

// assembleContextWithinBudget joins context blocks (already in priority order)
// separated by blank lines, including each only while the running total stays
// within budget. Lower-priority trailing blocks are dropped entirely rather
// than truncated mid-way, preserving each block's markdown structure. The
// returned string is prefixed with "\n\n" when non-empty so it can be appended
// directly to a prompt.
func assembleContextWithinBudget(parts []string, budget int) string {
	if budget <= 0 {
		return ""
	}
	var b strings.Builder
	total := 0
	for _, p := range parts {
		if p == "" {
			continue
		}
		n := len([]rune(p))
		if total+n > budget {
			continue
		}
		total += n
		b.WriteString("\n\n")
		b.WriteString(p)
	}
	return b.String()
}


// This function has been moved to ltm.go as ClassifyLTMEntry

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

func (c *Coordinator) SkillDetector() *skill.SkillPatternDetector {
	return c.skillDetector
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
	s, err := sidecar.NewSidecar(ctx, c.providerManager.GetProvider(c.sidecarModel), c.sidecarModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ sidecar model %q unavailable: %v (auto-skills and skill matching disabled — set --sidecar-model to a working model to enable)\n", c.sidecarModel, err)
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
	s, err := sidecar.NewSidecar(ctx, c.providerManager.GetProvider(c.guardModel), c.guardModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ guard model %q unavailable: %v (guard review disabled — tool calls will be denied until a working model is configured)\n", c.guardModel, err)
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

// SetCurrentStage records the high-level stage the coordinator is in:
// "model" while an LLM call is in flight, "tool" while a tool is being
// executed, "wrapping_up" during wrap-up, "idle" otherwise.
func (c *Coordinator) SetCurrentStage(stage string) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	newS.Stage = stage
	c.current.Store(newS)
	if stage == "idle" {
		c.currentStageStart = time.Time{}
		return
	}
	c.currentStageStartMu.Lock()
	c.currentStageStart = time.Now()
	c.currentStageStartMu.Unlock()
}

// SetCurrentStep records the current step number in the fantasy agent's step loop.
func (c *Coordinator) SetCurrentStep(n int) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	newS.Step = n
	c.current.Store(newS)
}

// SetCurrentTool records the tool name currently being executed.
func (c *Coordinator) SetCurrentTool(name string) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	newS.Tool = name
	c.current.Store(newS)
}

// SetCurrentModel records the model ID currently being used for an LLM call.
func (c *Coordinator) SetCurrentModel(modelID string) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	newS.Model = modelID
	c.current.Store(newS)
}

// GetCurrentStatus returns a human-readable snapshot of the coordinator's
// current state. Designed for SIGINT/watchdog diagnostics.
func (c *Coordinator) GetCurrentStatus() string {
	s := c.current.Load()
	c.currentStageStartMu.RLock()
	stageStart := c.currentStageStart
	c.currentStageStartMu.RUnlock()

	if s == nil || s.Stage == "" || s.Stage == "idle" {
		return "idle"
	}

	elapsed := ""
	if !stageStart.IsZero() {
		elapsed = fmt.Sprintf(" (%.0fs elapsed)", time.Since(stageStart).Seconds())
	}

	parts := []string{s.Stage}
	if s.Agent != "" {
		parts = append(parts, "agent="+s.Agent)
	}
	if s.Model != "" && (s.Stage == "model" || s.Stage == "wrapping_up") {
		parts = append(parts, "model="+s.Model)
	}
	if s.Tool != "" && s.Stage == "tool" {
		parts = append(parts, "tool="+s.Tool)
	}
	if s.Step > 0 {
		parts = append(parts, fmt.Sprintf("step=%d", s.Step))
	}
	if s.Task != "" {
		parts = append(parts, "task="+utils.TruncateString(s.Task, 60))
	}
	return strings.Join(parts, " ") + elapsed
}


func (c *Coordinator) GetCurrentAgentInfo() tools.AgentInfo {
	s := c.current.Load()
	if s == nil {
		return tools.AgentInfo{}
	}
	return tools.AgentInfo{Name: s.Agent, Task: s.Task}
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
		"verify": map[string]any{
			"type":        "string",
			"description": "Optional shell command that objectively verifies the task's deliverable exists/works (e.g. 'test -f workspace/report.md', 'go build ./...'). It runs after the agent reports success; a non-zero exit fails the task and triggers a retry. Use it for tasks with a checkable artifact so the agent cannot falsely claim completion.",
		},
	}
	if hasModelList {
		props["model"] = map[string]any{"type": "string", "description": "Model ID from Available Models to use for this task. Select the model whose strengths best match this task. If empty, the default team model will be used."}
	}
	return props
}

func (c *Coordinator) RunAgentsTool() fantasy.AgentTool {
	return &runAgentsTool{coordinator: c}
}


func (t *rejectPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *rejectPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

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
	var revisedTask TaskDef
	if entry.Task.Agent != "" {
		revisedTask = cloneTaskDef(entry.Task)
	} else {
		revisedTask = TaskDef{
			Agent: entry.Agent,
		}
	}
	revisedTask.Goal = fmt.Sprintf("%s\n\n## Plan Rejected\nYour previous plan was rejected for the following reason:\n\n%s\n\nPlease re-plan and submit a new plan.", entry.Goal, args.Reason)
	revisedTask.PlanFirst = true
	revisedTask.PlanID = ""
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.report(t.coordinator.newEvent("step").withMessage(fmt.Sprintf("plan %s rejected: %s", args.TodoID, args.Reason)))

	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{revisedTask})
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
// result for agentKey (lowercase agent name).
//
// Lookup order:
//  1. Exact match in current generation (fast path, same workspace state)
//  2. Exact match across all generations (fast path, workspace state may differ but goal is identical)
//  3. Sidecar semantic similarity in current generation only (slower, requires LLM call)
//
// Returns (cachedOutput, true) on a hit.
func (c *Coordinator) lookupTaskCache(ctx context.Context, agentKey, newTask string) (string, bool) {
	gen := c.cacheGeneration.Load()
	normalizedNew := strings.ToLower(strings.Join(strings.Fields(newTask), " "))

	c.taskResultCacheMu.RLock()
	all := c.taskResultCache[agentKey]
	c.taskResultCacheMu.RUnlock()

	if len(all) == 0 {
		return "", false
	}

	// Step 1: exact match in current generation
	for _, e := range all {
		if e.generation == gen {
			norm := strings.ToLower(strings.Join(strings.Fields(e.taskDesc), " "))
			if norm == normalizedNew {
				return e.output, true
			}
		}
	}

	// Step 2: exact match across all generations
	for _, e := range all {
		norm := strings.ToLower(strings.Join(strings.Fields(e.taskDesc), " "))
		if norm == normalizedNew {
			return e.output, true
		}
	}

	// Step 3: sidecar semantic similarity in current generation only
	s := c.Sidecar()
	if s == nil {
		return "", false
	}

	var currentGenEntries []cachedTaskEntry
	for _, e := range all {
		if e.generation == gen {
			currentGenEntries = append(currentGenEntries, e)
		}
	}
	if len(currentGenEntries) == 0 {
		return "", false
	}

	pastDescs := make([]string, len(currentGenEntries))
	for i, e := range currentGenEntries {
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
	if idx >= 0 && idx < len(currentGenEntries) {
		return currentGenEntries[idx].output, true
	}
	return "", false
}

// lookupTaskCacheAllGenerations checks for semantically similar tasks across ALL
// generations (not just current). This is used for duplicate detection before
// delegating tasks.
//
// Lookup order:
//  1. Exact match across all generations (fast path)
//  2. Sidecar semantic similarity across all generations (slower, requires LLM call)
//
// Returns (cachedOutput, cachedTaskDesc, true) on a hit.
func (c *Coordinator) lookupTaskCacheAllGenerations(ctx context.Context, agentKey, newTask string) (string, string, bool) {
	c.taskResultCacheMu.RLock()
	all := c.taskResultCache[agentKey]
	c.taskResultCacheMu.RUnlock()

	if len(all) == 0 {
		return "", "", false
	}

	normalizedNew := strings.ToLower(strings.Join(strings.Fields(newTask), " "))

	// Step 1: exact match across all generations
	for _, e := range all {
		norm := strings.ToLower(strings.Join(strings.Fields(e.taskDesc), " "))
		if norm == normalizedNew {
			return e.output, e.taskDesc, true
		}
	}

	// Step 2: sidecar semantic similarity across all generations
	s := c.Sidecar()
	if s == nil {
		return "", "", false
	}

	// Limit to last 100 entries to avoid overwhelming the sidecar
	startIdx := 0
	if len(all) > 100 {
		startIdx = len(all) - 100
	}
	recentEntries := all[startIdx:]

	pastDescs := make([]string, len(recentEntries))
	for i, e := range recentEntries {
		pastDescs[i] = e.taskDesc
	}

	sidecarCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if c.think {
		c.emitThinkSidecar("SimilarTask", fmt.Sprintf("checking semantic similarity across all history: %.50s", newTask))
	}
	idx, err := s.SimilarTask(sidecarCtx, newTask, pastDescs)
	if err != nil {
		return "", "", false
	}
	if idx >= 0 && idx < len(recentEntries) {
		return recentEntries[idx].output, recentEntries[idx].taskDesc, true
	}
	return "", "", false
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

// detectTaskCycle returns true if the DependsOn indices form a cycle.
func detectTaskCycle(tasks []TaskDef) bool {
	n := len(tasks)
	state := make([]int, n) // 0=unvisited, 1=visiting, 2=done
	var dfs func(i int) bool
	dfs = func(i int) bool {
		if state[i] == 1 {
			return true
		}
		if state[i] == 2 {
			return false
		}
		state[i] = 1
		for _, dep := range tasks[i].DependsOn {
			// Check for self-loop (task depends on itself)
			if dep == i {
				return true
			}
			if dep >= 0 && dep < n && dfs(dep) {
				return true
			}
		}
		state[i] = 2
		return false
	}
	for i := range tasks {
		if state[i] == 0 && dfs(i) {
			return true
		}
	}
	return false
}

func (c *Coordinator) checkDuplicateTasks(ctx context.Context, tasks []TaskDef) ([]string, map[int]bool) {
	var warnings []string
	duplicates := make(map[int]bool)

	// First pass: build local counts for this batch to handle duplicates within the batch
	localCounts := make(map[string]int)
	for _, t := range tasks {
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := strings.ToLower(t.Agent) + ":" + normalizeTaskDesc(desc)
		localCounts[key]++
	}

	c.delegatedTasksMu.Lock()
	// Second pass: check exact duplicates and increment global counts
	// Track how many we've seen in this batch so far (for in-batch dedup: first instance proceeds, rest are duplicates)
	batchSeen := make(map[string]int)
	for i, t := range tasks {
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := strings.ToLower(t.Agent) + ":" + normalizeTaskDesc(desc)
		batchSeen[key]++

		// Check if this exact task was already delegated in a previous round
		if c.delegatedTasks[key] > 0 {
			warnings = append(warnings, fmt.Sprintf("EXACT DUPLICATE: %s (agent=%s, count=%d)", truncateTaskDesc(desc), t.Agent, c.delegatedTasks[key]+batchSeen[key]))
			duplicates[i] = true
			continue
		}

		// Check for duplicates within the current batch (first instance proceeds, rest are duplicates)
		if batchSeen[key] > 1 {
			warnings = append(warnings, fmt.Sprintf("EXACT DUPLICATE (in batch): %s (agent=%s, count=%d)", truncateTaskDesc(desc), t.Agent, batchSeen[key]))
			duplicates[i] = true
			continue
		}
	}

	// Increment global counts for all non-duplicate tasks
	for i, t := range tasks {
		if duplicates[i] {
			continue
		}
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := strings.ToLower(t.Agent) + ":" + normalizeTaskDesc(desc)
		c.delegatedTasks[key]++
	}
	c.delegatedTasksMu.Unlock()

	// Third pass: semantic duplicate check (outside the lock to avoid holding mutex during slow I/O)
	for i, t := range tasks {
		if duplicates[i] {
			continue
		}
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		agentKey := strings.ToLower(t.Agent)
		dupCtx, dupCancel := context.WithTimeout(ctx, 5*time.Second)
		cachedOutput, cachedDesc, cacheOK := c.lookupTaskCacheAllGenerations(dupCtx, agentKey, desc)
		dupCancel()
		if cacheOK {
			warnings = append(warnings, fmt.Sprintf("SEMANTIC DUPLICATE: %s (similar to completed task: %q)", truncateTaskDesc(desc), truncateTaskDesc(cachedDesc)))
			duplicates[i] = true
			log.Printf("[WARN] duplicate task detected: agent=%q, task=%q, similar to=%q", t.Agent, desc, cachedDesc)
		} else {
			_ = cachedOutput
		}
	}
	return warnings, duplicates
}

func formatTaskResults(results []agentTaskResult, totalTasks int, duplicateWarnings []string) (string, error) {
	var b strings.Builder
	successCount := 0
	errorCount := 0
	planCount := 0
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		if r.err != nil {
			errorCount++
			b.WriteString(fmt.Sprintf("## Agent: %s\n**Status**: ERROR\n**Error**: %s", r.agentName, r.err))
		} else if r.planText != "" {
			// Plan submitted - don't count as success, just informational
			planCount++
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
	// Error only if all tasks failed AND no plans were submitted
	if successCount == 0 && errorCount > 0 && planCount == 0 {
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
	histDir := filepath.Join(workspace, stmLogsDir)
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
	b.WriteString("6. **Parallel vs sequential**: All tasks in one agent call run in parallel by default. Use `depends_on` to express dependencies within the same call — a task with `depends_on: [0]` waits for the task at index 0 to finish before starting. Prefer one agent call with `depends_on` over multiple sequential calls when possible.\n")
	b.WriteString("   - ✅ One call: [{agent:\"researcher\",goal:\"find X\"},{agent:\"coder\",goal:\"implement X\",depends_on:[0]}]\n")
	b.WriteString("   - ✅ Parallel (no dependency): [{agent:\"writer\",goal:\"write A\"},{agent:\"writer\",goal:\"write B\"}]\n")
	b.WriteString("   - ⚠️  Separate calls only when coordinator must process results before deciding next steps\n")
	b.WriteString("7. When delegating to a worker that needs skill knowledge, include ALL relevant skill summaries (name, file path) in the task description so the worker can call `load_skill` if needed\n")
	b.WriteString("8. **Trust worker expertise** — Workers have access to the full project context (AGENTS.md, tech stack, conventions, directory structure). They will explore the codebase, identify relevant files, and determine the best implementation approach. Do NOT pre-specify file paths, function names, or implementation steps unless they are non-obvious constraints.\n")
	b.WriteString("9. **Evaluate** results after each agent call — decide if more work is needed or if you can provide a final answer\n")
	b.WriteString("10. **Record** key findings and decisions with `stm_write` (append mode) after each meaningful agent result — use the matching section:\n")
	b.WriteString("    - `# 發現` — new facts discovered (API endpoints, file locations, test results, etc.)\n")
	b.WriteString("    - `# 決策` — design or implementation choices made\n")
	b.WriteString("    - `# 錯誤與修復` — errors encountered and how they were resolved\n")
	b.WriteString("    - `# 待解決` — open questions or blockers for later agents\n")
	b.WriteString("    Skip this step only if the agent result contains no new knowledge (e.g. pure \"done\" confirmations).\n")
	b.WriteString("11. **Synthesize** results into a coherent answer for the user\n")
	b.WriteString("12. When satisfied, call the finish tool with your final response\n\n")

	b.WriteString("## Deduplication Rules\n\n")
	b.WriteString("CRITICAL: BEFORE delegating ANY task, you MUST check the Task Status section above.\n\n")
	b.WriteString("- ⚠️ If a task appears in **COMPLETED**, you MUST NOT re-delegate it. Reference or synthesize the existing result.\n")
	b.WriteString("- ⚠️ If a SEMANTICALLY SIMILAR task (same goal, different wording) appears in **COMPLETED**, compare the actual intent - do NOT delegate duplicates with rephrased descriptions.\n")
	b.WriteString("- ⏸️ If a task appears in **PAUSED**, it is waiting for a sub-agent to complete. Wait for it to resume rather than delegating a duplicate.\n")
	b.WriteString("- If a task appears in **SKIPPED**, it was flagged as a duplicate by the system. Do NOT re-delegate it.\n")
	b.WriteString("- If a task appears in **IN PROGRESS**, wait for it to complete rather than delegating a duplicate.\n")
	b.WriteString("- ❌ DUPLICATE DETECTION: The system will return ERROR if you delegate a duplicate task. You must reference existing results instead.\n\n")

	b.WriteString("## Task Status\n\n")
	b.WriteString(c.buildTaskStatusContext())

	b.WriteString("## Available Agents\n\n")
	fmt.Fprintf(&b, "CRITICAL: You MUST use EXACTLY these names in the 'agent' field of the agent tool. Do NOT invent new names or use generic roles. Using an unknown name will result in an IMMEDIATE ERROR.\n\n")
	fmt.Fprintf(&b, "Valid names: %s\n\n", strings.Join(workerNames, ", "))
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
		if caps := ExtractCapabilitiesFromSystem(def.System); caps != "" {
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
	b.WriteString("Append to a specific section of long-term memory (ltm.md), a persistent file shared across sessions for this team.\n")
	b.WriteString("Use ltm_update to save important cross-session knowledge: project conventions, discovered APIs, recurring patterns, architecture decisions, and lessons learned.\n")
	b.WriteString("Available sections: `# 專案慣例`, `# 架構決策`, `# 常見模式`, `# 已知問題與解法`, `# 關鍵檔案`, `# 工具與指令`\n")
	b.WriteString("```json\n{\"content\": \"API endpoint /api/v2/users requires JWT in Authorization header\", \"section\": \"# 專案慣例\"}\n```\n\n")

	wsPath := c.session.Workspace
	sharedPath := filepath.Join(wsPath, sharedDir)
	b.WriteString("\n## Environment & Rules\n\n")
	fmt.Fprintf(&b, "- CWD: %s | Workspace: %s | Shared: %s | Time: %s\n", c.projectDir, wsPath, sharedPath, c.sessionTime.Format(time.RFC3339))
	fmt.Fprintf(&b, "- ALL intermediate files go to workspace: %s. Use %s for inter-agent sharing. NEVER write outside workspace.\n", wsPath, sharedPath)
	b.WriteString("- stm_write after each meaningful agent result (# 發現 / # 決策 / # 錯誤與修復 / # 待解決) AND before finish. ltm_update for cross-session knowledge.\n\n")

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

func (c *Coordinator) buildConcurrentTasksContext(excludeID string) string {
	items := c.taskTracker.TodoList().Items()
	var running []string
	for _, item := range items {
		if item.ID != excludeID && item.Status == TaskInProgress {
			running = append(running, fmt.Sprintf("- %s: %s", item.Agent, item.Desc))
		}
	}
	if len(running) == 0 {
		return ""
	}
	return "## Concurrent Tasks\n\nThe following agents are running in parallel with you. Avoid overlapping with their work:\n\n" + strings.Join(running, "\n")
}

// verifyTaskDeliverable runs the task's optional verify command and returns a
// non-nil error if the command exits non-zero (or cannot be run). This provides
// an objective, non-LLM check that the deliverable actually exists/works before
// a task is accepted as done. The command runs in the project directory using
// the team's (or agent's) configured shell, falling back to "sh".
func (c *Coordinator) verifyTaskDeliverable(parentCtx context.Context, agentDef *agent.AgentDef, command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}

	shell := "sh"
	if agentDef != nil && agentDef.Shell != "" {
		shell = agentDef.Shell
	} else if c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}

	timeout := time.Duration(c.session.Config.Timeout) * time.Second
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, "-c", command)
	if c.projectDir != "" {
		cmd.Dir = c.projectDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + utils.TruncateString(detail, 500)
		}
		return fmt.Errorf("%v%s", err, detail)
	}
	return nil
}

func (c *Coordinator) reflectOnFailure(ctx context.Context, agentName, goal, lastErr string) string {
	s := c.Sidecar()
	if s != nil {
		// Use a shorter timeout for reflection to avoid holding up retries
		reflectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		prompt := fmt.Sprintf("Agent %q failed to achieve goal: %q\nError: %s\n\nAnalyze the error and provide a concise hint (max 100 words) for the next attempt. Focus on what to change or avoid.", agentName, goal, lastErr)
		if reflection, err := s.Execute(reflectCtx, prompt); err == nil && strings.TrimSpace(reflection) != "" {
			return "\n\n## Reflection on Previous Failure\n\n" + reflection
		}
	}
	// Fallback: deterministic, LLM-free hint derived from the error so retries
	// are never blind even when no sidecar is configured or it is unavailable.
	if hint := localFailureHint(lastErr); hint != "" {
		return "\n\n## Reflection on Previous Failure\n\n" + hint
	}
	return ""
}

// sameFailure reports whether two error messages represent the same underlying
// failure, ignoring volatile prefixes like "attempt N failed". It is used to
// detect an agent stuck repeating an identical failing action across retries.
func sameFailure(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		// Drop a leading "attempt N failed:" wrapper if present.
		if i := strings.Index(s, "failed:"); i >= 0 && strings.HasPrefix(s, "attempt ") {
			s = strings.TrimSpace(s[i+len("failed:"):])
		}
		return s
	}
	na, nb := norm(a), norm(b)
	return na != "" && na == nb
}

// localFailureHint classifies a failure message and returns an actionable hint
// without calling any model. It pattern-matches common error shapes (timeout,
// missing file/command, permission, verification, step exhaustion).
func localFailureHint(lastErr string) string {
	e := strings.ToLower(lastErr)
	switch {
	case strings.Contains(e, "deliverable verification failed"):
		return "Your previous attempt reported success but the verification check failed — the expected deliverable was missing or invalid. Actually produce the artifact (create/modify the file, make it pass the check) before calling finish; do not claim completion prematurely."
	case strings.Contains(e, "deadline exceeded") || strings.Contains(e, "timed out") || strings.Contains(e, "context deadline"):
		return "The previous attempt timed out. Work in smaller steps, avoid long-running or interactive commands, and prioritize the core of the goal first."
	case strings.Contains(e, "no such file") || strings.Contains(e, "not found") || strings.Contains(e, "enoent"):
		return "A file or command was not found last time. Verify the path exists with ls/glob before using it, and use absolute paths under the workspace."
	case strings.Contains(e, "permission denied") || strings.Contains(e, "not permitted") || strings.Contains(e, "guard rule"):
		return "The previous attempt was blocked by a permission or guard rule. Use only the tools and paths you are allowed; do not retry the exact blocked action — find a permitted alternative."
	case strings.Contains(e, "step") && (strings.Contains(e, "limit") || strings.Contains(e, "count") || strings.Contains(e, "max")):
		return "You ran out of steps last time. Be more direct: skip exploratory actions and go straight to the actions that satisfy the goal."
	case strings.Contains(e, "duplicate"):
		return "This work overlaps with an already-completed task. Reuse the existing result instead of redoing it, or address the part that is genuinely missing."
	default:
		return "The previous attempt failed with: " + utils.TruncateString(strings.TrimSpace(lastErr), 300) + ". Change your approach rather than repeating the same actions."
	}
}

func (c *Coordinator) autoWriteSTMASync(agentName, taskDesc, output, errMsg string, success bool) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] autoWriteSTMASync recovered: %v", r)
			}
		}()
		c.autoWriteSTM(agentName, taskDesc, output, errMsg, success)
	}()
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
		c.conversationHistory = trimHistoryPreservingHead(c.conversationHistory, maxConversationHistory)
		return
	}
	compacted := c.compactMessages(ctx, c.conversationHistory[:compactCount])
	c.conversationHistory = append(compacted, c.conversationHistory[compactCount:]...)
	if len(c.conversationHistory) > maxConversationHistory {
		// Compaction did not shrink enough (e.g. sidecar unavailable so
		// compactMessages returned the input unchanged). Keep the first few
		// messages — which carry the original goal and instructions — plus the
		// most recent ones, instead of dropping the head entirely.
		c.conversationHistory = trimHistoryPreservingHead(c.conversationHistory, maxConversationHistory)
	}
}

// conversationHeadKeep is the number of earliest messages preserved when the
// conversation history is hard-trimmed. These usually contain the original goal
// and setup that later turns depend on.
const conversationHeadKeep = 4

// trimHistoryPreservingHead reduces msgs to at most max entries by keeping the
// first conversationHeadKeep messages and the most recent remainder. This avoids
// the "amnesia" failure where the original goal is dropped and the coordinator
// re-delegates already-completed work.
func trimHistoryPreservingHead(msgs []fantasy.Message, max int) []fantasy.Message {
	if max <= 0 {
		return nil
	}
	if len(msgs) <= max {
		return msgs
	}
	headKeep := conversationHeadKeep
	if headKeep >= max {
		headKeep = max / 4
	}
	tailKeep := max - headKeep
	trimmed := make([]fantasy.Message, 0, max)
	trimmed = append(trimmed, msgs[:headKeep]...)
	trimmed = append(trimmed, msgs[len(msgs)-tailKeep:]...)
	return trimmed
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
	if sd != nil {
		if len(sd.Tasks) > 0 {
			c.taskTracker.TodoList().Restore(sd.Tasks)

			c.taskResultCacheMu.Lock()
			gen := c.cacheGeneration.Load()
			for _, t := range sd.Tasks {
				if t.Status == TaskDone && t.Output != "" {
					agentKey := strings.ToLower(t.Agent)
					c.taskResultCache[agentKey] = append(c.taskResultCache[agentKey], cachedTaskEntry{
						taskDesc:   t.Desc,
						output:     t.Output,
						generation: gen,
					})
					if len(c.taskResultCache[agentKey]) > maxTaskCacheEntries {
						c.taskResultCache[agentKey] = c.taskResultCache[agentKey][1:]
					}
				}
			}
			c.taskResultCacheMu.Unlock()
		}
		c.taskTracker.TodoList().onChange = c.saveCheckpoint
	}
}

func (c *Coordinator) saveCheckpoint() {
	if c.sessionData == nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	c.sessionData.Tasks = c.taskTracker.TodoList().Items()
	_ = SaveSession(c.session.Workspace, c.sessionData)
}

// isInterruptedStatus reports whether a restored task status indicates the task
// was left incomplete by an interrupted (crashed/killed) run and must be
// re-driven on resume. Terminal states (done/skipped) and definitively-failed
// tasks (error, which already exhausted their retries) are left untouched.
func isInterruptedStatus(s TaskStatus) bool {
	switch s {
	case TaskInProgress, TaskPaused, TaskPlanned, TaskPending:
		return true
	default:
		return false
	}
}

// todoIDLess orders numeric todo IDs ("1","2",...) numerically, falling back to
// string comparison for non-numeric IDs.
func todoIDLess(a, b string) bool {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	return a < b
}

// resetInterruptedTasks finds tasks left in-flight by a previous run, resets
// each to pending so it can be re-driven on its original todo ID, and returns
// them in dependency-safe (ascending ID) order. Split from execution so the
// selection/reset logic can be unit-tested without an LLM provider.
func (c *Coordinator) resetInterruptedTasks() []*TodoItem {
	items := c.taskTracker.TodoList().Items()
	var interrupted []*TodoItem
	for _, it := range items {
		if isInterruptedStatus(it.Status) {
			interrupted = append(interrupted, it)
		}
	}
	sort.SliceStable(interrupted, func(i, j int) bool {
		return todoIDLess(interrupted[i].ID, interrupted[j].ID)
	})
	for _, it := range interrupted {
		c.taskTracker.TodoList().UpdateStatus(it.ID, TaskPending, "resumed after interruption")
	}
	return interrupted
}

// ResumeInterruptedTasks re-drives the worker tasks that a previous run left
// in-flight (restored from the session checkpoint). Completed work is reused via
// the result cache prepopulated in SetSessionData; only interrupted tasks are
// re-executed, on their original todo IDs and in ascending-ID order so that
// dependencies (which carry lower IDs) run first. It is a no-op on a fresh run
// because the todo list is empty. Returns the number of tasks re-driven and the
// first error encountered, if any.
func (c *Coordinator) ResumeInterruptedTasks(ctx context.Context) (int, error) {
	interrupted := c.resetInterruptedTasks()
	if len(interrupted) == 0 {
		return 0, nil
	}
	c.report(c.newEvent("step").withMessage(fmt.Sprintf("resuming %d interrupted task(s) from checkpoint", len(interrupted))))
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	var firstErr error
	count := 0
	for _, it := range interrupted {
		if ctx.Err() != nil {
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			break
		}
		task := TaskDef{Agent: it.Agent, Goal: it.Desc}
		if _, err := c.executeTask(ctx, task, it.ID); err != nil && firstErr == nil {
			firstErr = err
		}
		count++
	}
	return count, firstErr
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
	prevAgent := c.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
	prevTask := c.getSnapshotField(func(s *currentSnapshot) string { return s.Task })
	prevTodoID := c.getSnapshotField(func(s *currentSnapshot) string { return s.TodoID })
	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = resolvedName})
	c.updateSnapshot(func(s *currentSnapshot) { s.Task = task})
	c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = todoID})
	defer func() {
		c.updateSnapshot(func(s *currentSnapshot) { s.Agent = prevAgent})
		c.updateSnapshot(func(s *currentSnapshot) { s.Task = prevTask})
		c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = prevTodoID})
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
	taskCtx = tools.AskUserAwareDeadline(taskCtx)

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
	if c.forceMCP || agentDef.ForceMCP {
		taskCtx = context.WithValue(taskCtx, tools.AgentForceMCPKey, true)
	}
	if c.unattended {
		taskCtx = context.WithValue(taskCtx, tools.UnattendedKey, true)
	}

	timing := &taskTiming{}
	timing.reset()

	taskTS := time.Now().Format("20060102-150405")
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "working", task, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, resolvedName, "working", task)

	prompt := c.appendSkillContext(task, agentDef, resolvedName, task, todoID)

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
	c.taskTracker.TodoList().UpdateStatusAndOutput(todoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output)
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
	orchCtx = tools.AskUserAwareDeadline(orchCtx)
	orchCtx = context.WithValue(orchCtx, todoIDKey{}, CoordTodoID)
	if c.unattended {
		orchCtx = context.WithValue(orchCtx, tools.UnattendedKey, true)
	}

	orchModelID := c.resolveAgentModel(orchDef, "")
	orch, err := agent.CreateAgent(orchCtx, c.providerManager.GetProvider(orchModelID), agent.AgentConfig{
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
		case TaskInProgress, TaskPaused:
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
		case TaskInProgress, TaskPaused:
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

	// Start cleanup daemon for idle SSH sessions on first Run call (30 minute timeout, check every 5 minutes)
	if c.sshSessionMgr != nil {
		c.sshSessionMgr.StartCleanupDaemon(ctx, 5*time.Minute, 30*time.Minute)
	}

	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	EnsureWorkspaceDirs(c.session.Workspace)

	c.report(c.newEvent("step").withMessage("coordinator preparing"))

	// Crash-resume: before delegating new work, re-drive any worker tasks that a
	// previous interrupted run left in-flight (restored from the checkpoint).
	// No-op on a fresh run (empty todo list) or with --new (fresh session).
	if n, err := c.ResumeInterruptedTasks(ctx); err != nil {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("resume: re-drove %d interrupted task(s), with errors: %v", n, err)))
	} else if n > 0 {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("resume: re-drove %d interrupted task(s) from checkpoint", n)))
	}

	if c.sessionData != nil {
		c.sessionData.AddEntry("user", userPrompt)
	}

	systemPrompt := c.expandOrchestratorTemplate(orchDef.System)
	if systemPrompt == "" {
		systemPrompt = c.expandOrchestratorTemplate(defaultOrchestratorSystem)
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
	c.SetCurrentStage("idle")
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

func normalizeTaskDesc(task string) string {
	return strings.Join(strings.Fields(strings.ToLower(task)), " ")
}

// SkillMatchesPrompt returns true when the prompt contains any keyword
// extracted from the skill's name or description (case-insensitive).
// This is the LLM-free fallback used by DryRun().
func SkillMatchesPrompt(s *skill.SkillDef, prompt string) bool {
	p := strings.ToLower(prompt)
	if p == "" || s == nil {
		return false
	}
	for _, kw := range extractSkillKeywords(s) {
		if strings.Contains(p, kw) {
			return true
		}
	}
	return false
}

func (c *Coordinator) DryRun(ctx context.Context, userPrompt string) (*DryRunResult, error) {
	orchDef := c.GetOrchestratorDef()

	EnsureWorkspaceDirs(c.session.Workspace)

	result := &DryRunResult{
		UserPrompt: userPrompt,
	}
	if c.session != nil && c.session.Config.Name != "" {
		result.TeamName = c.session.Config.Name
	}
	if orchDef != nil {
		result.Model = c.resolveAgentModel(orchDef, "")
	}

	if c.session != nil {
		if c.session.Config.SidecarModel != "" {
			result.SidecarModel = c.session.Config.SidecarModel
		}
		if result.SidecarModel == "" {
			if resolved := config.LoadConfig(); resolved != nil {
				result.SidecarModel = resolved.ResolveSidecarModel(c.session.Config.SidecarModel)
			}
		}
	}

	// Skill matching: keyword-only, no LLM, no sidecar.
	allSkills := c.getSkills()
	matchedSet := map[string]bool{}
	for _, sk := range allSkills {
		if strings.Contains(strings.ToLower(userPrompt),strings.ToLower(sk.Name)) || SkillMatchesPrompt(sk, userPrompt) {
			matchedSet[strings.ToLower(sk.Name)] = true
		}
		result.AllSkills = append(result.AllSkills, DryRunSkillInfo{
			Name:        sk.Name,
			Description: sk.Description,
		})
	}
	for _, sk := range allSkills {
		if matchedSet[strings.ToLower(sk.Name)] {
			result.MatchedSkillNames = append(result.MatchedSkillNames, sk.Name)
		}
	}

	// Agent listing: derived from session config, not from an LLM.
	// Dedupe by def.Name so any agent registered under multiple map keys
	// (e.g. legacy aliases) still appears only once in the dry-run output.
	if c.session != nil {
		seenAgents := map[string]bool{}
		for _, def := range c.session.Agents {
			if def == nil {
				continue
			}
			if seenAgents[def.Name] {
				continue
			}
			seenAgents[def.Name] = true
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
	}

	c.report(c.newEvent("done").withAgent("coordinator").withMessage("dry-run complete (no LLM calls)").withTodoID(CoordTodoID))

	return result, nil
}

func cloneTaskDef(td TaskDef) TaskDef {
	clone := td
	if td.ContextFiles != nil {
		clone.ContextFiles = make([]string, len(td.ContextFiles))
		copy(clone.ContextFiles, td.ContextFiles)
	}
	if td.DependsOn != nil {
		clone.DependsOn = make([]int, len(td.DependsOn))
		copy(clone.DependsOn, td.DependsOn)
	}
	return clone
}
