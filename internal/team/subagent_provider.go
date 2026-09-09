package team

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// TaskResultSink is task-scoped: an attempt may submit evidence, but cannot
// transition task status, verify a deliverable, retry itself, or accept a run.
type TaskResultSink interface {
	Submit(context.Context, string, TaskResult) error
}

type AttemptRequest struct {
	RunID, BranchID, TaskID string
	Attempt                 int
	Agent                   *agent.AgentDef
	Task                    TaskDef
	Prompt                  string
	ModelID                 string
	MaxSteps                int
	Timeout                 time.Duration
	Tools                   ResolvedWorkerTools
	History                 []fantasy.Message
	// ExecutionTarget and BackendBinding are the canonical immutable target
	// and mutable session evidence consumed by ExecutionBackend. They are the
	// only execution identity fields the scheduler populates.
	ExecutionTarget execution.ExecutionTarget
	BackendBinding  *BackendBinding
	// Provider and ProviderBinding remain adapter-only compatibility fields for
	// legacy SubagentProvider implementations. ExecutionRegistry adapters fill
	// them from the canonical fields immediately before invoking that interface.
	Provider        string
	ProviderBinding *ProviderBinding
	// timing is package-private so Hufu-local can contribute to the existing
	// receipt timing accumulator without exposing Fantasy internals in the DTO.
	timing *taskTiming
}

type AttemptResult struct {
	// TypedResult is hufu-local compatibility only: its submit_result tool
	// stores the canonical result as a side effect of the run itself
	// (submitResultTool → storeSubmittedTaskResult), and this field is just a
	// convenience copy of that already-stored result. An external provider
	// MUST leave it nil (§8.3) — the coordinator never reads TypedResult from
	// the AttemptResult an external provider returns.
	Output      string
	TypedResult *TaskResult
	// ResultProposal is an external provider's untrusted raw final response,
	// kept for diagnostics/audit (spec.md §8.3, §9.1). It is nil for
	// hufu-local.
	ResultProposal *WorkerResultProposal
	// CanonicalResult is the Hufu-owned result an external provider computed
	// via ExternalResultCanonicalizer from ResultProposal and its own
	// observed WorkspaceDelta (§9.3, §19, PR-11's "canonicalize" step). It is
	// Hufu Go code producing this, not the untrusted provider process, so it
	// does not violate INV-05 the way a provider-populated TypedResult would.
	// The coordinator's dispatch code stores it via the same
	// storeSubmittedTaskResult path hufu-local's submit_result tool uses, so
	// both providers converge on one receipt/verification/completion
	// pipeline. Nil for hufu-local.
	CanonicalResult *TaskResult
	// WorkspaceDelta is the actually-observed change for this attempt, when
	// an external provider's ExecutionWorld captured one — populated even on
	// a failed/cancelled attempt when a final snapshot was possible (§16.3:
	// "post-cancel workspace snapshot"). Zero value for hufu-local.
	WorkspaceDelta    WorkspaceDelta
	Usage             ExecutionUsage
	StepsUsed         int
	StopReason        string
	ProviderSessionID string
	// ProviderTurnID is the last active provider turn, diagnostic only
	// (mirrors ProviderBinding.TurnID's doc comment).
	ProviderTurnID string
	TranscriptRef  string
	steps          []fantasy.StepResult
	agent          fantasy.Agent
}

type AttemptRunner interface {
	RunAttempt(context.Context, AttemptRequest) (AttemptResult, error)
}

type canonicalWorkerAttemptContext struct {
	Todo  *TodoItem
	Task  TaskDef
	Agent *agent.AgentDef
	Mode  WorkerToolResolutionMode
}

// canonicalWorkerAttemptContextForID resolves every authorization input from
// the durable task projection. AttemptRequest is only an execution DTO; it
// cannot choose a different agent, contract, workset, or lifecycle surface.
func (c *Coordinator) canonicalWorkerAttemptContextForID(taskID string) (*canonicalWorkerAttemptContext, error) {
	if c == nil {
		return nil, fmt.Errorf("canonical worker attempt context: coordinator is unavailable")
	}
	todo := c.todoItemByID(taskID)
	if todo == nil {
		return nil, fmt.Errorf("canonical worker attempt context: Todo %q does not exist", taskID)
	}
	if todo.PlanID != "" && !todo.PlanFirst {
		return nil, fmt.Errorf("canonical worker attempt context: Todo %q has an invalid plan lifecycle", taskID)
	}
	if todo.Status == TaskPlanned && !todo.PlanFirst {
		return nil, fmt.Errorf("canonical worker attempt context: Todo %q has an ambiguous legacy planned lifecycle", taskID)
	}
	agentDef, _, err := c.AgentPool().ResolveAgentName(todo.Agent)
	if err != nil {
		return nil, fmt.Errorf("canonical worker attempt context: resolve agent for Todo %q: %w", taskID, err)
	}
	if agentDef == nil {
		return nil, fmt.Errorf("canonical worker attempt context: agent for Todo %q is unavailable", taskID)
	}
	task := taskDefFromTodoItem(todo)
	return &canonicalWorkerAttemptContext{
		Todo:  todo,
		Task:  task,
		Agent: agentDef,
		Mode:  workerToolResolutionModeForTask(task),
	}, nil
}

func workerToolResolutionTaskProjection(task TaskDef) workerToolResolutionTaskProjectionValue {
	return workerToolResolutionTaskProjectionValue{
		Agent:          strings.ToLower(strings.TrimSpace(task.Agent)),
		ContractID:     task.ContractID,
		PlanFirst:      task.PlanFirst,
		PlanID:         task.PlanID,
		Execution:      cloneExecutionContract(task.Execution),
		WorksetBinding: cloneWorksetBinding(task.WorksetBinding),
	}
}

type workerToolResolutionTaskProjectionValue struct {
	Agent          string
	ContractID     string
	PlanFirst      bool
	PlanID         string
	Execution      ExecutionContract
	WorksetBinding *WorksetBinding
}

func workerToolResolutionTaskMatches(left, right TaskDef) bool {
	return reflect.DeepEqual(workerToolResolutionTaskProjection(left), workerToolResolutionTaskProjection(right))
}

func workerAgentResolutionAssertionMatches(left, right *agent.AgentDef) bool {
	return left != nil && right != nil && reflect.DeepEqual(left, right)
}

type SubagentCapabilities struct {
	SupportsHufuTools   bool `json:"supports_hufu_tools"`
	SupportsTypedResult bool `json:"supports_typed_result"`
	SupportsActivities  bool `json:"supports_activities"`
	SupportsResumeToken bool `json:"supports_resume_token"`
}

// SubagentProvider is intentionally narrower than Coordinator. A provider
// executes one already-authorized attempt and reports evidence; Hufu retains
// retry, recovery, verification, receipts, completion, and memory policy.
type SubagentProvider interface {
	AttemptRunner
	Name() string
	Capabilities() SubagentCapabilities
}
