package team

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

const maxSemanticRegressionReasons = 50

type SemanticRegressionDecision struct {
	Configured    bool     `json:"configured"`
	Clear         bool     `json:"clear"`
	BlockingCount int      `json:"blocking_count"`
	Reasons       []string `json:"reasons,omitempty"`
}

type InvariantVerificationValidation struct {
	Valid       bool
	Code        string
	Assessments []InvariantAssessment
}

func HasBlockingInvariantAssessment(mode InvariantVerificationMode, verification *InvariantVerificationResult) bool {
	if mode != InvariantVerificationGate || verification == nil {
		return false
	}
	for _, assessment := range verification.Assessments {
		if assessment.Severity == InvariantSeverityError && (assessment.Status == InvariantViolated || assessment.Status == InvariantUnknown) {
			return true
		}
	}
	return false
}

func invalidInvariantVerification(code string) InvariantVerificationValidation {
	return InvariantVerificationValidation{Code: code}
}

func ValidateInvariantVerificationResult(todo *TodoItem, runID string) InvariantVerificationValidation {
	if todo == nil || todo.TypedResult == nil {
		return invalidInvariantVerification("typed_result_missing")
	}
	result := todo.TypedResult
	if err := validateFindingSeverities(result.Findings); err != nil {
		return invalidInvariantVerification("attestation_invalid")
	}
	verification := result.InvariantVerification
	if verification == nil || verification.Assessments == nil || strings.TrimSpace(verification.ContextManifestFingerprint) == "" {
		return invalidInvariantVerification("envelope_missing")
	}
	if len(verification.Assessments) > maxInvariantAssessmentClaims {
		return invalidInvariantVerification("attestation_invalid")
	}
	manifest, code := findInvariantVerificationManifest(todo.ContextManifests, verification.ContextManifestFingerprint)
	if code != "" {
		return invalidInvariantVerification(code)
	}
	if code = validateInvariantManifestIdentity(todo, result, manifest, runID); code != "" {
		return invalidInvariantVerification(code)
	}
	expected, code := includedInvariantManifestItems(manifest)
	if code != "" {
		return invalidInvariantVerification(code)
	}
	assessments, code := validatePersistedInvariantAssessments(result, verification.Assessments, expected)
	if code != "" {
		return invalidInvariantVerification(code)
	}
	slices.SortFunc(assessments, func(left, right InvariantAssessment) int {
		return strings.Compare(left.InvariantID, right.InvariantID)
	})
	return InvariantVerificationValidation{Valid: true, Assessments: assessments}
}

func findInvariantVerificationManifest(manifests []ContextInjectionManifest, fingerprint string) (*ContextInjectionManifest, string) {
	var matched *ContextInjectionManifest
	for i := range manifests {
		candidate := &manifests[i]
		if candidate.Fingerprint != fingerprint {
			continue
		}
		if matched != nil {
			return nil, "manifest_ambiguous"
		}
		matched = candidate
	}
	if matched == nil {
		return nil, "manifest_missing"
	}
	return matched, ""
}

func validateInvariantManifestIdentity(todo *TodoItem, result *TaskResult, manifest *ContextInjectionManifest, runID string) string {
	if manifest.SchemaVersion != ContextManifestSchemaVersion {
		return "manifest_version"
	}
	if strings.TrimSpace(runID) == "" || manifest.RunID != runID {
		return "run_mismatch"
	}
	if contextManifestFingerprint(*manifest) != manifest.Fingerprint || manifest.TaskID != todo.ID || result.TaskID != todo.ID ||
		manifest.Attempt < 1 || result.Attempt != manifest.Attempt || strings.TrimSpace(manifest.ModelExecutionID) == "" || !manifest.ModelCalled ||
		result.Agent != manifest.Agent {
		return "identity_mismatch"
	}
	return ""
}

func includedInvariantManifestItems(manifest *ContextInjectionManifest) (map[string]ContextManifestItem, string) {
	expected := make(map[string]ContextManifestItem)
	for _, item := range manifest.Items {
		if item.Kind != string(contextstore.ContextInvariant) || !item.Included {
			continue
		}
		if item.Source != repositoryInvariantSource || !validFullContentHash(item.ContentHash) || !validInvariantVerificationSeverity(item.InvariantSeverity) {
			return nil, "attestation_invalid"
		}
		if _, duplicate := expected[item.ID]; duplicate {
			return nil, "assessment_set_mismatch"
		}
		expected[item.ID] = item
	}
	return expected, ""
}

func validatePersistedInvariantAssessments(result *TaskResult, persisted []InvariantAssessment, expected map[string]ContextManifestItem) ([]InvariantAssessment, string) {
	if len(persisted) != len(expected) {
		return nil, "assessment_set_mismatch"
	}
	seen := make(map[string]bool, len(persisted))
	assessments := make([]InvariantAssessment, len(persisted))
	for i, assessment := range persisted {
		item, ok := expected[assessment.ContextItemID]
		if !ok || seen[assessment.ContextItemID] {
			return nil, "assessment_set_mismatch"
		}
		seen[assessment.ContextItemID] = true
		if !validPersistedInvariantAssessmentIdentity(assessment, item) || !validPersistedInvariantAssessmentStatus(assessment, result.Findings) {
			return nil, "attestation_invalid"
		}
		assessments[i] = assessment
		assessments[i].MissingEvidence = slices.Clone(assessment.MissingEvidence)
		if assessment.FindingIndex != nil {
			assessments[i].FindingIndex = new(*assessment.FindingIndex)
		}
	}
	return assessments, ""
}

func validPersistedInvariantAssessmentIdentity(assessment InvariantAssessment, item ContextManifestItem) bool {
	summary := strings.TrimSpace(assessment.Summary)
	return assessment.InvariantID != "" && strings.HasSuffix(assessment.ContextItemID, ":"+assessment.InvariantID) &&
		assessment.InvariantContentHash == item.ContentHash && assessment.Severity == item.InvariantSeverity &&
		summary != "" && utf8.RuneCountInString(summary) <= maxInvariantAssessmentSummaryRunes
}

func validPersistedInvariantAssessmentStatus(assessment InvariantAssessment, findings []Finding) bool {
	switch assessment.Status {
	case InvariantPreserved:
		return assessment.FindingIndex == nil && len(assessment.MissingEvidence) == 0
	case InvariantViolated:
		return assessment.FindingIndex != nil && *assessment.FindingIndex >= 0 && *assessment.FindingIndex < len(findings) &&
			findings[*assessment.FindingIndex].Severity == string(assessment.Severity) && len(assessment.MissingEvidence) == 0
	case InvariantUnknown:
		if assessment.FindingIndex != nil || len(assessment.MissingEvidence) == 0 || len(assessment.MissingEvidence) > maxInvariantMissingEvidence {
			return false
		}
		for _, evidence := range assessment.MissingEvidence {
			trimmed := strings.TrimSpace(evidence)
			if trimmed == "" || utf8.RuneCountInString(trimmed) > maxInvariantMissingEvidenceRunes {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func EvaluateSemanticRegression(runID string, tasks []*TodoItem) SemanticRegressionDecision {
	type blocker struct {
		taskID      string
		invariantID string
		reason      string
	}
	decision := SemanticRegressionDecision{Clear: true}
	var blockers []blocker
	for _, todo := range tasks {
		if todo == nil || todo.InvariantVerification != InvariantVerificationGate {
			continue
		}
		decision.Configured = true
		validation := ValidateInvariantVerificationResult(todo, runID)
		if !validation.Valid {
			blockers = append(blockers, blocker{taskID: todo.ID, reason: fmt.Sprintf("task %s invariant verification invalid: %s", todo.ID, validation.Code)})
			continue
		}
		hasSemanticBlocker := false
		for _, assessment := range validation.Assessments {
			if assessment.Severity != InvariantSeverityError || (assessment.Status != InvariantViolated && assessment.Status != InvariantUnknown) {
				continue
			}
			hasSemanticBlocker = true
			blockers = append(blockers, blocker{
				taskID: todo.ID, invariantID: assessment.InvariantID,
				reason: fmt.Sprintf("task %s invariant %s is %s: %s", todo.ID, assessment.InvariantID, assessment.Status, utils.TruncateRunes(assessment.Summary, 256)),
			})
		}
		if !hasSemanticBlocker && todo.Status != TaskDone {
			blockers = append(blockers, blocker{taskID: todo.ID, reason: fmt.Sprintf("task %s invariant verification invalid: task_not_done", todo.ID)})
		}
	}
	if !decision.Configured {
		return decision
	}
	slices.SortFunc(blockers, func(left, right blocker) int {
		if byTask := strings.Compare(left.taskID, right.taskID); byTask != 0 {
			return byTask
		}
		return strings.Compare(left.invariantID, right.invariantID)
	})
	decision.BlockingCount = len(blockers)
	decision.Clear = len(blockers) == 0
	limit := min(len(blockers), maxSemanticRegressionReasons)
	decision.Reasons = make([]string, 0, limit)
	if len(blockers) <= maxSemanticRegressionReasons {
		for _, item := range blockers {
			decision.Reasons = append(decision.Reasons, item.reason)
		}
		return decision
	}
	for _, item := range blockers[:maxSemanticRegressionReasons-1] {
		decision.Reasons = append(decision.Reasons, item.reason)
	}
	decision.Reasons = append(decision.Reasons, fmt.Sprintf("and %d additional invariant blockers", len(blockers)-(maxSemanticRegressionReasons-1)))
	return decision
}
