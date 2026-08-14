package team

import (
	"context"
	"fmt"
	"time"
)

// RunFinalizationInput is an immutable snapshot of the facts a terminal run
// decision may use. It deliberately carries references to evidence/context,
// never transcripts or memory content.
type RunFinalizationInput struct {
	RunID      string
	Result     *RunResult
	Acceptance *AcceptanceResult
	Evidence   *EvidenceManifest
	Tasks      []TodoItem
	BranchID   string
}

// FinalizeRun is the common terminal path for coordinator finish, direct
// agents, and other non-tool completion paths. CompletionGate remains the
// only acceptance authority; the experience processor only proposes or
// confirms/rejects candidates based on that decision.
func (c *Coordinator) FinalizeRun(ctx context.Context, result *RunResult, acceptance *AcceptanceResult) *RunResult {
	if c == nil || result == nil {
		return result
	}
	finalCtx := ctx
	if finalCtx == nil || finalCtx.Err() != nil {
		var cancel context.CancelFunc
		finalCtx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
	}
	c.drainAsyncTasks()
	result.Acceptance = acceptance
	// Some terminal paths have no finish tool (for example cancellation and
	// LLM-free unresolved-task fallback). They must still receive the same
	// immutable evidence boundary before CompletionGate and experience policy.
	// Interactive finish/direct paths may have sealed it already; do not create
	// a second manifest for the same terminal decision.
	c.lastEvidenceManifestMu.RLock()
	manifest := c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	if manifest == nil && c.session != nil && c.session.Workspace != "" {
		if err := c.finalizeEvidenceManifest(finalCtx, acceptance); err != nil {
			downgradeRunForFinalizationError(result, fmt.Errorf("finalize evidence manifest: %w", err))
		} else {
			c.lastEvidenceManifestMu.RLock()
			manifest = c.lastEvidenceManifest
			c.lastEvidenceManifestMu.RUnlock()
		}
	}
	result.EvidenceManifest = manifest
	input := c.runFinalizationInput(result, acceptance)
	if err := c.ExperienceProcessor().Prepare(finalCtx, input); err != nil {
		downgradeRunForFinalizationError(result, fmt.Errorf("prepare experience: %w", err))
	}
	result = c.applyCompletionGate(finalCtx, result, acceptance)
	c.SetLastRunResult(result)
	return result
}

func (c *Coordinator) runFinalizationInput(result *RunResult, acceptance *AcceptanceResult) RunFinalizationInput {
	input := RunFinalizationInput{Result: result, Acceptance: acceptance}
	if c == nil {
		return input
	}
	input.RunID = c.executionRunID
	input.BranchID = c.activeBranchID()
	c.lastEvidenceManifestMu.RLock()
	input.Evidence = c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item != nil {
				input.Tasks = append(input.Tasks, *item)
			}
		}
	}
	return input
}

func downgradeRunForFinalizationError(result *RunResult, err error) {
	if result == nil || err == nil {
		return
	}
	result.Outcome = RunOutcomePartial
	result.GoalSatisfied = false
	result.StopReason = StopReasonEvidenceIncomplete
	result.ExitCode = 7
	result.Reason = err.Error()
}
