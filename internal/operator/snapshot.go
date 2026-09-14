package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

const snapshotHashDomain = "hufu-operator-snapshot-v1\x00"

func FinalizeSnapshot(snapshot OperatorSnapshot) (OperatorSnapshot, error) {
	snapshot = NormalizeSnapshot(snapshot)
	if err := ValidateSnapshot(snapshot); err != nil {
		return OperatorSnapshot{}, err
	}
	id, err := ComputeSnapshotID(snapshot)
	if err != nil {
		return OperatorSnapshot{}, err
	}
	snapshot.SnapshotID = id
	return snapshot, nil
}

func NormalizeSnapshot(snapshot OperatorSnapshot) OperatorSnapshot {
	snapshot.Outcome.GoalSatisfied = clonePointer(snapshot.Outcome.GoalSatisfied)
	snapshot.Learning.Exposures = clonePointer(snapshot.Learning.Exposures)
	snapshot.Learning.Applied = clonePointer(snapshot.Learning.Applied)
	snapshot.Learning.VerifiedSupport = clonePointer(snapshot.Learning.VerifiedSupport)
	snapshot.Learning.EligiblePromotions = clonePointer(snapshot.Learning.EligiblePromotions)
	snapshot.Activity.RawTaskStates = cloneSorted(snapshot.Activity.RawTaskStates)
	snapshot.Activity.RawReasonCodes = cloneSorted(snapshot.Activity.RawReasonCodes)
	snapshot.Integrity.ReasonCodes = cloneSorted(snapshot.Integrity.ReasonCodes)
	snapshot.Blockers = slices.Clone(snapshot.Blockers)
	if snapshot.Blockers == nil {
		snapshot.Blockers = []DiagnosticView{}
	}
	slices.SortFunc(snapshot.Blockers, func(left, right DiagnosticView) int {
		if order := compareString(left.Code, right.Code); order != 0 {
			return order
		}
		return compareString(left.Ref, right.Ref)
	})
	snapshot.LatestChanges = slices.Clone(snapshot.LatestChanges)
	if snapshot.LatestChanges == nil {
		snapshot.LatestChanges = []ChangeView{}
	}
	for index := range snapshot.LatestChanges {
		snapshot.LatestChanges[index].Refs = cloneSorted(snapshot.LatestChanges[index].Refs)
	}
	slices.SortFunc(snapshot.LatestChanges, func(left, right ChangeView) int {
		if left.EventOrdinal < right.EventOrdinal {
			return -1
		}
		if left.EventOrdinal > right.EventOrdinal {
			return 1
		}
		return compareString(left.EventID, right.EventID)
	})
	snapshot.RoleTargets = slices.Clone(snapshot.RoleTargets)
	if snapshot.RoleTargets == nil {
		snapshot.RoleTargets = []RoleTargetView{}
	}
	slices.SortFunc(snapshot.RoleTargets, func(left, right RoleTargetView) int {
		return compareString(left.Role, right.Role)
	})
	snapshot.SecondaryActions = slices.Clone(snapshot.SecondaryActions)
	if snapshot.SecondaryActions == nil {
		snapshot.SecondaryActions = []ActionSuggestion{}
	}
	for index := range snapshot.SecondaryActions {
		normalizeAction(&snapshot.SecondaryActions[index])
	}
	slices.SortFunc(snapshot.SecondaryActions, func(left, right ActionSuggestion) int {
		return compareString(left.ID, right.ID)
	})
	if snapshot.PrimaryAction != nil {
		action := *snapshot.PrimaryAction
		normalizeAction(&action)
		snapshot.PrimaryAction = &action
	}
	return snapshot
}

func ValidateSnapshot(snapshot OperatorSnapshot) error {
	if snapshot.SchemaVersion != SchemaVersion {
		return fmt.Errorf("operator snapshot schema version %d is unsupported", snapshot.SchemaVersion)
	}
	if !validActivity(snapshot.Activity.State) {
		return fmt.Errorf("operator snapshot activity %q is invalid", snapshot.Activity.State)
	}
	if !validAttention(snapshot.Attention) {
		return fmt.Errorf("operator snapshot attention %q is invalid", snapshot.Attention)
	}
	if !contains([]string{"valid", "degraded", "invalid", "unknown"}, snapshot.Integrity.Status) {
		return fmt.Errorf("operator snapshot integrity %q is invalid", snapshot.Integrity.Status)
	}
	if !contains([]string{"verified", "invalid", "unavailable"}, snapshot.Integrity.EventChain) {
		return fmt.Errorf("operator snapshot event chain %q is invalid", snapshot.Integrity.EventChain)
	}
	if !contains([]string{"consistent", "drift", "unavailable"}, snapshot.Integrity.Projection) {
		return fmt.Errorf("operator snapshot projection %q is invalid", snapshot.Integrity.Projection)
	}
	if !contains([]string{"legacy_base", "exact", "root", "default"}, snapshot.Scope.RequestedSemantics) {
		return fmt.Errorf("operator snapshot workspace semantics %q is invalid", snapshot.Scope.RequestedSemantics)
	}
	if !contains([]string{"verified", "absent", "ambiguous", "conflict", "unknown"}, snapshot.Scope.BindingStatus) {
		return fmt.Errorf("operator snapshot binding %q is invalid", snapshot.Scope.BindingStatus)
	}
	if !contains([]string{"explicit", "active_binding", "single_candidate", "legacy", "unknown"}, snapshot.Scope.SelectionSource) {
		return fmt.Errorf("operator snapshot selection source %q is invalid", snapshot.Scope.SelectionSource)
	}
	if snapshot.Scope.BindingStatus == "verified" && (snapshot.Scope.WorkspaceExact == "" || snapshot.Scope.RunID == "" || snapshot.Scope.BranchID == "") {
		return fmt.Errorf("verified operator snapshot scope is incomplete")
	}
	if !contains([]string{"", "completed", "unverified", "partial", "blocked", "failed", "cancelled", "stalled"}, snapshot.Outcome.RunOutcome) {
		return fmt.Errorf("operator snapshot outcome %q is invalid", snapshot.Outcome.RunOutcome)
	}
	if !contains([]string{"connected", "disconnected", "not_applicable", "unknown"}, snapshot.Freshness.LiveState) {
		return fmt.Errorf("operator snapshot live state %q is invalid", snapshot.Freshness.LiveState)
	}
	if snapshot.Freshness.EventOrdinal < 0 {
		return fmt.Errorf("operator snapshot event ordinal must not be negative")
	}
	if !contains([]string{"available", "not_applicable", "unavailable", "unknown"}, snapshot.Learning.Status) {
		return fmt.Errorf("operator snapshot learning status %q is invalid", snapshot.Learning.Status)
	}
	for _, role := range snapshot.RoleTargets {
		if role.Role == "" || !contains([]string{"verified", "unverified", "unavailable", "unknown"}, role.Availability) {
			return fmt.Errorf("operator snapshot role target %q has invalid availability %q", role.Role, role.Availability)
		}
	}
	for _, diagnostic := range snapshot.Blockers {
		if !contains([]string{"info", "warning", "error"}, diagnostic.Severity) {
			return fmt.Errorf("operator snapshot diagnostic %q has invalid severity %q", diagnostic.Code, diagnostic.Severity)
		}
	}
	counters := []struct {
		name  string
		value *int64
	}{
		{name: "exposures", value: snapshot.Learning.Exposures},
		{name: "applied", value: snapshot.Learning.Applied},
		{name: "verified_support", value: snapshot.Learning.VerifiedSupport},
		{name: "eligible_promotions", value: snapshot.Learning.EligiblePromotions},
	}
	for _, counter := range counters {
		if counter.value != nil && *counter.value < 0 {
			return fmt.Errorf("operator snapshot learning counter %s must not be negative", counter.name)
		}
	}
	return nil
}

func ComputeSnapshotID(snapshot OperatorSnapshot) (string, error) {
	snapshot = NormalizeSnapshot(snapshot)
	type freshnessHash struct {
		EventID         string `json:"event_id"`
		EventHash       string `json:"event_hash"`
		EventOrdinal    int64  `json:"event_ordinal"`
		ContextRevision string `json:"context_revision"`
		StaleReason     string `json:"stale_reason"`
	}
	type diagnosticHash struct {
		Code     string `json:"code"`
		Severity string `json:"severity"`
		Ref      string `json:"ref"`
	}
	type snapshotHashInput struct {
		SchemaVersion int              `json:"schema_version"`
		Scope         ResolvedScope    `json:"scope"`
		Activity      ActivityView     `json:"activity"`
		Outcome       OutcomeView      `json:"outcome"`
		Integrity     IntegrityView    `json:"integrity"`
		Freshness     freshnessHash    `json:"freshness"`
		Attention     string           `json:"attention"`
		Blockers      []diagnosticHash `json:"blockers"`
		LatestChanges []ChangeView     `json:"latest_changes"`
		RoleTargets   []RoleTargetView `json:"role_targets"`
		Learning      LearningView     `json:"learning"`
	}
	blockers := make([]diagnosticHash, 0, len(snapshot.Blockers))
	for _, blocker := range snapshot.Blockers {
		blockers = append(blockers, diagnosticHash{Code: blocker.Code, Severity: blocker.Severity, Ref: blocker.Ref})
	}
	input := snapshotHashInput{
		SchemaVersion: snapshot.SchemaVersion,
		Scope:         snapshot.Scope,
		Activity:      snapshot.Activity,
		Outcome:       snapshot.Outcome,
		Integrity:     snapshot.Integrity,
		Freshness: freshnessHash{
			EventID:         snapshot.Freshness.EventID,
			EventHash:       snapshot.Freshness.EventHash,
			EventOrdinal:    snapshot.Freshness.EventOrdinal,
			ContextRevision: snapshot.Freshness.ContextRevision,
			StaleReason:     snapshot.Freshness.StaleReason,
		},
		Attention:     snapshot.Attention,
		Blockers:      blockers,
		LatestChanges: snapshot.LatestChanges,
		RoleTargets:   snapshot.RoleTargets,
		Learning:      snapshot.Learning,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("marshal operator snapshot hash input: %w", err)
	}
	digest := sha256.Sum256(append([]byte(snapshotHashDomain), encoded...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func normalizeAction(action *ActionSuggestion) {
	action.Argv = cloneSortedPreservingOrder(action.Argv)
	action.SourceRefs = cloneSorted(action.SourceRefs)
}

func cloneSortedPreservingOrder(values []string) []string {
	result := slices.Clone(values)
	if result == nil {
		return []string{}
	}
	return result
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func compareString(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func contains(values []string, target string) bool {
	return slices.Contains(values, target)
}

func validActivity(value string) bool {
	return contains([]string{
		ActivityUnconfigured, ActivityReady, ActivityPreflight, ActivityPlanning,
		ActivityExecuting, ActivityVerifying, ActivityWaitingInput, ActivityWaitingApproval,
		ActivityBlocked, ActivityWrappingUp, ActivityInterrupted, ActivityFinished, ActivityUnknown,
	}, value)
}

func validAttention(value string) bool {
	return contains([]string{
		AttentionNone, AttentionInformational, AttentionActionAvailable,
		AttentionReviewRequired, AttentionHumanRequired, AttentionUnknown,
	}, value)
}
