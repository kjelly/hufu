package team

import (
	"fmt"
	"strings"
)

// ValidateTaskDecisionEvidence validates the configuration-only evidence
// declarations on a static task contract. It checks shape only; artifact
// existence, integrity, and workspace authorization belong to the runtime
// preflight because those facts can change after team configuration loads.
func ValidateTaskDecisionEvidence(input any) error {
	facts, artifacts, rates, assumptions, provenance, err := decisionEvidenceForInput(input)
	if err != nil {
		return err
	}
	for key, value := range facts {
		if canonicalString(key) == "" {
			return fmt.Errorf("decision-facts contains an empty key")
		}
		if _, err := canonicalEncode(value); err != nil {
			return fmt.Errorf("decision-facts[%q] is not canonicalizable: %w", key, err)
		}
	}
	for index, ref := range artifacts {
		if err := validateDeclaredArtifactRef(ref); err != nil {
			return fmt.Errorf("decision-artifacts[%d]: %w", index, err)
		}
	}
	if err := validateDecisionBaseRates(rates); err != nil {
		return err
	}
	seenAssumptions := make(map[string]bool, len(assumptions))
	for index, assumption := range assumptions {
		id := canonicalString(assumption.ID)
		if id == "" || canonicalString(assumption.Statement) == "" {
			return fmt.Errorf("decision-assumptions[%d] requires a non-empty id and statement", index)
		}
		if seenAssumptions[id] {
			return fmt.Errorf("decision-assumptions[%d] duplicates id %q", index, assumption.ID)
		}
		seenAssumptions[id] = true
		if !ValidAssumptionStatus(assumption.EffectiveStatus()) {
			return fmt.Errorf("decision-assumptions[%d] has unsupported status %q", index, assumption.Status)
		}
		for refIndex, ref := range assumption.EvidenceRefs {
			if err := validateDeclaredArtifactRef(ref); err != nil {
				return fmt.Errorf("decision-assumptions[%d].evidence-refs[%d]: %w", index, refIndex, err)
			}
		}
	}
	for index, provenance := range provenance {
		if canonicalString(provenance.SourceID) == "" {
			return fmt.Errorf("decision-provenance[%d] requires source-id", index)
		}
		if sourceType := strings.TrimSpace(provenance.SourceType); sourceType != "" && !validEvidenceSourceType(sourceType) {
			return fmt.Errorf("decision-provenance[%d] has unsupported source-type %q", index, provenance.SourceType)
		}
	}
	return nil
}

func decisionEvidenceForInput(input any) (map[string]any, []ArtifactRef, []BaseRateEvidence, []DecisionAssumption, []EvidenceProvenance, error) {
	switch value := input.(type) {
	case TaskDef:
		return value.DecisionFacts, value.DecisionArtifacts, value.DecisionBaseRates, value.DecisionAssumptions, value.DecisionProvenance, nil
	case TaskOccurrenceProjection:
		return value.DecisionFacts, value.DecisionArtifacts, value.DecisionBaseRates, value.DecisionAssumptions, value.DecisionProvenance, nil
	case *TaskOccurrenceProjection:
		if value == nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("decision evidence input is nil")
		}
		return value.DecisionFacts, value.DecisionArtifacts, value.DecisionBaseRates, value.DecisionAssumptions, value.DecisionProvenance, nil
	case *TodoItem:
		if value == nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("decision evidence input is nil")
		}
		return value.DecisionFacts, value.DecisionArtifacts, value.DecisionBaseRates, value.DecisionAssumptions, value.DecisionProvenance, nil
	default:
		return nil, nil, nil, nil, nil, fmt.Errorf("unsupported decision evidence input %T", input)
	}
}

func validateDecisionBaseRates(rates []BaseRateEvidence) error {
	for index, rate := range rates {
		if err := validateBaseRate(rate); err != nil {
			return fmt.Errorf("decision-base-rates[%d]: %w", index, err)
		}
	}
	return nil
}

func validateDeclaredArtifactRef(ref ArtifactRef) error {
	if strings.TrimSpace(ref.ID) == "" && strings.TrimSpace(ref.SHA256) == "" {
		return fmt.Errorf("artifact reference requires id or sha256")
	}
	if ref.Bytes < 0 || ref.ByteSize < 0 {
		return fmt.Errorf("artifact reference byte counts must not be negative")
	}
	return nil
}

func validEvidenceSourceType(sourceType string) bool {
	switch sourceType {
	case EvidenceSourceArtifact, EvidenceSourceURL, EvidenceSourceToolOutput, EvidenceSourceMemory, EvidenceSourceDeclared:
		return true
	default:
		return false
	}
}
