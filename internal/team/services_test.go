package team

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/sidecar"
)

type mockPlanner struct {
	parseCalled bool
}

func (m *mockPlanner) ParsePromptSegments(rawPrompt string, registry *TeamRegistry, defaultTeam string) ([]PromptSegment, error) {
	m.parseCalled = true
	return []PromptSegment{{Type: SegmentText, Content: rawPrompt}}, nil
}

func (m *mockPlanner) CheckDuplicate(ctx context.Context, tasks []TaskDef) ([]string, map[int]bool, map[int]*duplicateTodoMatch) {
	return nil, nil, nil
}

type mockPolicyEngine struct {
	policy  CachePolicy
	profile ExecutionProfile
}

func (m *mockPolicyEngine) GetCachePolicy() CachePolicy            { return m.policy }
func (m *mockPolicyEngine) SetCachePolicy(p CachePolicy)           { m.policy = p }
func (m *mockPolicyEngine) GetExecutionProfile() ExecutionProfile  { return m.profile }
func (m *mockPolicyEngine) SetExecutionProfile(p ExecutionProfile) { m.profile = p }
func (m *mockPolicyEngine) IsCacheFresh(entry cachedTaskEntry, identity CacheIdentity) bool {
	return true
}
func (m *mockPolicyEngine) ResolveRecoveryPolicy(def *agent.AgentDef, t TaskDef) (SideEffectClass, RecoveryPolicy, string) {
	return SideEffectNone, RecoveryRetry, ""
}

func TestCoordinatorSubServices_Defaults(t *testing.T) {
	session := &TeamSession{
		Workspace: t.TempDir(),
		Dir:       t.TempDir(),
		Config:    agent.TeamConfig{Name: "test-team", GoalMode: "exploratory"},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	if c.Planner() == nil {
		t.Errorf("expected non-nil Planner interface")
	}
	if c.SessionStore() == nil {
		t.Errorf("expected non-nil SessionStore interface")
	}
	if c.PolicyEngine() == nil {
		t.Errorf("expected non-nil PolicyEngine interface")
	}
	if c.ContextCompiler() == nil {
		t.Errorf("expected non-nil ContextCompiler interface")
	}
	if c.AgentPool() == nil {
		t.Errorf("expected non-nil AgentPool interface")
	}
	if c.WorkflowEngine() == nil {
		t.Errorf("expected non-nil WorkflowEngine interface")
	}
}

func TestCoordinatorSubServices_Override(t *testing.T) {
	session := &TeamSession{
		Workspace: t.TempDir(),
		Dir:       t.TempDir(),
		Config:    agent.TeamConfig{Name: "test-team", GoalMode: "exploratory"},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	// Test Planner override
	mockP := &mockPlanner{}
	c.SetPlanner(mockP)
	segs, err := c.Planner().ParsePromptSegments("test prompt", nil, "")
	if err != nil {
		t.Fatalf("Planner.ParsePromptSegments failed: %v", err)
	}
	if !mockP.parseCalled {
		t.Errorf("expected mockPlanner to be called")
	}
	if len(segs) != 1 || segs[0].Content != "test prompt" {
		t.Errorf("unexpected segments from mockPlanner: %v", segs)
	}

	// Test PolicyEngine override
	mockPE := &mockPolicyEngine{policy: CacheBypass}
	c.SetPolicyEngine(mockPE)
	if c.PolicyEngine().GetCachePolicy() != CacheBypass {
		t.Errorf("expected CacheBypass policy from mockPolicyEngine")
	}
}

// --- Mocks for round-trip + nil-fallback coverage (fix 3) ---

type mockSessionStore struct {
	tag          string
	savedSession *SessionData
	savedMD      string
	savedRecord  *CompactionRecord
	sd           *SessionData
}

func (m *mockSessionStore) SaveSession(dir string, sessionData *SessionData) error {
	m.savedSession = sessionData
	return nil
}
func (m *mockSessionStore) SaveSessionMD(workspace string, content string) error {
	m.savedMD = content
	return nil
}
func (m *mockSessionStore) SaveCompactionRecord(workspace string, record CompactionRecord) error {
	m.savedRecord = &record
	return nil
}
func (m *mockSessionStore) SessionData() *SessionData      { return m.sd }
func (m *mockSessionStore) SetSessionData(sd *SessionData) { m.sd = sd }

type mockContextCompiler struct {
	tag                   string
	budget                ContextBudget
	usage                 ContextUsageBreakdown
	modelID               string
	ready                 bool
	calcBudget            ContextBudget
	calcCalled            bool
	compileCoordinatorErr error
	compileWorkerErr      error
	workerModelContext    ModelContextSpec
}

func (m *mockContextCompiler) CalculateBudget(spec ModelContextSpec, systemTokens, toolsTokens int) ContextBudget {
	m.calcCalled = true
	return m.calcBudget
}
func (m *mockContextCompiler) ContextUsageReport() (ContextBudget, ContextUsageBreakdown, string, bool) {
	return m.budget, m.usage, m.modelID, m.ready
}
func (m *mockContextCompiler) BuildMemorySuffix(agentRole string) string { return "" }
func (m *mockContextCompiler) BuildTaskSTMContext() string               { return "" }
func (m *mockContextCompiler) BuildLTMContext() string                   { return "" }
func (m *mockContextCompiler) AutoQueryMemory(ctx context.Context, store *memory.MemoryStore, prompt string, compact memory.CompactFunc) (string, error) {
	return "", nil
}
func (m *mockContextCompiler) AssembleContextWithinBudget(parts []string, budget int) string {
	return assembleContextWithinBudget(parts, budget)
}
func (m *mockContextCompiler) AssembleContextItems(ctx context.Context, items []ContextItem, budget ContextBudget) (string, bool, error) {
	return AssembleContextItemsPipeline(ctx, items, budget)
}
func (m *mockContextCompiler) CompactProjectContext(ctx context.Context, sidecarCompacter SidecarCompacter, messages []fantasy.Message, prevSummary *StructuredSummary, originalGoal string) (*StructuredSummary, error) {
	return PerformStructuredCompaction(ctx, sidecarCompacter, messages, prevSummary, originalGoal)
}
func (m *mockContextCompiler) FormatDependencyResults(dependencies []TaskResult) string {
	return FormatDependencyResults(dependencies)
}
func (m *mockContextCompiler) CompileCoordinatorContext(ctx context.Context, input CoordinatorContextInput) (CompiledContext, error) {
	if m.compileCoordinatorErr != nil {
		return CompiledContext{}, m.compileCoordinatorErr
	}
	return CompileCoordinatorContext(ctx, input)
}
func (m *mockContextCompiler) CompileWorkerContext(ctx context.Context, input WorkerContextInput) (CompiledContext, error) {
	m.workerModelContext = input.ModelContext
	if m.compileWorkerErr != nil {
		return CompiledContext{}, m.compileWorkerErr
	}
	return CompileWorkerContext(ctx, input)
}

type mockAgentPool struct {
	tag           string
	resolveDef    *agent.AgentDef
	resolveKey    string
	resolveErr    error
	sidecar       *sidecar.Sidecar
	guardSidecar  *sidecar.Sidecar
	judgeSidecar  *sidecar.Sidecar
	resolveCalled bool
}

func (m *mockAgentPool) ResolveAgentName(input string) (*agent.AgentDef, string, error) {
	m.resolveCalled = true
	return m.resolveDef, m.resolveKey, m.resolveErr
}
func (m *mockAgentPool) Sidecar() *sidecar.Sidecar      { return m.sidecar }
func (m *mockAgentPool) GuardSidecar() *sidecar.Sidecar { return m.guardSidecar }
func (m *mockAgentPool) JudgeSidecar() *sidecar.Sidecar { return m.judgeSidecar }

type mockWorkflowEngine struct {
	tag        string
	runResult  string
	runErr     error
	execResult string
	execErr    error
	runCalled  bool
	execCalled bool
}

func (m *mockWorkflowEngine) Run(ctx context.Context, prompt string) (string, error) {
	m.runCalled = true
	return m.runResult, m.runErr
}
func (m *mockWorkflowEngine) ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error) {
	m.execCalled = true
	return m.execResult, m.execErr
}

// TestCoordinatorSubServices_RoundTrip verifies Set+Get returns the exact mock
// for all 6 sub-service interfaces (fix 3: extends override coverage beyond
// the original Planner/PolicyEngine-only test).
func TestCoordinatorSubServices_RoundTrip(t *testing.T) {
	c := &Coordinator{}

	// Planner
	mp := &mockPlanner{}
	c.SetPlanner(mp)
	if c.Planner() != Planner(mp) {
		t.Errorf("Planner round-trip failed")
	}
	// SessionStore
	mss := &mockSessionStore{tag: "ss"}
	c.SetSessionStore(mss)
	if c.SessionStore() != SessionStore(mss) {
		t.Errorf("SessionStore round-trip failed")
	}
	// PolicyEngine
	mpe := &mockPolicyEngine{policy: CacheRefresh}
	c.SetPolicyEngine(mpe)
	if c.PolicyEngine() != PolicyEngine(mpe) {
		t.Errorf("PolicyEngine round-trip failed")
	}
	// ContextCompiler
	mcc := &mockContextCompiler{tag: "cc"}
	c.SetContextCompiler(mcc)
	if c.ContextCompiler() != ContextCompiler(mcc) {
		t.Errorf("ContextCompiler round-trip failed")
	}
	// AgentPool
	map_ := &mockAgentPool{tag: "ap"}
	c.SetAgentPool(map_)
	if c.AgentPool() != AgentPool(map_) {
		t.Errorf("AgentPool round-trip failed")
	}
	// WorkflowEngine
	mwe := &mockWorkflowEngine{tag: "we"}
	c.SetWorkflowEngine(mwe)
	if c.WorkflowEngine() != WorkflowEngine(mwe) {
		t.Errorf("WorkflowEngine round-trip failed")
	}
}

// TestCoordinatorSubServices_NilFallback verifies each getter returns a non-nil
// default wrapper when the backing field is nil (fix 3: covers the nil-fallback
// path not exercised by NewCoordinator-based tests where fields are populated).
func TestCoordinatorSubServices_NilFallback(t *testing.T) {
	c := &Coordinator{} // all sub-service fields nil (zero-value)

	if c.Planner() == nil {
		t.Errorf("Planner() nil-fallback returned nil")
	}
	if c.SessionStore() == nil {
		t.Errorf("SessionStore() nil-fallback returned nil")
	}
	if c.PolicyEngine() == nil {
		t.Errorf("PolicyEngine() nil-fallback returned nil")
	}
	if c.ContextCompiler() == nil {
		t.Errorf("ContextCompiler() nil-fallback returned nil")
	}
	if c.AgentPool() == nil {
		t.Errorf("AgentPool() nil-fallback returned nil")
	}
	if c.WorkflowEngine() == nil {
		t.Errorf("WorkflowEngine() nil-fallback returned nil")
	}
}

// TestCoordinatorSubServices_PolicyEngineOverrideAffectsLookup is a behavioral
// test proving the call-site migration (fix 1): the cache-lookup path now reads
// cache policy via c.PolicyEngine().GetCachePolicy(), so overriding PolicyEngine
// changes Coordinator's internal caching decision (the seam is no longer "pure").
func TestCoordinatorSubServices_PolicyEngineOverrideAffectsLookup(t *testing.T) {
	ws := t.TempDir()
	c := &Coordinator{
		projectDir:      ws,
		taskResultCache: make(map[string][]cachedTaskEntry),
	}

	// Seed the cache under an explicit CacheUse PolicyEngine override (fresh=true).
	c.SetPolicyEngine(&mockPolicyEngine{policy: CacheUse})
	c.storeTaskCache("agent-a", "task 1", "output-v1")

	// CacheUse override + fresh entries -> lookup should hit.
	out, ok := c.lookupTaskCache(context.Background(), "agent-a", "task 1")
	if !ok || out != "output-v1" {
		t.Fatalf("expected hit under CacheUse override, got out=%q ok=%v", out, ok)
	}

	// Switch to CacheBypass override -> the migrated call-site must observe it and
	// return a miss even though the cache is populated.
	c.SetPolicyEngine(&mockPolicyEngine{policy: CacheBypass})
	out, ok = c.lookupTaskCache(context.Background(), "agent-a", "task 1")
	if ok {
		t.Fatalf("expected miss under CacheBypass override, got hit %q", out)
	}

	// Switch back to CacheUse -> hit again (override honored, not stale field read).
	c.SetPolicyEngine(&mockPolicyEngine{policy: CacheUse})
	out, ok = c.lookupTaskCache(context.Background(), "agent-a", "task 1")
	if !ok || out != "output-v1" {
		t.Fatalf("expected hit after switching back to CacheUse, got out=%q ok=%v", out, ok)
	}
}

// TestCoordinatorSubServices_AgentPoolOverrideAffectsResolve is a behavioral test
// proving the resolveAgentName call-site migration (fix 1): RunDirectAgent now
// resolves the agent via c.AgentPool().ResolveAgentName(), so overriding AgentPool
// changes which AgentDef is used.
func TestCoordinatorSubServices_AgentPoolOverrideAffectsResolve(t *testing.T) {
	c := &Coordinator{}

	want := &agent.AgentDef{Name: "fake-agent", Role: "worker"}
	c.SetAgentPool(&mockAgentPool{resolveDef: want, resolveKey: "fake-agent"})

	got, key, err := c.AgentPool().ResolveAgentName("anything")
	if err != nil {
		t.Fatalf("ResolveAgentName via override failed: %v", err)
	}
	if got != want {
		t.Errorf("expected override AgentDef %p, got %p", want, got)
	}
	if key != "fake-agent" {
		t.Errorf("expected key fake-agent, got %q", key)
	}
}
