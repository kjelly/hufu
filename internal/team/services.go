package team

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/sidecar"
)

// EventJournal owns the durable runtime event boundary. Session and task
// projections must only advance after Append succeeds. Context is included so
// tests and future journal backends can respect cancellation without changing
// the coordinator contract.
type EventJournal interface {
	Append(context.Context, RunEvent) (RunEvent, error)
	ReadEvents(context.Context) ([]RunEvent, error)
	VerifyHashChain(context.Context) error
}

type eventStoreJournal struct{ store *EventStore }

func (j eventStoreJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return RunEvent{}, err
	}
	if j.store == nil {
		return RunEvent{}, fmt.Errorf("event journal is unavailable")
	}
	return j.store.AppendPersistedContext(ctx, event)
}

func (j eventStoreJournal) ReadEvents(ctx context.Context) ([]RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if j.store == nil {
		return nil, fmt.Errorf("event journal is unavailable")
	}
	return j.store.ReadEvents()
}

func (j eventStoreJournal) VerifyHashChain(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j.store == nil {
		return fmt.Errorf("event journal is unavailable")
	}
	return j.store.VerifyHashChain()
}

// Planner defines the interface for task planning, prompt segment parsing, and duplicate checking.
type Planner interface {
	ParsePromptSegments(rawPrompt string, registry *TeamRegistry, defaultTeam string) ([]PromptSegment, error)
	CheckDuplicate(ctx context.Context, tasks []TaskDef) ([]string, map[int]bool, map[int]*duplicateTodoMatch)
}

// SessionStore defines the interface for session loading, saving, checkpointing, and session data.
type SessionStore interface {
	SaveSession(dir string, sessionData *SessionData) error
	SaveSessionMD(workspace string, content string) error
	SaveCompactionRecord(workspace string, record CompactionRecord) error
	SessionData() *SessionData
	SetSessionData(sd *SessionData)
}

// PolicyEngine defines the interface for cache policy, capability caching, freshness checks, and recovery policy resolution.
type PolicyEngine interface {
	GetCachePolicy() CachePolicy
	SetCachePolicy(policy CachePolicy)
	GetExecutionProfile() ExecutionProfile
	SetExecutionProfile(profile ExecutionProfile)
	IsCacheFresh(entry cachedTaskEntry, identity CacheIdentity) bool
	ResolveRecoveryPolicy(def *agent.AgentDef, t TaskDef) (SideEffectClass, RecoveryPolicy, string)
}

// ContextCompiler defines the interface for token context budget, system prompt assembly, context breakdown, and context pipeline management.
type ContextCompiler interface {
	CalculateBudget(spec ModelContextSpec, systemTokens, toolsTokens int) ContextBudget
	ContextUsageReport() (ContextBudget, ContextUsageBreakdown, string, bool)

	// Context Pipeline Methods (HF-PR-102 / spec.md)
	BuildMemorySuffix(agentRole string) string
	BuildTaskSTMContext() string
	BuildLTMContext() string
	AutoQueryMemory(ctx context.Context, store *memory.MemoryStore, prompt string, compact memory.CompactFunc) (string, error)
	AssembleContextWithinBudget(parts []string, budget int) string
	AssembleContextItems(ctx context.Context, items []ContextItem, budget ContextBudget) (string, bool, error)
	CompactProjectContext(ctx context.Context, sidecarCompacter SidecarCompacter, messages []fantasy.Message, prevSummary *StructuredSummary, originalGoal string) (*StructuredSummary, error)
	FormatDependencyResults(dependencies []TaskResult) string
	CompileCoordinatorContext(ctx context.Context, input CoordinatorContextInput) (CompiledContext, error)
	CompileWorkerContext(ctx context.Context, input WorkerContextInput) (CompiledContext, error)
}

// AgentPool defines the interface for resolving agents and managing sidecar instances.
type AgentPool interface {
	ResolveAgentName(input string) (*agent.AgentDef, string, error)
	Sidecar() *sidecar.Sidecar
	GuardSidecar() *sidecar.Sidecar
	JudgeSidecar() *sidecar.Sidecar
}

// WorkflowEngine defines the interface for executing orchestrator runs and task batches.
type WorkflowEngine interface {
	Run(ctx context.Context, prompt string) (string, error)
	ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error)
}

// ResolvedWorkerTools is the one source for both model-visible tool names and
// the concrete runtime allowlist. Capabilities remain descriptive; they never
// grant a tool independently of Tools.
type ResolvedWorkerTools struct {
	Tools        []fantasy.AgentTool
	Names        []string
	Capabilities []string
}

// WorkerToolResolutionMode identifies the lifecycle surface being built. The
// mode is part of the request so task-bound protocol tools cannot be omitted or
// supplied by an untrusted caller.
type WorkerToolResolutionMode string

const (
	WorkerToolResolutionNormal       WorkerToolResolutionMode = "normal"
	WorkerToolResolutionInitialPlan  WorkerToolResolutionMode = "initial_plan"
	WorkerToolResolutionApprovedPlan WorkerToolResolutionMode = "approved_plan"
	WorkerToolResolutionResultRepair WorkerToolResolutionMode = "result_repair"
	WorkerToolResolutionResume       WorkerToolResolutionMode = "resume"
)

// WorkerToolResolutionRequest carries the trusted runtime identity and
// lifecycle mode needed to construct a task's final worker tool surface.
type WorkerToolResolutionRequest struct {
	Task   TaskDef
	TodoID string
	Mode   WorkerToolResolutionMode
}

type ToolResolver interface {
	ResolveTaskTools(context.Context, *agent.AgentDef, WorkerToolResolutionRequest) (ResolvedWorkerTools, error)
}

type ModelRuntime interface {
	ResolveTaskModel(*agent.AgentDef, TaskDef) (string, error)
	ProviderFor(string) (*agent.OpenAICompatibleProvider, error)
}

// RuntimeServices is the constructor-injected bundle for coordinator runtime
// seams. It is intentionally a small struct rather than a DI framework: the
// Coordinator still owns scheduling and policy, while deterministic tests can
// replace only the capability boundary they exercise.
type RuntimeServices struct {
	Planner             Planner
	SessionStore        SessionStore
	PolicyEngine        PolicyEngine
	ContextCompiler     ContextCompiler
	AgentPool           AgentPool
	WorkflowEngine      WorkflowEngine
	EventJournal        EventJournal
	ToolResolver        ToolResolver
	ModelRuntime        ModelRuntime
	SubagentRegistry    *SubagentRegistry
	ExperienceProcessor ExperienceProcessor
}

// Default sub-service implementations wrapping Coordinator

type defaultPlanner struct {
	c *Coordinator
}

func (p *defaultPlanner) ParsePromptSegments(rawPrompt string, registry *TeamRegistry, defaultTeam string) ([]PromptSegment, error) {
	return ParsePromptWithLazyAgents(rawPrompt, registry, defaultTeam)
}

func (p *defaultPlanner) CheckDuplicate(ctx context.Context, tasks []TaskDef) ([]string, map[int]bool, map[int]*duplicateTodoMatch) {
	return p.c.checkDuplicateTasks(ctx, tasks)
}

type defaultSessionStore struct {
	c *Coordinator
}

func (s *defaultSessionStore) SaveSession(dir string, sessionData *SessionData) error {
	return SaveSession(dir, sessionData)
}

func (s *defaultSessionStore) SaveSessionMD(workspace string, content string) error {
	return SaveSessionMD(workspace, content)
}

func (s *defaultSessionStore) SaveCompactionRecord(workspace string, record CompactionRecord) error {
	return SaveCompactionRecord(workspace, record)
}

func (s *defaultSessionStore) SessionData() *SessionData {
	return s.c.SessionData()
}

func (s *defaultSessionStore) SetSessionData(sd *SessionData) {
	s.c.SetSessionData(sd)
}

type defaultPolicyEngine struct {
	c *Coordinator
}

func (pe *defaultPolicyEngine) GetCachePolicy() CachePolicy {
	return pe.c.GetCachePolicy()
}

func (pe *defaultPolicyEngine) SetCachePolicy(policy CachePolicy) {
	pe.c.SetCachePolicy(policy)
}

func (pe *defaultPolicyEngine) GetExecutionProfile() ExecutionProfile {
	return pe.c.ExecutionProfile()
}

func (pe *defaultPolicyEngine) SetExecutionProfile(profile ExecutionProfile) {
	pe.c.SetExecutionProfile(profile)
}

func (pe *defaultPolicyEngine) IsCacheFresh(entry cachedTaskEntry, identity CacheIdentity) bool {
	return entry.isFresh(identity)
}

func (pe *defaultPolicyEngine) ResolveRecoveryPolicy(def *agent.AgentDef, t TaskDef) (SideEffectClass, RecoveryPolicy, string) {
	return resolveTaskRecovery(def, t)
}

type defaultContextCompiler struct {
	c *Coordinator
}

func (cc *defaultContextCompiler) CalculateBudget(spec ModelContextSpec, systemTokens, toolsTokens int) ContextBudget {
	return CalculateContextBudget(spec, systemTokens, toolsTokens)
}

func (cc *defaultContextCompiler) ContextUsageReport() (ContextBudget, ContextUsageBreakdown, string, bool) {
	return cc.c.ContextUsageReport()
}

func (cc *defaultContextCompiler) BuildMemorySuffix(agentRole string) string {
	if cc.c == nil {
		return ""
	}
	return cc.c.buildMemorySuffixImpl(agentRole)
}

func (cc *defaultContextCompiler) BuildTaskSTMContext() string {
	if cc.c == nil {
		return ""
	}
	return cc.c.buildTaskSTMContextImpl()
}

func (cc *defaultContextCompiler) BuildLTMContext() string {
	if cc.c == nil {
		return ""
	}
	return cc.c.buildLTMContextImpl()
}

func (cc *defaultContextCompiler) AutoQueryMemory(ctx context.Context, store *memory.MemoryStore, prompt string, compact memory.CompactFunc) (string, error) {
	if store == nil {
		return "", nil
	}
	return memory.AutoQuery(ctx, store, prompt, compact)
}

func (cc *defaultContextCompiler) AssembleContextWithinBudget(parts []string, budget int) string {
	return assembleContextWithinBudget(parts, budget)
}

func (cc *defaultContextCompiler) AssembleContextItems(ctx context.Context, items []ContextItem, budget ContextBudget) (string, bool, error) {
	return AssembleContextItemsPipeline(ctx, items, budget)
}

func (cc *defaultContextCompiler) CompactProjectContext(ctx context.Context, sidecarCompacter SidecarCompacter, messages []fantasy.Message, prevSummary *StructuredSummary, originalGoal string) (*StructuredSummary, error) {
	return PerformStructuredCompaction(ctx, sidecarCompacter, messages, prevSummary, originalGoal)
}

func (cc *defaultContextCompiler) FormatDependencyResults(dependencies []TaskResult) string {
	return FormatDependencyResults(dependencies)
}

func (cc *defaultContextCompiler) CompileCoordinatorContext(ctx context.Context, input CoordinatorContextInput) (CompiledContext, error) {
	return CompileCoordinatorContext(ctx, input)
}

func (cc *defaultContextCompiler) CompileWorkerContext(ctx context.Context, input WorkerContextInput) (CompiledContext, error) {
	return CompileWorkerContext(ctx, input)
}

type defaultAgentPool struct {
	c *Coordinator
}

func (ap *defaultAgentPool) ResolveAgentName(input string) (*agent.AgentDef, string, error) {
	return ap.c.resolveAgentName(input)
}

func (ap *defaultAgentPool) Sidecar() *sidecar.Sidecar {
	return ap.c.Sidecar()
}

func (ap *defaultAgentPool) GuardSidecar() *sidecar.Sidecar {
	return ap.c.GuardSidecar()
}

func (ap *defaultAgentPool) JudgeSidecar() *sidecar.Sidecar {
	return ap.c.JudgeSidecar()
}

type defaultWorkflowEngine struct {
	c *Coordinator
}

func (we *defaultWorkflowEngine) Run(ctx context.Context, prompt string) (string, error) {
	return we.c.Run(ctx, prompt)
}

func (we *defaultWorkflowEngine) ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error) {
	return we.c.ExecuteTasks(ctx, tasks)
}

type defaultToolResolver struct{ c *Coordinator }

func workerToolResolutionModeForTask(task TaskDef) WorkerToolResolutionMode {
	if task.PlanFirst && task.PlanID == "" {
		return WorkerToolResolutionInitialPlan
	}
	if task.PlanFirst && task.PlanID != "" {
		return WorkerToolResolutionApprovedPlan
	}
	return WorkerToolResolutionNormal
}

func (r *defaultToolResolver) ResolveTaskTools(ctx context.Context, def *agent.AgentDef, req WorkerToolResolutionRequest) (ResolvedWorkerTools, error) {
	if r == nil || r.c == nil || def == nil {
		return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: agent definition is required")
	}
	task := req.Task
	mode := req.Mode
	if mode == "" {
		mode = WorkerToolResolutionNormal
	}
	if mode != WorkerToolResolutionNormal && mode != WorkerToolResolutionInitialPlan && mode != WorkerToolResolutionApprovedPlan && mode != WorkerToolResolutionResultRepair && mode != WorkerToolResolutionResume {
		return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: unsupported worker lifecycle mode %q", mode)
	}

	resultRequired := task.Execution.RequiresResult
	planRequired := false
	resultOnly := mode == WorkerToolResolutionResultRepair || mode == WorkerToolResolutionResume
	switch mode {
	case WorkerToolResolutionInitialPlan:
		planRequired = true
		resultRequired = false
	case WorkerToolResolutionApprovedPlan:
		resultRequired = true
	case WorkerToolResolutionResultRepair, WorkerToolResolutionResume:
		resultRequired = true
		resultOnly = true
	}
	// A closed sequence is a literal task contract, not a base sequence to
	// widen for another lifecycle phase. Initial planning requires submit_plan,
	// so reject a non-empty sequence before constructing any task-bound tools or
	// reaching provider construction.
	if mode == WorkerToolResolutionInitialPlan && len(task.Execution.ToolSequence) > 0 {
		return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: initial-plan mode is incompatible with closed execution tool_sequence; remove tool_sequence or disable plan-first")
	}
	if resultOnly || planRequired || resultRequired {
		if strings.TrimSpace(req.TodoID) == "" {
			return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: %s requires a Todo ID", mode)
		}
		if r.c.todoItemByID(req.TodoID) == nil {
			return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: Todo %q does not exist", req.TodoID)
		}
	}

	var workerTools []fantasy.AgentTool
	if !resultOnly {
		workerTools = r.c.selectWorkerToolsForTask(def, task)
	}
	tools := workerTools
	mcpAllowed := r.c.phaseWorkflow == nil || !r.c.phaseWorkflow.Enabled() || r.c.phaseWorkflow.State() == PhaseExecute
	if !resultOnly && r.c.mcpManager != nil && mcpAllowed {
		tools = append(tools, r.c.mcpManager.AsAgentTools()...)
		if len(def.MCPTools) > 0 {
			if err := r.c.mcpManager.LoadAgentMCPServer(def.Name, def.MCPTools, def.Shell); err != nil {
				return ResolvedWorkerTools{}, fmt.Errorf("load MCP server for agent %s: %w", def.Name, err)
			}
			tools = append(tools, r.c.mcpManager.GetAgentMCPTools(def.Name, def.Shell)...)
		}
	}
	// MCP and custom registries are also untrusted sources for a worker
	// surface. Apply the same final boundary after all providers have been
	// merged; filtering only the built-in core would still expose a
	// coordinator capability under an MCP/custom tool implementation.
	tools = r.c.filterCoordinatorOnlyWorkerTools(tools)
	// Never accept a caller-supplied implementation of a task-bound protocol
	// tool. The resolver owns these bindings and recreates them per Todo.
	tools = removeToolNames(tools, submitResultToolName, "submit_plan")
	if resultRequired {
		if r.c.toolDeniedByTeam(submitResultToolName) {
			return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: required protocol tool %q is denied by team policy", submitResultToolName)
		}
		tools = append(tools, &submitResultTool{coordinator: r.c, todoID: req.TodoID})
	}
	if planRequired {
		if r.c.toolDeniedByTeam("submit_plan") {
			return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: required protocol tool %q is denied by team policy", "submit_plan")
		}
		tools = append(tools, &submitPlanTool{coordinator: r.c, todoID: req.TodoID})
	}
	effectiveSequence := task.Execution.ToolSequence
	if resultOnly {
		effectiveSequence = []string{submitResultToolName}
	}
	tools = r.c.filterDeniedWorkerToolsWithGrants(tools, r.c.taskToolGrants(def, task))
	tools = r.c.filterCoordinatorOnlyWorkerTools(tools)
	return r.finalizeTaskTools(ctx, def, task, tools, resultOnly, resultRequired, planRequired, effectiveSequence)
}

func (r *defaultToolResolver) finalizeTaskTools(ctx context.Context, def *agent.AgentDef, task TaskDef, tools []fantasy.AgentTool, resultOnly, resultRequired, planRequired bool, effectiveSequence []string) (ResolvedWorkerTools, error) {
	modelID := r.c.resolveAgentModel(def, task.Model)
	filteredTools, err := r.c.filterWorkerToolsForModel(ctx, modelID, tools, resultOnly || resultRequired || planRequired, effectiveSequence)
	if err != nil {
		return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: %w", err)
	}
	tools = filteredTools
	if missing := missingExecutionTools(tools, effectiveSequence); len(missing) > 0 {
		return ResolvedWorkerTools{}, fmt.Errorf("execution tool_sequence requires unavailable tool(s) for agent %q: %s", def.Name, strings.Join(missing, ", "))
	}
	tools = filterToolsForSequence(tools, effectiveSequence)
	if resultOnly && (len(tools) != 1 || tools[0].Info().Name != submitResultToolName) {
		return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: result-only repair surface must contain exactly %q", submitResultToolName)
	}
	names := agentToolNames(tools)
	return ResolvedWorkerTools{Tools: tools, Names: names, Capabilities: append([]string(nil), names...)}, nil
}

func removeToolNames(candidate []fantasy.AgentTool, names ...string) []fantasy.AgentTool {
	removed := make(map[string]bool, len(names))
	for _, name := range names {
		removed[name] = true
	}
	filtered := make([]fantasy.AgentTool, 0, len(candidate))
	for _, tool := range candidate {
		if tool != nil && !removed[tool.Info().Name] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

type defaultModelRuntime struct{ c *Coordinator }

func (r *defaultModelRuntime) ResolveTaskModel(def *agent.AgentDef, task TaskDef) (string, error) {
	if r == nil || r.c == nil || def == nil {
		return "", fmt.Errorf("resolve task model: agent definition is required")
	}
	copyTask := task
	if err := r.c.validateTaskModel(&copyTask); err != nil {
		return "", err
	}
	return r.c.resolveAgentModel(def, copyTask.Model), nil
}

func (r *defaultModelRuntime) ProviderFor(modelID string) (*agent.OpenAICompatibleProvider, error) {
	if r == nil || r.c == nil || r.c.providerManager == nil {
		return nil, fmt.Errorf("model runtime provider is unavailable")
	}
	provider := r.c.providerManager.GetProvider(modelID)
	if provider == nil {
		return nil, fmt.Errorf("no provider for model %q", modelID)
	}
	return provider, nil
}
