package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	primaryEvidenceCollectorVersion   = "primary-collector@v1"
	primaryEvidenceNormalizerVersion  = "hufu-json-c14n@v1"
	primaryEvidenceDefaultCandidates  = 4096
	primaryEvidenceDefaultSourceBytes = 32 << 20
	primaryEvidenceMaxLineBytes       = 16 << 10
)

var (
	ErrDecisionEvidenceInsufficient      = errors.New("decision evidence insufficient")
	ErrDecisionEvidenceMandatoryOverflow = errors.New("decision evidence mandatory overflow")
	ErrDecisionEvidenceScanLimit         = errors.New("decision evidence scan limit exceeded")
	errPrimaryEvidenceOptionalOversize   = errors.New("optional evidence exceeds per-item view limit")
)

type preparedPrimaryEvidenceCandidate struct {
	item             PrimaryEvidenceItemV1
	sourceEventOrder uint64
	baseRate         bool
	assumption       bool
	mandatory        bool
	priority         int
	inventoryIndex   int
}

func CollectPrimaryDecisionEvidence(ctx context.Context, request PrimaryEvidenceCollectionRequest) (*PrimaryDecisionEvidenceV1, error) {
	if err := validatePrimaryEvidenceCollectionRequest(request); err != nil {
		return nil, err
	}
	limits := normalizePrimaryEvidenceLimits(request.Limits)
	if len(request.Candidates) > int(limits.MaxCandidates) {
		return nil, fmt.Errorf("%w: candidates=%d limit=%d", ErrDecisionEvidenceScanLimit, len(request.Candidates), limits.MaxCandidates)
	}
	var sourceBytes uint64
	for _, candidate := range request.Candidates {
		if math.MaxUint64-sourceBytes < candidate.Source.SourceArtifact.SizeBytes {
			return nil, fmt.Errorf("%w: source byte count overflow", ErrDecisionEvidenceScanLimit)
		}
		sourceBytes += candidate.Source.SourceArtifact.SizeBytes
	}
	if sourceBytes > limits.MaxAggregateSourceBytes {
		return nil, fmt.Errorf("%w: source_bytes=%d limit=%d", ErrDecisionEvidenceScanLimit, sourceBytes, limits.MaxAggregateSourceBytes)
	}

	candidates := normalizePrimaryEvidenceRootGroups(request.Candidates)
	sort.SliceStable(candidates, func(i, j int) bool { return primaryEvidenceCandidateSourceLess(candidates[i], candidates[j]) })
	if err := applyPrimaryEvidenceCandidateMandatoryFloor(candidates, limits); err != nil {
		return nil, err
	}
	prepared := make([]preparedPrimaryEvidenceCandidate, 0, len(candidates))
	inventory := make([]PrimaryEvidenceInventoryEntryV1, 0, len(request.Candidates))
	seenItems := make(map[string]struct{}, len(request.Candidates))
	itemDigests := make(map[string]string, len(request.Candidates))
	reasons := PrimaryEvidenceExcludedReasonCountsV1{}
	for _, candidate := range candidates {
		itemID, itemIDErr := primaryEvidenceItemID(candidate.Source)
		if itemIDErr != nil {
			return nil, itemIDErr
		}
		entry := PrimaryEvidenceInventoryEntryV1{
			ItemID: itemID, SourceRef: candidate.Source.SourceArtifact, SourceEventID: candidate.Source.SourceEventID,
			MandatoryReasons: slices.Clone(candidate.MandatoryReasons), Selection: "excluded", Reason: "budget",
		}
		item, err := preparePrimaryEvidenceCandidate(ctx, request.Store, candidate, limits.MaxItemViewBytes)
		if err != nil {
			if len(candidate.MandatoryReasons) > 0 {
				if errors.Is(err, errPrimaryEvidenceOptionalOversize) {
					return nil, fmt.Errorf("%w: item %s: %v", ErrDecisionEvidenceMandatoryOverflow, itemID, err)
				}
				return nil, fmt.Errorf("%w: item %s: %v", ErrDecisionEvidenceInsufficient, itemID, err)
			}
			if errors.Is(err, errPrimaryEvidenceOptionalOversize) {
				entry.Reason = "optional_oversize"
				reasons.OptionalOversize++
			} else {
				entry.Reason = "redacted_unusable"
				reasons.RedactedUnusable++
			}
			inventory = append(inventory, entry)
			continue
		}
		if _, duplicate := seenItems[item.ItemID]; duplicate {
			digest, digestErr := DecisionContractDigest("hufu/decision-evidence-item-content/v1", item)
			if digestErr != nil {
				return nil, digestErr
			}
			if itemDigests[item.ItemID] != digest {
				return nil, fmt.Errorf("duplicate evidence item %s has conflicting content", item.ItemID)
			}
			entry.Reason = "duplicate"
			reasons.Duplicate++
			inventory = append(inventory, entry)
			continue
		}
		seenItems[item.ItemID] = struct{}{}
		itemDigest, digestErr := DecisionContractDigest("hufu/decision-evidence-item-content/v1", item)
		if digestErr != nil {
			return nil, digestErr
		}
		itemDigests[item.ItemID] = itemDigest
		inventory = append(inventory, entry)
		prepared = append(prepared, preparedPrimaryEvidenceCandidate{
			item: item, sourceEventOrder: candidate.SourceEventOrder, baseRate: candidate.BaseRate,
			assumption: candidate.Assumption, mandatory: len(item.MandatoryReasons) > 0,
			priority: primaryEvidencePriority(item), inventoryIndex: len(inventory) - 1,
		})
	}
	if err := validatePrimaryEvidenceParentGraph(prepared); err != nil {
		return nil, err
	}
	if err := applyPrimaryEvidenceMandatoryFloor(prepared, limits); err != nil {
		return nil, err
	}
	sort.SliceStable(prepared, func(i, j int) bool { return primaryEvidenceLess(prepared[i], prepared[j]) })

	selected := make([]PrimaryEvidenceItemV1, 0, min(len(prepared), int(limits.MaxItems)))
	usedViewBytes := uint64(0)
	usedBudgetUnits := limits.PromptFramingBytes
	mandatoryCount := uint64(0)
	for _, candidate := range prepared {
		viewBytes := uint64(len(candidate.item.Content))
		availableViews := limits.MaxViewBytes - minUint64(usedViewBytes, limits.MaxViewBytes)
		availableContext := limits.MaxContextInputTokens - minUint64(usedBudgetUnits, limits.MaxContextInputTokens)
		contextAfterReserve := limits.MaxContextInputTokens - limits.ReservedFramingTokens
		availableAfterReserve := contextAfterReserve - minUint64(usedBudgetUnits, contextAfterReserve)
		fits := len(selected) < int(limits.MaxItems) && viewBytes <= availableViews && viewBytes <= availableContext && viewBytes <= availableAfterReserve
		if !fits {
			if candidate.mandatory {
				return nil, fmt.Errorf("%w: item %s", ErrDecisionEvidenceMandatoryOverflow, candidate.item.ItemID)
			}
			reasons.Budget++
			continue
		}
		selected = append(selected, candidate.item)
		inventory[candidate.inventoryIndex].Selection = "selected"
		inventory[candidate.inventoryIndex].Reason = "selected"
		usedViewBytes += viewBytes
		usedBudgetUnits += viewBytes
		if candidate.mandatory {
			mandatoryCount++
		}
	}
	knownGroups := make(map[string]struct{})
	unresolvedProvenance := uint64(0)
	baseRateIDs := make([]string, 0)
	assumptionIDs := make([]string, 0)
	for _, candidate := range prepared {
		if inventory[candidate.inventoryIndex].Selection != "selected" {
			continue
		}
		if candidate.item.Independence.KnownRoot && candidate.item.Independence.CountsTowardMinimum {
			knownGroups[candidate.item.Independence.GroupID] = struct{}{}
		} else if !candidate.item.Independence.KnownRoot {
			unresolvedProvenance++
		}
		if candidate.baseRate {
			baseRateIDs = append(baseRateIDs, candidate.item.ItemID)
		}
		if candidate.assumption {
			assumptionIDs = append(assumptionIDs, candidate.item.ItemID)
		}
	}
	if len(knownGroups) < int(limits.KnownIndependenceMinimum) || len(baseRateIDs) < int(limits.BaseRateMinimum) {
		return nil, fmt.Errorf("%w: independent_groups=%d/%d base_rates=%d/%d", ErrDecisionEvidenceInsufficient, len(knownGroups), limits.KnownIndependenceMinimum, len(baseRateIDs), limits.BaseRateMinimum)
	}

	inventoryBytes, err := CanonicalDecisionJSON(inventory)
	if err != nil {
		return nil, err
	}
	inventoryRef, err := putPrimaryEvidenceArtifact(ctx, request.Store, "decision-inventory", "application/json", inventoryBytes)
	if err != nil {
		return nil, err
	}
	sourceIndexDigest, err := primaryEvidenceSourceIndexDigest(request.BranchID, request.LogicalRunID, candidates)
	if err != nil {
		return nil, err
	}
	slices.Sort(baseRateIDs)
	slices.Sort(assumptionIDs)
	result := &PrimaryDecisionEvidenceV1{
		SchemaVersion: 1, Kind: "primary_decision_evidence", LogicalRunID: request.LogicalRunID, BranchID: request.BranchID,
		Generation: request.Generation, RequirementDigest: request.RequirementDigest, SupportCursor: request.SupportCursor,
		SupportRevisionDigest: request.SupportRevisionDigest, SourceIndexDigest: sourceIndexDigest,
		CollectorVersion: primaryEvidenceCollectorVersion, NormalizerVersion: primaryEvidenceNormalizerVersion,
		RedactionPolicyRef: request.RedactionPolicyRef, LimitsRef: request.LimitsRef,
		Scope: PrimaryEvidenceScopeV1{ExecutionRunIDs: sortedUniquePrimaryEvidenceStrings(request.ExecutionRunIDs), ImportedEvidenceRefs: sortedDecisionArtifactRefs(request.ImportedEvidenceRefs), ExcludesPrimaryLineage: true},
		Items: selected,
		Coverage: PrimaryEvidenceCoverageV1{
			InventoryRef: inventoryRef, InventoryCount: uint64(len(inventory)), SelectedCount: uint64(len(selected)),
			ExcludedCount: uint64(len(inventory) - len(selected)), ExcludedReasonCounts: reasons,
			MandatorySelectedCount: mandatoryCount, KnownIndependentGroups: uint64(len(knownGroups)), UnresolvedProvenanceCount: unresolvedProvenance,
		},
		Budget: PrimaryEvidenceBudgetV1{
			UsedViewBytes: usedViewBytes, UsedTokens: usedBudgetUnits, ReservedFramingTokens: limits.ReservedFramingTokens,
			CountMethod: "utf8_byte_upper_budget", MaxContextInputTokens: limits.MaxContextInputTokens,
		},
		BaseRateItemIDs: baseRateIDs, AssumptionItemIDs: assumptionIDs,
	}
	if err := ValidatePrimaryDecisionEvidence(result); err != nil {
		return nil, err
	}
	return result, nil
}

func primaryEvidenceCandidateSourceLess(left, right PrimaryEvidenceCandidate) bool {
	if left.SourceEventOrder != right.SourceEventOrder {
		return left.SourceEventOrder < right.SourceEventOrder
	}
	leftTask, rightTask := "", ""
	leftAttempt, rightAttempt := uint32(0), uint32(0)
	if left.Source.Task != nil {
		leftTask, leftAttempt = left.Source.Task.TaskID, left.Source.Task.Attempt
	}
	if right.Source.Task != nil {
		rightTask, rightAttempt = right.Source.Task.TaskID, right.Source.Task.Attempt
	}
	if leftTask != rightTask {
		return leftTask < rightTask
	}
	if leftAttempt != rightAttempt {
		return leftAttempt < rightAttempt
	}
	if left.Source.JSONPointer != right.Source.JSONPointer {
		return left.Source.JSONPointer < right.Source.JSONPointer
	}
	if left.Source.SourceArtifact.SHA256 != right.Source.SourceArtifact.SHA256 {
		return left.Source.SourceArtifact.SHA256 < right.Source.SourceArtifact.SHA256
	}
	leftID, _ := primaryEvidenceItemID(left.Source)
	rightID, _ := primaryEvidenceItemID(right.Source)
	return leftID < rightID
}

func minUint64(left, right uint64) uint64 {
	if left < right {
		return left
	}
	return right
}

func normalizePrimaryEvidenceLimits(limits PrimaryEvidenceLimits) PrimaryEvidenceLimits {
	if limits.MaxCandidates == 0 {
		limits.MaxCandidates = primaryEvidenceDefaultCandidates
	}
	if limits.MaxAggregateSourceBytes == 0 {
		limits.MaxAggregateSourceBytes = primaryEvidenceDefaultSourceBytes
	}
	return limits
}

func validatePrimaryEvidenceCollectionRequest(request PrimaryEvidenceCollectionRequest) error {
	if !decisionLogicalIDPattern.MatchString(request.LogicalRunID) || !validDecisionIdentifier(request.BranchID) || request.Generation == 0 || !validDecisionDigest(request.RequirementDigest) || !validDecisionDigest(request.SupportRevisionDigest) {
		return fmt.Errorf("primary evidence request has invalid identity")
	}
	if !validDecisionIdentifier(request.SupportCursor.EventID) || !validDecisionDigest(request.SupportCursor.EventHash) || !validDecisionDigest(request.SupportCursor.LineageDigest) {
		return fmt.Errorf("primary evidence request has invalid support cursor")
	}
	if request.Store == nil || validateDecisionArtifactRef(request.RedactionPolicyRef) != nil || validateDecisionArtifactRef(request.LimitsRef) != nil {
		return fmt.Errorf("primary evidence request has no store or policy references")
	}
	for _, runID := range request.ExecutionRunIDs {
		if !validDecisionIdentifier(runID) {
			return fmt.Errorf("primary evidence request has invalid execution run id %q", runID)
		}
	}
	for _, ref := range request.ImportedEvidenceRefs {
		if err := validateDecisionArtifactRef(ref); err != nil {
			return fmt.Errorf("primary evidence request has invalid import: %w", err)
		}
	}
	limits := normalizePrimaryEvidenceLimits(request.Limits)
	if limits.MaxItems == 0 || limits.MaxViewBytes == 0 || limits.MaxItemViewBytes == 0 || limits.MaxItemViewBytes > 65536 || limits.MaxContextInputTokens == 0 || limits.ReservedFramingTokens > limits.MaxContextInputTokens || limits.PromptFramingBytes > limits.MaxContextInputTokens-limits.ReservedFramingTokens {
		return fmt.Errorf("primary evidence request has invalid limits")
	}
	return nil
}

func preparePrimaryEvidenceCandidate(ctx context.Context, store ArtifactStore, candidate PrimaryEvidenceCandidate, maxItemBytes uint64) (PrimaryEvidenceItemV1, error) {
	candidate.MandatoryReasons = slices.Clone(candidate.MandatoryReasons)
	slices.Sort(candidate.MandatoryReasons)
	if err := validatePrimaryEvidenceSource(candidate.Source); err != nil {
		return PrimaryEvidenceItemV1{}, err
	}
	if !utf8.ValidString(candidate.Content) || (candidate.ContentFormat != "text" && candidate.ContentFormat != "json") {
		return PrimaryEvidenceItemV1{}, fmt.Errorf("evidence content is not valid UTF-8 text or JSON")
	}
	if err := validatePrimaryEvidenceVerification(candidate.Verification); err != nil {
		return PrimaryEvidenceItemV1{}, err
	}
	if err := validateMandatoryReasons(candidate.MandatoryReasons); err != nil {
		return PrimaryEvidenceItemV1{}, err
	}
	if candidate.EpistemicStatus != "observed" && candidate.EpistemicStatus != "reported" && candidate.EpistemicStatus != "advisory" {
		return PrimaryEvidenceItemV1{}, fmt.Errorf("invalid epistemic status %q", candidate.EpistemicStatus)
	}
	if candidate.BaseRate != (candidate.Source.SourceKind == "base_rate") || candidate.Assumption != (candidate.Source.SourceKind == "assumption") {
		return PrimaryEvidenceItemV1{}, fmt.Errorf("typed evidence flags do not match source kind")
	}
	content := candidate.Content
	if candidate.ContentFormat == "json" {
		redacted, err := utils.RedactJSONCompact([]byte(content))
		if err != nil {
			return PrimaryEvidenceItemV1{}, fmt.Errorf("redact evidence JSON: %w", err)
		}
		content = string(redacted)
	} else {
		content = strings.ReplaceAll(strings.ReplaceAll(utils.RedactSecrets(content), "\r\n", "\n"), "\r", "\n")
	}
	if candidate.BaseRate {
		if err := validatePrimaryEvidenceBaseRate(content); err != nil {
			return PrimaryEvidenceItemV1{}, err
		}
	}
	itemID, err := primaryEvidenceItemID(candidate.Source)
	if err != nil {
		return PrimaryEvidenceItemV1{}, err
	}
	view, extraction, metadataOnly, err := primaryEvidenceView(content, candidate.ContentFormat, maxItemBytes, len(candidate.MandatoryReasons) > 0, candidate.Source.SourceArtifact)
	if err != nil {
		return PrimaryEvidenceItemV1{}, err
	}
	if metadataOnly {
		candidate.Source.SourceKind = "artifact_metadata"
		candidate.ContentFormat = "json"
	}
	viewRef, err := putPrimaryEvidenceArtifact(ctx, store, "decision-view", contentMediaType(candidate.ContentFormat), []byte(view))
	if err != nil {
		return PrimaryEvidenceItemV1{}, err
	}
	groupID, known, err := primaryEvidenceIndependenceGroup(candidate.RootIdentities)
	if err != nil {
		return PrimaryEvidenceItemV1{}, err
	}
	return PrimaryEvidenceItemV1{
		ItemID: itemID, Source: candidate.Source, ViewRef: viewRef, ContentFormat: candidate.ContentFormat, Content: view,
		Verification: candidate.Verification, MandatoryReasons: slices.Clone(candidate.MandatoryReasons), EpistemicStatus: candidate.EpistemicStatus,
		Independence: PrimaryEvidenceIndependenceV1{GroupID: groupID, KnownRoot: known, CountsTowardMinimum: known}, Extraction: extraction,
	}, nil
}

func applyPrimaryEvidenceMandatoryFloor(candidates []preparedPrimaryEvidenceCandidate, limits PrimaryEvidenceLimits) error {
	baseRates := uint32(0)
	for index := range candidates {
		if candidates[index].baseRate {
			baseRates++
		}
	}
	if baseRates < limits.BaseRateMinimum {
		return fmt.Errorf("%w: base_rates=%d/%d", ErrDecisionEvidenceInsufficient, baseRates, limits.BaseRateMinimum)
	}

	knownGroups := make(map[string]struct{})
	for index := range candidates {
		item := candidates[index].item
		if !item.Independence.KnownRoot {
			continue
		}
		if _, exists := knownGroups[item.Independence.GroupID]; exists {
			continue
		}
		knownGroups[item.Independence.GroupID] = struct{}{}
	}
	if len(knownGroups) < int(limits.KnownIndependenceMinimum) {
		return fmt.Errorf("%w: independent_groups=%d/%d", ErrDecisionEvidenceInsufficient, len(knownGroups), limits.KnownIndependenceMinimum)
	}
	for index := range candidates {
		candidates[index].priority = primaryEvidencePriority(candidates[index].item)
	}
	return nil
}

func applyPrimaryEvidenceCandidateMandatoryFloor(candidates []PrimaryEvidenceCandidate, limits PrimaryEvidenceLimits) error {
	for index := range candidates {
		if candidates[index].Verification.State == "failed" {
			candidates[index].MandatoryReasons = appendMandatoryReason(candidates[index].MandatoryReasons, "failed_assertion")
		}
	}
	baseRates := uint32(0)
	for index := range candidates {
		if !candidates[index].BaseRate {
			continue
		}
		if baseRates < limits.BaseRateMinimum {
			candidates[index].MandatoryReasons = appendMandatoryReason(candidates[index].MandatoryReasons, "outside_view_minimum")
		}
		baseRates++
	}
	if baseRates < limits.BaseRateMinimum {
		return fmt.Errorf("%w: base_rates=%d/%d", ErrDecisionEvidenceInsufficient, baseRates, limits.BaseRateMinimum)
	}
	knownGroups := make(map[string]struct{})
	for index := range candidates {
		groupID, known, err := primaryEvidenceIndependenceGroup(candidates[index].RootIdentities)
		if err != nil {
			return err
		}
		if !known {
			continue
		}
		if _, exists := knownGroups[groupID]; exists {
			continue
		}
		knownGroups[groupID] = struct{}{}
		if uint32(len(knownGroups)) <= limits.KnownIndependenceMinimum {
			candidates[index].MandatoryReasons = appendMandatoryReason(candidates[index].MandatoryReasons, "independence_minimum")
		}
	}
	if len(knownGroups) < int(limits.KnownIndependenceMinimum) {
		return fmt.Errorf("%w: independent_groups=%d/%d", ErrDecisionEvidenceInsufficient, len(knownGroups), limits.KnownIndependenceMinimum)
	}
	return nil
}

func appendMandatoryReason(reasons []string, reason string) []string {
	result := slices.Clone(reasons)
	if !slices.Contains(result, reason) {
		result = append(result, reason)
	}
	slices.Sort(result)
	return result
}

func primaryEvidenceView(content, format string, maxBytes uint64, mandatory bool, source DecisionArtifactRef) (string, PrimaryEvidenceExtractionV1, bool, error) {
	sourceSize := uint64(len(content))
	wholeAlgorithm := "whole_text_v1"
	if format == "json" {
		wholeAlgorithm = "whole_json_v1"
	}
	if sourceSize <= maxBytes {
		lines := uint64(strings.Count(content, "\n") + 1)
		return content, PrimaryEvidenceExtractionV1{Algorithm: wholeAlgorithm, SelectedLineRanges: []PrimaryEvidenceLineRangeV1{{Start: 1, End: lines}}, SourceBytes: sourceSize, ViewBytes: sourceSize}, false, nil
	}
	if format == "json" || mandatory {
		return "", PrimaryEvidenceExtractionV1{}, false, fmt.Errorf("%w", errPrimaryEvidenceOptionalOversize)
	}
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if len(line) > primaryEvidenceMaxLineBytes {
			metadata, err := CanonicalDecisionJSON(map[string]any{"media_type": source.MediaType, "sha256": source.SHA256, "size_bytes": source.SizeBytes})
			if err != nil {
				return "", PrimaryEvidenceExtractionV1{}, false, err
			}
			if uint64(len(metadata)) > maxBytes {
				return "", PrimaryEvidenceExtractionV1{}, false, fmt.Errorf("artifact metadata exceeds per-item view limit")
			}
			return string(metadata), PrimaryEvidenceExtractionV1{Algorithm: "metadata_only_v1", Truncated: true, SourceBytes: sourceSize, ViewBytes: uint64(len(metadata)), RemovedLineCount: uint64(len(lines))}, true, nil
		}
	}
	return headTailPrimaryEvidenceView(lines, sourceSize, maxBytes)
}

func headTailPrimaryEvidenceView(lines []string, sourceSize, maxBytes uint64) (string, PrimaryEvidenceExtractionV1, bool, error) {
	headQuota := maxBytes * 3 / 4
	tailQuota := maxBytes - headQuota
	headEnd := 0
	headBytes := uint64(0)
	for headEnd < len(lines) && headBytes+uint64(len(lines[headEnd])+1) <= headQuota {
		headBytes += uint64(len(lines[headEnd]) + 1)
		headEnd++
	}
	tailStart := len(lines)
	tailBytes := uint64(0)
	for tailStart > headEnd && tailBytes+uint64(len(lines[tailStart-1])+1) <= tailQuota {
		tailStart--
		tailBytes += uint64(len(lines[tailStart]) + 1)
	}
	for {
		removed := tailStart - headEnd
		marker := fmt.Sprintf("[OMITTED_LINES:%d]", removed)
		parts := append([]string(nil), lines[:headEnd]...)
		parts = append(parts, marker)
		parts = append(parts, lines[tailStart:]...)
		view := strings.Join(parts, "\n")
		if uint64(len(view)) <= maxBytes {
			ranges := make([]PrimaryEvidenceLineRangeV1, 0, 2)
			if headEnd > 0 {
				ranges = append(ranges, PrimaryEvidenceLineRangeV1{Start: 1, End: uint64(headEnd)})
			}
			if tailStart < len(lines) {
				ranges = append(ranges, PrimaryEvidenceLineRangeV1{Start: uint64(tailStart + 1), End: uint64(len(lines))})
			}
			return view, PrimaryEvidenceExtractionV1{Algorithm: "head_tail_lines_v1", Truncated: true, SelectedLineRanges: ranges, SourceBytes: sourceSize, ViewBytes: uint64(len(view)), RemovedLineCount: uint64(removed)}, false, nil
		}
		if headEnd > 0 && (tailStart == len(lines) || headBytes >= tailBytes) {
			headEnd--
			headBytes -= uint64(len(lines[headEnd]) + 1)
		} else if tailStart < len(lines) {
			tailBytes -= uint64(len(lines[tailStart]) + 1)
			tailStart++
		} else {
			return "", PrimaryEvidenceExtractionV1{}, false, fmt.Errorf("omission marker cannot fit per-item view limit")
		}
	}
}

func primaryEvidenceItemID(source PrimaryEvidenceSourceV1) (string, error) {
	taskID, attempt, revision := any(nil), any(nil), any(nil)
	if source.Task != nil {
		taskID, attempt, revision = source.Task.TaskID, source.Task.Attempt, source.Task.OccurrenceRevision
	}
	digest, err := DecisionContractDigest("hufu/decision-evidence-item/v1", map[string]any{
		"collector_version": primaryEvidenceCollectorVersion, "json_pointer": source.JSONPointer,
		"source_artifact_sha256": source.SourceArtifact.SHA256, "source_event_id": source.SourceEventID,
		"task_attempt": attempt, "task_id": taskID, "task_occurrence_revision": revision,
	})
	if err != nil {
		return "", err
	}
	return "ei_" + digest, nil
}

func primaryEvidenceIndependenceGroup(roots []string) (string, bool, error) {
	roots = slices.Clone(roots)
	slices.Sort(roots)
	roots = slices.Compact(roots)
	if len(roots) == 0 {
		return "ig_unknown", false, nil
	}
	for _, root := range roots {
		if !validDecisionIdentifier(root) {
			return "", false, fmt.Errorf("invalid independence root %q", root)
		}
	}
	digest, err := DecisionContractDigest("hufu/decision-independence-group/v1", struct {
		RootIdentities []string `json:"root_identities"`
	}{RootIdentities: roots})
	if err != nil {
		return "", false, err
	}
	return "ig_" + digest, true, nil
}

func primaryEvidencePriority(item PrimaryEvidenceItemV1) int {
	if len(item.MandatoryReasons) > 0 {
		return 0
	}
	if item.Verification.State == "failed" {
		return 1
	}
	if item.Verification.State == "passed" && item.Verification.Scope == "specific_assertion" {
		return 2
	}
	switch item.Source.SourceKind {
	case "receipt":
		return 3
	case "base_rate":
		return 4
	case "fact":
		return 5
	case "artifact_text":
		return 6
	case "advisory_summary":
		return 7
	default:
		return 8
	}
}

func primaryEvidenceLess(left, right preparedPrimaryEvidenceCandidate) bool {
	if left.priority != right.priority {
		return left.priority < right.priority
	}
	if left.sourceEventOrder != right.sourceEventOrder {
		return left.sourceEventOrder < right.sourceEventOrder
	}
	leftTask, rightTask := "", ""
	leftAttempt, rightAttempt := uint32(0), uint32(0)
	if left.item.Source.Task != nil {
		leftTask, leftAttempt = left.item.Source.Task.TaskID, left.item.Source.Task.Attempt
	}
	if right.item.Source.Task != nil {
		rightTask, rightAttempt = right.item.Source.Task.TaskID, right.item.Source.Task.Attempt
	}
	if leftTask != rightTask {
		return leftTask < rightTask
	}
	if leftAttempt != rightAttempt {
		return leftAttempt < rightAttempt
	}
	if left.item.Source.JSONPointer != right.item.Source.JSONPointer {
		return left.item.Source.JSONPointer < right.item.Source.JSONPointer
	}
	return left.item.ItemID < right.item.ItemID
}

func putPrimaryEvidenceArtifact(ctx context.Context, store ArtifactStore, role, mediaType string, data []byte) (DecisionArtifactRef, error) {
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	result, err := store.Put(ctx, PutArtifactRequest{ID: role + "-" + digest, Kind: role, Role: role, Path: "decision/" + role + "/" + digest, MediaType: mediaType, Content: data})
	if err != nil {
		return DecisionArtifactRef{}, err
	}
	return DecisionArtifactRef{ID: result.ID, SHA256: result.SHA256, MediaType: result.MediaType, SizeBytes: uint64(result.ByteSize)}, nil
}

func primaryEvidenceSourceIndexDigest(branchID, logicalRunID string, candidates []PrimaryEvidenceCandidate) (string, error) {
	type occurrence struct {
		TaskID             string `json:"task_id"`
		OccurrenceRevision uint32 `json:"occurrence_revision"`
		ResultEventID      string `json:"result_event_id"`
	}
	refs := make([]DecisionArtifactRef, 0, len(candidates))
	occurrences := make([]occurrence, 0, len(candidates))
	seenRefs := make(map[DecisionArtifactRef]struct{})
	seenOccurrences := make(map[occurrence]struct{})
	for _, candidate := range candidates {
		ref := candidate.Source.SourceArtifact
		if _, exists := seenRefs[ref]; !exists {
			seenRefs[ref] = struct{}{}
			refs = append(refs, ref)
		}
		if task := candidate.Source.Task; task != nil {
			value := occurrence{TaskID: task.TaskID, OccurrenceRevision: task.OccurrenceRevision, ResultEventID: task.ResultEventID}
			if _, exists := seenOccurrences[value]; !exists {
				seenOccurrences[value] = struct{}{}
				occurrences = append(occurrences, value)
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		left, right := refs[i], refs[j]
		return left.SHA256 < right.SHA256 || left.SHA256 == right.SHA256 && (left.ID < right.ID || left.ID == right.ID && (left.MediaType < right.MediaType || left.MediaType == right.MediaType && left.SizeBytes < right.SizeBytes))
	})
	sort.Slice(occurrences, func(i, j int) bool {
		left, right := occurrences[i], occurrences[j]
		return left.TaskID < right.TaskID || left.TaskID == right.TaskID && (left.OccurrenceRevision < right.OccurrenceRevision || left.OccurrenceRevision == right.OccurrenceRevision && left.ResultEventID < right.ResultEventID)
	})
	return DecisionContractDigest("hufu/decision-source-index/v1", struct {
		BranchID        string                `json:"branch_id"`
		LogicalRunID    string                `json:"logical_run_id"`
		Sources         []DecisionArtifactRef `json:"sources"`
		TaskOccurrences []occurrence          `json:"task_occurrences"`
	}{BranchID: branchID, LogicalRunID: logicalRunID, Sources: refs, TaskOccurrences: occurrences})
}

func validatePrimaryEvidenceParentGraph(candidates []preparedPrimaryEvidenceCandidate) error {
	parents := make(map[string][]string, len(candidates))
	for _, candidate := range candidates {
		parents[candidate.item.ItemID] = candidate.item.Source.DerivedParentItemIDs
	}
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("evidence provenance contains a cycle at %s", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, parent := range parents[id] {
			if _, internal := parents[parent]; internal {
				if err := visit(parent); err != nil {
					return err
				}
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for id := range parents {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func validatePrimaryEvidenceSource(source PrimaryEvidenceSourceV1) error {
	validKinds := map[string]bool{"assertion": true, "receipt": true, "fact": true, "artifact_text": true, "artifact_metadata": true, "base_rate": true, "assumption": true, "advisory_summary": true}
	if !validKinds[source.SourceKind] || validateDecisionArtifactRef(source.SourceArtifact) != nil || len(source.JSONPointer) > 1024 || !validDecisionIdentifier(source.SourceEventID) {
		return fmt.Errorf("invalid primary evidence source")
	}
	if source.ProvenanceAuthority != "runtime_observed" && source.ProvenanceAuthority != "operator_declared" && source.ProvenanceAuthority != "model_reported" {
		return fmt.Errorf("invalid provenance authority")
	}
	if err := validateJSONPointer(source.JSONPointer); err != nil {
		return fmt.Errorf("invalid evidence JSON pointer: %w", err)
	}
	if source.Task != nil && (!validDecisionIdentifier(source.Task.TaskID) || source.Task.OccurrenceRevision == 0 || source.Task.Attempt == 0 || !validDecisionIdentifier(source.Task.ProducerExecutionRunID) || !validDecisionIdentifier(source.Task.ResultEventID)) {
		return fmt.Errorf("invalid evidence task source")
	}
	for _, identity := range []*string{source.ProducerAgentID, source.ModelIdentity, source.ProviderIdentity} {
		if identity != nil && !validDecisionIdentifier(*identity) {
			return fmt.Errorf("invalid evidence producer identity")
		}
	}
	if source.ProvenanceAuthority == "model_reported" && len(source.DerivedParentItemIDs) > 0 {
		return fmt.Errorf("model-reported evidence cannot set runtime-derived parents")
	}
	parents := slices.Clone(source.DerivedParentItemIDs)
	slices.Sort(parents)
	if len(parents) != len(slices.Compact(parents)) {
		return fmt.Errorf("evidence source has duplicate parent item ids")
	}
	for _, parent := range parents {
		if !validDecisionIdentifier(parent) {
			return fmt.Errorf("invalid evidence parent item id %q", parent)
		}
	}
	return nil
}

func validatePrimaryEvidenceVerification(value PrimaryEvidenceVerificationV1) error {
	validState := map[string]bool{"passed": true, "failed": true, "not_checked": true, "stale": true, "inconclusive": true}
	validScope := map[string]bool{"specific_assertion": true, "integrity_only": true, "none": true}
	if !validState[value.State] || !validScope[value.Scope] || len(value.AssertionRefs) > 64 {
		return fmt.Errorf("invalid evidence verification")
	}
	for _, ref := range value.AssertionRefs {
		if err := validateDecisionArtifactRef(ref); err != nil {
			return err
		}
	}
	return nil
}

func validateMandatoryReasons(reasons []string) error {
	valid := map[string]bool{"request_input": true, "required_criterion": true, "failed_assertion": true, "critical_assumption": true, "invalidation": true, "outside_view_minimum": true, "independence_minimum": true}
	if len(reasons) > 16 {
		return fmt.Errorf("too many mandatory evidence reasons")
	}
	seen := make(map[string]bool, len(reasons))
	for _, reason := range reasons {
		if !valid[reason] || seen[reason] {
			return fmt.Errorf("invalid or duplicate mandatory evidence reason %q", reason)
		}
		seen[reason] = true
	}
	return nil
}

func validatePrimaryEvidenceBaseRate(content string) error {
	var value struct {
		ReferenceClass string `json:"reference_class"`
		Metric         string `json:"metric"`
		SampleSize     uint64 `json:"sample_size"`
		Distribution   struct {
			Mean   float64 `json:"mean"`
			Median float64 `json:"median"`
			P10    float64 `json:"p10"`
			P90    float64 `json:"p90"`
		} `json:"distribution"`
		Source      DecisionArtifactRef `json:"source"`
		Limitations []string            `json:"limitations"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode typed base rate: %w", err)
	}
	d := value.Distribution
	if strings.TrimSpace(value.ReferenceClass) == "" || strings.TrimSpace(value.Metric) == "" || value.SampleSize == 0 ||
		math.IsNaN(d.Mean) || math.IsInf(d.Mean, 0) || math.IsNaN(d.Median) || math.IsInf(d.Median, 0) ||
		math.IsNaN(d.P10) || math.IsInf(d.P10, 0) || math.IsNaN(d.P90) || math.IsInf(d.P90, 0) || d.P10 > d.Median || d.Median > d.P90 ||
		validateDecisionArtifactRef(value.Source) != nil {
		return fmt.Errorf("typed base rate is invalid")
	}
	return nil
}

type primaryEvidenceDisjointSet struct {
	parent map[string]string
}

func newPrimaryEvidenceDisjointSet() *primaryEvidenceDisjointSet {
	return &primaryEvidenceDisjointSet{parent: make(map[string]string)}
}

func (d *primaryEvidenceDisjointSet) find(value string) string {
	parent, exists := d.parent[value]
	if !exists {
		d.parent[value] = value
		return value
	}
	if parent != value {
		d.parent[value] = d.find(parent)
	}
	return d.parent[value]
}

func (d *primaryEvidenceDisjointSet) union(left, right string) {
	leftRoot, rightRoot := d.find(left), d.find(right)
	if leftRoot == rightRoot {
		return
	}
	if leftRoot < rightRoot {
		d.parent[rightRoot] = leftRoot
	} else {
		d.parent[leftRoot] = rightRoot
	}
}

func normalizePrimaryEvidenceRootGroups(input []PrimaryEvidenceCandidate) []PrimaryEvidenceCandidate {
	result := slices.Clone(input)
	dsu := newPrimaryEvidenceDisjointSet()
	for _, candidate := range result {
		contentNode := "content:" + candidate.Source.SourceArtifact.SHA256
		dsu.find(contentNode)
		for _, root := range candidate.RootIdentities {
			dsu.union(contentNode, "root:"+root)
		}
	}
	componentRoots := make(map[string][]string)
	for node := range dsu.parent {
		if !strings.HasPrefix(node, "root:") {
			continue
		}
		component := dsu.find(node)
		componentRoots[component] = append(componentRoots[component], strings.TrimPrefix(node, "root:"))
	}
	for index := range result {
		component := dsu.find("content:" + result[index].Source.SourceArtifact.SHA256)
		roots := componentRoots[component]
		slices.Sort(roots)
		result[index].RootIdentities = slices.Compact(slices.Clone(roots))
	}
	return result
}

func contentMediaType(format string) string {
	if format == "json" {
		return "application/json"
	}
	return "text/plain"
}
