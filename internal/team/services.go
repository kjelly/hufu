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

type ToolResolver interface {
	ResolveTaskTools(context.Context, *agent.AgentDef, TaskDef, []fantasy.AgentTool) (ResolvedWorkerTools, error)
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

func (r *defaultToolResolver) ResolveTaskTools(_ context.Context, def *agent.AgentDef, task TaskDef, extras []fantasy.AgentTool) (ResolvedWorkerTools, error) {
	if r == nil || r.c == nil || def == nil {
		return ResolvedWorkerTools{}, fmt.Errorf("resolve task tools: agent definition is required")
	}
	tools := r.c.selectWorkerToolsForTask(def, task)
	mcpAllowed := r.c.phaseWorkflow == nil || !r.c.phaseWorkflow.Enabled() || r.c.phaseWorkflow.State() == PhaseExecute
	if r.c.mcpManager != nil && mcpAllowed {
		tools = append(tools, r.c.mcpManager.AsAgentTools()...)
		if len(def.MCPTools) > 0 {
			if err := r.c.mcpManager.LoadAgentMCPServer(def.Name, def.MCPTools, def.Shell); err != nil {
				return ResolvedWorkerTools{}, fmt.Errorf("load MCP server for agent %s: %w", def.Name, err)
			}
			tools = append(tools, r.c.mcpManager.GetAgentMCPTools(def.Name, def.Shell)...)
		}
	}
	if missing := missingExecutionTools(append(append([]fantasy.AgentTool(nil), tools...), extras...), task.Execution.ToolSequence); len(missing) > 0 {
		return ResolvedWorkerTools{}, fmt.Errorf("execution tool_sequence requires unavailable tool(s) for agent %q: %s", def.Name, strings.Join(missing, ", "))
	}
	tools = append(tools, extras...)
	tools = filterToolsForSequence(tools, task.Execution.ToolSequence)
	names := agentToolNames(tools)
	return ResolvedWorkerTools{Tools: tools, Names: names, Capabilities: append([]string(nil), names...)}, nil
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
