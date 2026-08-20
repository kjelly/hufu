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

// AcceptanceContractState distinguishes a missing contract from an explicitly
// supplied but empty contract. Both have no checks, but they carry different
// authoring/audit meaning and must not be collapsed into the same event.
type AcceptanceContractState string

const (
	AcceptanceContractUnset      AcceptanceContractState = "not_configured"
	AcceptanceContractEmpty      AcceptanceContractState = "empty"
	AcceptanceContractConfigured AcceptanceContractState = "configured"
)

func AcceptanceContractStateOf(spec *AcceptanceSpec) AcceptanceContractState {
	if spec == nil {
		return AcceptanceContractUnset
	}
	if !AcceptanceSpecHasChecks(*spec) {
		return AcceptanceContractEmpty
	}
	return AcceptanceContractConfigured
}

// AcceptanceSpecHasChecks reports whether an AcceptanceSpec contains any non-empty verification commands,
// required artifacts, verifications, criteria, or unresolved task check requirement.
func AcceptanceSpecHasChecks(spec AcceptanceSpec) bool {
	if spec.RequireNoUnresolvedTasks {
		return true
	}
	for _, command := range spec.Commands {
		if strings.TrimSpace(command) != "" {
			return true
		}
	}
	for _, path := range spec.RequiredArtifacts {
		if strings.TrimSpace(path) != "" {
			return true
		}
	}
	return len(spec.Verifications) > 0 || len(spec.Criteria) > 0
}

// ValidateAcceptanceSpec validates an AcceptanceSpec against the target goalMode.
// In outcome mode ("outcome"), an empty or missing acceptance contract is invalid
// and returns an acceptance_vacuous error because run-level completion cannot be achieved.
func ValidateAcceptanceSpec(spec *AcceptanceSpec, goalMode string) error {
	mode, err := ParseGoalMode(goalMode)
	if err != nil {
		mode = GoalModeOutcome
	}
	if mode != GoalModeOutcome {
		return nil
	}
	if spec == nil || !AcceptanceSpecHasChecks(*spec) {
		return fmt.Errorf("%s: empty acceptance contract in outcome mode", FindingAcceptanceVacuous)
	}
	if len(spec.Criteria) > 0 {
		if err := validateAcceptanceCriteria(spec.Criteria); err != nil {
			return err
		}
	}
	return nil
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
				var vr *VerificationResult
				var err error
				if spec.Type == VerifyWorksetComplete {
					vr, err = c.executeWorksetCompleteVerification(ctx, spec)
				} else {
					vr, err = ExecuteVerificationSpec(ctx, shell, workDir, spec)
				}
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
		c.sessionMu.Lock()
		c.sessionData.CriterionResults = results
		c.sessionMu.Unlock()
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
	progressedAt := ""
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
		c.tokensSinceCriterionProgress = 0
		c.turnsSinceCriterionProgress = 0
		c.tasksSinceCriterionProgress = 0
		c.noProgressReplanTripped = false
		c.noProgressStopTripped = false
		c.reliabilityUsageByAttempt = make(map[string]int)
		c.metricsMu.Unlock()
		if c.sessionData != nil {
			progressedAt = time.Now().UTC().Format(time.RFC3339Nano)
			c.sessionMu.Lock()
			c.sessionData.LastCriterionProgressAt = progressedAt
			c.sessionMu.Unlock()
		}
	} else if item.Kind == TaskKindDiagnostic && item.Status == TaskDone {
		c.recordDiagnosticCompletion(item)
	}
	if c.taskTracker != nil {
		_ = c.taskTracker.TodoList().SetProgress(item.ID, item.Progress, item.ProgressCriteria)
	}
	c.emitEvent("criterion_re_evaluated", "coordinator", item.ID, map[string]interface{}{"advances": item.Advances, "progress": item.Progress, "progress_criteria": item.ProgressCriteria, "progressed_at": progressedAt, "before": before, "after": results})
	c.recordCriterionCheckpoints(item, results)
	if c.reportStatus != nil {
		metrics := c.Metrics()
		c.report(c.newEvent("reliability_metrics").withMessage(fmt.Sprintf("criteria passed: %d; diagnostics since progress: %d; tokens since progress: %d", metrics.AcceptanceCriteriaPassed, metrics.DiagnosticTasksSinceProgress, metrics.TokensSinceCriterionProgress)).withTodoID(item.ID))
	}
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
		// Older planners omitted kind. In outcome mode that must not quietly
		// downgrade a mutation/outcome task into an ungoverned generic task.
		// Sidecars remain auxiliary by definition; every other untyped task is
		// treated as an outcome task and therefore needs semantic acceptance.
		//
		// The inference requires an acceptance contract to point at. Outcome is
		// the default goal mode, so inferring it with no criteria configured
		// would demand a link that cannot exist and reject every task in the
		// plan — which is exactly the shape of a `--default` run, where
		// acceptance is reported as not_configured. A task that declares
		// kind: outcome explicitly is still validated below.
		kind := task.Kind
		if kind == "" && !task.Sidecar && len(criteria) > 0 {
			kind = TaskKindOutcome
		}
		if kind == TaskKindDiagnostic && strings.TrimSpace(task.ExpectedStateChange) == "" {
			return fmt.Errorf("diagnostic task %q must state the uncertainty it resolves", task.Goal)
		}
		if kind != TaskKindOutcome && kind != TaskKindRepair {
			continue
		}
		if len(task.Advances) == 0 {
			return fmt.Errorf("%s task %q must reference at least one acceptance criterion", kind, task.Goal)
		}
		if task.Verify == "" && task.VerifySpec == nil {
			return fmt.Errorf("%s task %q must include an objective verifier for its acceptance criterion", kind, task.Goal)
		}
		// Existence is useful artifact evidence, but it cannot prove that an
		// outcome changed. Require a command or JSON assertion for work that
		// claims to advance an acceptance criterion; this blocks false successes
		// such as `test -s hosts.yml` for a task that promises correct roles.
		spec := VerificationSpec{}
		if task.VerifySpec != nil {
			spec = *task.VerifySpec
		}
		spec = NormalizeVerificationSpec(spec, task.Verify, task.VerifyMode)
		if isArtifactOnlyOutcomeVerifier(spec) {
			return fmt.Errorf("%s task %q has artifact-only verifier for an acceptance outcome; use command_exit or json_assert", kind, task.Goal)
		}
		for _, id := range task.Advances {
			if !criteria[id] {
				return fmt.Errorf("task %q advances unknown criterion %q", task.Goal, id)
			}
		}
	}
	return nil
}

// normalizeOutcomeTaskKinds closes a legacy planner escape hatch before the
// task enters the scheduler. In outcome mode an omitted kind used to survive
// as a generic task, which let it bypass outcome-only retry routing even when
// it claimed to advance an acceptance criterion. Sidecars are auxiliary by
// definition and intentionally retain their empty kind.
func (c *Coordinator) normalizeOutcomeTaskKinds(tasks []TaskDef) {
	if c == nil || c.session == nil {
		return
	}
	mode, err := ParseGoalMode(c.session.Config.GoalMode)
	if err != nil || mode != GoalModeOutcome {
		return
	}
	// Only promote when there is an acceptance contract for an outcome task to
	// advance; see validateTaskCriterionLinks for why inferring it without one
	// makes every untyped task unschedulable.
	if c.acceptanceSpec == nil || len(c.acceptanceSpec.Criteria) == 0 {
		return
	}
	for i := range tasks {
		if tasks[i].Kind == "" && !tasks[i].Sidecar {
			tasks[i].Kind = TaskKindOutcome
		}
	}
}

// isArtifactOnlyOutcomeVerifier rejects verifiers that only establish that a
// local artifact is present. They are valid supporting evidence, but they
// cannot prove the task's claimed acceptance outcome (for example, that a
// generated inventory contains the required roles). Keep the shell matching
// deliberately narrow: uncertain commands remain valid command_exit checks
// and are still guarded by the linked acceptance criterion evaluation.
func isArtifactOnlyOutcomeVerifier(spec VerificationSpec) bool {
	if spec.Mode == "observation" || spec.Type == VerifyFileExists || spec.Type == VerifyFileAbsent {
		return true
	}
	if spec.Type != VerifyCommandExit {
		return false
	}
	fields := strings.Fields(strings.TrimSpace(spec.Command))
	if len(fields) == 3 && fields[0] == "test" && isArtifactTestFlag(fields[1]) {
		return true
	}
	return len(fields) == 4 && fields[0] == "[" && isArtifactTestFlag(fields[1]) && fields[3] == "]"
}

func isArtifactTestFlag(flag string) bool {
	switch flag {
	case "-e", "-f", "-d", "-s", "-r", "-w", "-x":
		return true
	default:
		return false
	}
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
