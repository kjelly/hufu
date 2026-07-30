package team

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type CriterionState string

const (
	CriterionPending CriterionState = "pending"
	CriterionPassed  CriterionState = "passed"
	CriterionFailed  CriterionState = "failed"
	CriterionBlocked CriterionState = "blocked"
)

// CriterionResult is persisted in SessionData, making criterion state and its
// bounded verifier evidence available after restart and branch checkout.
type CriterionResult struct {
	ID               string                `json:"id"`
	State            CriterionState        `json:"state"`
	Evidence         []*VerificationResult `json:"evidence,omitempty"`
	EvaluatedAt      time.Time             `json:"evaluated_at"`
	InputFingerprint string                `json:"input_fingerprint,omitempty"`
	FailureReason    string                `json:"failure_reason,omitempty"`
}

func validateAcceptanceCriteria(criteria []AcceptanceCriterion) error {
	seen := make(map[string]bool, len(criteria))
	for _, criterion := range criteria {
		if strings.TrimSpace(criterion.ID) == "" {
			return fmt.Errorf("acceptance criterion requires an id")
		}
		if seen[criterion.ID] {
			return fmt.Errorf("duplicate acceptance criterion id %q", criterion.ID)
		}
		seen[criterion.ID] = true
		if err := validateVerificationSpec(NormalizeVerificationSpec(criterion.Verify, "", "")); err != nil {
			return fmt.Errorf("criterion %q: %w", criterion.ID, err)
		}
	}
	for _, criterion := range criteria {
		for _, dep := range criterion.DependsOn {
			if !seen[dep] {
				return fmt.Errorf("criterion %q depends on unknown criterion %q", criterion.ID, dep)
			}
		}
	}
	return nil
}

func (c *Coordinator) evaluateCriteria(ctx context.Context, criteria []AcceptanceCriterion) ([]CriterionResult, error) {
	if err := validateAcceptanceCriteria(criteria); err != nil {
		return nil, err
	}
	ordered := append([]AcceptanceCriterion(nil), criteria...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	byID := make(map[string]CriterionResult, len(ordered))
	results := make([]CriterionResult, 0, len(ordered))
	for len(results) < len(ordered) {
		progress := false
		for _, criterion := range ordered {
			if _, done := byID[criterion.ID]; done {
				continue
			}
			ready, blocked := true, false
			for _, dep := range criterion.DependsOn {
				depResult, ok := byID[dep]
				if !ok {
					ready = false
					break
				}
				if depResult.State != CriterionPassed {
					blocked = true
				}
			}
			if !ready {
				continue
			}
			result := CriterionResult{ID: criterion.ID, EvaluatedAt: time.Now().UTC()}
			if blocked {
				result.State = CriterionBlocked
				result.FailureReason = "dependency criterion did not pass"
			} else {
				spec := NormalizeVerificationSpec(criterion.Verify, "", "")
				shell := c.verificationShell()
				workDir := c.verificationWorkDir()
				vr, err := ExecuteVerificationSpec(ctx, shell, workDir, spec)
				if vr != nil {
					result.Evidence = []*VerificationResult{vr}
					vr.Fingerprint = ComputeVerificationFingerprintFull(spec, vr, workDir,
						strconv.Itoa(c.acceptanceContractRevision), c.verificationSecurityMode(shell))
					result.InputFingerprint = vr.Fingerprint
				}
				if err != nil || spec.Mode == "observation" {
					result.State = CriterionFailed
					if err != nil {
						result.FailureReason = err.Error()
					} else {
						result.FailureReason = "observation cannot satisfy criterion"
					}
				} else {
					result.State = CriterionPassed
				}
			}
			byID[result.ID] = result
			results = append(results, result)
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("acceptance criteria contain a dependency cycle")
		}
	}
	if c.sessionData != nil {
		c.sessionData.CriterionResults = results
	}
	return results, nil
}

func (c *Coordinator) verificationShell() string {
	if c != nil && c.session != nil && c.session.Config.Shell != "" {
		return c.session.Config.Shell
	}
	return "sh"
}

func (c *Coordinator) verificationSecurityMode(shell string) string {
	if c != nil && c.session != nil {
		return fmt.Sprintf("profile=%s;no-net=%t;force-mcp=%t;shell=%s",
			c.session.Config.ExecutionProfile, c.session.Config.NoNet, c.session.Config.ForceMCP, shell)
	}
	return fmt.Sprintf("profile=;no-net=false;force-mcp=false;shell=%s", shell)
}

func (c *Coordinator) reEvaluateAffectedCriteria(ctx context.Context, item *TodoItem) {
	if c == nil || item == nil {
		return
	}
	if len(item.Advances) == 0 {
		c.recordDiagnosticCompletion(item)
		return
	}
	if item.Kind == TaskKindRepair && item.Status == TaskDone && item.Progress == ProgressUnknown {
		limits := c.reliabilityConfig()
		repairLimitReached := false
		c.metricsMu.Lock()
		if c.antiThrashing.RepairsByCriterion == nil {
			c.antiThrashing.RepairsByCriterion = make(map[string]int)
		}
		attempts := repairAttemptCount(item)
		failureEvidence := failureOccurrenceCount(item)
		newSuccessfulAttempts := attempts - failureEvidence
		if newSuccessfulAttempts < 1 {
			newSuccessfulAttempts = 1
		}
		for _, id := range item.Advances {
			c.antiThrashing.RepairsByCriterion[id] += newSuccessfulAttempts
			if limits.HardEnforcement && limits.MaxRepairsPerCriterion > 0 && c.antiThrashing.RepairsByCriterion[id] >= limits.MaxRepairsPerCriterion {
				c.antiThrashing.markBlockedCriterion(id, item.Kind, FailureFingerprint{})
				repairLimitReached = true
			}
		}
		c.metricsMu.Unlock()
		if repairLimitReached {
			c.emitEvent("anti_thrashing_limit_reached", "coordinator", item.ID, map[string]interface{}{
				"limit":   "max-repairs-per-criterion",
				"count":   c.Metrics().RepairAttemptsByCriterion,
				"warning": true,
			})
		}
	}
	if c.acceptanceSpec == nil || c.sessionData == nil {
		c.recordDiagnosticCompletion(item)
		return
	}
	before := make(map[string]CriterionState)
	for _, r := range c.sessionData.CriterionResults {
		before[r.ID] = r.State
	}
	results, err := c.evaluateCriteria(ctx, c.acceptanceSpec.Criteria)
	if err != nil {
		return
	}
	advancedCriteria := make([]string, 0, len(item.Advances))
	for _, id := range item.Advances {
		for _, r := range results {
			if r.ID == id && r.State == CriterionPassed && before[id] != CriterionPassed {
				advancedCriteria = append(advancedCriteria, id)
				break
			}
		}
	}
	item.Progress = ProgressNoChange
	item.ProgressCriteria = nil
	if len(advancedCriteria) > 0 {
		item.Progress = ProgressAdvanced
		item.ProgressCriteria = append([]string(nil), advancedCriteria...)
		var items []*TodoItem
		if c.taskTracker != nil {
			items = c.taskTracker.TodoList().Items()
		}
		c.metricsMu.Lock()
		c.antiThrashing.DiagnosticSinceProgress = 0
		c.antiThrashing.DiagnosticTasksCounted = make(map[string]bool)
		c.antiThrashing.resetAfterCriterionProgress(advancedCriteria, items)
		c.metricsMu.Unlock()
	} else if item.Kind == TaskKindDiagnostic && item.Status == TaskDone {
		c.recordDiagnosticCompletion(item)
	}
	if c.taskTracker != nil {
		_ = c.taskTracker.TodoList().SetProgress(item.ID, item.Progress, item.ProgressCriteria)
	}
	c.emitEvent("criterion_re_evaluated", "coordinator", item.ID, map[string]interface{}{"advances": item.Advances, "progress": item.Progress, "progress_criteria": item.ProgressCriteria, "before": before, "after": results})
}

func (c *Coordinator) validateTaskCriterionLinks(tasks []TaskDef) error {
	if c == nil || c.session == nil {
		return nil
	}
	mode, err := ParseGoalMode(c.session.Config.GoalMode)
	if err != nil || mode != GoalModeOutcome {
		return nil
	}
	criteria := map[string]bool{}
	if c.acceptanceSpec != nil {
		for _, criterion := range c.acceptanceSpec.Criteria {
			criteria[criterion.ID] = true
		}
	}
	for _, task := range tasks {
		if task.Kind == TaskKindDiagnostic && strings.TrimSpace(task.ExpectedStateChange) == "" {
			return fmt.Errorf("diagnostic task %q must state the uncertainty it resolves", task.Goal)
		}
		if task.Kind != TaskKindOutcome && task.Kind != TaskKindRepair {
			continue
		}
		if len(task.Advances) == 0 {
			return fmt.Errorf("%s task %q must reference at least one acceptance criterion", task.Kind, task.Goal)
		}
		for _, id := range task.Advances {
			if !criteria[id] {
				return fmt.Errorf("task %q advances unknown criterion %q", task.Goal, id)
			}
		}
	}
	return nil
}

// criterionRetryTargets returns repair/outcome tasks that explicitly advance
// one of the failed criteria. Keeping this routing deterministic prevents a
// retry from being selected solely by narrative similarity or task ID.
func criterionRetryTargets(tasks []TaskDef, failed []string) []int {
	failedSet := make(map[string]struct{}, len(failed))
	for _, id := range failed {
		if id = strings.TrimSpace(id); id != "" {
			failedSet[id] = struct{}{}
		}
	}
	var targets []int
	for i, task := range tasks {
		if task.Kind != TaskKindRepair && task.Kind != TaskKindOutcome {
			continue
		}
		for _, id := range task.Advances {
			if _, ok := failedSet[id]; ok {
				targets = append(targets, i)
				break
			}
		}
	}
	return targets
}

func (c *Coordinator) failedCriteriaForTask(task TaskDef) []string {
	if c == nil || c.sessionData == nil {
		return append([]string(nil), task.Advances...)
	}
	states := make(map[string]CriterionState, len(c.sessionData.CriterionResults))
	for _, result := range c.sessionData.CriterionResults {
		states[result.ID] = result.State
	}
	failed := make([]string, 0, len(task.Advances))
	for _, id := range task.Advances {
		if states[id] != CriterionPassed {
			failed = append(failed, id)
		}
	}
	return failed
}
