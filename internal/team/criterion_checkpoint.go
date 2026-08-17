package team

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func requiresCriterionCheckpoint(item *TodoItem) bool {
	if item == nil || len(item.Advances) == 0 {
		return false
	}
	switch DefaultExecutionContract(item.Execution).Kind {
	case ExecutionKindInteractive, ExecutionKindExternal:
		return true
	default:
		return false
	}
}

// recordCriterionCheckpoints keeps one latest checkpoint per criterion. This
// bounds session growth while retaining the only evidence recovery may trust.
func (c *Coordinator) recordCriterionCheckpoints(item *TodoItem, results []CriterionResult) {
	// A criterion checkpoint describes a completed external/interactive state.
	// Recording it while the worker is merely in progress could make a stale
	// pre-existing artifact look like proof of the in-flight operation.
	if c == nil || c.sessionData == nil || item.Status != TaskDone || !requiresCriterionCheckpoint(item) {
		return
	}
	byID := make(map[string]CriterionResult, len(results))
	for _, result := range results {
		byID[result.ID] = result
	}
	for _, id := range item.Advances {
		result, ok := byID[id]
		if !ok {
			continue
		}
		checkpoint := CriterionCheckpoint{
			ID:               fmt.Sprintf("criterion-%s-%d", id, time.Now().UnixNano()),
			TaskID:           item.ID,
			CriterionID:      id,
			Proven:           result.State == CriterionPassed,
			Evidence:         cloneVerificationResults(result.Evidence),
			InputFingerprint: result.InputFingerprint,
			ResumeAction:     "reconcile",
			ReplayPolicy:     item.Recovery,
			CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
		}
		c.sessionMu.Lock()
		filtered := c.sessionData.CriterionCheckpoints[:0]
		for _, existing := range c.sessionData.CriterionCheckpoints {
			if existing.CriterionID != id {
				filtered = append(filtered, existing)
			}
		}
		c.sessionData.CriterionCheckpoints = append(filtered, checkpoint)
		c.sessionMu.Unlock()
		c.emitEvent("criterion_checkpoint_saved", "coordinator", item.ID, map[string]interface{}{"checkpoint": checkpoint})
	}
	c.saveCheckpoint()
}

func cloneVerificationResults(src []*VerificationResult) []*VerificationResult {
	if len(src) == 0 {
		return nil
	}
	result := make([]*VerificationResult, 0, len(src))
	for _, evidence := range src {
		result = append(result, cloneVerificationResult(evidence))
	}
	return result
}

// validateCriterionCheckpoint verifies that durable proof still matches the
// current criterion contract, workspace, security configuration and revision.
// A checkpoint is intentionally not a substitute for finish-time verification.
func (c *Coordinator) validateCriterionCheckpoint(item *TodoItem) error {
	if c == nil || c.sessionData == nil || !requiresCriterionCheckpoint(item) {
		return nil
	}
	for _, criterionID := range item.Advances {
		var checkpoint CriterionCheckpoint
		found := false
		c.viewSessionData(func(sd *SessionData) {
			for i := range sd.CriterionCheckpoints {
				if sd.CriterionCheckpoints[i].CriterionID == criterionID {
					cp := sd.CriterionCheckpoints[i]
					cp.Evidence = cloneVerificationResults(cp.Evidence)
					checkpoint = cp
					found = true
					break
				}
			}
		})
		if !found || !checkpoint.Proven || len(checkpoint.Evidence) == 0 || checkpoint.InputFingerprint == "" {
			return fmt.Errorf("criterion %q has no proven checkpoint; reconciliation or human review required", criterionID)
		}
		var criterion *AcceptanceCriterion
		if c.acceptanceSpec != nil {
			for i := range c.acceptanceSpec.Criteria {
				if c.acceptanceSpec.Criteria[i].ID == criterionID {
					criterion = &c.acceptanceSpec.Criteria[i]
					break
				}
			}
		}
		if criterion == nil {
			return fmt.Errorf("criterion checkpoint %q is stale: criterion is no longer in the acceptance contract", criterionID)
		}
		spec := NormalizeVerificationSpec(criterion.Verify, "", "")
		shell, workDir := c.verificationShell(), c.verificationWorkDir()
		for _, evidence := range checkpoint.Evidence {
			if evidence == nil {
				return fmt.Errorf("criterion checkpoint %q is stale: missing evidence", criterionID)
			}
			fingerprint := ComputeVerificationFingerprintFull(spec, evidence, workDir, strconv.Itoa(c.acceptanceContractRevision), c.verificationSecurityMode(shell))
			if strings.TrimSpace(evidence.Fingerprint) != fingerprint || checkpoint.InputFingerprint != fingerprint {
				return fmt.Errorf("criterion checkpoint %q is stale: verification inputs changed", criterionID)
			}
		}
	}
	return nil
}

// revalidateRecoveryCriteria obtains fresh objective evidence after an
// interrupted external/interactive operation has been reconciled. It does not
// trust an earlier checkpoint, because the external state may have changed
// while hufu was down.
func (c *Coordinator) revalidateRecoveryCriteria(ctx context.Context, item *TodoItem) error {
	if c == nil || !requiresCriterionCheckpoint(item) {
		return nil
	}
	if c.acceptanceSpec == nil || c.sessionData == nil {
		return fmt.Errorf("no acceptance contract/session data available for checkpointed recovery")
	}
	before := make(map[string]CriterionState)
	c.viewSessionData(func(sd *SessionData) {
		for _, result := range sd.CriterionResults {
			before[result.ID] = result.State
		}
	})
	results, err := c.evaluateCriteria(ctx, c.acceptanceSpec.Criteria)
	if err != nil {
		return err
	}
	states := make(map[string]CriterionState, len(results))
	for _, result := range results {
		states[result.ID] = result.State
	}
	for _, id := range item.Advances {
		if states[id] != CriterionPassed {
			return fmt.Errorf("criterion %q is %s", id, states[id])
		}
	}

	// Recovery verification is a real criterion transition, not merely a
	// transient safety check. Publish the same durable progress state as the
	// normal task-completion path so event replay and checkout retain the fresh
	// criterion evidence and its progress timestamp.
	advanced := make([]string, 0, len(item.Advances))
	for _, id := range item.Advances {
		if before[id] != CriterionPassed {
			advanced = append(advanced, id)
		}
	}
	item.Progress = ProgressNoChange
	item.ProgressCriteria = nil
	progressedAt := ""
	if len(advanced) > 0 {
		item.Progress = ProgressAdvanced
		item.ProgressCriteria = append([]string(nil), advanced...)
		progressedAt = time.Now().UTC().Format(time.RFC3339Nano)
		var items []*TodoItem
		if c.taskTracker != nil {
			items = c.taskTracker.TodoList().Items()
		}
		c.metricsMu.Lock()
		c.antiThrashing.DiagnosticSinceProgress = 0
		c.antiThrashing.DiagnosticTasksCounted = make(map[string]bool)
		c.antiThrashing.resetAfterCriterionProgress(advanced, items)
		c.tokensSinceCriterionProgress = 0
		c.turnsSinceCriterionProgress = 0
		c.tasksSinceCriterionProgress = 0
		c.noProgressReplanTripped = false
		c.noProgressStopTripped = false
		c.reliabilityUsageByAttempt = make(map[string]int)
		c.metricsMu.Unlock()
		c.sessionMu.Lock()
		c.sessionData.LastCriterionProgressAt = progressedAt
		c.sessionMu.Unlock()
	}
	if c.taskTracker != nil {
		_ = c.taskTracker.TodoList().SetProgress(item.ID, item.Progress, item.ProgressCriteria)
	}
	c.emitEvent("criterion_re_evaluated", "coordinator", item.ID, map[string]interface{}{
		"advances": item.Advances, "progress": item.Progress,
		"progress_criteria": item.ProgressCriteria, "progressed_at": progressedAt,
		"before": before, "after": results,
	})
	return nil
}
