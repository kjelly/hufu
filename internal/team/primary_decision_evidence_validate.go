package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"unicode/utf8"
)

func ValidatePrimaryDecisionEvidence(value *PrimaryDecisionEvidenceV1) error {
	if err := validatePrimaryEvidenceEnvelope(value); err != nil {
		return err
	}
	stats, seen, err := validatePrimaryEvidenceItems(value)
	if err != nil {
		return err
	}
	if stats.usedBytes != value.Budget.UsedViewBytes || value.Budget.UsedTokens < stats.usedBytes || value.Budget.UsedTokens+value.Budget.ReservedFramingTokens > value.Budget.MaxContextInputTokens {
		return fmt.Errorf("primary evidence used_view_bytes mismatch")
	}
	if stats.mandatoryCount != value.Coverage.MandatorySelectedCount || uint64(len(stats.knownGroups)) != value.Coverage.KnownIndependentGroups || stats.unresolved != value.Coverage.UnresolvedProvenanceCount {
		return fmt.Errorf("primary evidence coverage summary mismatch")
	}
	for _, ids := range [][]string{value.BaseRateItemIDs, value.AssumptionItemIDs} {
		if !slices.IsSorted(ids) {
			return fmt.Errorf("primary evidence typed item IDs are not sorted")
		}
		for _, id := range ids {
			if _, ok := seen[id]; !ok {
				return fmt.Errorf("primary evidence typed item %q is not selected", id)
			}
		}
	}
	return nil
}

func validatePrimaryEvidenceEnvelope(value *PrimaryDecisionEvidenceV1) error {
	if value == nil || value.SchemaVersion != 1 || value.Kind != "primary_decision_evidence" || value.CollectorVersion != primaryEvidenceCollectorVersion || value.NormalizerVersion != primaryEvidenceNormalizerVersion {
		return fmt.Errorf("primary evidence has invalid version or kind")
	}
	if !decisionLogicalIDPattern.MatchString(value.LogicalRunID) || !validDecisionIdentifier(value.BranchID) || value.Generation == 0 || !validDecisionDigest(value.RequirementDigest) || !validDecisionDigest(value.SupportRevisionDigest) || !validDecisionDigest(value.SourceIndexDigest) {
		return fmt.Errorf("primary evidence has invalid identity or digest")
	}
	if !validDecisionIdentifier(value.SupportCursor.EventID) || !validDecisionDigest(value.SupportCursor.EventHash) || !validDecisionDigest(value.SupportCursor.LineageDigest) ||
		validateDecisionArtifactRef(value.RedactionPolicyRef) != nil || validateDecisionArtifactRef(value.LimitsRef) != nil || validateDecisionArtifactRef(value.Coverage.InventoryRef) != nil {
		return fmt.Errorf("primary evidence has invalid cursor or reference")
	}
	if !value.Scope.ExcludesPrimaryLineage || value.Coverage.InventoryCount != value.Coverage.SelectedCount+value.Coverage.ExcludedCount || value.Coverage.SelectedCount != uint64(len(value.Items)) {
		return fmt.Errorf("primary evidence has inconsistent scope or coverage")
	}
	if !slices.IsSorted(value.Scope.ExecutionRunIDs) || len(slices.Compact(slices.Clone(value.Scope.ExecutionRunIDs))) != len(value.Scope.ExecutionRunIDs) {
		return fmt.Errorf("primary evidence execution scope is not sorted and unique")
	}
	for _, runID := range value.Scope.ExecutionRunIDs {
		if !validDecisionIdentifier(runID) {
			return fmt.Errorf("primary evidence has invalid execution run id %q", runID)
		}
	}
	if !decisionArtifactRefsSortedUnique(value.Scope.ImportedEvidenceRefs) {
		return fmt.Errorf("primary evidence imports are not sorted and unique")
	}
	for _, ref := range value.Scope.ImportedEvidenceRefs {
		if err := validateDecisionArtifactRef(ref); err != nil {
			return err
		}
	}
	reasonCount := value.Coverage.ExcludedReasonCounts.Duplicate + value.Coverage.ExcludedReasonCounts.OutOfScope +
		value.Coverage.ExcludedReasonCounts.Superseded + value.Coverage.ExcludedReasonCounts.RedactedUnusable +
		value.Coverage.ExcludedReasonCounts.OptionalOversize + value.Coverage.ExcludedReasonCounts.Budget +
		value.Coverage.ExcludedReasonCounts.UnsupportedMedia
	if reasonCount != value.Coverage.ExcludedCount {
		return fmt.Errorf("primary evidence excluded reason count mismatch")
	}
	if value.Budget.CountMethod != "pinned_tokenizer" && value.Budget.CountMethod != "utf8_byte_upper_budget" || value.Budget.MaxContextInputTokens == 0 {
		return fmt.Errorf("primary evidence has invalid budget")
	}
	return nil
}

type primaryEvidenceValidationStats struct {
	usedBytes      uint64
	mandatoryCount uint64
	unresolved     uint64
	knownGroups    map[string]struct{}
}

func validatePrimaryEvidenceItems(value *PrimaryDecisionEvidenceV1) (primaryEvidenceValidationStats, map[string]struct{}, error) {
	stats := primaryEvidenceValidationStats{knownGroups: make(map[string]struct{})}
	seen := make(map[string]struct{}, len(value.Items))
	baseRateIDs := sliceSet(value.BaseRateItemIDs)
	assumptionIDs := sliceSet(value.AssumptionItemIDs)
	for _, item := range value.Items {
		if _, duplicate := seen[item.ItemID]; duplicate {
			return stats, nil, fmt.Errorf("primary evidence has duplicate item %q", item.ItemID)
		}
		seen[item.ItemID] = struct{}{}
		if err := validatePrimaryEvidenceItem(item, baseRateIDs, assumptionIDs); err != nil {
			return stats, nil, err
		}
		if len(item.MandatoryReasons) > 0 {
			stats.mandatoryCount++
		}
		if item.Independence.KnownRoot {
			stats.knownGroups[item.Independence.GroupID] = struct{}{}
		} else {
			stats.unresolved++
		}
		stats.usedBytes += uint64(len(item.Content))
	}
	return stats, seen, nil
}

func validatePrimaryEvidenceItem(item PrimaryEvidenceItemV1, baseRateIDs, assumptionIDs map[string]struct{}) error {
	hash := sha256.Sum256([]byte(item.Content))
	if !validDecisionIdentifier(item.ItemID) || validateDecisionArtifactRef(item.ViewRef) != nil || !utf8.ValidString(item.Content) || len(item.Content) > 65536 ||
		(item.ContentFormat != "text" && item.ContentFormat != "json") || uint64(len(item.Content)) != item.Extraction.ViewBytes ||
		item.ViewRef.SizeBytes != item.Extraction.ViewBytes || item.ViewRef.SHA256 != hex.EncodeToString(hash[:]) {
		return fmt.Errorf("primary evidence item %q has invalid view", item.ItemID)
	}
	if err := validatePrimaryEvidenceSource(item.Source); err != nil {
		return err
	}
	if err := validatePrimaryEvidenceVerification(item.Verification); err != nil {
		return err
	}
	if err := validateMandatoryReasons(item.MandatoryReasons); err != nil {
		return err
	}
	if item.EpistemicStatus != "observed" && item.EpistemicStatus != "reported" && item.EpistemicStatus != "advisory" {
		return fmt.Errorf("primary evidence item %q has invalid epistemic status", item.ItemID)
	}
	if !validDecisionIdentifier(item.Independence.GroupID) || item.Independence.CountsTowardMinimum != item.Independence.KnownRoot {
		return fmt.Errorf("primary evidence item %q has invalid independence", item.ItemID)
	}
	if _, ok := baseRateIDs[item.ItemID]; ok {
		if item.Source.SourceKind != "base_rate" || item.ContentFormat != "json" {
			return fmt.Errorf("primary evidence item %q is not a typed base rate", item.ItemID)
		}
		if err := validatePrimaryEvidenceBaseRate(item.Content); err != nil {
			return err
		}
	}
	if _, ok := assumptionIDs[item.ItemID]; ok {
		if item.Source.SourceKind != "assumption" || item.ContentFormat != "json" {
			return fmt.Errorf("primary evidence item %q is not a typed assumption", item.ItemID)
		}
		decoded, err := primaryEvidenceItemValue(item)
		if err != nil {
			return err
		}
		if _, err := primaryEvidenceAssumption(decoded); err != nil {
			return err
		}
	}
	return nil
}

func decisionArtifactRefsSortedUnique(values []DecisionArtifactRef) bool {
	for index, value := range values {
		if index == 0 {
			continue
		}
		prior := values[index-1]
		if prior.SHA256 > value.SHA256 || prior.SHA256 == value.SHA256 && prior.ID >= value.ID {
			return false
		}
	}
	return true
}
