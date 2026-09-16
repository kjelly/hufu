package team

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

type expectedInvariantAssessment struct {
	definition InvariantDefinition
	item       ContextManifestItem
}

const (
	maxInvariantAssessmentClaims       = 128
	maxInvariantAssessmentSummaryRunes = 1000
	maxInvariantMissingEvidence        = 16
	maxInvariantMissingEvidenceRunes   = 256
)

func invariantAssessmentClaimsSchema(codexStrict bool) map[string]any {
	findingIndex := map[string]any{"type": "integer", "minimum": 0}
	missingEvidence := map[string]any{
		"type":     "array",
		"items":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxInvariantMissingEvidenceRunes},
		"maxItems": maxInvariantMissingEvidence,
	}
	required := []string{"invariant_id", "status", "summary"}
	if codexStrict {
		findingIndex["type"] = []string{"integer", "null"}
		missingEvidence["type"] = []string{"array", "null"}
		required = append(required, "finding_index", "missing_evidence")
	}
	return map[string]any{
		"type":     "array",
		"maxItems": maxInvariantAssessmentClaims,
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             required,
			"properties": map[string]any{
				"invariant_id":     map[string]any{"type": "string", "minLength": 1},
				"status":           map[string]any{"type": "string", "enum": []string{string(InvariantPreserved), string(InvariantViolated), string(InvariantUnknown)}},
				"summary":          map[string]any{"type": "string", "minLength": 1, "maxLength": maxInvariantAssessmentSummaryRunes},
				"finding_index":    findingIndex,
				"missing_evidence": missingEvidence,
			},
		},
	}
}

func cloneInvariantAssessmentClaims(claims *[]InvariantAssessmentClaim) *[]InvariantAssessmentClaim {
	if claims == nil {
		return nil
	}
	cloned := make([]InvariantAssessmentClaim, len(*claims))
	for i, claim := range *claims {
		cloned[i] = claim
		cloned[i].MissingEvidence = slices.Clone(claim.MissingEvidence)
		if claim.FindingIndex != nil {
			cloned[i].FindingIndex = new(*claim.FindingIndex)
		}
	}
	return new(cloned)
}

func validateFindingSeverities(findings []Finding) error {
	for i, finding := range findings {
		if finding.Severity == "" {
			continue
		}
		switch finding.Severity {
		case FindingSeverityError, FindingSeverityWarning, FindingSeverityInfo:
		default:
			return fmt.Errorf("findings[%d].severity has unknown value %q", i, finding.Severity)
		}
	}
	return nil
}

func normalizedInvariantAssessmentClaims(claims *[]InvariantAssessmentClaim) ([]InvariantAssessmentClaim, error) {
	if claims == nil {
		return nil, fmt.Errorf("invariant_assessments must be a non-null array")
	}
	if len(*claims) > maxInvariantAssessmentClaims {
		return nil, fmt.Errorf("invariant_assessments has %d entries; maximum is %d", len(*claims), maxInvariantAssessmentClaims)
	}
	normalized := make([]InvariantAssessmentClaim, len(*claims))
	for i, claim := range *claims {
		claim.MissingEvidence = slices.Clone(claim.MissingEvidence)
		claim.InvariantID = strings.TrimSpace(claim.InvariantID)
		if claim.InvariantID == "" {
			return nil, fmt.Errorf("invariant_assessments[%d].invariant_id is required", i)
		}
		claim.Summary = strings.TrimSpace(claim.Summary)
		if claim.Summary == "" || utf8.RuneCountInString(claim.Summary) > maxInvariantAssessmentSummaryRunes {
			return nil, fmt.Errorf("invariant_assessments[%d].summary must contain 1-%d runes after trimming", i, maxInvariantAssessmentSummaryRunes)
		}
		switch claim.Status {
		case InvariantPreserved, InvariantViolated, InvariantUnknown:
		default:
			return nil, fmt.Errorf("invariant_assessments[%d].status has unknown value %q", i, claim.Status)
		}
		if len(claim.MissingEvidence) > maxInvariantMissingEvidence {
			return nil, fmt.Errorf("invariant_assessments[%d].missing_evidence has %d entries; maximum is %d", i, len(claim.MissingEvidence), maxInvariantMissingEvidence)
		}
		for evidenceIndex, evidence := range claim.MissingEvidence {
			evidence = strings.TrimSpace(evidence)
			if evidence == "" || utf8.RuneCountInString(evidence) > maxInvariantMissingEvidenceRunes {
				return nil, fmt.Errorf("invariant_assessments[%d].missing_evidence[%d] must contain 1-%d runes after trimming", i, evidenceIndex, maxInvariantMissingEvidenceRunes)
			}
			claim.MissingEvidence[evidenceIndex] = evidence
		}
		switch claim.Status {
		case InvariantViolated:
			if claim.FindingIndex == nil || *claim.FindingIndex < 0 {
				return nil, fmt.Errorf("invariant_assessments[%d] violated claim requires a non-negative finding_index", i)
			}
			if len(claim.MissingEvidence) > 0 {
				return nil, fmt.Errorf("invariant_assessments[%d] violated claim cannot include missing_evidence", i)
			}
		case InvariantUnknown:
			if claim.FindingIndex != nil {
				return nil, fmt.Errorf("invariant_assessments[%d] unknown claim cannot include finding_index", i)
			}
			if len(claim.MissingEvidence) == 0 {
				return nil, fmt.Errorf("invariant_assessments[%d] unknown claim requires missing_evidence", i)
			}
		case InvariantPreserved:
			if claim.FindingIndex != nil {
				return nil, fmt.Errorf("invariant_assessments[%d] preserved claim cannot include finding_index", i)
			}
			if len(claim.MissingEvidence) > 0 {
				return nil, fmt.Errorf("invariant_assessments[%d] preserved claim cannot include missing_evidence", i)
			}
		}
		normalized[i] = claim
		normalized[i].MissingEvidence = slices.Clone(claim.MissingEvidence)
		if claim.FindingIndex != nil {
			normalized[i].FindingIndex = new(*claim.FindingIndex)
		}
	}
	return normalized, nil
}

func authoritativeTaskContextManifest(todo *TodoItem, attempt int, modelExecutionID string) (*ContextInjectionManifest, error) {
	if todo == nil {
		return nil, fmt.Errorf("invariant attestation requires a durable Todo")
	}
	if attempt < 1 {
		return nil, fmt.Errorf("invariant attestation requires a runtime attempt")
	}
	var matched *ContextInjectionManifest
	for i := range todo.ContextManifests {
		manifest := &todo.ContextManifests[i]
		if manifest.TaskID != todo.ID || manifest.Attempt != attempt || !manifest.ModelCalled {
			continue
		}
		if modelExecutionID != "" && manifest.ModelExecutionID != modelExecutionID {
			continue
		}
		if (manifest.Trigger != ContextTriggerTaskDispatch || manifest.Purpose != contextPurposeForTrigger(ContextTriggerTaskDispatch)) &&
			(manifest.Trigger != ContextTriggerRetry || manifest.Purpose != contextPurposeForTrigger(ContextTriggerRetry)) {
			continue
		}
		if matched != nil {
			return nil, fmt.Errorf("multiple authoritative context manifests match task %q attempt %d model execution %q", todo.ID, attempt, modelExecutionID)
		}
		matched = cloneContextInjectionManifest(manifest)
	}
	if matched == nil {
		return nil, fmt.Errorf("no context manifest with an authoritative task purpose matches task %q attempt %d model execution %q", todo.ID, attempt, modelExecutionID)
	}
	return matched, nil
}

func (c *Coordinator) invariantContextManifest(todo *TodoItem, attempt int, modelExecutionID string) (*ContextInjectionManifest, error) {
	if strings.TrimSpace(modelExecutionID) == "" {
		return nil, fmt.Errorf("invariant attestation requires model execution identity")
	}
	matched, err := authoritativeTaskContextManifest(todo, attempt, modelExecutionID)
	if err != nil {
		return nil, err
	}
	if matched.SchemaVersion != ContextManifestSchemaVersion {
		return nil, fmt.Errorf("context manifest for task %q uses schema version %d; version %d is required for invariant attestation", todo.ID, matched.SchemaVersion, ContextManifestSchemaVersion)
	}
	if matched.Fingerprint == "" || contextManifestFingerprint(*matched) != matched.Fingerprint {
		return nil, fmt.Errorf("context manifest for task %q has an invalid fingerprint", todo.ID)
	}
	return matched, nil
}

func (c *Coordinator) includedInvariantAssessments(manifest *ContextInjectionManifest) ([]expectedInvariantAssessment, error) {
	if c == nil || c.session == nil || manifest == nil {
		return nil, fmt.Errorf("invariant manifest validation requires a coordinator session and manifest")
	}
	catalog := make(map[string]InvariantDefinition, len(c.session.InvariantCatalog))
	for _, definition := range c.session.InvariantCatalog {
		catalog[definition.ID] = definition
	}
	prefix := "invariant:" + normalizedName(c.session.Config.Name) + ":"
	included := make([]expectedInvariantAssessment, 0)
	seen := make(map[string]bool)
	for _, item := range manifest.Items {
		if item.Kind != string(contextstore.ContextInvariant) {
			continue
		}
		if item.Source != repositoryInvariantSource || !validFullContentHash(item.ContentHash) || !validInvariantVerificationSeverity(item.InvariantSeverity) {
			return nil, fmt.Errorf("context manifest invariant %q has invalid attribution", item.ID)
		}
		definitionID := strings.TrimPrefix(item.ID, prefix)
		definition, ok := catalog[definitionID]
		if !ok || definitionID == item.ID {
			return nil, fmt.Errorf("context manifest invariant %q is absent from the current repository catalog", item.ID)
		}
		materialized, err := materializeInvariantContextItem(c, definition)
		if err != nil {
			return nil, err
		}
		if materialized.ID != item.ID || materialized.ContentHash != item.ContentHash || definition.Severity != item.InvariantSeverity {
			return nil, fmt.Errorf("repository invariant %q changed since context manifest %q was persisted", definitionID, manifest.Fingerprint)
		}
		if !item.Included {
			continue
		}
		if seen[definitionID] {
			return nil, fmt.Errorf("context manifest contains duplicate included invariant %q", definitionID)
		}
		seen[definitionID] = true
		included = append(included, expectedInvariantAssessment{definition: definition, item: item})
	}
	slices.SortFunc(included, func(left, right expectedInvariantAssessment) int {
		return strings.Compare(left.definition.ID, right.definition.ID)
	})
	return included, nil
}

func invocationMetadataFromManifest(manifest *ContextInjectionManifest) InvocationMetadata {
	if manifest == nil {
		return InvocationMetadata{}
	}
	return InvocationMetadata{
		RunID:                     manifest.RunID,
		TaskID:                    manifest.TaskID,
		AgentName:                 manifest.Agent,
		AgentRole:                 manifest.AgentRole,
		ModelExecutionID:          manifest.ModelExecutionID,
		Attempt:                   manifest.Attempt,
		Phase:                     manifest.Phase,
		Trigger:                   manifest.Trigger,
		Purpose:                   manifest.Purpose,
		ParentRequestID:           manifest.RequestID,
		ParentManifestFingerprint: manifest.Fingerprint,
		EnvironmentFingerprint:    manifest.Environment,
	}
}

func (c *Coordinator) invariantRepairInstructions(todoID string, attempt int, modelExecutionID string) (string, InvocationMetadata, error) {
	todo := c.todoItemByID(todoID)
	if todo == nil || todo.InvariantVerification == "" {
		return "", InvocationMetadata{}, nil
	}
	manifest, err := c.invariantContextManifest(todo, attempt, modelExecutionID)
	if err != nil {
		return "", InvocationMetadata{}, err
	}
	included, err := c.includedInvariantAssessments(manifest)
	if err != nil {
		return "", InvocationMetadata{}, err
	}
	var prompt strings.Builder
	prompt.WriteString("\n\n## Required invariant assessments\nSubmit exactly one invariant_assessments claim for each listed invariant ID. Submit [] when this list is empty. Do not add IDs.\n")
	for _, expected := range included {
		fmt.Fprintf(&prompt, "- %s (%s)\n", expected.definition.ID, expected.definition.Severity)
	}
	var boundedContent strings.Builder
	boundedContent.WriteString("\n## Bounded invariant content\n")
	for _, expected := range included {
		fmt.Fprintf(&boundedContent, "\n### %s (%s)\n%s\n", expected.definition.ID, expected.definition.Severity, canonicalInvariantContent(expected.definition))
	}
	prompt.WriteString(utils.TruncateRunes(boundedContent.String(), 12000))
	metadata := invocationMetadataFromManifest(manifest)
	// The manifest is a content-free routing projection and intentionally does
	// not carry the workset itself. Result-only invariant repair still needs the
	// original scope, however: an empty touched-path set means "all repository
	// invariants apply". Rehydrate that runtime-owned scope from the durable Todo
	// occurrence and clone it so later context propagation cannot mutate the
	// checkpointed binding.
	if todo.WorksetBinding != nil {
		metadata.TouchedPaths = slices.Clone(todo.WorksetBinding.TouchedPaths)
	}
	return prompt.String(), metadata, nil
}

func (c *Coordinator) invariantRepairIdentity(todo *TodoItem, attempt int) (InvocationMetadata, error) {
	if todo == nil || todo.InvariantVerification == "" {
		return InvocationMetadata{}, nil
	}
	matched, err := authoritativeTaskContextManifest(todo, attempt, "")
	if err != nil {
		return InvocationMetadata{}, err
	}
	_, metadata, err := c.invariantRepairInstructions(todo.ID, attempt, matched.ModelExecutionID)
	return metadata, err
}

// attestInvariantClaims converts model-authored claims into a runtime-owned
// attestation using only durable task state and the immutable repository
// catalog. It must run before any result transaction/cache write.
func (c *Coordinator) attestInvariantClaims(todoID string, attempt int, modelExecutionID string, claims *[]InvariantAssessmentClaim, result *TaskResult) error {
	if c == nil {
		return fmt.Errorf("invariant attestation coordinator is unavailable")
	}
	todo := c.todoItemByID(todoID)
	if todo == nil {
		return fmt.Errorf("invariant attestation Todo %q does not exist", todoID)
	}
	if todo.InvariantVerification == "" {
		if claims != nil {
			return fmt.Errorf("ordinary task must omit invariant_assessments or submit null")
		}
		return nil
	}
	if c.session == nil {
		return fmt.Errorf("invariant attestation session is unavailable")
	}
	if result == nil {
		return fmt.Errorf("invariant attestation requires a canonical task result")
	}
	if err := validateFindingSeverities(result.Findings); err != nil {
		return err
	}
	manifest, err := c.invariantContextManifest(todo, attempt, modelExecutionID)
	if err != nil {
		return err
	}
	normalizedClaims, err := normalizedInvariantAssessmentClaims(claims)
	if err != nil {
		return err
	}

	included, err := c.includedInvariantAssessments(manifest)
	if err != nil {
		return err
	}
	expected := make(map[string]expectedInvariantAssessment, len(included))
	for _, invariant := range included {
		expected[invariant.definition.ID] = invariant
	}

	claimByID := make(map[string]InvariantAssessmentClaim, len(normalizedClaims))
	for i, claim := range normalizedClaims {
		if _, duplicate := claimByID[claim.InvariantID]; duplicate {
			return fmt.Errorf("invariant_assessments[%d] duplicates invariant %q", i, claim.InvariantID)
		}
		if _, ok := expected[claim.InvariantID]; !ok {
			return fmt.Errorf("invariant_assessments[%d] names invariant %q that was not included in this model context", i, claim.InvariantID)
		}
		claimByID[claim.InvariantID] = claim
	}
	if len(claimByID) != len(expected) {
		missing := make([]string, 0, len(expected))
		for id := range expected {
			if _, ok := claimByID[id]; !ok {
				missing = append(missing, id)
			}
		}
		slices.Sort(missing)
		return fmt.Errorf("invariant_assessments must contain exactly one claim for each included invariant; missing %v", missing)
	}

	ids := make([]string, 0, len(expected))
	for id := range expected {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	assessments := make([]InvariantAssessment, 0, len(ids))
	for _, id := range ids {
		claim := claimByID[id]
		expectedInvariant := expected[id]
		if claim.Status == InvariantViolated {
			index := *claim.FindingIndex
			if index >= len(result.Findings) {
				return fmt.Errorf("invariant %q finding_index %d is outside canonical findings", id, index)
			}
			if result.Findings[index].Severity != string(expectedInvariant.definition.Severity) {
				return fmt.Errorf("invariant %q finding severity %q does not equal definition severity %q", id, result.Findings[index].Severity, expectedInvariant.definition.Severity)
			}
		}
		assessment := InvariantAssessment{
			InvariantID:          id,
			ContextItemID:        expectedInvariant.item.ID,
			InvariantContentHash: expectedInvariant.item.ContentHash,
			Severity:             expectedInvariant.item.InvariantSeverity,
			Status:               claim.Status,
			Summary:              claim.Summary,
			MissingEvidence:      slices.Clone(claim.MissingEvidence),
		}
		if claim.FindingIndex != nil {
			assessment.FindingIndex = new(*claim.FindingIndex)
		}
		assessments = append(assessments, assessment)
	}
	result.InvariantVerification = &InvariantVerificationResult{
		ContextManifestFingerprint: manifest.Fingerprint,
		Assessments:                assessments,
	}
	return nil
}
