package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FailureFingerprint identifies an equivalent failure independently of the
// task ID that happened to observe it.
type FailureFingerprint struct {
	CriterionID     string           `json:"criterion_id,omitempty"`
	Component       string           `json:"component,omitempty"`
	Operation       string           `json:"operation,omitempty"`
	Class           TaskFailureClass `json:"class"`
	NormalizedError string           `json:"normalized_error"`
	Digest          string           `json:"digest"`
	Occurrences     int              `json:"occurrences,omitempty"`
}

type RecoveryStrategy string

const (
	RecoveryStrategyRetry         RecoveryStrategy = "retry"
	RecoveryStrategyReflection    RecoveryStrategy = "reflection"
	RecoveryStrategyModelEscalate RecoveryStrategy = "model_escalation"
	RecoveryStrategyToolChange    RecoveryStrategy = "tool_change"
	RecoveryStrategySpecialist    RecoveryStrategy = "specialist"
	RecoveryStrategyReconcile     RecoveryStrategy = "reconcile"
	RecoveryStrategyHuman         RecoveryStrategy = "human"
)

// RecoveryHypothesis is required for a materially different repair after an
// equivalent failure repeats. It is intentionally structural; hufu does not
// try to prove a natural-language cause.
type RecoveryHypothesis struct {
	CriterionID         string           `json:"criterion_id,omitempty" yaml:"criterion-id,omitempty"`
	ObservedFailure     string           `json:"observed_failure" yaml:"observed-failure"`
	Evidence            []EvidenceRef    `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	HypothesizedCause   string           `json:"hypothesized_cause" yaml:"hypothesized-cause"`
	ProposedChange      string           `json:"proposed_change" yaml:"proposed-change"`
	DifferenceFromPrior string           `json:"difference_from_prior" yaml:"difference-from-prior"`
	ExpectedChange      string           `json:"expected_change" yaml:"expected-change"`
	Strategy            RecoveryStrategy `json:"strategy" yaml:"strategy"`
	ReconciliationPlan  string           `json:"reconciliation_plan,omitempty" yaml:"reconciliation-plan,omitempty"`
}

func (h RecoveryHypothesis) Validate(repeated bool, prior RecoveryStrategy) error {
	for name, value := range map[string]string{
		"observed_failure": h.ObservedFailure, "hypothesized_cause": h.HypothesizedCause,
		"proposed_change": h.ProposedChange, "expected_change": h.ExpectedChange,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("recovery hypothesis requires %s", name)
		}
	}
	if h.Strategy == "" {
		return fmt.Errorf("recovery hypothesis requires strategy")
	}
	if repeated && (prior == h.Strategy || strings.TrimSpace(h.DifferenceFromPrior) == "") {
		return fmt.Errorf("repeated failure requires a different recovery strategy and difference_from_prior")
	}
	return nil
}

// ValidateForCriterion ensures a repair hypothesis is attached to the
// acceptance criterion whose failure triggered the repair.
func (h RecoveryHypothesis) ValidateForCriterion(criterionID string, repeated bool, prior RecoveryStrategy) error {
	if err := h.Validate(repeated, prior); err != nil {
		return err
	}
	if repeated && strings.TrimSpace(criterionID) == "" {
		return fmt.Errorf("repeated recovery hypothesis requires a non-empty failed criterion")
	}
	if strings.TrimSpace(criterionID) != "" && strings.TrimSpace(h.CriterionID) != strings.TrimSpace(criterionID) {
		return fmt.Errorf("recovery hypothesis criterion %q does not match failed criterion %q", h.CriterionID, criterionID)
	}
	return nil
}

// ValidateForTask applies replay safety to a repeated repair proposal. Unsafe
// side effects and non-replayable execution require an explicit reconcile
// strategy or a reconciliation plan before the proposal can be accepted.
func (h RecoveryHypothesis) ValidateForTask(criterionID string, repeated bool, prior RecoveryStrategy, task TaskDef) error {
	if err := h.ValidateForCriterion(criterionID, repeated, prior); err != nil {
		return err
	}
	if !repeated || CanAutomaticallyReplay(task) {
		return nil
	}
	if task.Recovery == RecoveryNever && h.Strategy != RecoveryStrategyHuman {
		return fmt.Errorf("never-replay task requires human recovery strategy")
	}
	if h.Strategy != RecoveryStrategyReconcile && strings.TrimSpace(h.ReconciliationPlan) == "" {
		return fmt.Errorf("non-replayable recovery requires reconcile strategy or reconciliation plan")
	}
	return nil
}

var volatileFailurePart = regexp.MustCompile(`(?i)\b(task|attempt|retry|run|duration|elapsed)[ _-]*(id|number|count)?\s*[=:]?\s*[A-Za-z0-9_.:-]+`)
var timestampFailurePart = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ][0-9:.+-]+\b`)

// NormalizeFailureError removes only known volatile metadata. Paths, exit
// codes, assertion names and failure classes are deliberately preserved.
func NormalizeFailureError(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	s = timestampFailurePart.ReplaceAllString(s, "<timestamp>")
	s = volatileFailurePart.ReplaceAllString(s, "$1=<volatile>")
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

func NewFailureFingerprint(criterionID, component, operation string, class TaskFailureClass, errText string) FailureFingerprint {
	f := FailureFingerprint{CriterionID: strings.TrimSpace(criterionID), Component: strings.TrimSpace(component), Operation: strings.TrimSpace(operation), Class: class, NormalizedError: NormalizeFailureError(errText), Occurrences: 1}
	b := sha256.Sum256([]byte(strings.Join([]string{f.CriterionID, f.Component, f.Operation, string(f.Class), f.NormalizedError}, "\x00")))
	f.Digest = "ffp_" + hex.EncodeToString(b[:])
	return f
}

func SameFailureFingerprint(a, b FailureFingerprint) bool {
	return a.Digest != "" && a.Digest == b.Digest
}

// AntiThrashingState is run-scoped and intentionally reconstructible from
// the persisted fingerprint list on each TodoItem.
type AntiThrashingState struct {
	Counts                  map[string]int
	LastStrategy            map[string]RecoveryStrategy
	RejectedStrategies      map[string]map[RecoveryStrategy]bool
	DiagnosticSinceProgress int
	RepairsByCriterion      map[string]int
	Warnings                int
	StrategyChanges         int
	HardBlocked             bool
	DiagnosticTasksCounted  map[string]bool
	BlockedCriteria         map[string]bool
	BlockedScopes           map[string]bool
	BlockedFingerprints     map[string]bool
	BlockedDiagnostics      bool
	BlockedRepairs          bool
}

func (s *AntiThrashingState) reset() {
	s.Counts = make(map[string]int)
	s.LastStrategy = make(map[string]RecoveryStrategy)
	s.RejectedStrategies = make(map[string]map[RecoveryStrategy]bool)
	s.RepairsByCriterion = make(map[string]int)
	s.DiagnosticSinceProgress = 0
	s.Warnings = 0
	s.StrategyChanges = 0
	s.HardBlocked = false
	s.DiagnosticTasksCounted = make(map[string]bool)
	s.BlockedCriteria = make(map[string]bool)
	s.BlockedScopes = make(map[string]bool)
	s.BlockedFingerprints = make(map[string]bool)
	s.BlockedDiagnostics = false
	s.BlockedRepairs = false
}

func failureCriterionIDs(item *TodoItem) []string {
	if item == nil || len(item.Advances) == 0 {
		return []string{""}
	}
	ids := append([]string(nil), item.Advances...)
	sort.Strings(ids)
	return ids
}

func (s *AntiThrashingState) record(item *TodoItem, fp FailureFingerprint, strategy RecoveryStrategy, limits ReliabilityConfig) (repeated, limited bool) {
	if s.Counts == nil {
		s.reset()
	}
	s.Counts[fp.Digest]++
	repeated = s.Counts[fp.Digest] >= 2
	if strategy != "" && s.LastStrategy[fp.Digest] != "" && s.LastStrategy[fp.Digest] != strategy {
		s.StrategyChanges++
	}
	if strategy != "" {
		s.LastStrategy[fp.Digest] = strategy
	}
	if item != nil && item.Kind == TaskKindDiagnostic {
		s.recordDiagnostic(item)
	}
	if item != nil && item.Kind == TaskKindRepair {
		for _, id := range item.Advances {
			s.RepairsByCriterion[id]++
		}
	}
	fingerprintLimited := limits.MaxSameFailureFingerprint > 0 && s.Counts[fp.Digest] >= limits.MaxSameFailureFingerprint
	diagnosticLimited := limits.MaxDiagnosticTasksWithoutProgress > 0 && s.DiagnosticSinceProgress >= limits.MaxDiagnosticTasksWithoutProgress
	limited = fingerprintLimited || diagnosticLimited
	repairLimited := false
	if item != nil && item.Kind == TaskKindRepair {
		for _, id := range item.Advances {
			if limits.MaxRepairsPerCriterion > 0 && s.RepairsByCriterion[id] >= limits.MaxRepairsPerCriterion {
				limited = true
				repairLimited = true
			}
		}
	}
	if repeated || limited {
		s.Warnings++
	}
	if limited && limits.HardEnforcement {
		if fingerprintLimited {
			s.markBlockedFingerprint(fp)
		}
		if diagnosticLimited {
			s.markBlockedScope(item, fp)
		}
		if repairLimited {
			for _, criterion := range item.Advances {
				if limits.MaxRepairsPerCriterion > 0 && s.RepairsByCriterion[criterion] >= limits.MaxRepairsPerCriterion {
					s.markBlockedCriterion(criterion, item.Kind, fp)
				}
			}
		}
	}
	return repeated, limited
}

func (s *AntiThrashingState) rememberRejectedStrategy(digest string, strategy RecoveryStrategy) {
	if s == nil || digest == "" || strategy == "" {
		return
	}
	if s.RejectedStrategies == nil {
		s.RejectedStrategies = make(map[string]map[RecoveryStrategy]bool)
	}
	if s.RejectedStrategies[digest] == nil {
		s.RejectedStrategies[digest] = make(map[RecoveryStrategy]bool)
	}
	s.RejectedStrategies[digest][strategy] = true
}

func (s *AntiThrashingState) strategyWasRejected(digest string, strategy RecoveryStrategy) bool {
	return s != nil && strategy != "" && s.RejectedStrategies[digest][strategy]
}

func (s *AntiThrashingState) markBlockedFingerprint(fp FailureFingerprint) {
	s.HardBlocked = true
	if s.BlockedFingerprints == nil {
		s.BlockedFingerprints = make(map[string]bool)
	}
	if fp.Digest != "" {
		s.BlockedFingerprints[fp.Digest] = true
	}
}

func (s *AntiThrashingState) markBlockedScope(item *TodoItem, fp FailureFingerprint) {
	s.HardBlocked = true
	if s.BlockedCriteria == nil {
		s.BlockedCriteria = make(map[string]bool)
	}
	if s.BlockedScopes == nil {
		s.BlockedScopes = make(map[string]bool)
	}
	s.markBlockedFingerprint(fp)
	if item == nil {
		return
	}
	for _, criterion := range item.Advances {
		s.markBlockedCriterion(criterion, item.Kind, fp)
	}
	if len(item.Advances) == 0 {
		s.BlockedScopes[antiThrashingScopeKey("", item.Kind)] = true
		switch item.Kind {
		case TaskKindDiagnostic:
			s.BlockedDiagnostics = true
		case TaskKindRepair:
			s.BlockedRepairs = true
		}
	}
}

func (s *AntiThrashingState) markBlockedCriterion(criterion string, kind TaskKind, fp FailureFingerprint) {
	criterion = strings.TrimSpace(criterion)
	if criterion == "" {
		return
	}
	s.HardBlocked = true
	if s.BlockedCriteria == nil {
		s.BlockedCriteria = make(map[string]bool)
	}
	if s.BlockedScopes == nil {
		s.BlockedScopes = make(map[string]bool)
	}
	s.BlockedCriteria[criterion] = true
	s.BlockedScopes[antiThrashingScopeKey(criterion, kind)] = true
	s.markBlockedFingerprint(fp)
}

func (s *AntiThrashingState) blocksTask(task TaskDef, item *TodoItem) bool {
	if !s.HardBlocked {
		return false
	}
	if item != nil {
		for _, fp := range item.FailureFingerprints {
			if fp.CriterionID != "" && !taskAdvancesCriterion(task, fp.CriterionID) {
				continue
			}
			if s.BlockedFingerprints[fp.Digest] {
				return true
			}
		}
	}
	if len(s.BlockedScopes) > 0 {
		if len(task.Advances) == 0 {
			return s.BlockedScopes[antiThrashingScopeKey("", task.Kind)]
		}
		for _, criterion := range task.Advances {
			if s.BlockedScopes[antiThrashingScopeKey(criterion, task.Kind)] {
				return true
			}
		}
		return false
	}
	for _, criterion := range task.Advances {
		if s.BlockedCriteria[criterion] {
			return true
		}
	}
	if len(task.Advances) == 0 {
		switch task.Kind {
		case TaskKindDiagnostic:
			return s.BlockedDiagnostics
		case TaskKindRepair:
			return s.BlockedRepairs
		}
	}
	return false
}

func taskAdvancesCriterion(task TaskDef, criterion string) bool {
	criterion = strings.TrimSpace(criterion)
	if criterion == "" {
		return true
	}
	for _, id := range task.Advances {
		if strings.TrimSpace(id) == criterion {
			return true
		}
	}
	return false
}

func antiThrashingScopeKey(criterion string, kind TaskKind) string {
	return strings.TrimSpace(criterion) + "\x00" + string(kind)
}

// resetAfterCriterionProgress clears anti-thrashing state whose scope was the
// criterion that actually advanced. A live repair can hit its limit before
// criterion verification runs; leaving that scope blocked would make live
// enforcement disagree with replay, which rebuilds the counter after the
// ProgressAdvanced checkpoint.
func (s *AntiThrashingState) resetAfterCriterionProgress(criteria []string, items []*TodoItem) {
	if s == nil {
		return
	}
	advanced := make(map[string]bool, len(criteria))
	for _, criterion := range criteria {
		criterion = strings.TrimSpace(criterion)
		if criterion == "" {
			continue
		}
		advanced[criterion] = true
		delete(s.RepairsByCriterion, criterion)
		delete(s.BlockedCriteria, criterion)
		for _, kind := range []TaskKind{TaskKindOutcome, TaskKindRepair, TaskKindDiagnostic} {
			delete(s.BlockedScopes, antiThrashingScopeKey(criterion, kind))
		}
	}
	for _, item := range items {
		if item == nil || len(item.FailureFingerprints) == 0 {
			continue
		}
		for _, fp := range item.FailureFingerprints {
			fingerprintCriterion := strings.TrimSpace(fp.CriterionID)
			if fingerprintCriterion != "" {
				if !advanced[fingerprintCriterion] {
					continue
				}
			} else {
				// Legacy fingerprints have no criterion identity. Clear them
				// only when their task has exactly one criterion and that
				// criterion advanced; never guess in a partial multi-criterion
				// task and erase non-advanced evidence.
				if len(item.Advances) != 1 || !advanced[strings.TrimSpace(item.Advances[0])] {
					continue
				}
			}
			delete(s.BlockedFingerprints, fp.Digest)
			// Failure counts and last strategy are anti-thrashing counters for
			// the current criterion, not immutable history. Reset them after
			// progress so replay cannot re-block from stale evidence.
			delete(s.Counts, fp.Digest)
			delete(s.LastStrategy, fp.Digest)
			delete(s.RejectedStrategies, fp.Digest)
		}
	}
	s.HardBlocked = len(s.BlockedCriteria) > 0 || len(s.BlockedScopes) > 0 || len(s.BlockedFingerprints) > 0 || s.BlockedDiagnostics || s.BlockedRepairs
}

// repairAttemptCount reconstructs actual executions without adding two views
// of the same attempt. Distinct fingerprints provide a lower bound from
// failure evidence; Retries records DAG resets, so Retries+1 is the execution
// count when a retry exists. The larger bound wins. MaxRetries is a configured
// ceiling, not evidence that those attempts ran, and is deliberately ignored.
func repairAttemptCount(item *TodoItem) int {
	if item == nil || item.Kind != TaskKindRepair {
		return 0
	}
	failureOccurrences := failureOccurrenceCount(item)
	distinctFailures := make(map[string]bool, len(item.FailureFingerprints))
	for _, fp := range item.FailureFingerprints {
		if fp.Digest != "" {
			distinctFailures[fp.Digest] = true
		}
	}
	attempts := failureOccurrences
	// A completed repair has one successful execution in addition to any
	// failed executions represented by fingerprints. Keep that success in the
	// durable count so a task that fails repeatedly and then succeeds does not
	// undercount after replay.
	if item.Status == TaskDone {
		attempts++
	}
	if len(distinctFailures) > attempts {
		attempts = len(distinctFailures)
	}
	if item.Retries > 0 && item.Retries+1 > attempts {
		attempts = item.Retries + 1
	}
	if attempts == 0 && item.Status == TaskDone {
		return 1
	}
	return attempts
}

func failureOccurrenceCount(item *TodoItem) int {
	if item == nil {
		return 0
	}
	total := 0
	for _, fp := range item.FailureFingerprints {
		if fp.Digest == "" {
			continue
		}
		occurrences := fp.Occurrences
		if occurrences < 1 {
			occurrences = 1 // legacy persisted fingerprints
		}
		total += occurrences
	}
	return total
}

// rebuild reconstructs all run-level anti-thrashing state from the durable
// task evidence. This is called after session/event replay so a restart cannot
// silently reset hard enforcement or permit equivalent work again.
func (s *AntiThrashingState) rebuild(items []*TodoItem, limits ReliabilityConfig) {
	s.reset()
	for _, item := range items {
		if item == nil {
			continue
		}
		if item.Kind == TaskKindDiagnostic && (len(item.FailureFingerprints) > 0 || item.Status == TaskDone || item.Status == TaskError || item.Status == TaskBlocked) {
			s.recordDiagnostic(item)
		}
		if item.Kind == TaskKindRepair {
			repairAttempts := repairAttemptCount(item)
			for _, criterion := range item.Advances {
				s.RepairsByCriterion[criterion] += repairAttempts
			}
		}
		for _, fp := range item.FailureFingerprints {
			if fp.Digest == "" {
				continue
			}
			occurrences := fp.Occurrences
			if occurrences < 1 {
				occurrences = 1
			}
			strategy := RecoveryStrategy("")
			if item.RecoveryHypothesis != nil {
				strategy = item.RecoveryHypothesis.Strategy
			}
			prior := s.LastStrategy[fp.Digest]
			repeated := s.Counts[fp.Digest] > 0 || occurrences > 1
			if repeated && item.Kind == TaskKindRepair {
				validationPrior := prior
				if validationPrior == "" && occurrences > 1 {
					// An aggregated fingerprint with multiple occurrences has
					// no separate task record for its first attempt. Its single
					// hypothesis cannot demonstrate a strategy change, so treat
					// the current strategy as the prior one.
					validationPrior = strategy
				}
				hypothesisInvalid := s.strategyWasRejected(fp.Digest, strategy) || item.RecoveryHypothesis == nil || item.RecoveryHypothesis.ValidateForTask(fp.CriterionID, true, validationPrior, taskDefFromTodoItem(item)) != nil
				if hypothesisInvalid {
					s.rememberRejectedStrategy(fp.Digest, strategy)
					if limits.HardEnforcement {
						if strings.TrimSpace(fp.CriterionID) != "" {
							s.markBlockedCriterion(fp.CriterionID, item.Kind, fp)
						} else {
							s.markBlockedFingerprint(fp)
						}
					}
				}
			}
			s.Counts[fp.Digest] += occurrences
			if strategy != "" {
				if prior != "" && prior != strategy {
					s.StrategyChanges++
				}
				s.LastStrategy[fp.Digest] = strategy
			}
		}
		if item.Progress == ProgressAdvanced {
			s.DiagnosticSinceProgress = 0
			s.DiagnosticTasksCounted = make(map[string]bool)
			progressCriteria := item.ProgressCriteria
			if len(progressCriteria) == 0 {
				// Legacy events did not persist the exact advanced subset.
				progressCriteria = item.Advances
			}
			s.resetAfterCriterionProgress(progressCriteria, items)
		}
	}

	for _, item := range items {
		if item == nil {
			continue
		}
		for _, fp := range item.FailureFingerprints {
			if fp.Digest == "" {
				continue
			}
			limited := limits.MaxSameFailureFingerprint > 0 && s.Counts[fp.Digest] >= limits.MaxSameFailureFingerprint
			if limited && limits.HardEnforcement {
				s.markBlockedFingerprint(fp)
			}
			if limited || s.Counts[fp.Digest] >= 2 {
				s.Warnings++
			}
		}
		if item.Kind == TaskKindRepair && limits.MaxRepairsPerCriterion > 0 && limits.HardEnforcement {
			for _, criterion := range item.Advances {
				if s.RepairsByCriterion[criterion] >= limits.MaxRepairsPerCriterion {
					s.markBlockedCriterion(criterion, item.Kind, FailureFingerprint{})
				}
			}
		}
		if item.Kind == TaskKindDiagnostic && limits.MaxDiagnosticTasksWithoutProgress > 0 && s.DiagnosticSinceProgress >= limits.MaxDiagnosticTasksWithoutProgress && limits.HardEnforcement {
			s.markBlockedScope(item, FailureFingerprint{})
		}
	}
}

func (s *AntiThrashingState) recordDiagnostic(item *TodoItem) bool {
	if item == nil {
		return false
	}
	if s.DiagnosticTasksCounted == nil {
		s.DiagnosticTasksCounted = make(map[string]bool)
	}
	key := item.ID
	if key == "" {
		key = item.Desc
	}
	if s.DiagnosticTasksCounted[key] {
		return false
	}
	s.DiagnosticTasksCounted[key] = true
	s.DiagnosticSinceProgress++
	return true
}
