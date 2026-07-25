package team

import (
	"context"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/sidecar"
)

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

// ContextCompiler defines the interface for token context budget, system prompt assembly, and context breakdown.
type ContextCompiler interface {
	CalculateBudget(spec ModelContextSpec, systemTokens, toolsTokens int) ContextBudget
	ContextUsageReport() (ContextBudget, ContextUsageBreakdown, string, bool)
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
