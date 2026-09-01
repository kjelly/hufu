package team

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
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
	// timing is package-private so Hufu-local can contribute to the existing
	// receipt timing accumulator without exposing Fantasy internals in the DTO.
	timing *taskTiming
}

type AttemptResult struct {
	Output            string
	TypedResult       *TaskResult
	Usage             ExecutionUsage
	StepsUsed         int
	StopReason        string
	ProviderSessionID string
	TranscriptRef     string
	steps             []fantasy.StepResult
	agent             fantasy.Agent
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
		Execution:      task.Execution,
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
