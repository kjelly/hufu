package team

import (
	"context"
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
