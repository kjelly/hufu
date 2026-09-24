package team

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/execution"
)

// Reasons a provider failure did not fall back, recorded on the attempt's
// receipt.
const (
	fallbackDeniedNotInFallbackOn = "failure_class_not_in_fallback_on"
	fallbackDeniedNoNextCandidate = "no_next_candidate"
	fallbackDeniedSideEffects     = "side_effects_executed"
	fallbackDeniedCallsUnrecorded = "executed_calls_unrecorded"
	fallbackDeniedBudgetExhausted = "budget_exhausted"
	fallbackDeniedCancelled       = "cancelled"
)

// executionFallbackState is one dispatch's position in its execution route.
// Every dispatch starts at the primary, and a normal retry returns to it;
// only a fallback moves to the next candidate. The occurrence's frozen
// ExecutionTarget never changes: the target an attempt ran on is recorded on
// its receipt.
type executionFallbackState struct {
	route *ExecutionRouteBinding
	index int
	used  int
	// pending is set when the next attempt is a fallback.
	pending      bool
	from         execution.ExecutionTarget
	failureClass ProviderFailureClass
	// lastAttempt and lastFallback let a resumed attempt (same number)
	// keep its candidate.
	lastAttempt  int
	lastFallback bool
}

func newExecutionFallbackState(route *ExecutionRouteBinding) *executionFallbackState {
	return &executionFallbackState{route: route}
}

// beginAttempt starts an attempt and reports whether it is a fallback.
func (s *executionFallbackState) beginAttempt(attempt int) bool {
	if attempt == s.lastAttempt {
		return s.lastFallback
	}
	fallback := s.pending
	s.pending = false
	if !fallback {
		s.index = 0
	}
	s.lastAttempt, s.lastFallback = attempt, fallback
	return fallback
}

// target is the candidate the current attempt runs on.
func (s *executionFallbackState) target(primary execution.ExecutionTarget) execution.ExecutionTarget {
	if s.route == nil || s.index >= len(s.route.Candidates) {
		return primary
	}
	return s.route.Candidates[s.index]
}

func (s *executionFallbackState) candidateIndex() *int {
	if s.route == nil {
		return nil
	}
	index := s.index
	return &index
}

// executionFallbackInput is the evidence a fallback decision is made from.
type executionFallbackInput struct {
	Err error
	// AttemptContextErr is the attempt's own context error: a task timeout or
	// a cancelled round is never a provider failure.
	AttemptContextErr error
	ParentContextErr  error
	SideEffect        SideEffectClass
	// Isolated means the attempt ran in an isolated world that was never
	// applied, so its writes are discarded with it.
	Isolated bool
	// Recorder is the attempt's executed-call record; nil means no record
	// exists, which never allows a fallback past a side effect.
	Recorder       *executedToolCallRecorder
	BudgetExceeded bool
	// ResultRepair means the worker's model call finished and the attempt
	// went through result-only repair; a repair failure never falls back.
	ResultRepair bool
}

// decide classifies an attempt's failure and decides whether the next
// attempt falls back to the next candidate. It returns the provider failure
// class ("" when the failure is not a provider failure), whether to fall
// back, and, when a provider failure did not fall back, why.
func (s *executionFallbackState) decide(in executionFallbackInput) (ProviderFailureClass, bool, string) {
	if s.route == nil || len(s.route.Candidates) < 2 || in.Err == nil || in.AttemptContextErr != nil || in.ResultRepair {
		return "", false, ""
	}
	if in.ParentContextErr != nil {
		return "", false, fallbackDeniedCancelled
	}
	class := ClassifyProviderError(in.Err)
	if class == "" || class == ProviderOther {
		return "", false, ""
	}
	switch {
	case !slices.Contains(s.route.FallbackOn, class):
		return class, false, fallbackDeniedNotInFallbackOn
	case s.index+1 >= len(s.route.Candidates) || s.used >= len(s.route.Candidates)-1:
		return class, false, fallbackDeniedNoNextCandidate
	}
	if reason := fallbackSideEffectDenial(in); reason != "" {
		return class, false, reason
	}
	if in.BudgetExceeded {
		return class, false, fallbackDeniedBudgetExhausted
	}
	s.from = s.route.Candidates[s.index]
	s.failureClass = class
	s.index++
	s.used++
	s.pending = true
	return class, true, ""
}

// fallbackSideEffectDenial allows a new candidate only when the failed
// attempt left nothing behind that the new one would repeat.
func fallbackSideEffectDenial(in executionFallbackInput) string {
	if in.SideEffect == SideEffectNone || in.Isolated {
		return ""
	}
	if in.Recorder == nil {
		return fallbackDeniedCallsUnrecorded
	}
	calls := in.Recorder.snapshot()
	if in.SideEffect == SideEffectWorkspaceWrite {
		for _, call := range calls {
			if !isReadOnlyToolCall(call.name, call.input) {
				return fallbackDeniedSideEffects
			}
		}
		return ""
	}
	// external_write, infra_mutation, credential_mutation, unknown, and an
	// unclassified effect fall back only when no call ran at all.
	if len(calls) > 0 {
		return fallbackDeniedSideEffects
	}
	return ""
}

// recordExecutionFallback appends execution_fallback_decided before the
// fallback attempt starts.
func (c *Coordinator) recordExecutionFallback(ctx context.Context, todoID, agentName string, fromAttempt int, s *executionFallbackState) error {
	to := s.route.Candidates[s.index]
	c.report(c.newEvent("step").withAgent(agentName).withTodoID(todoID).
		withMessage(fmt.Sprintf("falling back from %s to %s after %s (execution route %q)", s.from, to, s.failureClass, s.route.Name)))
	if !c.hasDurableEventJournal() {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"task_id": todoID, "occurrence_attempt": c.taskAttempt(todoID), "from_attempt": fromAttempt,
		"from_target": s.from, "to_target": to, "candidate_index": s.index,
		"failure_class": s.failureClass, "route": s.route.Name,
	})
	if err != nil {
		return fmt.Errorf("encode execution fallback: %w", err)
	}
	if _, err := c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{
		Type: string(EventExecutionFallbackDecided), Actor: "execution-route", TaskID: todoID, Attempt: fromAttempt, Payload: payload,
	}); err != nil {
		return fmt.Errorf("record execution fallback: %w", err)
	}
	return nil
}

// routeCandidateAllowed reports whether an attempt's target (or, for a
// targetless request, its provider model ID) is one of the occurrence's
// frozen route candidates.
func (c *Coordinator) routeCandidateAllowed(route *ExecutionRouteBinding, target execution.ExecutionTarget, modelID string) bool {
	if route == nil {
		return false
	}
	for _, candidate := range route.Candidates {
		if !target.IsZero() && target == candidate {
			return true
		}
		if target.IsZero() && modelID != "" && modelID == c.executionModelIDForTarget(candidate, candidate.Model) {
			return true
		}
	}
	return false
}

func budgetExceededNow(c *Coordinator) bool {
	exceeded, _ := c.budgetExceeded()
	return exceeded
}

// setAttemptExecutionTarget records the target a task's current attempt runs
// on, so the execution-event shadow reports it instead of the frozen primary.
func (c *Coordinator) setAttemptExecutionTarget(taskID string, target execution.ExecutionTarget) {
	if c == nil || taskID == "" || target.IsZero() {
		return
	}
	c.attemptExecutionTargets.Store(taskID, target)
}

func (c *Coordinator) clearAttemptExecutionTarget(taskID string) {
	if c != nil {
		c.attemptExecutionTargets.Delete(taskID)
	}
}

func (c *Coordinator) attemptExecutionTarget(taskID string) (execution.ExecutionTarget, bool) {
	if c == nil {
		return execution.ExecutionTarget{}, false
	}
	value, ok := c.attemptExecutionTargets.Load(taskID)
	if !ok {
		return execution.ExecutionTarget{}, false
	}
	target, ok := value.(execution.ExecutionTarget)
	return target, ok
}

// accumulateExecutionFallbackMetrics counts the execution-route fallbacks of
// this run from their durable events. Each invocation starts a new run ID, so
// fallbacks from an earlier run are not counted.
func (c *Coordinator) accumulateExecutionFallbackMetrics(metrics *RunMetrics) {
	if c == nil || c.eventStore == nil || metrics == nil {
		return
	}
	events, err := c.eventStore.QueryEvents(EventQuery{RunID: c.executionRunID, Types: []string{string(EventExecutionFallbackDecided)}})
	if err != nil {
		return
	}
	for _, event := range events {
		var payload struct {
			FailureClass ProviderFailureClass `json:"failure_class"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		metrics.WorkerFallbacksTotal++
		if metrics.WorkerFallbacksByClass == nil {
			metrics.WorkerFallbacksByClass = make(map[ProviderFailureClass]int)
		}
		metrics.WorkerFallbacksByClass[payload.FailureClass]++
	}
}
