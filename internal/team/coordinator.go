package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/audit"
	"github.com/kjelly/hufu/internal/config"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/hooks"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
	"gopkg.in/yaml.v3"
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

// llmUsageReceiptExpectedKey marks streams whose cumulative usage will be
// accounted by a worker/direct-agent execution receipt. Auxiliary streams
// (coordinator, sub-agent, repair, and plan-reviewer calls) have no such
// receipt and must feed the no-progress token counter directly.
type llmUsageReceiptExpectedKey struct{}

// taskTranscriptKey carries an optional runner-owned transcript recorder into
// the streaming callbacks for a verbatim task.
type taskTranscriptKey struct{}

// executionAttemptKey binds tool-call receipts and submit_result claims to the
// exact task attempt that produced them.
type executionAttemptKey struct{}

// delegationChainKey carries the "/"-joined chain of agent names that led to
// the current request_agent call, propagated through the context (the same
// way todoIDKey is) since the coordinator's mutable snapshot only ever holds
// the single currently-running agent's flat name.
type delegationChainKey struct{}

type TaskDef struct {
	ID string `json:"id,omitempty"`
	// WhenGoalContains selects a static team task contract for a later
	// coordinator dispatch. It is configuration-only and is never exposed as a
	// coordinator tool parameter.
	WhenGoalContains string `json:"-" yaml:"when-goal-contains,omitempty"`
	// Phase is configuration-only when a runtime workflow is enabled. The
	// coordinator cannot set it through the agent tool; static task contracts
	// bind it to a runtime-owned workflow phase.
	Phase Phase `json:"-" yaml:"phase,omitempty"`
	// Action is a static execute-phase contract. Its JSON omission prevents a
	// coordinator from choosing a provider, action type, or payload at runtime.
	Action           *Action `json:"-" yaml:"action,omitempty"`
	ContractID       string  `json:"contract_id,omitempty"`
	ContractHash     string  `json:"contract_hash,omitempty"`
	ContractRevision int     `json:"contract_revision,omitempty"`
	Agent            string  `json:"agent"`
	Goal             string  `json:"goal"`
	Constraints      string  `json:"constraints,omitempty"`
	Model            string  `json:"model,omitempty"`
	Sidecar          bool    `json:"sidecar,omitempty"`
	Summarize        bool    `json:"summarize,omitempty"`
	// OutputMode controls how the worker's output is returned to the
	// coordinator. "verbatim" captures tool activity in a runner-owned
	// transcript artifact and returns only its manifest, keeping raw output out
	// of later LLM context. Empty and "summary" preserve legacy behavior.
	OutputMode   string   `json:"output_mode,omitempty" yaml:"output-mode,omitempty"`
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
	Verify     string            `json:"verify,omitempty" yaml:"verify,omitempty"`
	VerifyMode string            `json:"verify_mode,omitempty" yaml:"verify-mode,omitempty"`
	VerifySpec *VerificationSpec `json:"verify_spec,omitempty" yaml:"verify-spec,omitempty"`
	Requires   []string          `json:"requires,omitempty"`
	MaxRetries int               `json:"max_retries,omitempty"` // Maximum number of retries if verify fails
	OnFailure  *int              `json:"on_failure,omitempty"`  // 0-based index of the task to jump back to if verify fails
	// Escalate makes each retry after a failure re-run the task on the next
	// stronger model in the model-list (ordered weakest→strongest).
	Escalate bool `json:"escalate,omitempty"`
	// AdversarialVerify is the number of skeptic LLM verifiers (0 = disabled,
	// capped at maxSkeptics) that independently try to refute the result after
	// the task succeeds; a majority refutation fails the task into the retry
	// path with the refutation as feedback.
	AdversarialVerify int `json:"adversarial_verify,omitempty"`
	// SideEffect classifies the task's side-effect risk (none, workspace_write, external_write, infra_mutation, credential_mutation).
	SideEffect SideEffectClass `json:"side_effect,omitempty"`
	// Recovery controls interrupted task recovery behavior (retry, reconcile, manual, never).
	Recovery RecoveryPolicy `json:"recovery,omitempty"`
	// ReconcileTool specifies an optional read-only probe command to verify state during crash recovery.
	ReconcileTool string `json:"reconcile_tool,omitempty"`
	// Execution encapsulates execution contract semantics (kind, requires_result, requires_verification, allows_replay).
	Execution           ExecutionContract   `json:"execution,omitempty" yaml:"execution,omitempty"`
	Kind                TaskKind            `json:"kind,omitempty" yaml:"kind,omitempty"`
	Advances            []string            `json:"advances,omitempty" yaml:"advances,omitempty"`
	ExpectedStateChange string              `json:"expected_state_change,omitempty" yaml:"expected_state_change,omitempty"`
	RecoveryHypothesis  *RecoveryHypothesis `json:"recovery_hypothesis,omitempty" yaml:"recovery_hypothesis,omitempty"`
	ResourceClaims      []string            `json:"resource_claims,omitempty" yaml:"resource_claims,omitempty"`
	Resources           []ResourceClaim     `json:"resources,omitempty" yaml:"resources,omitempty"`
}

// ResourceClaimMode describes how a task uses a shared resource.
type ResourceClaimMode string

const (
	ResourceRead      ResourceClaimMode = "read"
	ResourceWrite     ResourceClaimMode = "write"
	ResourceExclusive ResourceClaimMode = "exclusive"
)

// ResourceClaim is a scheduler-level lock declaration. Legacy
// TaskDef.ResourceClaims entries are treated as exclusive claims.
type ResourceClaim struct {
	Resource string            `json:"resource" yaml:"resource"`
	Mode     ResourceClaimMode `json:"mode" yaml:"mode"`
}

// UnmarshalJSON handles legacy "task" field by mapping it to Goal, and legacy "strict_result" / "strict-result" fields.
func (t *TaskDef) UnmarshalJSON(data []byte) error {
	type Alias TaskDef
	aux := &struct {
		Task             *string `json:"task"`
		StrictResult     *bool   `json:"strict_result"`
		StrictResultDash *bool   `json:"strict-result"`
		*Alias
	}{Alias: (*Alias)(t)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if t.Goal == "" && aux.Task != nil {
		t.Goal = *aux.Task
	}
	if (aux.StrictResult != nil && *aux.StrictResult) || (aux.StrictResultDash != nil && *aux.StrictResultDash) {
		t.Execution.RequiresResult = true
	}
	return nil
}

// UnmarshalYAML handles legacy "task" field by mapping it to Goal, and legacy "strict-result" / "strict_result" fields.
func (t *TaskDef) UnmarshalYAML(node *yaml.Node) error {
	type Alias TaskDef
	var aux struct {
		Task              *string `yaml:"task"`
		StrictResult      *bool   `yaml:"strict-result"`
		StrictResultUnder *bool   `yaml:"strict_result"`
		*Alias            `yaml:",inline"`
	}
	aux.Alias = (*Alias)(t)
	if err := node.Decode(&aux); err != nil {
		return err
	}
	if t.Goal == "" && aux.Task != nil {
		t.Goal = *aux.Task
	}
	if (aux.StrictResult != nil && *aux.StrictResult) || (aux.StrictResultUnder != nil && *aux.StrictResultUnder) {
		t.Execution.RequiresResult = true
	}
	return nil
}

type DirectAgentResult struct {
	AgentName string
	Output    string
	Error     error
	// ReplanRequired marks a first-threshold no-progress result. The fast path
	// must escalate to the coordinator instead of retrying the direct worker,
	// since each direct attempt starts a fresh run-scoped budget.
	ReplanRequired bool
	// Steps is the number of LLM/tool steps the direct agent executed.
	// Used by the fast-path router to drive auto-escalation decisions.
	Steps int
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
	mu                 sync.RWMutex
	session            *TeamSession
	providerManager    *agent.ProviderManager
	mcpManager         *mcp.MCPToolManager
	coreTools          []fantasy.AgentTool
	agentCache         map[string]fantasy.Agent
	agentToolNameCache map[string][]string
	agentCacheMu       sync.RWMutex
	round              int
	baseRounds         int // rounds completed before the last round-state reset (resume/continue)
	verbose            bool
	think              bool
	reportStatus       StatusReporter
	sessionData        *SessionData
	// sessionMu guards all reads and writes of sessionData. Parallel task
	// goroutines (dag_scheduler -> executeTask) concurrently mutate the shared
	// sessionData through persistContextManifest and saveCheckpoint; without
	// this lock their read-modify-write plus json.Marshal in SaveSession races.
	// It is distinct from c.mu: c.mu also guards sub-service pointers and is
	// reentered via SessionStore(), so it cannot be held across SaveSession.
	sessionMu                         sync.RWMutex
	taskTracker                       *TaskTracker
	skills                            []*skill.SkillDef
	conversationHistory               []fantasy.Message
	conversationHistorySourceCounts   []int
	conversationHistoryMu             sync.Mutex
	conversationHistorySourceOffset   int
	lastCompactionSummary             *StructuredSummary
	initialPrompt                     string
	coordinatorProtocolRepairsAttempt atomic.Int32
	coordinatorProtocolRepairsSuccess atomic.Int32
	coordinatorPolicyRepairsAttempt   atomic.Int32
	coordinatorPolicyRepairsSuccess   atomic.Int32
	coordinatorPolicyRepairPending    atomic.Bool
	contextRequestSeq                 atomic.Uint64
	initialToolCorrections            atomic.Int32
	projectDir                        string
	// Context budget reporting (§5.4). Populated by buildSystemPrompt so the
	// execution report can emit a token-usage breakdown without re-deriving the
	// assembled prompt.
	ctxReportMu                sync.RWMutex
	lastCtxBreakdown           ContextUsageBreakdown
	lastCtxBudget              ContextBudget
	lastCtxModel               string
	lastCtxReportReady         bool
	wrapUp                     atomic.Int32
	initialDelegationAttempted atomic.Bool
	finishCalled               atomic.Bool // set when the finish tool completes; cleared per orchestrator run
	continuationInterrupted    atomic.Bool // set when a continuation stops before a workflow can safely finish
	// freshSession requests an event-store root branch on the next run. It is
	// set by CLI --new so archived events remain auditable without being
	// replayed into the new session's task projection.
	freshSession atomic.Bool
	// freshSessionMemory prevents a --new invocation from injecting a prior
	// session's archive into its new coordinator or worker prompts. Unlike
	// freshSession, it remains set for this coordinator's whole lifetime.
	freshSessionMemory           atomic.Bool
	current                      atomic.Pointer[currentSnapshot]
	currentStageStart            time.Time
	currentStageStartMu          sync.RWMutex
	auditLogger                  *audit.AuditLogger
	sshSessionMgr                *tools.SSHSessionManager
	terminalSessionMgr           *TerminalSessionManager
	terminalBroker               *TerminalBroker
	terminalControlMu            sync.Mutex
	terminalPauses               map[string]*terminalTaskPause
	terminalRoundCancels         map[string]context.CancelFunc
	terminalRoundDone            map[string]chan struct{}
	terminalRoundShutdownTimeout time.Duration
	ptyTerminalEnabled           bool
	// Silent-stall watchdog (coordinator_stall_watchdog.go): detects periods
	// with no forward-progress signal at all, distinct from a single slow
	// tool call, and leaves a goroutine dump as evidence.
	stallActivityAt   atomic.Int64 // unix nano, last observed forward-progress signal
	stallLastDumpAt   atomic.Int64 // unix nano, 0 = no dump yet in the current stall episode
	stallDumps        atomic.Int32 // total dumps written this run, capped at stallMaxDumps
	stallThreshold    time.Duration
	stallMaxDumps     int32
	stallWatchdogOnce sync.Once
	skillUsage        map[string]*skillUsageState
	skillUsageMu      sync.Mutex
	delegatedTasks    map[string]int
	delegatedTasksMu  sync.Mutex
	taskResultCache   map[string][]cachedTaskEntry // agent → ordered list of past results
	taskResultCacheMu sync.RWMutex
	cachePolicy       CachePolicy
	cachePolicyMu     sync.RWMutex
	executionProfile  ExecutionProfile
	// modelExecutionID is set only on an isolated extra-model coordinator.
	// It disambiguates receipts/manifests that share a Todo attempt.
	modelExecutionID       string
	executionProfileMu     sync.RWMutex
	goalMode               GoalMode
	goalModeMu             sync.RWMutex
	capabilityCache        map[string]CapabilityResult
	capabilityCacheMu      sync.Mutex
	capabilityInflight     map[string]chan CapabilityResult
	cacheGeneration        atomic.Int64 // bumped each time coordinator starts a new delegation round
	journal                *taskJournal // persistent task-result journal (nil when disabled)
	noJournal              bool
	eventStore             *EventStore     // append-only session event store
	emittedTaskTransitions map[string]bool // all durable event idempotency keys; legacy name retained for compatibility
	eventOnceMu            sync.Mutex
	dualWriteFailures      atomic.Int64
	memoryStore            *memory.MemoryStore
	contextRepo            contextstore.Repository // canonical context store used by prompt assembly and maintenance
	memoryRankingPolicy    MemoryRuntimeRankingPolicy
	workerMemorySvc        WorkerMemoryService // WP-3 per-worker memory recall service.
	sharedMemorySvc        SharedMemoryService // canonical shared persistent memory service.
	asyncTasksWg           sync.WaitGroup      // tracks in-flight async candidate writes before run finalization
	skillsMu               sync.RWMutex
	modelList              []config.ModelEntry
	sidecarModel           string
	sidecarInst            *sidecar.Sidecar
	sidecarInitMu          sync.Mutex
	sidecarInit            bool
	guardModel             string
	guardInst              *sidecar.Sidecar
	guardInitMu            sync.Mutex
	guardInit              bool
	judgeModel             string
	judgeInst              *sidecar.Sidecar
	judgeInitMu            sync.Mutex
	judgeInit              bool
	planReviewerModel      string
	cachedWorkerContext    string
	workerCtxOnce          sync.Once
	autoLoadedSkills       []*skill.SkillDef
	autoLoadedSkillsMu     sync.RWMutex
	forcedSkillNames       map[string]bool // set of skill names specified via --skill
	maxConcurrent          int
	// providerSem holds a lazily-created concurrency-limiting channel per
	// provider name (e.g. "ollama"), built from session.Config.Providers[name].
	// MaxConcurrent. A local model dispatched by many workers is not the same
	// as many workers able to usefully run concurrent inference (spec.md item
	// 5), so this gates in addition to, not instead of, maxConcurrent above.
	providerSemMu sync.Mutex
	providerSem   map[string]chan struct{}
	sessionTime   time.Time
	stmWriteMu    sync.Mutex // serializes Read-Modify-Write STM operations to prevent lost-updates
	ltmWriteMu    sync.Mutex // Protect LTM file reads and writes

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

	taskResults          map[string]*TaskResult
	taskResultsMu        sync.RWMutex
	stepReceipts         *ExecutionStepReceiptRegistry
	taskAttempts         map[string]int
	taskAttemptsMu       sync.RWMutex
	toolPolicyVerdicts   map[string]string
	toolPolicyVerdictsMu sync.Mutex
	structuredStepRunner StructuredStepRunner

	// executionEvents is initialized for each top-level Run/Continue call and
	// receives attempt-level telemetry for `hufu improve`.
	executionEventsMu     sync.RWMutex
	executionEvents       *executionEventLogger
	executionRunID        string
	executionTeamRevision string

	// lastCompletedRunDeprecatedReport snapshots the per-run deprecated
	// memory-usage aggregate just before beginExecutionRun's deferred close
	// clears the active event store / executionRunID. Without this, a
	// post-run --report call would always see an empty executionRunID and
	// the report would omit the just-completed run's counts (HF-MEM5-007:
	// per-run aggregate must remain observable after the run finishes).
	//
	// lastCompletedRunDeprecatedCaptured is the sentinel for "a completed
	// run was captured". Its presence is independent of whether the
	// captured aggregate has entries — a zero-use run captures an empty
	// slice, and that empty slice is the authoritative answer for that run.
	// Without this independent flag, DeprecatedMemoryToolReport would
	// treat an empty captured slice as "no run captured yet" and fall
	// through to disk rehydration, misreporting the just-finished run as
	// having inherited the prior run's compatibility counts.
	lastCompletedRunDeprecatedReport  []DeprecatedMemoryToolUsage
	lastCompletedRunDeprecatedCapture bool
	lastCompletedRunDeprecatedMu      sync.RWMutex

	// One-shot startup validation of configured model names.
	validateModelsOnce sync.Once
	validateModelsErr  error

	// Unattended / budget controls for no-human-watching operation.
	unattended                    bool
	autoApprove                   bool
	maxWallClock                  time.Duration // 0 = unlimited
	tokenBudget                   int64         // 0 = unlimited; cumulative LLM tokens
	tokensUsed                    atomic.Int64
	acceptanceCmd                 string // optional shell command run at finish
	acceptanceSpec                *AcceptanceSpec
	acceptanceContractFixed       bool
	acceptanceContractRevision    int
	continuationResume            *ContinuationCheckpoint
	metricsMu                     sync.RWMutex
	retriesByFailureClass         map[TaskFailureClass]int
	retrySuppressionsByReason     map[string]int
	retrySuppressionSeen          map[string]bool
	preflightFailuresCaught       int
	nonAssertingVerifiersRejected int
	antiThrashing                 AntiThrashingState
	compactions                   int
	tokensSinceCriterionProgress  int64
	// turnsSinceCriterionProgress counts coordinator model turns
	// (runOrchestrator invocations) since the last objective criterion
	// advancement. Reset only by criterion progress (§8.1, WP-12).
	turnsSinceCriterionProgress int
	// tasksSinceCriterionProgress counts TodoItems added (AddBatch) since
	// the last objective criterion advancement. Reset only by criterion
	// progress (§8.1, WP-12).
	tasksSinceCriterionProgress int
	// noProgressReplanTripped records whether the first no-progress
	// threshold (replan_required) has already fired for the current
	// accumulation, so the second threshold (stop-and-partial) is only
	// reached after a fresh accumulation crosses again (§8.1, WP-12).
	noProgressReplanTripped   bool
	noProgressStopTripped     bool
	reliabilityUsageByAttempt map[string]int
	// noProgressUsageOwner routes accounting from an isolated extra-model
	// clone back to the coordinator that owns the active run budget.
	noProgressUsageOwner     *Coordinator
	noProgressUsageNamespace string
	rollbackCmd              string // optional shell command run on acceptance failure
	selfHealingAttempts      int
	// acceptanceRecovery permits the bounded repair turns requested after a
	// blocking acceptance failure.  A run may already be in wrap-up because a
	// round/budget circuit breaker fired; refusing every new delegation there
	// makes acceptance self-healing impossible.  The flag is cleared when the
	// run is reset or a finish succeeds.
	acceptanceRecovery       atomic.Bool
	budgetTripped            atomic.Bool
	lastRunResult            *RunResult
	lastRunResultMu          sync.RWMutex
	lastEvidenceManifest     *EvidenceManifest
	lastEvidenceManifestMu   sync.RWMutex
	diagnosticPackets        []DiagnosticPacket
	diagnosticPacketsMu      sync.RWMutex
	pendingDiagnosticPackets map[string]DiagnosticPacket
	planRevisions            []PlanRevision
	planRevisionsMu          sync.RWMutex
	planMaxTasks             int
	planMaxAttempts          int
	planReviews              map[string]PlanReviewResult
	planReviewsMu            sync.RWMutex
	// contractWarnings deduplicates contract_warning events per
	// (todoID, code, message) within a single dispatch cycle, so that both
	// the ExecuteTasks preflight and the executeTask execution-path check
	// (and the crash-resume path that bypasses ExecuteTasks) collectively
	// emit at most one warning per finding per task. This is a shared pointer
	// so cloned/isolated coordinators (extra-models) participate in the same
	// dedup set rather than each re-emitting the same warning.
	// contractWarningsOnce guards the lazy initialization of the pointer
	// because the scheduler dispatches ready tasks in goroutines and
	// extra-models spawns per-model clones concurrently; unsynchronized nil
	// checks would race and could lose the dedup set. Refs: §4.3, WP-02.
	contractWarnings     *contractWarningDedup
	contractWarningsOnce sync.Once
	// runOrchestratorOverride is a deterministic test seam for continuation
	// and recovery integration tests; production coordinators leave it nil.
	runOrchestratorOverride func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error)
	// workerAgentOverride is a deterministic integration-test seam; production
	// execution always creates the configured worker agent.
	workerAgentOverride fantasy.Agent
	repairAgentOverride fantasy.Agent

	// Decoupled sub-services (§17 struct-level interface decoupling)
	planner             Planner
	sessionStore        SessionStore
	policyEngine        PolicyEngine
	repairController    *RepairController
	authorizationPolicy AuthorizationPolicy
	secretRegistry      *tools.SecretRegistry
	contextCompiler     ContextCompiler
	agentPool           AgentPool
	workflowEngine      WorkflowEngine
	eventJournal        EventJournal
	toolResolver        ToolResolver
	modelRuntime        ModelRuntime
	subagentRegistry    *SubagentRegistry
	experienceProcessor ExperienceProcessor
	phaseWorkflow       *runtimeWorkflow
}

// SetUnattended enables unattended (no-human) mode: ask_user returns safe
// defaults, only explicitly-allowed tools may run, and path consent
// fast-denies instead of prompting a human who isn't there.
func (c *Coordinator) SetUnattended(v bool) {
	c.unattended = v
	tools.SetProcessUnattended(v)
}

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
func (c *Coordinator) SetAcceptance(cmd string) error {
	spec := AcceptanceSpec{}
	if cmd != "" {
		spec.Commands = []string{cmd}
	}
	return c.SetAcceptanceSpecWithReason(spec, "set_acceptance")
}

// SetAcceptanceSpec sets an explicit AcceptanceSpec for run-level acceptance.
func (c *Coordinator) SetAcceptanceSpec(spec AcceptanceSpec) error {
	return c.SetAcceptanceSpecWithReason(spec, "set_acceptance_spec")
}

// SetAcceptanceSpecWithReason sets an explicit AcceptanceSpec with an audit reason and persists audit entries.
func (c *Coordinator) SetAcceptanceSpecWithReason(spec AcceptanceSpec, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	spec = cloneAcceptanceSpec(spec)

	var oldSpec *AcceptanceSpec
	if c.acceptanceSpec != nil {
		oldCopy := cloneAcceptanceSpec(*c.acceptanceSpec)
		oldSpec = &oldCopy
	}

	oldSpecJSON := "none"
	if oldSpec != nil {
		if b, err := json.Marshal(oldSpec); err == nil {
			oldSpecJSON = string(b)
		}
	}
	newSpecJSON := ""
	if b, err := json.Marshal(spec); err == nil {
		newSpecJSON = string(b)
	}
	oldState := AcceptanceContractStateOf(oldSpec)
	newState := AcceptanceContractStateOf(&spec)

	goalModeStr := string(c.GoalMode())
	if valErr := ValidateAcceptanceSpec(&spec, goalModeStr); valErr != nil {
		rejectReason := fmt.Sprintf("%s (rejected: %v)", reason, valErr)
		audit.LogAcceptanceRejected("coordinator", string(oldState), oldSpecJSON, string(newState), newSpecJSON, rejectReason)
		c.persistAcceptanceAuditEvent("acceptance_contract_rejected", "rejected", oldState, oldSpecJSON, newState, newSpecJSON, rejectReason)
		c.report(c.newEvent("acceptance_contract_rejected").
			withData(map[string]any{
				"old_spec":  oldSpec,
				"new_spec":  spec,
				"old_state": oldState,
				"new_state": newState,
				"reason":    reason,
				"status":    "rejected",
				"error":     valErr.Error(),
			}).
			withMessage(fmt.Sprintf("acceptance contract update rejected. reason: %s, error: %v", reason, valErr)))
		c.emitEvent("acceptance_contract_rejected", "coordinator", "", map[string]interface{}{
			"old_spec": oldSpec, "new_spec": spec, "old_state": oldState, "new_state": newState,
			"reason": reason, "status": "rejected", "error": valErr.Error(),
		})
		return valErr
	}

	message := fmt.Sprintf("acceptance contract set. reason: %s", reason)
	if c.acceptanceContractFixed && c.acceptanceSpec != nil {
		message = fmt.Sprintf("acceptance contract modified after run start. reason: %s", reason)
	}
	// A normal initial configured contract is initialization, not a
	// modification event. An explicit initial empty contract is retained as an
	// audit event because it must remain distinguishable from no contract.
	if (c.acceptanceContractFixed && c.acceptanceSpec != nil) ||
		(oldState == AcceptanceContractUnset && newState == AcceptanceContractEmpty) {
		c.report(c.newEvent("acceptance_contract_modified").
			withData(map[string]any{
				"old_spec":  oldSpec,
				"new_spec":  spec,
				"old_state": oldState,
				"new_state": newState,
				"status":    "accepted",
				"reason":    reason,
			}).
			withMessage(message))
	}
	audit.LogAcceptanceChange(audit.EventAcceptanceModified, "accepted", "coordinator", string(oldState), oldSpecJSON, string(newState), newSpecJSON, reason)
	c.persistAcceptanceAuditEvent("acceptance_contract_modified", "accepted", oldState, oldSpecJSON, newState, newSpecJSON, reason)

	c.acceptanceContractRevision++
	revision := AcceptanceContractRevision{
		Revision:  c.acceptanceContractRevision,
		Timestamp: time.Now().Format(time.RFC3339Nano),
		OldSpec:   oldSpec,
		NewSpec:   spec,
		Reason:    reason,
	}
	if c.sessionData != nil {
		c.sessionData.AcceptanceContractRevisions = append(c.sessionData.AcceptanceContractRevisions, revision)
		if c.session != nil && c.session.Workspace != "" {
			_ = SaveSession(c.session.Workspace, c.sessionData)
		}
	}
	c.emitEvent("acceptance_contract_modified", "coordinator", "", map[string]interface{}{
		"revision": revision.Revision, "old_spec": oldSpec, "new_spec": spec,
		"old_state": oldState, "new_state": newState, "reason": reason,
	})

	specCopy := cloneAcceptanceSpec(spec)
	c.acceptanceSpec = &specCopy
	if len(spec.Commands) > 0 {
		c.acceptanceCmd = spec.Commands[0]
	}
	c.acceptanceContractFixed = true
	return nil
}

func (c *Coordinator) persistAcceptanceAuditEvent(event, status string, oldState AcceptanceContractState, oldSpecJSON string, newState AcceptanceContractState, newSpecJSON, reason string) {
	if c.session == nil || c.session.Workspace == "" {
		return
	}
	auditDir := filepath.Join(c.session.Workspace, "logs")
	_ = os.MkdirAll(auditDir, 0o755)
	auditPath := filepath.Join(auditDir, "acceptance_audit.jsonl")

	rec := map[string]any{
		"timestamp": time.Now().Format(time.RFC3339Nano),
		"event":     event,
		"status":    status,
		"team":      c.session.Config.Name,
		"old_state": oldState,
		"old_spec":  oldSpecJSON,
		"new_state": newState,
		"new_spec":  newSpecJSON,
		"reason":    reason,
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return
	}
	b, err := utils.RedactJSONCompact(encoded)
	if err != nil {
		return
	}
	f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// LastRunResult returns the last computed RunResult for this coordinator.
func (c *Coordinator) LastRunResult() *RunResult {
	c.lastRunResultMu.RLock()
	defer c.lastRunResultMu.RUnlock()
	return c.lastRunResult
}

// SetLastRunResult sets the computed RunResult for this coordinator.
func (c *Coordinator) SetLastRunResult(res *RunResult) {
	if res != nil {
		c.annotateRunCompletionSemantics(res)
	}
	c.lastRunResultMu.Lock()
	c.lastRunResult = res
	c.lastRunResultMu.Unlock()
	// Persist the canonical result immediately. This is intentionally best
	// effort: the normal checkpoint path still owns task/session durability,
	// while an outcome must never disappear merely because the process exits
	// after finish.
	if c.sessionData != nil {
		c.sessionData.RunResult = res
		if c.session != nil && c.session.Workspace != "" {
			_ = SaveSession(c.session.Workspace, c.sessionData)
		}
	}
}

// annotateRunCompletionSemantics projects canonical task/result evidence onto
// the terminal result. It never upgrades a review to fixed_and_verified based
// on prose or advisory acceptance.
func (c *Coordinator) annotateRunCompletionSemantics(res *RunResult) {
	if c == nil || res == nil {
		return
	}
	res.AcceptanceAdvisory = c.ExecutionProfile().AcceptanceMode == AcceptanceAdvisory
	if c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) == 0 || res.GoalMode != GoalModeExploratory {
		return
	}
	allDone := true
	for _, item := range items {
		if item == nil || item.Status != TaskDone {
			allDone = false
			continue
		}
		if item.TypedResult != nil && len(item.TypedResult.Findings) > 0 {
			res.FindingsPresent = true
		}
	}
	res.CompletedReview = allDone
	// An exploratory review cannot establish an implementation outcome. This
	// stays false even if its advisory acceptance command passed.
	res.FixedAndVerified = false
}

// SetRollback sets an optional shell command run on acceptance failure in unattended mode.
func (c *Coordinator) SetRollback(cmd string) { c.rollbackCmd = cmd }

func (c *Coordinator) chooseAskUserResponse(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (tools.AskUserResponse, error) {
	s := c.AgentPool().Sidecar()
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
func (c *Coordinator) addStepTokens(steps []fantasy.StepResult) int64 {
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
	return total
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
	var coordinator *Coordinator
	coreTools := agent.BuildAllAgentTools(projectDir, tools.WithAllowedPaths(allowedPaths), tools.WithPathConsent(pathConsent), tools.WithArtifactOpener(func(ctx context.Context, ref string) (io.ReadCloser, error) {
		if coordinator == nil {
			return nil, fmt.Errorf("artifact resolver is not initialized")
		}
		return coordinator.openArtifactRef(ctx, ref)
	}), tools.WithWorkspaceName(filepath.Base(session.Workspace)), tools.WithHooks(hookRegistry), tools.WithRestrictedBash(rbashMode), tools.WithRestrictedPath(restrictedPath), tools.WithNetworkBlock(noNet), tools.WithForceMCP(forceMCP), tools.WithDirenv(direnv))
	pm, err := agent.NewProviderManager(defaultProviderURL, defaultProviderAPIKey, session.Config.Providers)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider manager: %w", err)
	}
	c := &Coordinator{
		providerManager:           pm,
		session:                   session,
		mcpManager:                mcpManager,
		coreTools:                 coreTools,
		agentCache:                make(map[string]fantasy.Agent),
		agentToolNameCache:        make(map[string][]string),
		retrySuppressionsByReason: make(map[string]int),
		retrySuppressionSeen:      make(map[string]bool),
		verbose:                   verbose,
		think:                     think,
		reportStatus:              func(event StatusEvent) {},
		taskTracker:               NewTaskTracker(),
		skills:                    session.Skills,
		projectDir:                projectDir,
		skillUsage:                make(map[string]*skillUsageState),
		delegatedTasks:            make(map[string]int),
		pendingPlans:              make(map[string]*PlanEntry),
		approvedOutputs:           make(map[string]string),
		approvedErrors:            make(map[string]error),
		taskResults:               make(map[string]*TaskResult),
		stepReceipts:              NewExecutionStepReceiptRegistry(),
		taskAttempts:              make(map[string]int),
		toolPolicyVerdicts:        make(map[string]string),
		taskResultCache:           make(map[string][]cachedTaskEntry),
		pendingDiagnosticPackets:  make(map[string]DiagnosticPacket),
		capabilityCache:           make(map[string]CapabilityResult),
		capabilityInflight:        make(map[string]chan CapabilityResult),
		memoryStore:               memoryStore,
		modelList:                 modelList,
		sidecarModel:              roleModels.Sidecar,
		guardModel:                roleModels.Guard,
		judgeModel:                roleModels.Judge,
		planReviewerModel:         roleModels.PlanReviewer,
		maxConcurrent:             maxConcurrent,
		sessionTime:               time.Now(),
		hooks:                     hookRegistry,
		rbashMode:                 rbashMode,
		restrictedPath:            restrictedPath,
		noNet:                     noNet,
		forceMCP:                  forceMCP,
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
	coordinator = c
	// Context lookup is coordinator-owned so it can use the canonical router.
	// It still enters the exact same selection, policy, unattended, force-MCP,
	// and closed-sequence gates as every other worker tool.
	c.coreTools = append(c.coreTools, &contextQueryTool{coordinator: c}, &contextGetTool{coordinator: c})
	phaseWorkflow, err := newRuntimeWorkflow(session)
	if err != nil {
		return nil, err
	}
	phaseWorkflow.setEventEmitter(func(eventType string, phase Phase, details LifecycleEventPayload) {
		details.Phase = string(phase)
		c.emitEvent(eventType, "runtime", "", details)
		c.persistRuntimeContextSnapshot(phase)
	})
	phaseWorkflow.setRepositoryRoot(c.projectDir)
	c.phaseWorkflow = phaseWorkflow

	if len(session.Config.Workflow.Phases) == 0 {
		fmt.Fprintln(os.Stderr, "Warning: prompt-defined workflow constraints are deprecated and will be removed in a future release. Please migrate to runtime-enforced Workflow ExecutionContext.")
	}

	effectiveGoalMode, err := ResolveEffectiveGoalMode(session.Config.GoalMode, session.Config.ExecutionProfile)
	if err != nil {
		return nil, fmt.Errorf("invalid effective goal mode: %w", err)
	}
	c.goalMode = effectiveGoalMode

	if session.Config.AcceptanceSpec != nil {
		if err := c.SetAcceptanceSpec(*session.Config.AcceptanceSpec); err != nil {
			return nil, err
		}
	} else if session.Config.Acceptance != "" {
		if err := c.SetAcceptance(session.Config.Acceptance); err != nil {
			return nil, err
		}
	} else {
		if err := ValidateAcceptanceSpec(nil, string(effectiveGoalMode)); err != nil {
			return nil, err
		}
	}

	c.planner = &defaultPlanner{c: c}
	c.sessionStore = &defaultSessionStore{c: c}
	c.policyEngine = &defaultPolicyEngine{c: c}
	c.repairController = NewRepairController()
	c.authorizationPolicy = defaultAuthorizationPolicy{}
	c.secretRegistry = tools.NewSecretRegistry()
	utils.RegisterSecretRedactor(c.secretRegistry)
	registerProviderSecrets(c.secretRegistry, session, defaultProviderAPIKey)
	c.contextCompiler = &defaultContextCompiler{c: c}
	c.agentPool = &defaultAgentPool{c: c}
	c.structuredStepRunner = &coordinatorDeclaredToolRunner{c: c}
	// Scrub legacy hufu-managed records before they are reloaded into prompts or
	// opened for append. User workspace artifacts are deliberately excluded.
	if redactErr := RedactWorkspaceManagedRecords(session.Workspace); redactErr != nil {
		log.Printf("warning: could not redact managed workspace records: %v", redactErr)
	}
	// Canonical context is now required for every coordinator. Refusing to run
	// without it prevents a fallback to legacy Markdown/JSONL truth after the
	// cutover; callers can repair workspace permissions and retry safely.
	repo, openErr := contextstore.OpenSQLite(filepath.Join(session.Workspace, "context.sqlite"))
	if openErr != nil {
		return nil, fmt.Errorf("open canonical context store: %w", openErr)
	}
	c.contextRepo = repo
	if err := c.loadAdoptedMemoryPolicy(context.Background()); err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("load adopted memory policy: %w", err)
	}
	c.workerMemorySvc = NewWorkerMemoryService(repo, nil)
	c.sharedMemorySvc = NewSharedMemoryService(repo)
	c.workflowEngine = &defaultWorkflowEngine{c: c}
	c.eventJournal = eventStoreJournal{}
	c.toolResolver = &defaultToolResolver{c: c}
	c.modelRuntime = &defaultModelRuntime{c: c}
	c.subagentRegistry = NewSubagentRegistry(NewHufuLocalSubagentProvider(c))
	c.experienceProcessor = &defaultExperienceProcessor{c: c}

	// Enable sidecar for skill pattern detection
	if s := c.AgentPool().Sidecar(); s != nil {
		c.skillDetector.SetSidecar(s)
	}

	auditLogger, err := audit.NewAuditLogger(session.Workspace, session.Config.Name)
	if err == nil {
		c.auditLogger = auditLogger
		auditLogger.SetRedactor(c.SecretRegistry())
		audit.SetDefault(auditLogger)
	}

	// Initialize SSH session manager
	c.sshSessionMgr = tools.NewSSHSessionManager()
	terminalSessionMgr, err := NewTerminalSessionManager(session.Workspace, func(eventType, taskID string, payload map[string]interface{}) {
		c.emitEvent(eventType, "terminal", taskID, payload)
	})
	if err != nil {
		return nil, fmt.Errorf("initialize terminal session manager: %w", err)
	}
	terminalSessionMgr.SetActiveTaskRoundChecker(c.isTerminalRoundActive)
	c.terminalSessionMgr = terminalSessionMgr

	c.coreTools = append(c.coreTools,
		&requestAgentTool{coordinator: c},
		&todoTool{coordinator: c},
		&loadSkillTool{coordinator: c},
		&saveSkillTool{coordinator: c},
		&stmWriteTool{coordinator: c},
		&ltmUpdateTool{coordinator: c},
		&teamInfoTool{coordinator: c},
		&terminalTool{coordinator: c},
		&terminalStartTool{coordinator: c},
		&terminalWriteTool{coordinator: c},
		&terminalReadTool{coordinator: c},
		&terminalWaitTool{coordinator: c},
		&terminalCloseTool{coordinator: c},
		&terminalListTool{coordinator: c},
		&terminalReconcileTool{coordinator: c},
		&reconcileTaskTool{coordinator: c},
	)

	// MemoryStore is a legacy migration adapter only. The model-facing memory
	// tools are backed by context.sqlite whenever it is available, so enabling
	// canonical memory must not depend on creating a second record store.
	if c.contextRepo != nil || c.memoryStore != nil {
		c.coreTools = append(c.coreTools,
			&memorySaveLTMWrapper{coordinator: c},
			&canonicalMemoryQueryTool{coordinator: c},
		)
	}

	guardReviewer := func(ctx context.Context, toolName, args string, rules []string) (bool, string, error) {
		s := c.AgentPool().GuardSidecar()
		prof := c.ExecutionProfile()
		if s == nil {
			if err := c.recordAuxiliaryFallback(ctx, "guard_reviewer", "no_model_fallback"); err != nil {
				return false, "", err
			}
			if prof.PolicyFailureMode == PolicyFailClosed || prof.StrictPolicy {
				return false, "guard reviewer unavailable under PolicyFailClosed policy", fmt.Errorf("guard reviewer unavailable")
			}
			return true, "", nil
		}
		agentName, _ := ctx.Value(tools.AgentNameKey).(string)
		result, err := s.ReviewToolCall(sidecar.WithPurpose(ctx, "guard_reviewer"), agentName, toolName, args, rules)
		if err != nil {
			if prof.PolicyFailureMode == PolicyFailOpen {
				return true, "", nil
			}
			return false, "", err
		}
		return result.Approved, result.Reason, nil
	}
	tools.SetGuardReviewer(c.coreTools, guardReviewer)

	pathReviewer := func(ctx context.Context, command string, path string) (bool, error) {
		s := c.AgentPool().Sidecar()
		if s == nil {
			if err := c.recordAuxiliaryFallback(ctx, "path_reviewer", "no_model_fallback"); err != nil {
				return false, err
			}
			return true, nil
		}
		return s.ReviewPathAccess(sidecar.WithPurpose(ctx, "path_reviewer"), command, path)
	}
	tools.SetPathReviewer(c.coreTools, pathReviewer)

	if !planMode {
		if history := LoadConversationHistory(session.Workspace); len(history) > 0 {
			c.conversationHistory = history
			c.conversationHistorySourceCounts = make([]int, len(history))
			for i := range c.conversationHistorySourceCounts {
				c.conversationHistorySourceCounts[i] = 1
			}

			if persisted := LoadSession(session.Workspace); persisted != nil {
				c.conversationHistorySourceCounts = normalizeSourceCounts(len(history), persisted.ConversationHistorySourceCounts)
				c.conversationHistorySourceOffset = persisted.ConversationHistorySourceOffset
				if c.conversationHistorySourceOffset < 0 {
					c.conversationHistorySourceOffset = 0
				}
			}
		}
	}

	if pathConsent != nil {
		coordinator := c
		pathConsent.SetAgentInfoSource(func() tools.AgentInfo {
			return coordinator.GetCurrentAgentInfo()
		})
	}

	// Every tool an agent is shown must be in its runtime allowlist. Checking it
	// here costs nothing and surfaces a violation before the first model call,
	// rather than as a mid-run task failure that looks like a model mistake.
	if err := c.validateToolGrants(); err != nil {
		return nil, fmt.Errorf("tool grant validation failed: %w", err)
	}

	return c, nil
}

func registerProviderSecrets(registry *tools.SecretRegistry, session *TeamSession, defaultKey string) {
	if registry == nil {
		return
	}
	if defaultKey != "" {
		_ = registry.Register(tools.SecretRef{Name: "provider.default.api_key", Source: "resolved provider configuration", ExactValue: defaultKey})
	}
	if session == nil {
		return
	}
	for name, provider := range session.Config.Providers {
		if provider.ProviderAPIKey == "" {
			continue
		}
		_ = registry.Register(tools.SecretRef{
			Name:       "provider." + name + ".api_key",
			Source:     "team provider configuration",
			ExactValue: provider.ProviderAPIKey,
		})
	}
}

// RegisterProviderSecretsGlobally installs resolved provider credentials in a
// process-memory registry before session lifecycle files are generated. This
// covers archive/resume paths that run before NewCoordinator is constructed.
func RegisterProviderSecretsGlobally(session *TeamSession, defaultKey string) {
	registry := tools.NewSecretRegistry()
	registerProviderSecrets(registry, session, defaultKey)
	utils.RegisterSecretRedactor(registry)
}

// ResetConversation clears the accumulated coordinator conversation history so
// the next Run/ContinueWithPrompt starts fresh. Used by the chat REPL's /reset.
func (c *Coordinator) ResetConversation() {
	c.conversationHistoryMu.Lock()
	c.conversationHistory = nil
	c.conversationHistorySourceCounts = nil
	c.conversationHistorySourceOffset = 0
	if c.sessionData != nil {
		c.sessionData.ConversationHistorySourceCounts = nil
		c.sessionData.ConversationHistorySourceOffset = 0
	}
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
	c.acceptanceRecovery.Store(false)
	c.finishCalled.Store(false)
	c.continuationInterrupted.Store(false)
	c.initialToolCorrections.Store(0)
	c.delegatedTasksMu.Lock()
	c.delegatedTasks = make(map[string]int)
	c.delegatedTasksMu.Unlock()
}

// SetStatusReporter wraps fn so every delivered StatusEvent also feeds the
// stall watchdog. This is the one place that can see every report call
// regardless of path: c.report(event) calls c.reportStatus directly, and the
// streaming callbacks in coordinator_task_run.go capture reportFn :=
// c.reportStatus once and call it many times per task. Wrapping here means
// touchActivity does not need to be added at each of those call sites, and
// cannot be forgotten at a future one.
func (c *Coordinator) SetStatusReporter(fn StatusReporter) {
	if fn == nil {
		return
	}
	c.reportStatus = func(event StatusEvent) {
		// The watchdog's own stall notification must not reset the idle
		// clock it is reporting on, or a persistent stall would only ever
		// produce a single dump.
		if event.Type != runStallEventType {
			c.touchActivity()
		}
		fn(event)
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
	if c.reportStatus != nil {
		c.reportStatus(event)
	}
}

func (c *Coordinator) newEvent(eventType string) StatusEvent {
	teamName := ""
	if c.session != nil {
		teamName = c.session.Config.Name
	}
	return StatusEvent{Type: eventType, TeamName: teamName}
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

func (c *Coordinator) SetTaskTracker(tracker *TaskTracker) {
	c.taskTracker = tracker
}

// TerminalManager exposes the coordinator-owned terminal resource manager.
// Callers must bind requests to the current TODO ID via WithTerminalTaskID.
func (c *Coordinator) TerminalManager() TerminalManager {
	if c == nil {
		return nil
	}
	return c.terminalSessionMgr
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
func buildAgentTaskProperties(workerNames []string, hasModelList bool, sharedDirPath string, capabilityNames []string, allowContextFiles bool) map[string]any {
	contextFilesDesc := "Optional files from the shared directory to provide as context"
	if sharedDirPath != "" {
		contextFilesDesc = fmt.Sprintf("Optional files from the shared directory (%s) to provide as context", sharedDirPath)
	}
	props := map[string]any{
		"agent":         map[string]any{"type": "string", "enum": workerNames, "description": "Agent name for a new task. Do not select a worker whose prior task is already successful; retrieve its result with team_info/task_result instead."},
		"goal":          map[string]any{"type": "string", "description": "The desired OUTCOME — what should be achieved. Do NOT include implementation details (file paths, function names, step-by-step instructions). Workers are specialists who determine their own approach."},
		"constraints":   map[string]any{"type": "string", "description": "Non-obvious constraints the worker MUST respect (e.g., 'must use Python 3.11', 'cannot modify the public API'). Do NOT include obvious project conventions."},
		"plan_first":    map[string]any{"type": "boolean", "description": "If true, the agent must draft a task execution plan and call submit_plan before doing any work. Use this for complex tasks where you want to review the approach before execution. After receiving the plan, call approve_plan, modify_plan, or reject_plan."},
		"summarize":     map[string]any{"type": "boolean", "description": "If true, summarize the agent's output before returning. Use for tasks that produce verbose output where only key points matter."},
		"output_mode":   map[string]any{"type": "string", "enum": []string{"summary", "verbatim"}, "description": "Output contract. Use verbatim when complete tool output is required: hufu captures a transcript artifact and returns its compact manifest instead of asking the worker to reproduce raw output."},
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
			"description": "Legacy fallback: use only when the check cannot be expressed by verify_spec. It accepts a runnable shell command (e.g. 'go build ./...') and runs after the agent reports success; a non-zero exit fails the task and triggers a retry. `test -f` and `test -d` retain shell semantics; only unambiguous `test -e` checks are translated to typed verification.",
		},
		"verify_mode": map[string]any{"type": "string", "enum": []string{"success", "expected_failure", "observation"}, "description": "Legacy verify mode; prefer mode inside verify_spec."},
		"verify_spec": map[string]any{
			"type":        "object",
			"description": "Preferred typed verification contract. Use this by default for file/path checks and JSON assertions; use command_exit only when a shell command is genuinely required. Supports command_exit, file_exists, file_absent, and json_assert. Takes precedence over legacy verify/verify_mode.",
			"properties": map[string]any{
				"type": map[string]any{
					"type":        "string",
					"enum":        []string{"command_exit", "file_exists", "file_absent", "json_assert"},
					"description": "Verification type: command_exit (run a shell command), file_exists (assert a file/dir exists), file_absent (assert a path does not exist), json_assert (read JSON and assert field values).",
				},
				"command": map[string]any{"type": "string", "description": "Shell command to run (required for command_exit, optional for json_assert to produce the JSON)."},
				"path":    map[string]any{"type": "string", "description": "File or directory path (required for file_exists/file_absent; the JSON file path for json_assert)."},
				"mode":    map[string]any{"type": "string", "enum": []string{"success", "expected_failure", "observation"}, "description": "Verification mode: success (default, pass on exit 0), expected_failure (pass on non-zero), observation (record evidence, never fails task)."},
				"assertions": map[string]any{
					"type":        "array",
					"description": "Required for json_assert. Each entry has 'path' (dot-separated JSON key, e.g. 'status.code') and 'equals' (expected scalar value).",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":   map[string]any{"type": "string"},
							"equals": map[string]any{"description": "Expected scalar value (string, number, or boolean)"},
						},
					},
				},
			},
		},
		"adversarial_verify": map[string]any{
			"type":        "integer",
			"description": "Optional number of skeptic LLM verifiers (1-3, odd recommended) that independently try to refute the result after the task succeeds. If a majority refutes, the task fails and retries with the refutation as feedback. Use for high-stakes tasks where 'verify' alone cannot check quality.",
		},
		"execution": map[string]any{
			"type":        "object",
			"description": "Execution contract. Use steps for artifact-producing workflows that require validator receipts, bounded pre-mutation repair, digest freeze, mutation, and verification. tool_sequence remains the legacy exact call budget for atomic tasks and cannot be combined with steps. tool_input_sequence can require JSON input fields. tool_input_field and tool_input_value_sequence are a paired scalar form permitted only for homogeneous tool sequences; for any mixed sequence (including a non-submit tool followed by submit_result), use tool_input_sequence or omit all input constraints. tool_expected_exit_codes declares expected non-zero observation outcomes such as timeout exit 124, so bounded discovery can continue without weakening other failures.",
			"dependentRequired": map[string]any{
				"tool_input_field":          []string{"tool_input_value_sequence"},
				"tool_input_value_sequence": []string{"tool_input_field"},
			},
			"properties": map[string]any{
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{"inline", "process", "interactive", "external"},
					"description": "Execution kind: inline (default), process, interactive, external.",
				},
				"requires_result": map[string]any{
					"type":        "boolean",
					"description": "If true, requires the worker to submit a structured task result.",
				},
				"requires_verification": map[string]any{
					"type":        "boolean",
					"description": "If true, requires an objective verifier contract (e.g. verify command).",
				},
				"allows_replay": map[string]any{
					"type":        "boolean",
					"description": "If true, task allows replay upon protocol/execution failure.",
				},
				"forbid_artifacts": map[string]any{
					"type":        "boolean",
					"description": "If true, submit_result must omit artifacts; Hufu rejects artifact declarations before accepting the result.",
				},
				"tool_sequence": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string", "pattern": `^\S+$`},
					"description": "Optional exact tool-call sequence for an atomic task. It is the complete call budget, not a tool-type summary: include one entry per call in order (repeat bash for every bash call and include write when the task writes a file), then end with submit_result. Hufu exposes only these tools and rejects out-of-order or extra calls.",
				},
				"tool_input_sequence": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "object"},
					"description": "Optional required JSON fields for each tool_sequence slot. It must have the same length; use an empty object for an unconstrained slot. Hufu denies a call that omits or changes a declared field before it runs.",
				},
				"tool_input_canonical_sequence": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "boolean"},
					"description": "Optional per-slot template-owned input binding. A true entry requires a same-length non-empty tool_input_sequence entry; Hufu replaces any model-authored input with that exact JSON object and records a policy decision. Use false for slots that need runtime-produced IDs or result content.",
				},
				"tool_input_transform_sequence": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional per-slot runtime-owned structural transform. It must align with tool_sequence; only Hufu-supported transform names are accepted. The transform runs after input binding and before the underlying tool, preserving the closed sequence.",
				},
				"tool_input_field": map[string]any{
					"type":        "string",
					"description": "Optional scalar input field, permitted only for a homogeneous tool_sequence. It requires a complete tool_input_value_sequence. For a mixed sequence, including submit_result with another tool, use per-slot tool_input_sequence or omit input constraints.",
				},
				"tool_input_value_sequence": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional required scalar values for tool_input_field, one per tool_sequence slot; use an empty string as wildcard. It is valid only with tool_input_field for a homogeneous sequence. For a mixed sequence, including submit_result with another tool, use per-slot tool_input_sequence or omit both scalar properties.",
				},
				"tool_expected_exit_codes": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "array", "items": map[string]any{"type": "integer", "not": map[string]any{"const": 0}}},
					"description": "Optional expected non-zero process exit codes, one integer array per tool_sequence slot. Use an empty array when normal success-only handling is required. A declared code (for example timeout exit 124) is returned as normal observation evidence and the sequence continues.",
				},
				"steps": map[string]any{
					"type":        "array",
					"description": "Provider-neutral execution DAG. Validation may be repairable only before mutation; every mutation must consume an artifact frozen by an ancestor validator.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":         map[string]any{"type": "string"},
							"tool":       map[string]any{"type": "string"},
							"input":      map[string]any{"type": "object", "description": "Typed provider-specific input carried without interpretation by Hufu."},
							"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"effect":     map[string]any{"type": "string", "enum": []string{"read", "produce", "validate", "mutate", "verify"}},
							"outputs": map[string]any{
								"type": "array",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"name":   map[string]any{"type": "string"},
										"kind":   map[string]any{"type": "string", "enum": []string{"artifact", "fact", "receipt"}},
										"schema": map[string]any{"type": "string"},
										"path":   map[string]any{"type": "string", "description": "Optional artifact path used by the built-in declared-tool runner to compute the runtime digest."},
										"scope":  map[string]any{"type": "string", "enum": []string{"task", "secret"}},
									},
									"required": []string{"name"},
								},
							},
							"references": map[string]any{
								"type":        "array",
								"description": "Typed task-local references to successful dependency outputs; runtime resolves these without coordinator prose copying.",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"target":  map[string]any{"type": "string"},
										"step_id": map[string]any{"type": "string"},
										"task_id": map[string]any{"type": "string", "description": "Alternative to step_id: successful dependency task id from this run_agents batch."},
										"output":  map[string]any{"type": "string"},
										"kind":    map[string]any{"type": "string", "enum": []string{"artifact", "fact", "receipt"}},
										"schema":  map[string]any{"type": "string"},
										"scope":   map[string]any{"type": "string", "enum": []string{"task", "secret"}},
									},
									"required": []string{"target", "output", "kind"},
								},
							},
							"consumes":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"on_failure":  map[string]any{"type": "string", "enum": []string{"terminal", "repairable"}},
							"max_repairs": map[string]any{"type": "integer", "minimum": 0},
						},
						"required": []string{"id", "tool", "effect"},
					},
				},
			},
		},
	}
	if len(capabilityNames) > 0 {
		props["requires"] = map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string", "enum": capabilityNames},
			"description": "Optional configured preflight capability names that must be available before this task starts. Do not use this for execution style; use execution.kind=interactive for interactive work.",
		}
	}
	if hasModelList {
		props["model"] = map[string]any{"type": "string", "description": "Model ID from Available Models to use for this task. Select the model whose strengths best match this task. If empty, the default team model will be used."}
		props["escalate"] = map[string]any{"type": "boolean", "description": "If true, each retry after a failure re-runs this task on the next stronger model in Available Models (ordered weakest→strongest). Start cheap tasks on a fast model with escalate:true so only failures pay for a stronger model. Not applicable to agents with extra-models."}
	}
	if !allowContextFiles {
		delete(props, "context_files")
	}
	return props
}

func (c *Coordinator) RunAgentsTool() fantasy.AgentTool {
	return &runAgentsTool{coordinator: c}
}

// Sub-service interface getters and setters (§17 struct-level interface decoupling)

// Planner returns the Planner sub-service interface.
func (c *Coordinator) Planner() Planner {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.planner == nil {
		return &defaultPlanner{c: c}
	}
	return c.planner
}

// SetPlanner sets a custom Planner implementation.
func (c *Coordinator) SetPlanner(p Planner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.planner = p
}

// SessionStore returns the SessionStore sub-service interface.
func (c *Coordinator) SessionStore() SessionStore {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.sessionStore == nil {
		return &defaultSessionStore{c: c}
	}
	return c.sessionStore
}

// SetSessionStore sets a custom SessionStore implementation.
func (c *Coordinator) SetSessionStore(ss SessionStore) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionStore = ss
}

// PolicyEngine returns the PolicyEngine sub-service interface.
func (c *Coordinator) PolicyEngine() PolicyEngine {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.policyEngine == nil {
		return &defaultPolicyEngine{c: c}
	}
	return c.policyEngine
}

// SetPolicyEngine sets a custom PolicyEngine implementation.
func (c *Coordinator) SetPolicyEngine(pe PolicyEngine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policyEngine = pe
}

// RepairController returns the coordinator's fail-closed recovery service.
func (c *Coordinator) RepairController() *RepairController {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.repairController == nil {
		return NewRepairController()
	}
	return c.repairController
}

func (c *Coordinator) SetRepairController(rc *RepairController) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repairController = rc
}

func (c *Coordinator) AuthorizationPolicy() AuthorizationPolicy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.authorizationPolicy == nil {
		return defaultAuthorizationPolicy{}
	}
	return c.authorizationPolicy
}

func (c *Coordinator) SetAuthorizationPolicy(policy AuthorizationPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authorizationPolicy = policy
}

func (c *Coordinator) SecretRegistry() *tools.SecretRegistry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.secretRegistry == nil {
		return tools.NewSecretRegistry()
	}
	return c.secretRegistry
}

func (c *Coordinator) AuthorizeToolCall(ctx context.Context, req ToolAuthorizationRequest) (PolicyDecision, error) {
	decision, err := c.AuthorizationPolicy().AuthorizeToolCall(ctx, req)
	_ = c.emitEvent("policy_decision", "policy", "", map[string]interface{}{"kind": "tool", "agent": req.Agent, "tool": req.Tool, "decision": decision, "error": errorString(err)})
	return decision, err
}

func (c *Coordinator) AuthorizeMCPCall(ctx context.Context, req MCPAuthorizationRequest) (PolicyDecision, error) {
	decision, err := c.AuthorizationPolicy().AuthorizeMCPCall(ctx, req)
	_ = c.emitEvent("policy_decision", "policy", "", map[string]interface{}{"kind": "mcp", "agent": req.Agent, "server": req.Server, "tool": req.Tool, "decision": decision, "error": errorString(err)})
	return decision, err
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ContextCompiler returns the ContextCompiler sub-service interface.
func (c *Coordinator) ContextCompiler() ContextCompiler {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.contextCompiler == nil {
		return &defaultContextCompiler{c: c}
	}
	return c.contextCompiler
}

// SetContextCompiler sets a custom ContextCompiler implementation.
func (c *Coordinator) SetContextCompiler(cc ContextCompiler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contextCompiler = cc
}

// AgentPool returns the AgentPool sub-service interface.
func (c *Coordinator) AgentPool() AgentPool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.agentPool == nil {
		return &defaultAgentPool{c: c}
	}
	return c.agentPool
}

// SetAgentPool sets a custom AgentPool implementation.
func (c *Coordinator) SetAgentPool(ap AgentPool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agentPool = ap
}

// WorkflowEngine returns the WorkflowEngine sub-service interface.
func (c *Coordinator) WorkflowEngine() WorkflowEngine {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.workflowEngine == nil {
		return &defaultWorkflowEngine{c: c}
	}
	return c.workflowEngine
}

// SetWorkflowEngine sets a custom WorkflowEngine implementation.
func (c *Coordinator) SetWorkflowEngine(we WorkflowEngine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workflowEngine = we
}

// EventJournal returns the coordinator's durable event boundary. Tests may
// inject a failing or in-memory journal without opening a JSONL file.
func (c *Coordinator) EventJournal() EventJournal {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.eventJournal != nil {
		return c.eventJournal
	}
	return eventStoreJournal{store: c.eventStore}
}

func (c *Coordinator) SetEventJournal(journal EventJournal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eventJournal = journal
}

func (c *Coordinator) ToolResolver() ToolResolver {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.toolResolver != nil {
		return c.toolResolver
	}
	return &defaultToolResolver{c: c}
}

func (c *Coordinator) SetToolResolver(resolver ToolResolver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolResolver = resolver
}

func (c *Coordinator) ModelRuntime() ModelRuntime {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.modelRuntime != nil {
		return c.modelRuntime
	}
	return &defaultModelRuntime{c: c}
}

func (c *Coordinator) SetModelRuntime(runtime ModelRuntime) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modelRuntime = runtime
}

func (c *Coordinator) SubagentRegistry() *SubagentRegistry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.subagentRegistry != nil {
		return c.subagentRegistry
	}
	return NewSubagentRegistry(NewHufuLocalSubagentProvider(c))
}

func (c *Coordinator) SetSubagentRegistry(registry *SubagentRegistry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subagentRegistry = registry
}

func (c *Coordinator) ExperienceProcessor() ExperienceProcessor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.experienceProcessor != nil {
		return c.experienceProcessor
	}
	return &defaultExperienceProcessor{c: c}
}

func (c *Coordinator) SetExperienceProcessor(processor ExperienceProcessor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.experienceProcessor = processor
}

// RuntimeServices returns the active coordinator capability bundle. The
// method is primarily an internal test seam; public construction remains
// backwards compatible through NewCoordinator.
func (c *Coordinator) RuntimeServices() RuntimeServices {
	return RuntimeServices{
		Planner:             c.Planner(),
		SessionStore:        c.SessionStore(),
		PolicyEngine:        c.PolicyEngine(),
		ContextCompiler:     c.ContextCompiler(),
		AgentPool:           c.AgentPool(),
		WorkflowEngine:      c.WorkflowEngine(),
		EventJournal:        c.EventJournal(),
		ToolResolver:        c.ToolResolver(),
		ModelRuntime:        c.ModelRuntime(),
		SubagentRegistry:    c.SubagentRegistry(),
		ExperienceProcessor: c.ExperienceProcessor(),
	}
}

// setRuntimeServices is the internal constructor-injection path. Nil fields
// retain the production default, so a test never needs to build a fake DI
// container merely to replace one service.
func (c *Coordinator) setRuntimeServices(services RuntimeServices) {
	if services.Planner != nil {
		c.SetPlanner(services.Planner)
	}
	if services.SessionStore != nil {
		c.SetSessionStore(services.SessionStore)
	}
	if services.PolicyEngine != nil {
		c.SetPolicyEngine(services.PolicyEngine)
	}
	if services.ContextCompiler != nil {
		c.SetContextCompiler(services.ContextCompiler)
	}
	if services.AgentPool != nil {
		c.SetAgentPool(services.AgentPool)
	}
	if services.WorkflowEngine != nil {
		c.SetWorkflowEngine(services.WorkflowEngine)
	}
	if services.EventJournal != nil {
		c.SetEventJournal(services.EventJournal)
	}
	if services.ToolResolver != nil {
		c.SetToolResolver(services.ToolResolver)
	}
	if services.ModelRuntime != nil {
		c.SetModelRuntime(services.ModelRuntime)
	}
	if services.SubagentRegistry != nil {
		c.SetSubagentRegistry(services.SubagentRegistry)
	}
	if services.ExperienceProcessor != nil {
		c.SetExperienceProcessor(services.ExperienceProcessor)
	}
}

func (c *Coordinator) storeSubmittedTaskResult(todoID string, res *TaskResult) {
	c.taskResultsMu.Lock()
	if c.taskResults == nil {
		c.taskResults = make(map[string]*TaskResult)
	}
	c.taskResults[todoID] = res
	c.taskResultsMu.Unlock()

	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		_ = c.taskTracker.TodoList().SetTypedResult(todoID, res)
	}
}

// clearSubmittedTaskResult removes the attempt-scoped result before an
// execution retry. The prior attempt's result remains in its execution
// receipt, but must not satisfy RequiresResult for the new worker attempt.
func (c *Coordinator) clearSubmittedTaskResult(todoID string) {
	if c == nil || todoID == "" {
		return
	}
	c.taskResultsMu.Lock()
	if c.taskResults != nil {
		delete(c.taskResults, todoID)
	}
	c.taskResultsMu.Unlock()

	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		_ = c.taskTracker.TodoList().SetTypedResult(todoID, nil)
	}
}

func (c *Coordinator) GetTaskResult(todoID string) *TaskResult {
	c.taskResultsMu.RLock()
	var res *TaskResult
	if c.taskResults != nil {
		res = c.taskResults[todoID]
	}
	c.taskResultsMu.RUnlock()
	if res != nil {
		return res
	}
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item.ID == todoID && item.TypedResult != nil {
				return item.TypedResult
			}
		}
	}
	return nil
}

// SetExecutionProfile sets the active execution profile for the coordinator.
func (c *Coordinator) SetExecutionProfile(profile ExecutionProfile) {
	if c == nil {
		return
	}
	if profile.Name == ProfileDefault && c.session != nil {
		if mode := strings.ToLower(strings.TrimSpace(c.session.Config.AcceptanceMode)); mode != "" {
			switch AcceptanceMode(mode) {
			case AcceptanceAdvisory, AcceptanceBlocking:
				profile.AcceptanceMode = AcceptanceMode(mode)
			}
		}
	}
	c.executionProfileMu.Lock()
	c.executionProfile = profile
	c.executionProfileMu.Unlock()

	if profile.DisableTaskCache {
		c.SetCachePolicy(CacheBypass)
	} else if profile.DefaultCachePolicy != "" {
		c.SetCachePolicy(profile.DefaultCachePolicy)
	}

	if profile.DisableHistoricalMemory || profile.DisableHistoricalTaskReuse {
		c.conversationHistoryMu.Lock()
		c.conversationHistory = nil
		c.conversationHistorySourceCounts = nil
		c.conversationHistorySourceOffset = 0
		c.conversationHistoryMu.Unlock()
	}
}

// ExecutionProfile returns the active execution profile for the coordinator.
func (c *Coordinator) ExecutionProfile() ExecutionProfile {
	if c == nil {
		return BuiltinProfiles()[ProfileDefault]
	}
	c.executionProfileMu.RLock()
	defer c.executionProfileMu.RUnlock()
	if c.executionProfile.Name == "" {
		return BuiltinProfiles()[ProfileDefault]
	}
	return c.executionProfile
}

// SetGoalMode sets the active goal mode for the coordinator.
func (c *Coordinator) SetGoalMode(mode GoalMode) error {
	if c == nil {
		return nil
	}
	if mode != "" {
		parsed, err := ParseGoalMode(string(mode))
		if err != nil {
			return err
		}
		mode = parsed
	}
	if mode == GoalModeOutcome {
		// Validate before changing the mode so an exploratory coordinator cannot
		// transition into outcome mode while carrying a vacuous contract. Read
		// the contract under c.mu, matching SetAcceptance's lock order.
		c.mu.RLock()
		var spec *AcceptanceSpec
		if c.acceptanceSpec != nil {
			copy := cloneAcceptanceSpec(*c.acceptanceSpec)
			spec = &copy
		}
		c.mu.RUnlock()
		if err := ValidateAcceptanceSpec(spec, string(GoalModeOutcome)); err != nil {
			return err
		}
	}
	c.goalModeMu.Lock()
	c.goalMode = mode
	c.goalModeMu.Unlock()
	return nil
}

// GoalMode returns the active goal mode for the coordinator.
func (c *Coordinator) GoalMode() GoalMode {
	if c == nil {
		return GoalModeOutcome
	}
	c.goalModeMu.RLock()
	mode := c.goalMode
	c.goalModeMu.RUnlock()
	if mode != "" {
		return mode
	}
	if c.session != nil && c.session.Config.GoalMode != "" {
		return GoalMode(c.session.Config.GoalMode)
	}
	prof := c.ExecutionProfile()
	if prof.DefaultGoalMode != "" {
		return prof.DefaultGoalMode
	}
	return GoalModeOutcome
}

func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(eval)
	}
	return filepath.Clean(abs)
}

// ValidateWorkspaceIsolationPaths verifies workspace path isolation without requiring a fully initialized Coordinator.
func ValidateWorkspaceIsolationPaths(workspace, projectDir, teamDir, teamName string, prof ExecutionProfile) error {
	if !prof.RequireWorkspaceIsolation {
		return nil
	}
	if err := ValidateWorkspaceSeparation(workspace, projectDir); err != nil {
		return fmt.Errorf("workspace separation: %w", err)
	}

	cleanWS := canonicalPath(workspace)
	cleanProject := canonicalPath(projectDir)

	if cleanWS == "" || cleanProject == "" {
		return nil
	}

	isEqualOrDescendant := func(parent, child string) bool {
		if parent == child {
			return true
		}
		rel, err := filepath.Rel(parent, child)
		if err != nil {
			return false
		}
		return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}

	// 1. Control workspace cannot equal, be inside (descendant of), or be parent of subject project root.
	if isEqualOrDescendant(cleanProject, cleanWS) {
		return fmt.Errorf("RequireWorkspaceIsolation policy violation: workspace %q is inside or equal to subject project root %q", workspace, projectDir)
	}
	if isEqualOrDescendant(cleanWS, cleanProject) {
		return fmt.Errorf("RequireWorkspaceIsolation policy violation: subject project root %q is inside workspace %q", projectDir, workspace)
	}

	// 2. Control workspace cannot equal, be inside, or be parent of team definition directory (control dir).
	if teamDir != "" && teamName != "default" {
		cleanTeamDir := canonicalPath(teamDir)
		if cleanTeamDir != "" {
			if isEqualOrDescendant(cleanTeamDir, cleanWS) {
				return fmt.Errorf("RequireWorkspaceIsolation policy violation: workspace %q is inside or equal to team definition directory %q", workspace, teamDir)
			}
			if isEqualOrDescendant(cleanWS, cleanTeamDir) {
				return fmt.Errorf("RequireWorkspaceIsolation policy violation: team definition directory %q is inside workspace %q", teamDir, workspace)
			}
		}
	}
	return nil
}

// ValidateWorkspaceIsolation verifies that the active workspace is isolated from the project root if RequireWorkspaceIsolation is set.
func (c *Coordinator) ValidateWorkspaceIsolation() error {
	if c == nil || c.session == nil {
		return nil
	}
	return ValidateWorkspaceIsolationPaths(c.session.Workspace, c.projectDir, c.session.Dir, c.session.Config.Name, c.ExecutionProfile())
}

// ValidateResourceLocks verifies capability locks and workspace availability if RequireLockedResources is set.
func (c *Coordinator) ValidateResourceLocks(ctx context.Context) error {
	if c == nil {
		return nil
	}
	prof := c.ExecutionProfile()
	if !prof.RequireLockedResources {
		return nil
	}
	if c.session != nil {
		if err := EnsureWorkspaceDirs(c.session.Workspace); err != nil {
			return fmt.Errorf("RequireLockedResources policy violation: workspace lock check failed: %w", err)
		}
	}
	reqsMap := c.capabilityRequirementsByName()
	if len(reqsMap) > 0 {
		var reqList []agent.CapabilityRequirement
		for _, r := range reqsMap {
			reqList = append(reqList, r)
		}
		results, err := c.checkCapabilityRequirements(ctx, reqList)
		if err != nil {
			return fmt.Errorf("RequireLockedResources policy violation: capability check error: %w", err)
		}
		for _, res := range results {
			if !res.Available {
				return fmt.Errorf("RequireLockedResources policy violation: capability %q probe failed: %s", res.Scope, res.Reason)
			}
		}
	}
	return nil
}

func (c *Coordinator) drainAsyncTasks() {
	if c == nil {
		return
	}
	c.asyncTasksWg.Wait()
}

func (c *Coordinator) sharedMemoryService() SharedMemoryService {
	if c == nil || c.contextRepo == nil {
		return nil
	}
	if c.sharedMemorySvc != nil {
		return c.sharedMemorySvc
	}
	c.sharedMemorySvc = NewSharedMemoryService(c.contextRepo)
	return c.sharedMemorySvc
}
